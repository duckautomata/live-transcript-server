package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kennygrant/sanitize"
)

func (app *App) RegisterRoutes(mux *http.ServeMux) {
	err := os.MkdirAll(app.TempDir, 0755)
	if err != nil {
		slog.Error("cannot create tmp folder", "func", "RegisterRoutes", "err", err)
	}

	for _, cs := range app.Channels {
		err := os.MkdirAll(cs.BaseMediaFolder, 0755)
		if err != nil {
			slog.Error("cannot create base media folder", "key", cs.Key, "func", "RegisterRoutes", "err", err)
		}
	}

	slog.Info("registering endpoints", "func", "RegisterRoutes")

	// API Key protected routes
	mux.HandleFunc("POST /{channel}/activate", app.apiKeyMiddleware(app.activateHandler))
	mux.HandleFunc("POST /{channel}/deactivate", app.apiKeyMiddleware(app.deactivateHandler))
	mux.HandleFunc("POST /{channel}/sync", app.apiKeyMiddleware(app.syncHandler))
	mux.HandleFunc("POST /{channel}/line", app.apiKeyMiddleware(app.lineHandler))
	mux.HandleFunc("POST /{channel}/media/{id}", app.apiKeyMiddleware(app.mediaHandler))
	mux.HandleFunc("GET /{channel}/statuscheck", app.apiKeyMiddleware(app.statuscheckHandler))

	// Public routes
	mux.HandleFunc("GET /{channel}/websocket", app.wsHandler)
	mux.HandleFunc("GET /{channel}/stream/{streamID}/{filename}", app.streamHandler)
	mux.HandleFunc("GET /{channel}/download/{streamID}/{filename}", app.downloadHandler)
	mux.HandleFunc("GET /{channel}/frame/{streamID}/{filename}", app.getFrameHandler)
	mux.HandleFunc("GET /{channel}/transcript/{streamID}", app.getTranscriptHandler)
	mux.HandleFunc("POST /{channel}/clip", app.postClipHandler)
	mux.HandleFunc("POST /{channel}/trim", app.postTrimHandler)
}

func (app *App) apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if app.ApiKey == "" {
			next(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key != app.ApiKey {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// Adds the necessary CORS headers to all responses.
// Accepts any origin.
func CorsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set the allowed origin.
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Set the allowed methods
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")

		// Set the allowed headers
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")

		// Expose headers so clients can read them (e.g. filename from Content-Disposition)
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")

		// Cache the preflight response for 24 hours to reduce OPTIONS requests
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight OPTIONS requests
		// This is sent by the browser to check permissions *before*
		// sending the actual request.
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Serve the next handler in the chain
		next.ServeHTTP(w, r)
	})
}

// Activate a stream and send a message to all clients.
// Returns true if the stream was activated and a message was sent, false otherwise.
func (app *App) activateStream(ctx context.Context, cs *ChannelState, activeId string, activeTitle string, startTime string, mediaType string) bool {

	// 1. Get current stream state from DB
	currentStream, err := app.GetStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream from db", "key", cs.Key, "err", err)
		return false
	}

	var msg WebSocketMessage

	// If no stream exists, or the ID is different, it's a new stream
	if currentStream == nil || currentStream.ActiveID != activeId {
		StreamAudioPlayed.WithLabelValues(cs.Key).Set(0)
		StreamFramesDownloads.WithLabelValues(cs.Key).Set(0)
		StreamAudioClipped.WithLabelValues(cs.Key).Set(0)
		StreamVideoClipped.WithLabelValues(cs.Key).Set(0)

		// Remove previous stream activation metric when a new stream is activated. If there was a previous active stream.
		if currentStream != nil && currentStream.ActiveID != "" {
			deleted := ActivatedStreams.DeleteLabelValues(cs.Key, currentStream.ActiveID, currentStream.ActiveTitle)
			if deleted {
				slog.Info("removed old stream activation metric", "key", cs.Key, "func", "activateStream", "oldStreamID", currentStream.ActiveID)
			}
		}

		newStream := &Stream{
			ChannelID:   cs.Key,
			ActiveID:    activeId,
			ActiveTitle: activeTitle,
			StartTime:   startTime,
			IsLive:      true,
			MediaType:   mediaType,
		}

		// Deactivate previous stream if it was live
		if currentStream != nil && currentStream.IsLive {
			if err := app.SetStreamLive(ctx, cs.Key, currentStream.ActiveID, false); err != nil {
				slog.Error("failed to deactivate previous stream", "key", cs.Key, "streamID", currentStream.ActiveID, "err", err)
			}
		}

		if err := app.UpsertStream(ctx, newStream); err != nil {
			slog.Error("failed to upsert new stream", "key", cs.Key, "err", err)
			return false
		}

		// Set new active stream folder
		cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, activeId)
		if err := os.MkdirAll(cs.ActiveMediaFolder, 0755); err != nil {
			slog.Error("failed to create media folder", "key", cs.Key, "path", cs.ActiveMediaFolder, "err", err)
			return false
		}

		// Rotation Logic: Query DB for all streams to apply retention policy
		allStreams, err := app.GetAllStreams(ctx, cs.Key)
		if err == nil {
			// allStreams is sorted by start_time DESC (newest first).
			// We want to keep 'keepCount' streams (active + NumPastStreams).
			keepCount := cs.NumPastStreams + 1
			if len(allStreams) > keepCount {
				for i := keepCount; i < len(allStreams); i++ {
					streamToDelete := allStreams[i]
					// Delete from DB (Stream meta)
					if err := app.DeleteStream(ctx, cs.Key, streamToDelete.ActiveID); err != nil {
						slog.Error("failed to delete stream from db", "key", cs.Key, "streamID", streamToDelete.ActiveID, "err", err)
						continue
					}
					// Delete Transcript from DB
					if err := app.DeleteTranscript(ctx, cs.Key, streamToDelete.ActiveID); err != nil {
						slog.Error("failed to delete transcript from db", "key", cs.Key, "streamID", streamToDelete.ActiveID, "err", err)
					}
					// Delete from Filesystem
					pathToDelete := filepath.Join(cs.BaseMediaFolder, streamToDelete.ActiveID)
					slog.Info("deleting old stream folder", "key", cs.Key, "path", pathToDelete)
					os.RemoveAll(pathToDelete)
				}
			}
		} else {
			slog.Error("failed to get all streams for rotation", "key", cs.Key, "err", err)
		}

		// Broadcast pastStreams event
		pastStreams, err := app.GetPastStreams(ctx, cs.Key, activeId)
		if err == nil {
			pastStreamsMsg := WebSocketMessage{
				Event: EventPastStreams,
				Data:  EventPastStreamsData{Streams: pastStreams},
			}
			cs.broadcast(pastStreamsMsg)
		} else {
			slog.Error("failed to get past streams for broadcast", "key", cs.Key, "err", err)
		}

		data := EventNewStreamData{
			ActiveID:    newStream.ActiveID,
			ActiveTitle: newStream.ActiveTitle,
			StartTime:   newStream.StartTime,
			MediaType:   newStream.MediaType,
			IsLive:      newStream.IsLive,
		}
		msg = WebSocketMessage{
			Event: EventNewStream,
			Data:  data,
		}
		slog.Debug("received new stream id, sending newstream event", "key", cs.Key, "func", "activateStream", "activeID", activeId)

	} else {
		// Same stream ID
		if !currentStream.IsLive {
			// Reactivate: Update the specific stream to be live
			if err := app.SetStreamLive(ctx, cs.Key, currentStream.ActiveID, true); err != nil {
				slog.Error("failed to set stream live", "key", cs.Key, "err", err)
				return false
			}
			currentStream.IsLive = true
			data := EventStatusData{
				ActiveID:    currentStream.ActiveID,
				ActiveTitle: currentStream.ActiveTitle,
				IsLive:      currentStream.IsLive,
			}
			msg = WebSocketMessage{
				Event: EventStatus,
				Data:  data,
			}
			slog.Debug("reactivating existing stream, sending status event", "key", cs.Key, "func", "activateStream", "activeID", activeId)
		} else {
			// Already active
			slog.Debug("stream is already active, skipping event", "key", cs.Key, "func", "activateStream", "activeID", activeId)
		}
	}

	if msg.Event != "" {
		cs.broadcast(msg)
		return true
	}

	return false
}

// Deactivate a stream and send a message to all clients.
// Returns true if the stream was deactivated and a message was sent, false otherwise.
func (app *App) deactivateStream(ctx context.Context, cs *ChannelState, activeId string) bool {

	currentStream, err := app.GetStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream from db", "key", cs.Key, "err", err)
		return false
	}

	var msg WebSocketMessage
	if currentStream != nil && currentStream.ActiveID == activeId && currentStream.IsLive {
		deleted := ActivatedStreams.DeleteLabelValues(cs.Key, currentStream.ActiveID, currentStream.ActiveTitle)
		if deleted {
			slog.Info("successfully removed stream metric on deactivation", "key", cs.Key, "func", "deactivateStream", "streamID", activeId)
		} else {
			slog.Info("failed to remove stream metric on deactivation", "key", cs.Key, "func", "deactivateStream", "streamID", activeId)
		}

		if err := app.SetStreamLive(ctx, cs.Key, activeId, false); err != nil {
			slog.Error("failed to set stream not live", "key", cs.Key, "err", err)
			return false
		}

		// msg = fmt.Sprintf("![]status\n%s\n%s\n%v", currentStream.ActiveID, currentStream.ActiveTitle, false)
		data := EventStatusData{
			ActiveID:    currentStream.ActiveID,
			ActiveTitle: currentStream.ActiveTitle,
			IsLive:      false,
		}
		msg = WebSocketMessage{
			Event: EventStatus,
			Data:  data,
		}
		slog.Debug("deactivating stream", "key", cs.Key, "func", "deactivateStream", "activeID", activeId)
	}

	if msg.Event != "" {
		cs.broadcast(msg)
		return true
	}

	return false
}

// Handle a sync request from the worker. Sets current stream state and replaces transcript.
func (app *App) syncHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "syncHandler", "key", key)
		return
	}

	uploadStartTime := time.Now()

	decoder := json.NewDecoder(r.Body)
	var data WorkerData
	if err := decoder.Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", cs.Key, "func", "syncHandler", "err", err)
		return
	}
	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	// Update Stream Info
	stream := &Stream{
		ChannelID:   cs.Key,
		ActiveID:    data.ActiveID,
		ActiveTitle: data.ActiveTitle,
		StartTime:   data.StartTime,
		IsLive:      data.IsLive,
		MediaType:   data.MediaType,
	}
	if err := app.UpsertStream(r.Context(), stream); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to upsert stream", "err", err)
		return
	}

	// For every line in the transcript, check if we have the media file.
	// If we do, set MediaAvailable to true.
	for i := range data.Transcript {
		line := &data.Transcript[i]

		// Check for media in ActiveMediaFolder
		if cs.ActiveMediaFolder != "" {
			m4aPath := filepath.Join(cs.ActiveMediaFolder, fmt.Sprintf("%d.m4a", line.ID))
			rawPath := filepath.Join(cs.ActiveMediaFolder, fmt.Sprintf("%d.raw", line.ID))
			if _, err := os.Stat(m4aPath); err == nil {
				line.MediaAvailable = true
			} else if _, err := os.Stat(rawPath); err == nil {
				line.MediaAvailable = true
			}
		} else if data.ActiveID != "" {
			cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, data.ActiveID)
			m4aPath := filepath.Join(cs.ActiveMediaFolder, fmt.Sprintf("%d.m4a", line.ID))
			rawPath := filepath.Join(cs.ActiveMediaFolder, fmt.Sprintf("%d.raw", line.ID))
			if _, err := os.Stat(m4aPath); err == nil {
				line.MediaAvailable = true
			} else if _, err := os.Stat(rawPath); err == nil {
				line.MediaAvailable = true
			}
		}
	}

	if err := app.ReplaceTranscript(r.Context(), cs.Key, data.ActiveID, data.Transcript); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to replace transcript", "err", err)
		return
	}

	app.broadcastNewLine(r.Context(), cs, data.ActiveID, uploadTime, nil)

	if uploadTime > 5*1000 {
		slog.Warn("slow upload time", "key", cs.Key, "func", "syncHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}
	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", cs.Key, "func", "syncHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Sync data received and processed successfully"))
}

// Handle a new line request from the worker. Appends a new line to the transcript.
func (app *App) lineHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "lineHandler", "key", key)
		return
	}

	uploadStartTime := time.Now()

	decoder := json.NewDecoder(r.Body)
	var data Line
	if err := decoder.Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", cs.Key, "func", "lineHandler", "err", err)
		return
	}
	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	// Get Active Stream to determine where to append line
	stream, err := app.GetStream(r.Context(), cs.Key)
	if err != nil || stream == nil {
		http.Error(w, "No active stream found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("no active stream for line append", "key", cs.Key, "func", "lineHandler")
		return
	}

	lastID, err := app.GetLastLineID(r.Context(), cs.Key, stream.ActiveID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get last line id", "err", err)
		return
	}
	expectedID := lastID + 1
	// Special case: if DB is empty (lastID = -1), we expect 0.
	// This works because -1 + 1 = 0. And we expect the first line to be 0.

	// Force MediaAvailable to false for new lines
	data.MediaAvailable = false

	if data.ID != expectedID {
		http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
		ServerOOS.Inc()
		slog.Warn("line id mismatch. Requesting worker to send current state.", "key", cs.Key, "func", "lineHandler", "lastID", lastID, "newLineID", data.ID)
		return
	}

	if err := app.InsertTranscriptLine(r.Context(), cs.Key, stream.ActiveID, data); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to insert transcript line", "err", err)
		return
	}

	app.broadcastNewLine(r.Context(), cs, stream.ActiveID, uploadTime, &data)
	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", cs.Key, "func", "lineHandler", "uploadTimeMs", time.Since(uploadStartTime).Milliseconds(), "processingTimeMs", time.Since(processStartTime).Milliseconds(), "lineId", data.ID)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Line data received and processed successfully"))
}

// Handle a media file upload. Saves the file to the media folder and converts it.
func (app *App) mediaHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "mediaHandler", "key", key)
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Unable to get file", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	defer file.Close()

	// Use ActiveMediaFolder for uploads
	if cs.ActiveMediaFolder == "" {
		// Try to recover from DB
		stream, err := app.GetStream(r.Context(), cs.Key)
		if err == nil && stream != nil {
			cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, stream.ActiveID)
			os.MkdirAll(cs.ActiveMediaFolder, 0755)
		} else {
			http.Error(w, "No active stream", http.StatusBadRequest)
			Http400Errors.Inc()
			slog.Warn("media upload attempted with no active stream", "key", cs.Key)
			return
		}
	}

	rawFilePath := filepath.Join(cs.ActiveMediaFolder, fmt.Sprintf("%d.raw", id))
	dst, err := os.Create(rawFilePath)
	if err != nil {
		http.Error(w, "Unable to create file", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to create raw file", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}

	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		http.Error(w, "Unable to save file", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to save raw file", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}
	dst.Close() // Close explicitly to flush write

	// Convert to m4a
	m4aFile := ChangeExtension(rawFilePath, ".m4a")
	if err := FfmpegConvert(rawFilePath, m4aFile); err != nil {
		os.Remove(rawFilePath)
		os.Remove(m4aFile)
		http.Error(w, "Unable to convert media", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to convert media", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}

	// Extract first frame if video
	stream, err := app.GetStream(r.Context(), cs.Key)
	if err == nil && stream != nil && stream.MediaType == "video" {
		jpgFile := ChangeExtension(rawFilePath, ".jpg")
		if err := FfmpegExtractFrame(rawFilePath, jpgFile, 480); err != nil {
			slog.Error("unable to extract frame", "key", cs.Key, "func", "mediaHandler", "err", err)
			// Don't fail the request if frame extraction fails
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Media received and processed successfully"))

	// Update DB and broadcast
	success := true
	// Get Active Stream explicitly for DB updates
	streamForDB, err := app.GetStream(r.Context(), cs.Key)
	activeID := ""
	if err == nil && streamForDB != nil {
		activeID = streamForDB.ActiveID
	}

	if activeID != "" {
		if err := app.SetMediaAvailable(r.Context(), cs.Key, activeID, id, true); err != nil {
			if err.Error() == "line not found" {
				// This usually fails if the media was uploaded before the line was added.
				// Wait 500ms then try again to give the line time to be added.
				slog.Warn("line not found, retrying", "key", cs.Key, "id", id)
				time.Sleep(500 * time.Millisecond)
				if err := app.SetMediaAvailable(r.Context(), cs.Key, activeID, id, true); err != nil {
					slog.Error("failed to set media available on retry", "key", cs.Key, "id", id, "err", err)
					Http500Errors.Inc()
					success = false
				}
			} else {
				slog.Error("failed to set media available", "key", cs.Key, "id", id, "err", err)
				Http500Errors.Inc()
				success = false
			}
		}
	} else {
		slog.Warn("cannot set media available: no active stream", "key", cs.Key, "id", id)
	}

	// We don't fail the request because the file is saved and converted.
	if success && activeID != "" {
		// Broadcast new media availability
		// Get last 100 available IDs
		ids, err := app.GetLastAvailableMediaIDs(r.Context(), cs.Key, activeID, 100)
		if err != nil {
			slog.Error("failed to get last available media ids. Fallback to single id", "key", cs.Key, "err", err)
			ids = []int{id}
		}
		app.broadcastNewMedia(cs, ids)
	}
}

// Handle an activate request from the worker. Activates a stream and sends a message to all clients.
func (app *App) activateHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "activateHandler", "key", key)
		return
	}
	processStartTime := time.Now()

	// Parse the query parameters
	query := r.URL.Query()
	streamID := query.Get("id")
	title := query.Get("title")
	startTime := query.Get("startTime")
	mediaType := query.Get("mediaType")

	// Check if the required parameters are present
	if streamID == "" || title == "" || startTime == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid parameters", "key", cs.Key, "func", "activateHandler", "streamID", streamID, "title", title, "startTime", startTime)
		return
	}

	var startTimeUnix int64

	// Try to parse the provided startTime; if it fails, use the current time.
	parsedTime, err := strconv.ParseInt(startTime, 10, 64)
	if err != nil {
		slog.Warn("invalid or empty startTime received, using current system time", "key", cs.Key, "func", "activateHandler", "receivedTime", startTime)
		startTimeUnix = time.Now().Unix()
	} else {
		startTimeUnix = parsedTime
	}

	// Convert the final timestamp back to a string for use in other functions.
	finalStartTimeStr := strconv.FormatInt(startTimeUnix, 10)

	activated := app.activateStream(r.Context(), cs, streamID, title, finalStartTimeStr, mediaType)

	if activated {
		ActivatedStreams.WithLabelValues(cs.Key, streamID, title).Set(float64(startTimeUnix))
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully activated", cs.Key))
		slog.Debug("activated stream", "key", cs.Key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID, "mediaType", mediaType)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream is already activated", cs.Key))
		slog.Debug("id already activated", "key", cs.Key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	}
}

func (app *App) deactivateHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "deactivateHandler", "key", key)
		return
	}
	processStartTime := time.Now()

	// Parse the query parameters
	query := r.URL.Query()
	streamID := query.Get("id")

	// Check if the required parameters are present
	if streamID == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid parameters, streamID is empty", "key", cs.Key, "func", "deactivateHandler")
		return
	}

	deactivated := app.deactivateStream(r.Context(), cs, streamID)

	if deactivated {
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully deactivated", cs.Key))
		slog.Debug("deactivated stream", "key", cs.Key, "func", "deactivateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream was not deactivated", cs.Key))
		slog.Debug("id already deactivated", "key", cs.Key, "func", "deactivateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	}
}

func (app *App) statuscheckHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "statuscheckHandler", "key", key)
		return
	}

	cs.ClientsLock.Lock()
	size := cs.ClientConnections
	cs.ClientsLock.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write(fmt.Appendf(nil, "Current number of clients: %d", size))
}

func (app *App) streamHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "streamHandler", "key", key)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", cs.Key, "func", "streamHandler", "method", r.Method)
		return
	}

	stream, err := app.GetStream(r.Context(), cs.Key)
	mediaType := "none"
	if err == nil && stream != nil {
		mediaType = stream.MediaType
	}

	// Helper to extract id and format
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")

	// We allow streaming from past streams if they exist.
	// if requestedStreamID != activeID { ... }

	if mediaType == "none" {
		http.Error(w, "Media stream is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot retrieve media. Media type is none", "key", cs.Key, "func", "streamHandler")
		return
	}
	processStartTime := time.Now()

	ext := filepath.Ext(filename)
	idStr := strings.TrimSuffix(filename, ext)
	allowedExts := map[string]string{
		".m4a": "audio/mp4",
		".mp4": "video/mp4",
		".mp3": "audio/mpeg",
	}
	contentType, ok := allowedExts[ext]
	if !ok {
		http.Error(w, "Invalid extension", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, fmt.Sprintf("%s%s", idStr, ext))

	// Check if the file exists
	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			Http400Errors.Inc()
			slog.Warn("file not found for the requested id", "key", cs.Key, "func", "streamHandler", "id", idStr, "ext", ext)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to check file", "key", cs.Key, "func", "streamHandler", "id", idStr, "err", err)
		return
	}

	switch ext {
	case ".m4a", ".mp3":
		TotalAudioPlayed.WithLabelValues(cs.Key).Inc()
		StreamAudioPlayed.WithLabelValues(cs.Key).Inc()
	case ".mp4":
		TotalVideoPlayed.WithLabelValues(cs.Key).Inc()
		StreamVideoPlayed.WithLabelValues(cs.Key).Inc()
	}

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow stream processing time", "key", cs.Key, "func", "streamHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "id", idStr)
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

func (app *App) getFrameHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "getFrameHandler", "key", key)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", cs.Key, "func", "getFrameHandler", "method", r.Method)
		return
	}

	stream, err := app.GetStream(r.Context(), cs.Key)
	mediaType := "none"
	activeID := ""
	if err == nil && stream != nil {
		mediaType = stream.MediaType
		activeID = stream.ActiveID
	}

	if mediaType != "video" {
		http.Error(w, "Frame download is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot retrieve frame. Media type is not video", "key", cs.Key, "func", "getFrameHandler", "mediaType", mediaType)
		return
	}
	processStartTime := time.Now()

	// Helper to extract id and format
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	ext := filepath.Ext(filename)

	if requestedStreamID != "" && requestedStreamID != activeID {
		http.Error(w, "Stream ID mismatch", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream id mismatch", "key", cs.Key, "func", "getFrameHandler", "requestedStreamID", requestedStreamID, "activeID", activeID)
		return
	}

	if ext != ".jpg" {
		http.Error(w, "Invalid extension", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid extension", "key", cs.Key, "func", "getFrameHandler", "ext", ext)
		return
	}

	idStr := strings.TrimSuffix(filename, ".jpg")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("unable to convert id to int", "key", cs.Key, "func", "getFrameHandler", "id", idStr, "err", err)
		return
	}

	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, fmt.Sprintf("%d.jpg", id))

	// Check if the file exists
	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "No frame found", http.StatusNotFound)
			// Don't consider this an error as it will flood the logs.
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to check frame file", "key", cs.Key, "func", "getFrameHandler", "id", id, "err", err)
		return
	}

	TotalFramesDownloads.WithLabelValues(cs.Key).Inc()
	StreamFramesDownloads.WithLabelValues(cs.Key).Inc()

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow frame processing time", "key", cs.Key, "func", "getFrameHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "id", idStr)
	}

	w.Header().Set("Cache-Control", "public, max-age=31536000") // Cache for 1 year, as frames are immutable for an ID. The browser will grab new frames when the stream id changes.
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, filePath)
}

func (app *App) postClipHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "postClipHandler", "key", key)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a POST", "key", cs.Key, "func", "postClipHandler", "method", r.Method)
		return
	}

	stream, err := app.GetStream(r.Context(), cs.Key)
	mediaType := "none"
	if err == nil && stream != nil {
		mediaType = stream.MediaType
	}
	processStartTime := time.Now()

	// Parse JSON body
	var req struct {
		StreamID string `json:"stream_id"`
		Start    int    `json:"start"`
		End      int    `json:"end"`
		Type     string `json:"type"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	start := req.Start
	end := req.End
	reqMediaType := req.Type

	clipExt := ".m4a"
	switch reqMediaType {
	case "mp4":
		if mediaType != "video" {
			http.Error(w, "Video clipping is disabled for this stream", http.StatusMethodNotAllowed)
			Http400Errors.Inc()
			slog.Warn("cannot clip mp4. Media type is not 'video'", "key", cs.Key, "func", "postClipHandler", "mediaType", mediaType)
			return
		}
		clipExt = ".mp4"
	case "mp3":
		clipExt = ".mp3"
	case "m4a", "":
		// Default to m4a
		clipExt = ".m4a"
	default:
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid media type", "key", cs.Key, "func", "postClipHandler", "mediaType", reqMediaType)
		return
	}

	if start < 0 || end <= start || end-start >= app.MaxClipSize {
		slog.Warn("invalid start or end id", "key", cs.Key, "func", "postClipHandler", "start", start, "end", end, "requestedClipSize", 1+end-start, "maxClipSize", app.MaxClipSize)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	uniqueID := fmt.Sprintf("%d-%d-%d", start, end, time.Now().UnixNano())
	streamFolder := filepath.Join(cs.BaseMediaFolder, req.StreamID)
	if _, err := os.Stat(streamFolder); err != nil {
		http.Error(w, "Stream not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream not found", "key", cs.Key, "func", "postClipHandler", "streamID", req.StreamID)
		return
	}
	mergedRawPath, err := cs.MergeRawAudio(streamFolder, req.Start, req.End, uniqueID)
	if err != nil {
		os.Remove(mergedRawPath)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to merge raw audio", "key", cs.Key, "func", "postClipHandler", "startID", start, "endID", end, "err", err)
		return
	}
	defer os.Remove(mergedRawPath) // Delete the merged raw file when done

	mediaFilePath := filepath.Join(streamFolder, uniqueID+clipExt)

	// Note: audio has to be recoded to m4a otherwise it will be broken. Video can be remixed to a different container without any compatibility issues.
	if reqMediaType == "mp4" {
		err = FfmpegRemux(mergedRawPath, mediaFilePath)
		if err == nil {
			// Also create m4a
			m4aPath := filepath.Join(streamFolder, uniqueID+".m4a")
			if err := FfmpegConvert(mergedRawPath, m4aPath); err != nil {
				slog.Error("failed to create sidecar m4a for clip", "key", cs.Key, "id", uniqueID, "err", err)
				Http500Errors.Inc()
				// We don't fail the request, just log it. The MP4 is good.
			}
		}
	} else {
		err = FfmpegConvert(mergedRawPath, mediaFilePath)
	}

	if err != nil {
		os.Remove(mediaFilePath)
		slog.Error("unable to convert raw media to new extension", "key", cs.Key, "func", "postClipHandler", "extension", clipExt, "err", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}

	switch clipExt {
	case ".m4a", ".mp3":
		TotalAudioClipped.WithLabelValues(cs.Key).Inc()
		StreamAudioClipped.WithLabelValues(cs.Key).Inc()
	case ".mp4":
		TotalVideoClipped.WithLabelValues(cs.Key).Inc()
		StreamVideoClipped.WithLabelValues(cs.Key).Inc()
	}

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow clip processing time", "key", cs.Key, "func", "postClipHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "start", start, "end", end, "mediaType", reqMediaType)
	}

	writeJSON(w, map[string]string{
		"status": "success",
		"id":     uniqueID,
	})
}

func (app *App) downloadHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "downloadHandler", "key", key)
		return
	}

	// Public routes
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", cs.Key, "func", "downloadHandler", "method", r.Method)
		return
	}

	// Parse path parameters
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")

	if _, err := os.Stat(filepath.Join(cs.BaseMediaFolder, requestedStreamID)); err != nil {
		http.Error(w, "Stream not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream not found", "key", cs.Key, "func", "downloadHandler", "streamID", requestedStreamID)
		return
	}

	ext := filepath.Ext(filename)
	idStr := strings.TrimSuffix(filename, ext)

	// Check for optional name query param
	queryName := r.URL.Query().Get("name")
	downloadFilename := filename
	if queryName != "" {
		downloadFilename = sanitize.BaseName(queryName) + ext
	}

	contentTypes := map[string]string{
		".m4a": "audio/mp4",
		".mp4": "video/mp4",
		".mp3": "audio/mpeg",
		".jpg": "image/jpeg",
	}

	contentType, ok := contentTypes[ext]
	if !ok {
		http.Error(w, "Invalid file extension", http.StatusBadRequest)
		return
	}

	// Check if file exists in BaseMediaFolder/{streamID}
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, idStr+ext)
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", downloadFilename))
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

func (app *App) postTrimHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "postTrimHandler", "key", key)
		return
	}

	// Parse JSON body
	var req struct {
		StreamID string  `json:"stream_id"`
		ID       string  `json:"id"`
		Start    float64 `json:"start"`
		End      float64 `json:"end"`
		Filename string  `json:"filename"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if _, err := os.Stat(filepath.Join(cs.BaseMediaFolder, req.StreamID)); err != nil {
		http.Error(w, "Stream not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream not found", "key", cs.Key, "func", "postTrimHandler", "streamID", req.StreamID)
		return
	}
	// Validate inputs
	if req.ID == "" || req.Filename == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	// Find source file
	var srcPath string
	extensions := []string{".m4a", ".mp4", ".mp3"}

	for _, e := range extensions {
		path := filepath.Join(cs.BaseMediaFolder, req.StreamID, req.ID+e)
		if _, err := os.Stat(path); err == nil {
			srcPath = path
			break
		}
	}

	if srcPath == "" {
		http.Error(w, "Source file not found", http.StatusNotFound)
		Http400Errors.Inc()
		return
	}

	// Trim to a temporary file, then overwrite the original
	tempTrimmedPath := srcPath + ".tmp"
	if err := FfmpegTrim(srcPath, tempTrimmedPath, req.Start, req.End); err != nil {
		slog.Error("failed to trim file", "err", err)
		http.Error(w, "Failed to trim file", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}

	// Overwrite the original
	if err := os.Rename(tempTrimmedPath, srcPath); err != nil {
		slog.Error("failed to overwrite original file with trimmed version", "err", err)
		http.Error(w, "Failed to save trimmed file", http.StatusInternalServerError)
		Http500Errors.Inc()
		os.Remove(tempTrimmedPath) // Clean up
		return
	}

	// Success. The file is saved on disk. Client can now download it using the ID.
	writeJSON(w, map[string]string{
		"status": "success",
		"id":     req.ID,
	})
}

func (app *App) getTranscriptHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "getTranscriptHandler", "key", key)
		return
	}

	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "Missing streamID", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	lines, err := app.GetTranscript(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get transcript", "key", cs.Key, "streamID", streamID, "err", err)
		return
	}

	writeJSON(w, lines)
}

// --- HTTP Helper Functions ---

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to write JSON response", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": message})
}
