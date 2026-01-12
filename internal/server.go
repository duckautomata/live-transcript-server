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
		err := os.MkdirAll(cs.MediaFolder, 0755)
		if err != nil {
			slog.Error("cannot create media folder", "key", cs.Key, "func", "RegisterRoutes", "err", err)
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
	mux.HandleFunc("GET /{channel}/audio", app.getAudioHandler)
	mux.HandleFunc("GET /{channel}/clip", app.getClipHandler)
	mux.HandleFunc("GET /{channel}/download", app.downloadHandler)
	mux.HandleFunc("GET /{channel}/trim", app.trimHandler)
	mux.HandleFunc("GET /{channel}/frame", app.getFrameHandler)
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// Set the allowed headers
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")

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

		if err := app.UpsertStream(ctx, newStream); err != nil {
			slog.Error("failed to upsert new stream", "key", cs.Key, "err", err)
			return false
		}

		// Clear transcript for new stream
		if err := app.ClearTranscript(ctx, cs.Key); err != nil {
			slog.Error("failed to clear transcript", "key", cs.Key, "err", err)
			return false
		}

		cs.ResetAudioFile()
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
			// Reactivate
			if err := app.SetStreamLive(ctx, cs.Key, true); err != nil {
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

		if err := app.SetStreamLive(ctx, cs.Key, false); err != nil {
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
		m4aPath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.m4a", line.ID))
		rawPath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.raw", line.ID))
		if _, err := os.Stat(m4aPath); err == nil {
			line.MediaAvailable = true
		} else if _, err := os.Stat(rawPath); err == nil {
			line.MediaAvailable = true
		}
	}

	if err := app.ReplaceTranscript(r.Context(), cs.Key, data.Transcript); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to replace transcript", "err", err)
		return
	}

	app.broadcastNewLine(r.Context(), cs, uploadTime, nil)

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

	lastID, err := app.GetLastLineID(r.Context(), cs.Key)
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

	if err := app.InsertTranscriptLine(r.Context(), cs.Key, data); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to insert transcript line", "err", err)
		return
	}

	app.broadcastNewLine(r.Context(), cs, uploadTime, &data)
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
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	defer file.Close()

	// Save raw file
	rawFilePath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.raw", id))
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
	if err := app.SetMediaAvailable(r.Context(), cs.Key, id, true); err != nil {
		if err.Error() == "line not found" {
			// This usually fails if the media was uploaded before the line was added.
			// Wait 500ms then try again to give the line time to be added.
			slog.Warn("line not found, retrying", "key", cs.Key, "id", id)
			time.Sleep(500 * time.Millisecond)
			if err := app.SetMediaAvailable(r.Context(), cs.Key, id, true); err != nil {
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
	// We don't fail the request because the file is saved and converted.
	if success {
		// Broadcast new media availability
		// Get last 100 available IDs
		ids, err := app.GetLastAvailableMediaIDs(r.Context(), cs.Key, 100)
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

func (app *App) getAudioHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "getAudioHandler", "key", key)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", cs.Key, "func", "getAudioHandler", "method", r.Method)
		return
	}

	stream, err := app.GetStream(r.Context(), cs.Key)
	mediaType := "none"
	activeID := ""
	if err == nil && stream != nil {
		mediaType = stream.MediaType
		activeID = stream.ActiveID
	}

	if mediaType == "none" {
		http.Error(w, "Audio download is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot retrieve audio. Media type is none", "key", cs.Key, "func", "getAudioHandler")
		return
	}
	processStartTime := time.Now()

	// Extract the ID from the query parameter
	query := r.URL.Query()
	idStr := query.Get("id")
	isStream := query.Get("stream")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("unable to convert id to int", "key", cs.Key, "func", "getAudioHandler", "id", idStr, "err", err)
		return
	}

	filePath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.m4a", id))

	// Check if the file exists
	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "No audio found", http.StatusNotFound)
			Http400Errors.Inc()
			slog.Warn("no audio file found for the requested id", "key", cs.Key, "func", "getAudioHandler", "id", id)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to check audio file", "key", cs.Key, "func", "getAudioHandler", "id", id, "err", err)
		return
	}

	TotalAudioPlayed.WithLabelValues(cs.Key).Inc()
	StreamAudioPlayed.WithLabelValues(cs.Key).Inc()

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow audio processing time", "key", cs.Key, "func", "getAudioHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "id", idStr, "stream", isStream)
	}

	// Enable Content-Disposition to have the browser automatically download the audio
	if isStream != "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_%d.m4a\"", activeID, id))
	}
	w.Header().Set("Content-Type", "audio/mp4")
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

	// Extract the ID from the query parameter
	query := r.URL.Query()
	idStr := query.Get("id")
	requestedStreamID := query.Get("stream_id")

	if requestedStreamID != "" && requestedStreamID != activeID {
		http.Error(w, "Stream ID mismatch", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream id mismatch", "key", cs.Key, "func", "getFrameHandler", "requestedStreamID", requestedStreamID, "activeID", activeID)
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("unable to convert id to int", "key", cs.Key, "func", "getFrameHandler", "id", idStr, "err", err)
		return
	}

	filePath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.jpg", id))

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

func (app *App) getClipHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "getClipHandler", "key", key)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", cs.Key, "func", "getClipHandler", "method", r.Method)
		return
	}

	stream, err := app.GetStream(r.Context(), cs.Key)
	mediaType := "none"
	startTime := ""
	if err == nil && stream != nil {
		mediaType = stream.MediaType
		startTime = stream.StartTime
	}

	if mediaType == "none" {
		http.Error(w, "Clipping is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot clip media. Media type is none", "key", cs.Key, "func", "getClipHandler")
		return
	}
	processStartTime := time.Now()

	// Extract the ID from the query parameter
	query := r.URL.Query()
	startStr := query.Get("start")
	endStr := query.Get("end")
	clipName := strings.TrimSpace(query.Get("name"))
	reqMediaType := strings.TrimSpace(query.Get("type"))
	start, err := strconv.Atoi(startStr)
	end, err2 := strconv.Atoi(endStr)

	clipExt := ".m4a"
	switch reqMediaType {
	case "mp4":
		if mediaType != "video" {
			http.Error(w, "Video clipping is disabled for this stream", http.StatusMethodNotAllowed)
			Http400Errors.Inc()
			slog.Warn("cannot clip mp4. Media type is not 'video'", "key", cs.Key, "func", "getClipHandler", "mediaType", mediaType)
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
		slog.Warn("invalid media type", "key", cs.Key, "func", "getClipHandler", "mediaType", reqMediaType)
		return
	}

	if err != nil {
		slog.Warn("unable to convert start id to int", "key", cs.Key, "func", "getClipHandler", "start", startStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if err2 != nil {
		slog.Warn("unable to convert end id to int", "key", cs.Key, "func", "getClipHandler", "end", endStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if start < 0 || end <= start || end-start >= app.MaxClipSize {
		slog.Warn("invalid start or end id", "key", cs.Key, "func", "getClipHandler", "start", start, "end", end, "requestedClipSize", 1+end-start, "maxClipSize", app.MaxClipSize, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	uniqueID := fmt.Sprintf("%d-%d-%d", start, end, time.Now().UnixNano())
	mergedMediaPath, err := cs.MergeRawAudio(start, end, uniqueID)
	if err != nil {
		os.Remove(mergedMediaPath)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to merge raw audio", "key", cs.Key, "func", "getClipHandler", "startID", start, "endID", end, "err", err)
		return
	}
	defer os.Remove(mergedMediaPath) // Delete the merged raw file when done

	mediaFilePath := filepath.Join(cs.MediaFolder, uniqueID+clipExt)

	// Note: audio has to be recoded to m4a otherwise it will be broken. Video can be remixed to a different container without any compatibility issues.
	if reqMediaType == "mp4" {
		err = FfmpegRemux(mergedMediaPath, mediaFilePath)
	} else {
		err = FfmpegConvert(mergedMediaPath, mediaFilePath)
	}
	if err != nil {
		os.Remove(mediaFilePath)
		slog.Error("unable to convert raw media to new extension", "key", cs.Key, "func", "getClipHandler", "extension", clipExt, "err", err)
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

	if clipName == "" {
		clipName = fmt.Sprintf("%d-%d", start, end)
	}
	unixTimeInt, err := strconv.Atoi(startTime)
	unixTimeInt64 := int64(unixTimeInt)
	if err != nil {
		unixTimeInt64 = time.Now().Unix()
	}
	yymmdd := time.Unix(unixTimeInt64, 0).Format("20060102")
	attachmentName := fmt.Sprintf("%s-%s-%s", cs.Key, yymmdd, clipName)

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow clip processing time", "key", cs.Key, "func", "getClipHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "start", startStr, "end", endStr, "clipName", clipName, "mediaType", reqMediaType)
	}

	// use BaseName rather than Name because BaseName removes / where as Name removes anything before the last /
	// Also BaseName preserves capitalization.
	sanitizedName := sanitize.BaseName(attachmentName)
	writeJSON(w, map[string]string{
		"id":       uniqueID,
		"filename": sanitizedName,
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

	query := r.URL.Query()
	id := query.Get("id")
	stream := query.Get("stream")
	filename := query.Get("filename")

	if id == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	if stream != "true" && filename == "" {
		http.Error(w, "Missing filename", http.StatusBadRequest)
		return
	}

	// Find the file. It could be .m4a, .mp4, .mp3, or .jpg
	var filePath string
	var contentType string
	var ext string
	extensions := []string{".m4a", ".mp4", ".mp3", ".jpg"}
	contentTypes := map[string]string{
		".m4a": "audio/mp4",
		".mp4": "video/mp4",
		".mp3": "audio/mpeg",
		".jpg": "image/jpeg",
	}

	for _, e := range extensions {
		path := filepath.Join(cs.MediaFolder, id+e)
		if _, err := os.Stat(path); err == nil {
			filePath = path
			ext = e
			contentType = contentTypes[e]
			break
		}
	}

	if filePath == "" {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	if stream != "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s%s\"", filename, ext))
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

func (app *App) trimHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("invalid channel name", "func", "trimHandler", "key", key)
		return
	}

	query := r.URL.Query()
	id := query.Get("id")
	startStr := query.Get("start")
	endStr := query.Get("end")
	filename := query.Get("filename")

	if id == "" || startStr == "" || endStr == "" || filename == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	start, err := strconv.ParseFloat(startStr, 64)
	if err != nil {
		http.Error(w, "Invalid start time", http.StatusBadRequest)
		return
	}
	end, err := strconv.ParseFloat(endStr, 64)
	if err != nil {
		http.Error(w, "Invalid end time", http.StatusBadRequest)
		return
	}

	// Find source file
	var srcPath string
	var ext string
	var contentType string
	extensions := []string{".m4a", ".mp4", ".mp3"}
	contentTypes := map[string]string{
		".m4a": "audio/mp4",
		".mp4": "video/mp4",
		".mp3": "audio/mpeg",
	}

	for _, e := range extensions {
		path := filepath.Join(cs.MediaFolder, id+e)
		if _, err := os.Stat(path); err == nil {
			srcPath = path
			ext = e
			contentType = contentTypes[e]
			break
		}
	}

	if srcPath == "" {
		http.Error(w, "Source file not found", http.StatusNotFound)
		return
	}

	trimmedID := fmt.Sprintf("%s-trimmed-%d", id, time.Now().UnixNano())
	trimmedPath := filepath.Join(cs.MediaFolder, trimmedID+ext)

	if err := FfmpegTrim(srcPath, trimmedPath, start, end); err != nil {
		slog.Error("failed to trim file", "err", err)
		http.Error(w, "Failed to trim file", http.StatusInternalServerError)
		return
	}
	defer os.Remove(trimmedPath)

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s%s\"", filename, ext))
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, trimmedPath)
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
