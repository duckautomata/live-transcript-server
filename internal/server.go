package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"live-transcript-server/internal/storage"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kennygrant/sanitize"
	"github.com/lithammer/shortuuid/v4"
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
	mux.HandleFunc("POST /{channel}/line/{streamID}", app.apiKeyMiddleware(app.lineHandler))
	mux.HandleFunc("POST /{channel}/media/{streamID}/{id}", app.apiKeyMiddleware(app.mediaHandler))
	mux.HandleFunc("GET /{channel}/statuscheck", app.apiKeyMiddleware(app.statuscheckHandler))

	// Public routes
	mux.HandleFunc("GET /{channel}/websocket", app.wsHandler)
	mux.HandleFunc("GET /{channel}/stream/{streamID}/{type}/{filename}", app.streamHandler)
	mux.HandleFunc("GET /{channel}/download/{streamID}/{type}/{filename}", app.downloadHandler)
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

func NewApp(apiKey string, db *sql.DB, channelsConfig []ChannelConfig, storageConfig StorageConfig, tempDir string) *App {
	app := &App{
		ApiKey: apiKey,
		DB:     db,
		Upgrader: websocket.Upgrader{
			ReadBufferSize:    1024,
			WriteBufferSize:   1024,
			EnableCompression: true,
			CheckOrigin:       func(r *http.Request) bool { return true },
		},
		Channels:    make(map[string]*ChannelState),
		MaxConn:     10_000, // through testing, assuming a steady flow of connections, 10k connections will use 200 millicores
		MaxClipSize: 30,
		TempDir:     tempDir,
	}

	for _, cc := range channelsConfig {
		baseFolder := filepath.Join(tempDir, cc.Name)
		os.MkdirAll(baseFolder, 0755)

		cs := &ChannelState{
			Key:             cc.Name,
			BaseMediaFolder: baseFolder,
			NumPastStreams:  cc.NumPastStreams,
		}
		app.Channels[cc.Name] = cs
	}

	var store storage.Storage
	var err error
	ctx := context.Background()

	if storageConfig.Type == "r2" {
		store, err = storage.NewR2Storage(ctx, storageConfig.R2.AccountId, storageConfig.R2.AccessKeyId, storageConfig.R2.SecretAccessKey, storageConfig.R2.Bucket, storageConfig.R2.PublicUrl)
	} else {
		// Default to local storage
		store, err = storage.NewLocalStorage(tempDir, "")
	}

	if err != nil {
		panic(fmt.Sprintf("failed to initialize storage: %v", err))
	}
	app.Storage = store
	return app
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
		if app.Storage.IsLocal() {
			cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, activeId)
			if err := os.MkdirAll(cs.ActiveMediaFolder, 0755); err != nil {
				slog.Error("failed to create media folder", "key", cs.Key, "path", cs.ActiveMediaFolder, "err", err)
				return false
			}
		}

		// Rotation Logic: Query DB for all streams to apply retention policy
		allStreams, err := app.GetAllStreams(ctx, cs.Key)
		if err == nil {
			// allStreams is sorted by start_time DESC (newest first).

			if app.Storage.IsLocal() {
				// Local Storage: Keep 'keepCount' streams (active + NumPastStreams).
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
						// Delete from Storage
						// Run efficiently in background to avoid blocking the request
						go func(channelKey, activeID string) {
							// Create a background context for the deletion
							bgCtx := context.Background()

							storageKey := fmt.Sprintf("%s/%s", channelKey, activeID)
							slog.Info("deleting old stream storage (async)", "key", channelKey, "storageKey", storageKey)
							if err := app.Storage.DeleteFolder(bgCtx, storageKey); err != nil {
								slog.Error("failed to delete stream folder from storage", "key", channelKey, "storageKey", storageKey, "err", err)
							} else {
								slog.Info("successfully deleted old stream storage", "key", channelKey, "storageKey", storageKey)
							}
						}(cs.Key, streamToDelete.ActiveID)
					}
				}
			} else {
				// R2 Storage: Check existence of streams in storage.
				// If a stream is missing from R2 (e.g. deleted by lifecycle policy), remove it from DB.
				// We check all past streams (skip the first one which is the new active stream).
				if len(allStreams) > 1 {
					go func(channelKey string, streams []Stream) {
						bgCtx := context.Background()
						// Iterate over all past streams
						for i := 1; i < len(streams); i++ {
							streamToCheck := streams[i]
							storageKey := fmt.Sprintf("%s/%s", channelKey, streamToCheck.ActiveID)

							exists, err := app.Storage.StreamExists(bgCtx, storageKey)
							if err != nil {
								slog.Error("failed to check if stream exists in storage", "key", channelKey, "streamID", streamToCheck.ActiveID, "err", err)
								continue
							}

							if !exists {
								slog.Info("stream not found in storage (likely deleted by lifecycle), removing from db", "key", channelKey, "streamID", streamToCheck.ActiveID)

								// Delete from DB (Stream meta)
								if err := app.DeleteStream(bgCtx, channelKey, streamToCheck.ActiveID); err != nil {
									slog.Error("failed to delete stream from db", "key", channelKey, "streamID", streamToCheck.ActiveID, "err", err)
								}
								// Delete Transcript from DB
								if err := app.DeleteTranscript(bgCtx, channelKey, streamToCheck.ActiveID); err != nil {
									slog.Error("failed to delete transcript from db", "key", channelKey, "streamID", streamToCheck.ActiveID, "err", err)
								}
							}
						}
					}(cs.Key, allStreams)
				}
			}
		} else {
			slog.Error("failed to get all streams for rotation", "key", cs.Key, "err", err)
		}

		// Broadcast pastStreams event if there is any
		pastStreams, err := app.GetPastStreams(ctx, cs.Key, activeId)
		if err != nil {
			slog.Error("failed to get past streams for broadcast", "key", cs.Key, "err", err)
		} else {
			// an empty slice will be a nil pointer (null in the json message)
			// client should be able to interpret a null array as empty.
			pastStreamsMsg := WebSocketMessage{
				Event: EventPastStreams,
				Data:  EventPastStreamsData{Streams: pastStreams},
			}
			cs.broadcast(pastStreamsMsg)
		}
		data := EventNewStreamData{
			ActiveID:     newStream.ActiveID,
			ActiveTitle:  newStream.ActiveTitle,
			StartTime:    newStream.StartTime,
			MediaType:    newStream.MediaType,
			MediaBaseURL: app.Storage.GetURL(""),
			IsLive:       newStream.IsLive,
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

	// Check database for all available media files
	availableFiles, err := app.GetLastAvailableMediaFiles(r.Context(), cs.Key, data.ActiveID, -1)
	if err != nil {
		slog.Error("failed to get available media files", "key", cs.Key, "err", err)
	}

	// For every line in the transcript, check if we have the media file.
	// If we do, set MediaAvailable to true.
	for i := range data.Transcript {
		line := &data.Transcript[i]
		if fileID, ok := availableFiles[line.ID]; ok {
			line.MediaAvailable = true
			line.FileID = fileID
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

	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "Stream ID required", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("stream id required", "key", cs.Key, "func", "lineHandler")
		return
	}

	lastID, err := app.GetLastLineID(r.Context(), cs.Key, streamID)
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

	if err := app.InsertTranscriptLine(r.Context(), cs.Key, streamID, data); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to insert transcript line", "err", err)
		return
	}

	app.broadcastNewLine(r.Context(), cs, streamID, uploadTime, &data)
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

	// Timings map
	timings := make(map[string]float64)

	retrieveStart := time.Now()
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

	streamID := r.PathValue("streamID")

	// Verification
	verifStart := time.Now()
	exists, err := app.StreamExists(r.Context(), cs.Key, streamID)
	timings["verification"] = time.Since(verifStart).Seconds()
	MediaProcessingDuration.WithLabelValues("verification", cs.Key).Observe(timings["verification"])

	if err != nil {
		slog.Error("failed to check stream exists", "key", cs.Key, "func", "mediaHandler", "err", err)
		Http500Errors.Inc()
		return
	}
	if !exists {
		slog.Warn("stream does not exist", "key", cs.Key, "func", "mediaHandler", "streamID", streamID)
		Http400Errors.Inc()
		return
	}

	// Create temp file
	tempRawHost := filepath.Join(app.TempDir, fmt.Sprintf("%s_%s_%d.raw", cs.Key, streamID, id))
	dst, err := os.Create(tempRawHost)
	if err != nil {
		http.Error(w, "Unable to create temp file", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to create temp raw file", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}

	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		os.Remove(tempRawHost)
		http.Error(w, "Unable to save file", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to save temp raw file", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}
	dst.Close()
	defer os.Remove(tempRawHost) // Clean up

	// Total retrieve time (including parsing and saving) minus verification time which was interleaved
	timings["retrieve_file"] = time.Since(retrieveStart).Seconds() - timings["verification"]
	MediaProcessingDuration.WithLabelValues("retrieve_file", cs.Key).Observe(timings["retrieve_file"])

	// Convert to m4a
	convertStart := time.Now()
	tempM4aHost := ChangeExtension(tempRawHost, ".m4a")
	if err := FfmpegConvert(tempRawHost, tempM4aHost); err != nil {
		http.Error(w, "Unable to convert media", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to convert media", "key", cs.Key, "func", "mediaHandler", "err", err)
		return
	}
	timings["convert_m4a"] = time.Since(convertStart).Seconds()
	MediaProcessingDuration.WithLabelValues("convert_m4a", cs.Key).Observe(timings["convert_m4a"])
	defer os.Remove(tempM4aHost)

	// Upload Raw
	uploadRawStart := time.Now()
	fileID := shortuuid.New()
	rawKey := fmt.Sprintf("%s/%s/raw/%s.raw", cs.Key, streamID, fileID)
	rawReader, _ := os.Open(tempRawHost) // Should exist
	defer rawReader.Close()
	// Use a detached context for upload to ensure it completes even if client disconnects
	uploadCtx := context.WithoutCancel(r.Context())
	rawInfo, _ := os.Stat(tempRawHost)
	if _, err := app.Storage.Save(uploadCtx, rawKey, rawReader, rawInfo.Size()); err != nil {
		slog.Error("failed to upload raw file", "key", cs.Key, "err", err)
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		return
	}
	timings["upload_raw"] = time.Since(uploadRawStart).Seconds()
	MediaProcessingDuration.WithLabelValues("upload_raw", cs.Key).Observe(timings["upload_raw"])

	// Upload M4A
	uploadM4aStart := time.Now()
	m4aKey := fmt.Sprintf("%s/%s/audio/%s.m4a", cs.Key, streamID, fileID)
	m4aReader, _ := os.Open(tempM4aHost)
	defer m4aReader.Close()
	m4aInfo, _ := os.Stat(tempM4aHost)
	if _, err := app.Storage.Save(uploadCtx, m4aKey, m4aReader, m4aInfo.Size()); err != nil {
		slog.Error("failed to upload m4a file", "key", cs.Key, "err", err)
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}
	timings["upload_m4a"] = time.Since(uploadM4aStart).Seconds()
	MediaProcessingDuration.WithLabelValues("upload_m4a", cs.Key).Observe(timings["upload_m4a"])

	// Extract Frame
	// Fetch stream info to check media type
	stream, err := app.GetStreamByID(r.Context(), cs.Key, streamID)
	if err != nil {
		slog.Warn("failed to get stream for frame extraction", "key", cs.Key, "streamID", streamID, "err", err)
	}

	if stream != nil && stream.MediaType == "video" {
		extractFrameStart := time.Now()
		tempJpgHost := ChangeExtension(tempRawHost, ".jpg")
		if err := FfmpegExtractFrame(tempRawHost, tempJpgHost, 480); err == nil {
			timings["extract_frame"] = time.Since(extractFrameStart).Seconds()
			MediaProcessingDuration.WithLabelValues("extract_frame", cs.Key).Observe(timings["extract_frame"])
			defer os.Remove(tempJpgHost)

			uploadFrameStart := time.Now()
			jpgKey := fmt.Sprintf("%s/%s/frame/%s.jpg", cs.Key, streamID, fileID)
			jpgReader, _ := os.Open(tempJpgHost)
			defer jpgReader.Close()
			jpgInfo, _ := os.Stat(tempJpgHost)
			if _, err := app.Storage.Save(uploadCtx, jpgKey, jpgReader, jpgInfo.Size()); err != nil {
				slog.Error("failed to upload frame", "key", cs.Key, "err", err)
			}
			timings["upload_frame"] = time.Since(uploadFrameStart).Seconds()
			MediaProcessingDuration.WithLabelValues("upload_frame", cs.Key).Observe(timings["upload_frame"])
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Media received and processed successfully"))

	// Update DB
	updateDbStart := time.Now()
	success := true
	if err := app.SetMediaAvailable(r.Context(), cs.Key, streamID, id, fileID, true); err != nil {
		if err.Error() == "line not found" {
			time.Sleep(500 * time.Millisecond)
			if err := app.SetMediaAvailable(r.Context(), cs.Key, streamID, id, fileID, true); err != nil {
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
	timings["update_db"] = time.Since(updateDbStart).Seconds()
	MediaProcessingDuration.WithLabelValues("update_db", cs.Key).Observe(timings["update_db"])

	if success && streamID != "" {
		files, err := app.GetLastAvailableMediaFiles(r.Context(), cs.Key, streamID, 100)
		if err != nil {
			slog.Error("failed to get last available media files", "key", cs.Key, "err", err)
			files = map[int]string{id: fileID}
		}
		app.broadcastNewMedia(cs, streamID, files)
	}

	slog.Debug("media processing timings", "key", cs.Key, "streamID", streamID, "id", id, "timings", timings)
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

	// Check if storage is local. If R2, this endpoint is disabled.
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	// Helper to extract id and format
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	mediaType := r.PathValue("type")
	processStartTime := time.Now()

	// Validate mediaType
	allowedMediaTypes := []string{"audio", "clips"}
	if !slices.Contains(allowedMediaTypes, mediaType) {
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		return
	}

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
	// Local Storage Path: BaseMediaFolder/streamID/type/filename
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, mediaType, fmt.Sprintf("%s%s", idStr, ext))

	// Check if the file exists
	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			Http400Errors.Inc()
			slog.Warn("file not found for the requested id", "key", cs.Key, "func", "streamHandler", "requestedStreamID", requestedStreamID, "type", mediaType, "filename", filename, "id", idStr, "ext", ext)
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

	// Check if storage is local. If R2, this endpoint is disabled.
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	processStartTime := time.Now()

	// Helper to extract id and format
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	ext := filepath.Ext(filename)
	processStartTime = time.Now()

	if requestedStreamID == "" {
		http.Error(w, "Stream ID required", http.StatusNotFound)
		Http400Errors.Inc()
		slog.Warn("stream id required", "key", cs.Key, "func", "getFrameHandler", "requestedStreamID", requestedStreamID)
		return
	}

	if ext != ".jpg" {
		http.Error(w, "Invalid extension", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid extension", "key", cs.Key, "func", "getFrameHandler", "ext", ext)
		return
	}
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, "frame", filename)

	// Check if the file exists
	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "No frame found", http.StatusNotFound)
			// Don't consider this an error as it will flood the logs.
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to check frame file", "key", cs.Key, "func", "getFrameHandler", "filename", filename, "err", err)
		return
	}

	TotalFramesDownloads.WithLabelValues(cs.Key).Inc()
	StreamFramesDownloads.WithLabelValues(cs.Key).Inc()

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow frame processing time", "key", cs.Key, "func", "getFrameHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "filename", filename)
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

	uniqueID := shortuuid.New()

	// Get FileIDs for the range
	fileIDs, err := app.GetFileIDsInRange(r.Context(), cs.Key, req.StreamID, start, end)
	if err != nil {
		slog.Error("failed to get file ids for clip", "key", cs.Key, "err", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}

	// Check if all requested IDs were found
	expectedCount := end - start + 1
	if len(fileIDs) != expectedCount {
		slog.Warn("missing file ids for clip", "key", cs.Key, "func", "postClipHandler", "expected", expectedCount, "got", len(fileIDs))
		http.Error(w, "Missing media files for requested range", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}

	mergedRawPath, err := app.MergeRawAudio(r.Context(), cs.Key, req.StreamID, fileIDs, uniqueID)
	if err != nil {
		os.Remove(mergedRawPath)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to merge raw audio", "key", cs.Key, "func", "postClipHandler", "startID", start, "endID", end, "err", err)
		return
	}
	defer os.Remove(mergedRawPath) // Delete the merged raw file when done

	// Convert/Remux to Temp File
	tempMediaFile := filepath.Join(app.TempDir, uniqueID+clipExt)
	sidecarFile := ""

	// Note: audio has to be recoded to m4a otherwise it will be broken. Video can be remixed to a different container without any compatibility issues.
	if reqMediaType == "mp4" {
		err = FfmpegRemux(mergedRawPath, tempMediaFile)
		if err == nil {
			// Generate sidecar m4a in case the client has a slow connection, they can use the audio to clip while the video is loading.
			sidecarFile = filepath.Join(app.TempDir, uniqueID+".m4a")
			if err := FfmpegConvert(mergedRawPath, sidecarFile); err != nil {
				slog.Error("failed to generate sidecar m4a", "key", cs.Key, "err", err)
				// Don't fail the entire request, just log it. The mp4 is still good.
				os.Remove(sidecarFile)
				sidecarFile = ""
			} else {
				defer os.Remove(sidecarFile)
			}
		}
	} else {
		err = FfmpegConvert(mergedRawPath, tempMediaFile)
	}

	if err != nil {
		os.Remove(tempMediaFile)
		slog.Error("unable to convert raw media to new extension", "key", cs.Key, "func", "postClipHandler", "extension", clipExt, "err", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}
	defer os.Remove(tempMediaFile)

	// Upload Clip
	clipKey := fmt.Sprintf("%s/%s/clips/%s%s", cs.Key, req.StreamID, uniqueID, clipExt)
	clipReader, _ := os.Open(tempMediaFile)
	defer clipReader.Close()
	clipInfo, _ := os.Stat(tempMediaFile)
	if _, err := app.Storage.Save(r.Context(), clipKey, clipReader, clipInfo.Size()); err != nil {
		slog.Error("failed to upload clip", "key", cs.Key, "err", err)
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		return
	}

	// Upload m4a sidecar file
	if sidecarFile != "" {
		sidecarKey := fmt.Sprintf("%s/%s/clips/%s.m4a", cs.Key, req.StreamID, uniqueID)
		f, _ := os.Open(sidecarFile)
		defer f.Close()
		sidecarInfo, _ := os.Stat(sidecarFile)
		if _, err := app.Storage.Save(r.Context(), sidecarKey, f, sidecarInfo.Size()); err != nil {
			slog.Error("failed to upload sidecar m4a", "key", sidecarKey, "err", err)
		}
	}

	switch clipExt {
	case ".m4a", ".mp3":
		TotalAudioClipped.WithLabelValues(cs.Key).Inc()
		StreamAudioClipped.WithLabelValues(cs.Key).Inc()
	case ".mp4":
		TotalVideoClipped.WithLabelValues(cs.Key).Inc()
		StreamVideoClipped.WithLabelValues(cs.Key).Inc()
	}

	if time.Since(processStartTime).Seconds() > 5 {
		slog.Warn("slow clip processing time", "key", cs.Key, "func", "postClipHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "start", start, "end", end, "mediaType", reqMediaType)
	}

	writeJSON(w, map[string]string{
		"status":  "success",
		"clip_id": uniqueID,
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
	mediaType := r.PathValue("type")

	// Check if storage is local. If R2, this endpoint is disabled.
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	// Validate mediaType
	validMediaTypes := []string{"audio", "clips", "frame"}
	if !slices.Contains(validMediaTypes, mediaType) {
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		return
	}

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
	downloadFilename := requestedStreamID + "_" + filename
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

	// Check if file exists in BaseMediaFolder/{streamID}/{type}
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, mediaType, idStr+ext)
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

	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a POST", "key", cs.Key, "func", "postTrimHandler", "method", r.Method)
		return
	}

	// Parse JSON body
	var trimReq struct {
		StreamID   string  `json:"stream_id"`
		ClipID     string  `json:"clip_id"`
		FileFormat string  `json:"file_format"`
		Start      float64 `json:"start"`
		End        float64 `json:"end"`
	}

	if err := json.NewDecoder(r.Body).Decode(&trimReq); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	start := trimReq.Start
	end := trimReq.End

	if start < 0 || end <= start {
		http.Error(w, "Invalid start or end time", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if !slices.Contains([]string{"m4a", "mp3", "mp4"}, trimReq.FileFormat) {
		http.Error(w, "Invalid file format", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	uniqueID := shortuuid.New()
	sourceKey := fmt.Sprintf("%s/%s/clips/%s.%s", cs.Key, trimReq.StreamID, trimReq.ClipID, trimReq.FileFormat)

	// Download Source to Temp
	tempSource := filepath.Join(app.TempDir, fmt.Sprintf("src_%s.%s", uniqueID, trimReq.FileFormat))
	reader, err := app.Storage.Get(r.Context(), sourceKey)
	if err != nil {
		slog.Error("failed to download source for trim", "key", sourceKey, "err", err)
		http.Error(w, "Source file not found", http.StatusNotFound)
		return
	}

	outFile, err := os.Create(tempSource)
	if err != nil {
		reader.Close()
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(outFile, reader)
	reader.Close()
	outFile.Close()
	defer os.Remove(tempSource)

	// Trim
	tempDest := filepath.Join(app.TempDir, fmt.Sprintf("trim_%s.%s", uniqueID, trimReq.FileFormat))
	if err := FfmpegTrim(tempSource, tempDest, start, end); err != nil {
		http.Error(w, "Trim failed", http.StatusInternalServerError)
		slog.Error("ffmpeg trim failed", "err", err)
		return
	}
	defer os.Remove(tempDest)

	// Upload
	destKey := fmt.Sprintf("%s/%s/clips/%s.%s", cs.Key, trimReq.StreamID, uniqueID, trimReq.FileFormat)
	f, _ := os.Open(tempDest)
	defer f.Close()
	destInfo, _ := os.Stat(tempDest)
	if _, err := app.Storage.Save(r.Context(), destKey, f, destInfo.Size()); err != nil {
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		slog.Error("failed to upload trimmed clip", "err", err)
		return
	}

	writeJSON(w, map[string]string{
		"status":  "success",
		"clip_id": uniqueID,
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

// StartReconciliationLoop starts a background task that periodically checks R2 storage
// for missing streams and removes them from the database.
func (app *App) StartReconciliationLoop() {
	if app.Storage.IsLocal() {
		return
	}

	ticker := time.NewTicker(8 * time.Hour)
	go func() {
		for range ticker.C {
			slog.Info("starting reconciliation loop", "func", "StartReconciliationLoop")
			ctx := context.Background()

			for _, cs := range app.Channels {
				// Get all streams
				streams, err := app.GetAllStreams(ctx, cs.Key)
				if err != nil {
					slog.Error("failed to get streams for reconciliation", "key", cs.Key, "err", err)
					continue
				}

				// Skip the first one (active stream)
				if len(streams) <= 1 {
					continue
				}

				updatesMade := false
				for i := 1; i < len(streams); i++ {
					stream := streams[i]
					storageKey := fmt.Sprintf("%s/%s", cs.Key, stream.ActiveID)

					exists, err := app.Storage.StreamExists(ctx, storageKey)
					if err != nil {
						slog.Error("reconciliation: check failed", "key", cs.Key, "streamID", stream.ActiveID, "err", err)
						continue
					}

					if !exists {
						slog.Info("reconciliation: stream missing in R2, deleting from db", "key", cs.Key, "streamID", stream.ActiveID)
						if err := app.DeleteStream(ctx, cs.Key, stream.ActiveID); err != nil {
							slog.Error("reconciliation: failed to delete stream", "key", cs.Key, "streamID", stream.ActiveID, "err", err)
						}
						if err := app.DeleteTranscript(ctx, cs.Key, stream.ActiveID); err != nil {
							slog.Error("reconciliation: failed to delete transcript", "key", cs.Key, "streamID", stream.ActiveID, "err", err)
						}
						updatesMade = true
					}
				}

				if updatesMade {
					// Broadcast new list of streams

					// To be safe and consistent with activateStream logic:
					currentStream, _ := app.GetStream(ctx, cs.Key)
					activeID := ""
					if currentStream != nil {
						activeID = currentStream.ActiveID
					}

					finalPastStreams, err := app.GetPastStreams(ctx, cs.Key, activeID)
					if err == nil {
						msg := WebSocketMessage{
							Event: EventPastStreams,
							Data:  EventPastStreamsData{Streams: finalPastStreams},
						}
						cs.broadcast(msg)
					}
				}
			}
		}
	}()
}
