package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"live-transcript-server/internal/media"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/storage"

	"github.com/kennygrant/sanitize"
	"github.com/lithammer/shortuuid/v4"
)

// mediaContentTypes maps the media file extensions the server serves to their
// content types. Shared by the stream and download handlers.
var mediaContentTypes = map[string]string{
	".m4a": "audio/mp4",
	".mp4": "video/mp4",
	".mp3": "audio/mpeg",
	".jpg": "image/jpeg",
}

// getStatusHandler is the public server + workers status endpoint.
func (app *App) getStatusHandler(w http.ResponseWriter, r *http.Request) {
	workers, err := app.Store.GetAllWorkerStatus(r.Context())
	if err != nil {
		http.Error(w, "Database Error", http.StatusInternalServerError)
		slog.Error("failed to get worker status", "err", err)
		return
	}

	now := time.Now().Unix()
	for i := range workers {
		workers[i].IsActive = now-workers[i].LastSeen < int64(workerActiveWindow.Seconds())
	}

	writeJSON(w, model.FullInfoResponse{
		Server: model.ServerInfo{
			Version:   app.Version,
			BuildTime: app.BuildTime,
		},
		Workers: workers,
	})
}

// streamHandler serves media files for inline playback. Only available with
// local storage — with R2 the client streams from the public bucket URL.
func (app *App) streamHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	mediaType := r.PathValue("type")
	processStartTime := time.Now()

	allowedMediaTypes := []string{"audio", "clips"}
	if !slices.Contains(allowedMediaTypes, mediaType) {
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	ext := filepath.Ext(filename)
	idStr := strings.TrimSuffix(filename, ext)
	contentType, ok := mediaContentTypes[ext]
	if !ok || ext == ".jpg" { // frames are served by getFrameHandler
		http.Error(w, "Invalid extension", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	if !isValidID(requestedStreamID) || !isValidID(idStr) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	// Local storage path: BaseMediaFolder/streamID/type/filename
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, mediaType, idStr+ext)

	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			metrics.Http400Errors.Inc()
			slog.Warn("file not found for the requested id", "key", cs.Key, "func", "streamHandler", "requestedStreamID", requestedStreamID, "type", mediaType, "filename", filename)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		app.report500(r, err, "unable to check file", "key", cs.Key, "func", "streamHandler", "id", idStr)
		return
	}

	switch ext {
	case ".m4a", ".mp3":
		metrics.TotalAudioPlayed.WithLabelValues(cs.Key).Inc()
		metrics.StreamAudioPlayed.WithLabelValues(cs.Key).Inc()
	case ".mp4":
		metrics.TotalVideoPlayed.WithLabelValues(cs.Key).Inc()
		metrics.StreamVideoPlayed.WithLabelValues(cs.Key).Inc()
	}

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow stream processing time", "key", cs.Key, "func", "streamHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "id", idStr)
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

// getFrameHandler serves the preview frame for a line. Only available with
// local storage.
func (app *App) getFrameHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	processStartTime := time.Now()
	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	ext := filepath.Ext(filename)

	if ext != ".jpg" {
		http.Error(w, "Invalid extension", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid extension", "key", cs.Key, "func", "getFrameHandler", "ext", ext)
		return
	}
	idStr := strings.TrimSuffix(filename, ext)
	if !isValidID(requestedStreamID) || !isValidID(idStr) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, "frame", idStr+ext)

	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			// Not an error: frames only exist for video streams, and clients
			// probe optimistically. Logging this would flood the logs.
			http.Error(w, "No frame found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		app.report500(r, err, "unable to check frame file", "key", cs.Key, "func", "getFrameHandler", "filename", filename)
		return
	}

	metrics.TotalFramesDownloads.WithLabelValues(cs.Key).Inc()
	metrics.StreamFramesDownloads.WithLabelValues(cs.Key).Inc()

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow frame processing time", "key", cs.Key, "func", "getFrameHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "filename", filename)
	}

	w.Header().Set("Cache-Control", "public, max-age=31536000") // Frames are immutable for an ID; the browser grabs new frames when the stream id changes.
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, filePath)
}

// downloadHandler serves media files as attachments. Only available with local
// storage.
func (app *App) downloadHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.Storage.IsLocal() {
		http.Error(w, "Endpoint disabled for remote storage", http.StatusBadRequest)
		return
	}

	filename := r.PathValue("filename")
	requestedStreamID := r.PathValue("streamID")
	mediaType := r.PathValue("type")

	validMediaTypes := []string{"audio", "clips", "frame"}
	if !slices.Contains(validMediaTypes, mediaType) {
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	if !isValidID(requestedStreamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	if _, err := os.Stat(filepath.Join(cs.BaseMediaFolder, requestedStreamID)); err != nil {
		http.Error(w, "Stream not found", http.StatusNotFound)
		metrics.Http400Errors.Inc()
		slog.Warn("stream not found", "key", cs.Key, "func", "downloadHandler", "streamID", requestedStreamID)
		return
	}

	ext := filepath.Ext(filename)
	idStr := strings.TrimSuffix(filename, ext)
	if !isValidID(idStr) {
		http.Error(w, "Invalid file name", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	contentType, ok := mediaContentTypes[ext]
	if !ok {
		http.Error(w, "Invalid file extension", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	// Optional name query param overrides the download filename.
	downloadFilename := requestedStreamID + "_" + filename
	if queryName := r.URL.Query().Get("name"); queryName != "" {
		downloadFilename = sanitize.BaseName(queryName) + ext
	}

	filePath := filepath.Join(cs.BaseMediaFolder, requestedStreamID, mediaType, idStr+ext)
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		app.report500(r, err, "unable to check file", "key", cs.Key, "func", "downloadHandler", "filename", filename)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", downloadFilename))
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

// getTranscriptHandler returns the full transcript for a stream as JSON.
func (app *App) getTranscriptHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	streamID := r.PathValue("streamID")
	if !isValidID(streamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	dbFetchStart := time.Now()
	lines, err := app.Store.GetTranscript(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to get transcript", "key", cs.Key, "streamID", streamID)
		return
	}
	metrics.RequestProcessingDuration.WithLabelValues("getTranscriptHandler", "db_fetch", cs.Key).Observe(time.Since(dbFetchStart).Seconds())

	jsonEncodeStart := time.Now()
	writeJSON(w, lines)
	metrics.RequestProcessingDuration.WithLabelValues("getTranscriptHandler", "json_encode", cs.Key).Observe(time.Since(jsonEncodeStart).Seconds())
}

// postClipHandler merges a range of lines' raw media into a single clip,
// converts it to the requested format, and uploads it to storage.
func (app *App) postClipHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	observe := func(step string, since time.Time) {
		metrics.MediaProcessingDuration.WithLabelValues(step, cs.Key).Observe(time.Since(since).Seconds())
	}
	processStartTime := time.Now()

	var req struct {
		StreamID string `json:"stream_id"`
		Start    int    `json:"start"`
		End      int    `json:"end"`
		Type     string `json:"type"`
	}

	decodeStart := time.Now()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	observe("decode_json", decodeStart)

	if !isValidID(req.StreamID) {
		http.Error(w, "Invalid stream ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	stream, err := app.Store.GetStreamByID(r.Context(), cs.Key, req.StreamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to get stream for clip", "key", cs.Key, "func", "postClipHandler")
		return
	}
	mediaType := "none"
	if stream != nil {
		mediaType = stream.MediaType
	}

	start := req.Start
	end := req.End
	reqMediaType := req.Type

	clipExt := ".m4a"
	switch reqMediaType {
	case "mp4":
		if mediaType != "video" {
			http.Error(w, "Video clipping is disabled for this stream", http.StatusMethodNotAllowed)
			metrics.Http400Errors.Inc()
			slog.Warn("cannot clip mp4. Media type is not 'video'", "key", cs.Key, "func", "postClipHandler", "mediaType", mediaType)
			return
		}
		clipExt = ".mp4"
	case "mp3":
		clipExt = ".mp3"
	case "m4a", "":
		clipExt = ".m4a"
	default:
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		slog.Warn("invalid media type", "key", cs.Key, "func", "postClipHandler", "mediaType", reqMediaType)
		return
	}

	if start < 0 || end < start || end-start >= app.MaxClipSize {
		slog.Warn("invalid start or end id", "key", cs.Key, "func", "postClipHandler", "start", start, "end", end, "requestedClipSize", 1+end-start, "maxClipSize", app.MaxClipSize)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	uniqueID := shortuuid.New()

	dbGetStart := time.Now()
	fileIDs, err := app.Store.GetFileIDsInRange(r.Context(), cs.Key, req.StreamID, start, end)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, err, "failed to get file ids for clip", "key", cs.Key)
		return
	}
	observe("db_get_files", dbGetStart)

	// Check if all requested IDs were found
	expectedCount := end - start + 1
	if len(fileIDs) != expectedCount {
		http.Error(w, "Missing media files for requested range", http.StatusInternalServerError)
		app.report500(r, fmt.Errorf("expected %d file ids, got %d", expectedCount, len(fileIDs)), "missing file ids for clip", "key", cs.Key, "func", "postClipHandler")
		return
	}

	mergeAudioStart := time.Now()
	mergedRawPath, err := media.MergeRawAudio(r.Context(), app.Storage, app.TempDir, cs.Key, req.StreamID, fileIDs, uniqueID)
	if err != nil {
		// MergeRawAudio cleans up its own partial output on error.
		http.Error(w, "Server error", http.StatusInternalServerError)
		app.report500(r, err, "unable to merge raw audio", "key", cs.Key, "func", "postClipHandler", "startID", start, "endID", end)
		return
	}
	defer os.Remove(mergedRawPath)
	observe("merge_audio", mergeAudioStart)

	// Convert/remux to the requested container.
	convertStart := time.Now()
	tempMediaFile := filepath.Join(app.TempDir, uniqueID+clipExt)
	sidecarFile := ""

	// Note: audio has to be recoded to m4a otherwise it will be broken. Video
	// can be remuxed to a different container without compatibility issues.
	if reqMediaType == "mp4" {
		err = app.Media.Remux(mergedRawPath, tempMediaFile)
		if err == nil {
			// Generate a sidecar m4a so clients on slow connections can use
			// the audio to clip while the video is still loading.
			sidecarFile = filepath.Join(app.TempDir, uniqueID+".m4a")
			if err := app.Media.Convert(mergedRawPath, sidecarFile); err != nil {
				slog.Error("failed to generate sidecar m4a", "key", cs.Key, "err", err)
				// Don't fail the entire request. The mp4 is still good.
				os.Remove(sidecarFile)
				sidecarFile = ""
			} else {
				defer os.Remove(sidecarFile)
			}
		}
	} else {
		err = app.Media.Convert(mergedRawPath, tempMediaFile)
	}
	if err != nil {
		os.Remove(tempMediaFile)
		http.Error(w, "Server error", http.StatusInternalServerError)
		app.report500(r, err, "unable to convert raw media to new extension", "key", cs.Key, "func", "postClipHandler", "extension", clipExt)
		return
	}
	defer os.Remove(tempMediaFile)
	observe("convert_remux", convertStart)

	// Upload with a detached context so a client disconnect doesn't leave a
	// half-written clip in storage.
	uploadCtx := context.WithoutCancel(r.Context())

	uploadClipStart := time.Now()
	if err := app.uploadFile(uploadCtx, storage.ClipKey(cs.Key, req.StreamID, uniqueID, clipExt), tempMediaFile); err != nil {
		slog.Error("failed to upload clip", "key", cs.Key, "err", err)
		http.Error(w, "Storage Error", http.StatusInternalServerError)
		return
	}
	observe("upload_clip", uploadClipStart)

	if sidecarFile != "" {
		uploadSidecarStart := time.Now()
		if err := app.uploadFile(uploadCtx, storage.ClipKey(cs.Key, req.StreamID, uniqueID, ".m4a"), sidecarFile); err != nil {
			slog.Error("failed to upload sidecar m4a", "key", cs.Key, "err", err)
		}
		observe("upload_sidecar", uploadSidecarStart)
	}

	switch clipExt {
	case ".m4a", ".mp3":
		metrics.TotalAudioClipped.WithLabelValues(cs.Key).Inc()
		metrics.StreamAudioClipped.WithLabelValues(cs.Key).Inc()
	case ".mp4":
		metrics.TotalVideoClipped.WithLabelValues(cs.Key).Inc()
		metrics.StreamVideoClipped.WithLabelValues(cs.Key).Inc()
	}

	if time.Since(processStartTime).Seconds() > 10 {
		slog.Warn("slow clip processing time", "key", cs.Key, "func", "postClipHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "start", start, "end", end, "mediaType", reqMediaType)
	}

	writeJSON(w, map[string]string{
		"status":  "success",
		"clip_id": uniqueID,
	})
}

// postTrimHandler cuts an existing clip down to a sub-range and uploads the
// result as a new clip.
func (app *App) postTrimHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	observe := func(step string, since time.Time) {
		metrics.MediaProcessingDuration.WithLabelValues(step, cs.Key).Observe(time.Since(since).Seconds())
	}

	var trimReq struct {
		StreamID   string  `json:"stream_id"`
		ClipID     string  `json:"clip_id"`
		FileFormat string  `json:"file_format"`
		Start      float64 `json:"start"`
		End        float64 `json:"end"`
	}

	decodeStart := time.Now()
	if err := json.NewDecoder(r.Body).Decode(&trimReq); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	observe("decode_json", decodeStart)

	start := trimReq.Start
	end := trimReq.End
	if start < 0 || end <= start {
		http.Error(w, "Invalid start or end time", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	if !slices.Contains([]string{"m4a", "mp3", "mp4"}, trimReq.FileFormat) {
		http.Error(w, "Invalid file format", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	if !isValidID(trimReq.StreamID) || !isValidID(trimReq.ClipID) {
		http.Error(w, "Invalid stream or clip ID", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	uniqueID := shortuuid.New()
	sourceKey := storage.ClipKey(cs.Key, trimReq.StreamID, trimReq.ClipID, "."+trimReq.FileFormat)

	// Download the source clip to a temp file.
	downloadStart := time.Now()
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
		app.report500(r, err, "unable to create temp file for trim", "key", cs.Key, "func", "postTrimHandler")
		return
	}
	_, copyErr := io.Copy(outFile, reader)
	reader.Close()
	outFile.Close()
	defer os.Remove(tempSource)
	if copyErr != nil {
		http.Error(w, "Server Error", http.StatusInternalServerError)
		app.report500(r, copyErr, "unable to download source for trim", "key", cs.Key, "func", "postTrimHandler")
		return
	}
	observe("download_source", downloadStart)

	// Trim
	trimStart := time.Now()
	tempDest := filepath.Join(app.TempDir, fmt.Sprintf("trim_%s.%s", uniqueID, trimReq.FileFormat))
	if err := app.Media.Trim(tempSource, tempDest, start, end); err != nil {
		http.Error(w, "Trim failed", http.StatusInternalServerError)
		slog.Error("ffmpeg trim failed", "err", err)
		return
	}
	defer os.Remove(tempDest)
	observe("trim_processing", trimStart)

	// Upload with a detached context so a client disconnect doesn't leave a
	// half-written clip in storage.
	uploadStart := time.Now()
	destKey := storage.ClipKey(cs.Key, trimReq.StreamID, uniqueID, "."+trimReq.FileFormat)
	if err := app.uploadFile(context.WithoutCancel(r.Context()), destKey, tempDest); err != nil {
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		slog.Error("failed to upload trimmed clip", "err", err)
		return
	}
	observe("upload_trim", uploadStart)

	switch trimReq.FileFormat {
	case "m4a", "mp3":
		metrics.TotalAudioTrimmed.WithLabelValues(cs.Key).Inc()
		metrics.StreamAudioTrimmed.WithLabelValues(cs.Key).Inc()
	case "mp4":
		metrics.TotalVideoTrimmed.WithLabelValues(cs.Key).Inc()
		metrics.StreamVideoTrimmed.WithLabelValues(cs.Key).Inc()
	}

	writeJSON(w, map[string]string{
		"status":  "success",
		"clip_id": uniqueID,
	})
}
