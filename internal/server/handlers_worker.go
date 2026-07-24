package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"live-transcript-server/internal/media"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/storage"
	"live-transcript-server/internal/store"

	"github.com/lithammer/shortuuid/v4"
)

// syncHandler handles a sync request from the worker. Sets current stream
// state and replaces the transcript.
func (app *App) syncHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	uploadStartTime := time.Now()

	var data model.WorkerData
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", cs.Key, "func", "syncHandler", "err", err)
		return
	}
	if !isValidID(data.StreamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid stream id", "key", cs.Key, "func", "syncHandler", "streamID", data.StreamID)
		return
	}
	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	stream := &model.Stream{
		ChannelID:   cs.Key,
		StreamID:    data.StreamID,
		StreamTitle: data.StreamTitle,
		StartTime:   data.StartTime,
		IsLive:      data.IsLive,
		MediaType:   data.MediaType,
	}
	if err := app.Store.UpsertStream(r.Context(), stream); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to upsert stream")
		return
	}

	// The worker's copy of a line doesn't know whether we already hold its
	// media, so carry the availability flags over from the database.
	availableFiles, err := app.Store.GetLastAvailableMediaFiles(r.Context(), cs.Key, data.StreamID, -1)
	if err != nil {
		slog.Error("failed to get available media files", "key", cs.Key, "err", err)
	}
	for i := range data.Transcript {
		line := &data.Transcript[i]
		if fileID, ok := availableFiles[line.ID]; ok {
			line.MediaAvailable = true
			line.FileID = fileID
		}
	}

	if err := app.Store.ReplaceTranscript(r.Context(), cs.Key, data.StreamID, data.Transcript); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to replace transcript")
		return
	}
	app.bumpAdminChange(cs.Key)

	app.broadcastNewLine(r.Context(), cs, data.StreamID, uploadTime, nil)

	if uploadTime > 5*1000 {
		slog.Warn("slow upload time", "key", cs.Key, "func", "syncHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}
	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", cs.Key, "func", "syncHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Sync data received and processed successfully"))
}

// lineHandler handles a new line from the worker. Appends the line to the
// transcript, answering 409 when the worker and server are out of sync so the
// worker re-uploads its full state via /sync.
func (app *App) lineHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	uploadStartTime := time.Now()

	decodeStart := time.Now()
	var data model.Line
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", cs.Key, "func", "lineHandler", "err", err)
		return
	}
	metrics.RequestProcessingDuration.WithLabelValues("lineHandler", "decode_json", cs.Key).Observe(time.Since(decodeStart).Seconds())

	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	streamID := r.PathValue("streamID")
	if !isValidID(streamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid stream id", "key", cs.Key, "func", "lineHandler", "streamID", streamID)
		return
	}

	// Force MediaAvailable to false for new lines
	data.MediaAvailable = false

	dbInsertStart := time.Now()
	err := app.Store.InsertNextLine(r.Context(), cs.Key, streamID, data)
	if errors.Is(err, store.ErrOutOfSync) {
		http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
		metrics.ServerOOS.Inc()
		slog.Warn("line id mismatch. Requesting worker to send current state.", "key", cs.Key, "func", "lineHandler", "newLineID", data.ID, "err", err)
		return
	}
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to insert transcript line")
		return
	}
	metrics.RequestProcessingDuration.WithLabelValues("lineHandler", "db_insert", cs.Key).Observe(time.Since(dbInsertStart).Seconds())

	broadcastStart := time.Now()
	app.broadcastNewLine(r.Context(), cs, streamID, uploadTime, &data)
	metrics.RequestProcessingDuration.WithLabelValues("lineHandler", "broadcast", cs.Key).Observe(time.Since(broadcastStart).Seconds())

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", cs.Key, "func", "lineHandler", "uploadTimeMs", time.Since(uploadStartTime).Milliseconds(), "processingTimeMs", time.Since(processStartTime).Milliseconds(), "lineId", data.ID)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Line data received and processed successfully"))
}

// uploadFile streams a local file into storage under key.
func (app *App) uploadFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if _, err := app.Storage.Save(ctx, key, f, info.Size()); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return nil
}

// mediaHandler handles a media file upload from the worker: save to a temp
// file, convert to m4a, upload raw + m4a (+ a frame for video streams) to
// storage, then mark the line's media available. The DB commit happens BEFORE
// the response is written so a 200 always means the media is actually
// retrievable.
func (app *App) mediaHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	observe := func(step string, since time.Time) {
		metrics.MediaProcessingDuration.WithLabelValues(step, cs.Key).Observe(time.Since(since).Seconds())
	}

	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	retrieveStart := time.Now()
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Unable to get file", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	defer file.Close()

	streamID := r.PathValue("streamID")
	if !isValidID(streamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	verifStart := time.Now()
	exists, err := app.Store.StreamExists(r.Context(), cs.Key, streamID)
	verifDuration := time.Since(verifStart)
	observe("verification", verifStart)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to check stream exists", "key", cs.Key, "func", "mediaHandler")
		return
	}
	if !exists {
		http.Error(w, "Stream not found", http.StatusNotFound)
		slog.Warn("stream does not exist", "key", cs.Key, "func", "mediaHandler", "streamID", streamID)
		metrics.Http400Errors.Inc()
		return
	}

	// Save the upload to a temp file.
	tempRawHost := filepath.Join(app.TempDir, fmt.Sprintf("%s_%s_%d.raw", cs.Key, streamID, id))
	dst, err := os.Create(tempRawHost)
	if err != nil {
		http.Error(w, "Unable to create temp file", http.StatusInternalServerError)
		app.report500(r, err, "unable to create temp raw file", "key", cs.Key, "func", "mediaHandler")
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		os.Remove(tempRawHost)
		http.Error(w, "Unable to save file", http.StatusInternalServerError)
		app.report500(r, err, "unable to save temp raw file", "key", cs.Key, "func", "mediaHandler")
		return
	}
	dst.Close()
	defer os.Remove(tempRawHost)

	// Total retrieve time (including parsing and saving) minus the DB
	// verification, which was interleaved.
	metrics.MediaProcessingDuration.WithLabelValues("retrieve_file", cs.Key).Observe((time.Since(retrieveStart) - verifDuration).Seconds())

	// Convert to m4a
	convertStart := time.Now()
	tempM4aHost := media.ChangeExtension(tempRawHost, ".m4a")
	if err := app.Media.Convert(tempRawHost, tempM4aHost); err != nil {
		http.Error(w, "Unable to convert media", http.StatusInternalServerError)
		app.report500(r, err, "unable to convert media", "key", cs.Key, "func", "mediaHandler")
		return
	}
	observe("convert_m4a", convertStart)
	defer os.Remove(tempM4aHost)

	// Use a detached context for uploads and the DB commit so they complete
	// even if the worker disconnects mid-request.
	uploadCtx := context.WithoutCancel(r.Context())
	fileID := shortuuid.New()

	uploadRawStart := time.Now()
	if err := app.uploadFile(uploadCtx, storage.RawKey(cs.Key, streamID, fileID), tempRawHost); err != nil {
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		app.report500(r, err, "failed to upload raw file", "key", cs.Key)
		return
	}
	observe("upload_raw", uploadRawStart)

	uploadM4aStart := time.Now()
	if err := app.uploadFile(uploadCtx, storage.AudioKey(cs.Key, streamID, fileID), tempM4aHost); err != nil {
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		app.report500(r, err, "failed to upload m4a file", "key", cs.Key)
		return
	}
	observe("upload_m4a", uploadM4aStart)

	// Extract a preview frame for video streams.
	stream, err := app.Store.GetStreamByID(uploadCtx, cs.Key, streamID)
	if err != nil {
		slog.Warn("failed to get stream for frame extraction", "key", cs.Key, "streamID", streamID, "err", err)
	}
	if stream != nil && stream.MediaType == "video" {
		extractFrameStart := time.Now()
		tempJpgHost := media.ChangeExtension(tempRawHost, ".jpg")
		if err := app.Media.ExtractFrame(tempRawHost, tempJpgHost, 480); err == nil {
			observe("extract_frame", extractFrameStart)
			defer os.Remove(tempJpgHost)

			uploadFrameStart := time.Now()
			if err := app.uploadFile(uploadCtx, storage.FrameKey(cs.Key, streamID, fileID), tempJpgHost); err != nil {
				slog.Error("failed to upload frame", "key", cs.Key, "err", err)
			}
			observe("upload_frame", uploadFrameStart)
		}
	}

	// Mark the line's media available BEFORE responding, so a 200 means the
	// commit happened. The bounded retry covers the worker posting media for
	// a line that is still in flight to /line.
	updateDbStart := time.Now()
	err = app.Store.SetMediaAvailable(uploadCtx, cs.Key, streamID, id, fileID, true)
	if errors.Is(err, store.ErrNotFound) {
		time.Sleep(500 * time.Millisecond)
		err = app.Store.SetMediaAvailable(uploadCtx, cs.Key, streamID, id, fileID, true)
	}
	observe("update_db", updateDbStart)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to set media available", "key", cs.Key, "id", id)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Media received and processed successfully"))

	// Post-response: tell clients about the newly available media.
	files, err := app.Store.GetLastAvailableMediaFiles(uploadCtx, cs.Key, streamID, 100)
	if err != nil {
		slog.Error("failed to get last available media files", "key", cs.Key, "err", err)
		files = map[int]string{id: fileID}
	}
	app.broadcastNewMedia(cs, streamID, files)
}

// activateHandler handles an activate request from the worker.
func (app *App) activateHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	processStartTime := time.Now()

	query := r.URL.Query()
	streamID := query.Get("id")
	title := query.Get("title")
	startTime := query.Get("startTime")
	mediaType := query.Get("mediaType")

	if streamID == "" || title == "" || startTime == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid parameters", "key", cs.Key, "func", "activateHandler", "streamID", streamID, "title", title, "startTime", startTime)
		return
	}
	if !isValidID(streamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid stream id", "key", cs.Key, "func", "activateHandler", "streamID", streamID)
		return
	}

	// Try to parse the provided startTime; if it fails, use the current time.
	startTimeUnix, err := strconv.ParseInt(startTime, 10, 64)
	if err != nil {
		slog.Warn("invalid or empty startTime received, using current system time", "key", cs.Key, "func", "activateHandler", "receivedTime", startTime)
		startTimeUnix = time.Now().Unix()
	}
	finalStartTimeStr := strconv.FormatInt(startTimeUnix, 10)

	activated := app.activateStream(r.Context(), cs, streamID, title, finalStartTimeStr, mediaType)

	if activated {
		metrics.ActivatedStreams.WithLabelValues(cs.Key, streamID, title).Set(float64(startTimeUnix))
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully activated", cs.Key))
		slog.Debug("new stream activated", "key", cs.Key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID, "title", title, "mediaType", mediaType)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream is already activated", cs.Key))
		slog.Debug("id already activated", "key", cs.Key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	}
}

func (app *App) deactivateHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	processStartTime := time.Now()

	streamID := r.URL.Query().Get("id")
	if !isValidID(streamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid parameters, streamID is empty or malformed", "key", cs.Key, "func", "deactivateHandler", "streamID", streamID)
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

// workerStatusHandler records the worker's heartbeat for each channel it runs.
func (app *App) workerStatusHandler(w http.ResponseWriter, r *http.Request) {
	var req model.WorkerStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	lastSeen := time.Now().Unix()
	for _, key := range req.Keys {
		if err := app.Store.UpsertWorkerStatus(r.Context(), key, req.Version, req.BuildTime, lastSeen); err != nil {
			slog.Error("failed to upsert worker status", "key", key, "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (app *App) statuscheckHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	w.WriteHeader(http.StatusOK)
	w.Write(fmt.Appendf(nil, "Current number of clients: %d", cs.Hub.Connections()))
}

// IncomingStreamsResponse is returned by GET /{channel}/incoming.
type IncomingStreamsResponse struct {
	URLs []string `json:"urls"`
}

// getIncomingHandler returns the queued Pingcord URLs for a channel without
// removing them. The worker calls DELETE /{channel}/incoming once it's done
// with a URL.
func (app *App) getIncomingHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	urls, err := app.Store.GetIncomingStreams(r.Context(), cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to get incoming streams", "key", cs.Key, "func", "getIncomingHandler")
		return
	}
	if urls == nil {
		urls = []string{}
	}
	writeJSON(w, IncomingStreamsResponse{URLs: urls})
}

// deleteIncomingHandler removes a single URL from the incoming queue. The URL
// is read from the `url` query parameter so the client doesn't need a JSON
// body. Returns 204 on success, 404 if the URL was not queued.
func (app *App) deleteIncomingHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "Missing required parameter: url", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	rowsAffected, err := app.Store.DeleteIncomingStream(r.Context(), cs.Key, url)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to delete incoming stream", "key", cs.Key, "func", "deleteIncomingHandler", "url", url)
		return
	}
	if rowsAffected == 0 {
		http.Error(w, "URL not queued", http.StatusNotFound)
		return
	}
	app.bumpAdminChange(cs.Key)

	slog.Info("incoming stream removed", "key", cs.Key, "func", "deleteIncomingHandler", "url", url)
	w.WriteHeader(http.StatusNoContent)
}

// RestartStatusResponse is returned by GET /{channel}/restart.
type RestartStatusResponse struct {
	Pending     bool  `json:"pending"`
	RequestedAt int64 `json:"requestedAt"`
}

// postRestartHandler marks the channel as needing a worker restart. The worker
// reads this signal on its poll loop, abandons the current stream, and calls
// DELETE /{channel}/restart to acknowledge.
func (app *App) postRestartHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	now := time.Now().Unix()
	if err := app.Store.UpsertRestartRequest(r.Context(), cs.Key, now); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to upsert restart request", "key", cs.Key, "func", "postRestartHandler")
		return
	}
	app.bumpAdminChange(cs.Key)

	slog.Info("worker restart requested", "key", cs.Key, "func", "postRestartHandler", "requestedAt", now)
	w.WriteHeader(http.StatusNoContent)
}

// getRestartHandler returns whether a restart is pending for the channel.
// Idempotent and side-effect free.
func (app *App) getRestartHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	requestedAt, err := app.Store.GetRestartRequest(r.Context(), cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to get restart request", "key", cs.Key, "func", "getRestartHandler")
		return
	}
	writeJSON(w, RestartStatusResponse{
		Pending:     requestedAt > 0,
		RequestedAt: requestedAt,
	})
}

// deleteRestartHandler clears the pending restart flag. The worker calls this
// once it has acted on the restart. Returns 204 if cleared, 404 if no restart
// was pending.
func (app *App) deleteRestartHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	rowsAffected, err := app.Store.DeleteRestartRequest(r.Context(), cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to delete restart request", "key", cs.Key, "func", "deleteRestartHandler")
		return
	}
	if rowsAffected == 0 {
		http.Error(w, "No restart pending", http.StatusNotFound)
		return
	}
	app.bumpAdminChange(cs.Key)

	slog.Info("restart request cleared", "key", cs.Key, "func", "deleteRestartHandler")
	w.WriteHeader(http.StatusNoContent)
}
