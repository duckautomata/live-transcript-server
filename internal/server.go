package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kennygrant/sanitize"
)

// Rewriting Initialize and Middleware to be clean and correct with shadowing handling
func (ws *WebSocketServer) Initialize(handle func(string, func(http.ResponseWriter, *http.Request))) {
	err := os.MkdirAll(ws.mediaFolder, 0755)
	if err != nil {
		slog.Error("cannot create media folder", "key", ws.key, "func", "Initialize", "err", err)
	}

	data, err := ws.archive.FileToClientData()
	if err != nil {
		slog.Error("cannot read in gob archive", "key", ws.key, "func", "Initialize", "err", err)
	} else {
		slog.Info("read in state from file", "key", ws.key, "func", "Initialize")
		ws.clientData = data
	}

	slog.Info("creating endpoints", "key", ws.key, "func", "Initialize")
	handle(fmt.Sprintf("/ws/%s", ws.key), ws.wsHandler)

	// Protected endpoints
	handle(fmt.Sprintf("/%s/activate", ws.key), ws.apiKeyMiddleware(ws.activateHandler))
	handle(fmt.Sprintf("/%s/deactivate", ws.key), ws.apiKeyMiddleware(ws.deactivateHandler))
	handle(fmt.Sprintf("/%s/upload", ws.key), ws.apiKeyMiddleware(ws.uploadHandler))
	handle(fmt.Sprintf("/%s/update", ws.key), ws.apiKeyMiddleware(ws.updateHandler))
	handle(fmt.Sprintf("/%s/statuscheck", ws.key), ws.apiKeyMiddleware(ws.statuscheckHandler))

	// Public endpoints
	handle(fmt.Sprintf("/%s/audio", ws.key), ws.getAudioHandler)
	handle(fmt.Sprintf("/%s/clip", ws.key), ws.getClipHandler)

	slog.Info("starting save loop in go routine", "key", ws.key, "func", "Initialize")
	go ws.saveDataLoop()
}

func (ws *WebSocketServer) apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.apiKey == "" {
			next(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key != ws.apiKey {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (w *WebSocketServer) activateStream(activeId string, activeTitle string, startTime string, mediaType string) bool {
	isNewStream := false
	w.streamLock.Lock()
	defer w.streamLock.Unlock()

	msg := ""

	// Case 1: A completely new stream is starting (different ID)
	if w.clientData.ActiveID != activeId {
		StreamAudioPlayed.WithLabelValues(w.key).Set(0)
		StreamAudioClipped.WithLabelValues(w.key).Set(0)
		StreamVideoClipped.WithLabelValues(w.key).Set(0)

		// If a previous stream was active for this key, remove its Prometheus metric first.
		if w.clientData.ActiveID != "" {
			ActivatedStreams.DeleteLabelValues(w.key, w.clientData.ActiveID, w.clientData.ActiveTitle)
			slog.Info("removing old stream metric", "key", w.key, "func", "activateStream", "oldStreamID", w.clientData.ActiveID)
		}

		// Update server state with the new stream's info
		w.clientData.ActiveID = activeId
		w.clientData.ActiveTitle = activeTitle
		w.clientData.StartTime = startTime
		w.clientData.IsLive = true
		w.clientData.MediaType = mediaType
		isNewStream = true

		// Prepare the broadcast message for clients
		msg = fmt.Sprintf("![]newstream\n%s\n%s\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.StartTime, w.clientData.MediaType, w.clientData.IsLive)
		slog.Debug("received new stream id, sending newstream event", "key", w.key, "func", "activateStream", "activeID", activeId)

	} else {
		// Case 2: The same stream is being reactivated
		if !w.clientData.IsLive {
			w.clientData.IsLive = true
			msg = fmt.Sprintf("![]status\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.IsLive)
			slog.Debug("reactivating existing stream, sending status event", "key", w.key, "func", "activateStream", "activeID", activeId)
		} else {
			// Case 3: A request to activate an already active stream
			slog.Debug("stream is already active, skipping event", "key", w.key, "func", "activateStream", "activeID", activeId)
		}
	}

	if isNewStream {
		w.transcriptLock.Lock()
		w.clientData.Transcript = make([]Line, 0)
		w.transcriptLock.Unlock()
		w.ResetAudioFile()
	}

	if msg != "" {
		w.broadcast([]byte(msg))
		return true // Indicates a change was made
	}

	return false // Indicates no change was made
}

func (w *WebSocketServer) deactivateStream(activeId string) bool {
	w.streamLock.Lock()
	defer w.streamLock.Unlock()

	msg := ""
	if w.clientData.ActiveID == activeId && w.clientData.IsLive {
		// Remove the gauge from Prometheus since the stream is no longer active.
		deleted := ActivatedStreams.DeleteLabelValues(w.key, w.clientData.ActiveID, w.clientData.ActiveTitle)
		if deleted {
			slog.Info("successfully removed stream metric on deactivation", "key", w.key, "func", "deactivateStream", "streamID", activeId)
		} else {
			slog.Info("failed to remove stream metric on deactivation", "key", w.key, "func", "deactivateStream", "streamID", activeId)
		}

		w.clientData.IsLive = false
		msg = fmt.Sprintf("![]status\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.IsLive)
		slog.Debug("deactivating stream", "key", w.key, "func", "deactivateStream", "activeID", activeId)
	}

	if msg != "" {
		w.broadcast([]byte(msg))
		return true // Indicates a change was made
	}

	return false // Indicates no change was made
}

func (w *WebSocketServer) saveDataLoop() {
	for {
		time.Sleep(time.Minute * 1)

		// Very susecptiale to deadlock.
		w.clientsLock.Lock()
		w.streamLock.Lock()
		w.transcriptLock.Lock()

		// Saving new data to file
		if err := w.archive.ClientDataToFile(w.clientData); err != nil {
			slog.Error("unable to save current state to file", "key", w.key, "func", "saveDataLoop", "err", err)
		}

		w.transcriptLock.Unlock()
		w.streamLock.Unlock()
		w.clientsLock.Unlock()
	}
}

func (ws *WebSocketServer) uploadHandler(w http.ResponseWriter, r *http.Request) {
	uploadStartTime := time.Now()

	// Decode the JSON data from the request body
	decoder := json.NewDecoder(r.Body)
	var data ClientData
	if err := decoder.Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", ws.key, "func", "uploadHandler", "err", err)
		return
	}
	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	// Very susecptiale to deadlock. Pt 2
	ws.clientsLock.Lock()
	ws.streamLock.Lock()
	ws.transcriptLock.Lock()
	ws.clientData = &data
	ws.transcriptLock.Unlock()
	ws.streamLock.Unlock()
	ws.clientsLock.Unlock()

	ws.refreshAll(uploadTime, processStartTime)

	if uploadTime > 5*1000 {
		slog.Warn("slow upload time", "key", ws.key, "func", "uploadHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}
	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", ws.key, "func", "uploadHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds())
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON UploadData data received and processed successfully"))
}

func (ws *WebSocketServer) updateHandler(w http.ResponseWriter, r *http.Request) {
	uploadStartTime := time.Now()

	// if !ws.clientData.IsLive {
	// 	http.Error(w, "Stream is not live yet. Please activate stream before sending data.", http.StatusBadRequest)
	// 	slog.Warn("received update but no stream is live.", "key", ws.key, "func", "updateHandler")
	// 	return
	// }

	// Decode the JSON data from the request body
	decoder := json.NewDecoder(r.Body)
	var data UpdateData
	if err := decoder.Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Error("unable to decode JSON data", "key", ws.key, "func", "updateHandler", "err", err)
		return
	}
	uploadTime := time.Since(uploadStartTime).Milliseconds()
	processStartTime := time.Now()

	ws.transcriptLock.Lock()
	if len(ws.clientData.Transcript) > 0 {
		// If the next ID does not match us with our current data, we return a reqeust to send us entire state
		lastID := ws.clientData.Transcript[len(ws.clientData.Transcript)-1].ID
		if data.NewLine.ID-1 != lastID {
			ws.transcriptLock.Unlock()
			http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
			ServerOOS.Inc()
			slog.Warn("current last id is not one behind new line id. Requesting worker to send current state.", "key", ws.key, "func", "updateHandler", "lastID", lastID, "newLineID", data.NewLine.ID)
			return
		}
	} else if data.NewLine.ID > 0 {
		// Our state is empty, but we got an ID that is greater than 0. Meaning we are missing some data.
		ws.transcriptLock.Unlock()
		http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
		ServerOOS.Inc()
		slog.Warn("current state is empty but we got an id > 0. Requesting worker to send current state.", "key", ws.key, "func", "updateHandler", "newLineID", data.NewLine.ID)
		return
	}
	// else, our state is empty and we received a 0 id. So all is good.
	ws.clientData.Transcript = append(ws.clientData.Transcript, data.NewLine)
	ws.transcriptLock.Unlock()

	if ws.clientData.MediaType == "none" || data.RawB64Data == "" {
		ws.refreshAll(uploadTime, processStartTime)
		if time.Since(processStartTime).Seconds() > 1 {
			slog.Warn("slow processing time", "key", ws.key, "func", "updateHandler", "uploadTimeMs", time.Since(uploadStartTime).Milliseconds(), "processingTimeMs", time.Since(processStartTime).Milliseconds(), "lineId", data.NewLine.ID)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("JSON Line data received and processed successfully"))
		return
	}

	// Only process raw data if media type is not none and there is data
	rawFile, fileErr := ws.RawB64ToFile(data.RawB64Data, data.NewLine.ID, "raw")
	m4aFile := ChangeExtension(rawFile, ".m4a")
	convertError := FfmpegConvert(rawFile, m4aFile)
	ws.refreshAll(uploadTime, processStartTime)

	if fileErr != nil {
		http.Error(w, "Unable to save raw media to file.", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to save raw media to file.", "key", ws.key, "func", "updateHandler", "err", fileErr)
		return
	}

	if convertError != nil {
		os.Remove(rawFile)
		os.Remove(m4aFile)
		http.Error(w, "Unable to convert raw media to m4a.", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to convert raw media to m4a.", "key", ws.key, "func", "updateHandler", "err", convertError)
		return
	}

	if uploadTime > 5*1000 {
		slog.Warn("slow upload time", "key", ws.key, "func", "updateHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds(), "lineId", data.NewLine.ID)
	}
	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow processing time", "key", ws.key, "func", "updateHandler", "uploadTimeMs", uploadTime, "processingTimeMs", time.Since(processStartTime).Milliseconds(), "lineId", data.NewLine.ID)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Line data received and processed successfully"))
}

func (ws *WebSocketServer) activateHandler(w http.ResponseWriter, r *http.Request) {
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
		slog.Warn("invalid parameters", "key", ws.key, "func", "activateHandler", "streamID", streamID, "title", title, "startTime", startTime)
		return
	}

	var startTimeUnix int64

	// Try to parse the provided startTime; if it fails, use the current time.
	parsedTime, err := strconv.ParseInt(startTime, 10, 64)
	if err != nil {
		slog.Warn("invalid or empty startTime received, using current system time", "key", ws.key, "func", "activateHandler", "receivedTime", startTime)
		startTimeUnix = time.Now().Unix()
	} else {
		startTimeUnix = parsedTime
	}

	// Convert the final timestamp back to a string for use in other functions.
	finalStartTimeStr := strconv.FormatInt(startTimeUnix, 10)

	activated := ws.activateStream(streamID, title, finalStartTimeStr, mediaType)

	if activated {
		ActivatedStreams.WithLabelValues(ws.key, streamID, title).Set(float64(startTimeUnix))
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully activated", ws.key))
		slog.Debug("activated stream", "key", ws.key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID, "mediaType", mediaType)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream is already activated", ws.key))
		slog.Debug("id already activated", "key", ws.key, "func", "activateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	}
}

func (ws *WebSocketServer) deactivateHandler(w http.ResponseWriter, r *http.Request) {
	processStartTime := time.Now()

	// Parse the query parameters
	query := r.URL.Query()
	streamID := query.Get("id")

	// Check if the required parameters are present
	if streamID == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid parameters, streamID is empty", "key", ws.key, "func", "deactivateHandler")
		return
	}

	deactivated := ws.deactivateStream(streamID)

	if deactivated {
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully deactivated", ws.key))
		slog.Debug("deactivated stream", "key", ws.key, "func", "deactivateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream was not deactivated", ws.key))
		slog.Debug("id already deactivated", "key", ws.key, "func", "deactivateHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "streamID", streamID)
	}
}

func (ws *WebSocketServer) statuscheckHandler(w http.ResponseWriter, r *http.Request) {
	ws.clientsLock.Lock()
	size := ws.clientConnections
	ws.clientsLock.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write(fmt.Appendf(nil, "Current number of clients: %d", size))
}

func (ws *WebSocketServer) getAudioHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", ws.key, "func", "getAudioHandler", "method", r.Method)
		return
	}

	if ws.clientData.MediaType == "none" {
		http.Error(w, "Audio download is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot retrieve audio. Media type is none", "key", ws.key, "func", "getAudioHandler")
		return
	}
	processStartTime := time.Now()

	// Extract the ID from the query parameter
	query := r.URL.Query()
	idStr := query.Get("id")
	stream := query.Get("stream")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("unable to convert id to int", "key", ws.key, "func", "getAudioHandler", "id", idStr, "err", err)
		return
	}

	filePath := filepath.Join(ws.mediaFolder, fmt.Sprintf("%d.m4a", id))

	// Check if the file exists
	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "No audio found", http.StatusNotFound)
			Http400Errors.Inc()
			slog.Warn("no audio file found for the requested id", "key", ws.key, "func", "getAudioHandler", "id", id)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to check audio file", "key", ws.key, "func", "getAudioHandler", "id", id, "err", err)
		return
	}

	TotalAudioPlayed.WithLabelValues(ws.key).Inc()
	StreamAudioPlayed.WithLabelValues(ws.key).Inc()

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow audio processing time", "key", ws.key, "func", "getAudioHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "id", idStr, "stream", stream)
	}

	// Enable Content-Disposition to have the browser automatically download the audio
	if stream != "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_%d.m4a\"", ws.clientData.ActiveID, id))
	}
	w.Header().Set("Content-Type", "audio/mp4")
	http.ServeFile(w, r, filePath)
}

func (ws *WebSocketServer) getClipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("invalid request. Method is not a GET", "key", ws.key, "func", "getClipHandler", "method", r.Method)
		return
	}

	if ws.clientData.MediaType == "none" {
		http.Error(w, "Clipping is disabled for this stream", http.StatusMethodNotAllowed)
		Http400Errors.Inc()
		slog.Warn("cannot clip media. Media type is none", "key", ws.key, "func", "getClipHandler")
		return
	}
	processStartTime := time.Now()

	// Extract the ID from the query parameter
	query := r.URL.Query()
	startStr := query.Get("start")
	endStr := query.Get("end")
	clipName := strings.TrimSpace(query.Get("name"))
	mediaType := strings.TrimSpace(query.Get("type"))
	start, err := strconv.Atoi(startStr)
	end, err2 := strconv.Atoi(endStr)

	clipExt := ".m4a"
	contentType := "audio/mp4"

	if mediaType == "mp4" {
		if ws.clientData.MediaType != "video" {
			http.Error(w, "Video clipping is disabled for this stream", http.StatusMethodNotAllowed)
			Http400Errors.Inc()
			slog.Warn("cannot clip mp4. Media type is not 'video'", "key", ws.key, "func", "getClipHandler", "mediaType", ws.clientData.MediaType)
			return
		}
		clipExt = ".mp4"
		contentType = "video/mp4"
	} else if mediaType == "mp3" {
		clipExt = ".mp3"
		contentType = "audio/mpeg"
	} else if mediaType == "m4a" || mediaType == "" {
		// Default to m4a
		clipExt = ".m4a"
		contentType = "audio/mp4"
	} else {
		http.Error(w, "Invalid media type", http.StatusBadRequest)
		Http400Errors.Inc()
		slog.Warn("invalid media type", "key", ws.key, "func", "getClipHandler", "mediaType", mediaType)
		return
	}

	if err != nil {
		slog.Warn("unable to convert start id to int", "key", ws.key, "func", "getClipHandler", "start", startStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if err2 != nil {
		slog.Warn("unable to convert end id to int", "key", ws.key, "func", "getClipHandler", "end", endStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if start < 0 || end <= start || end-start >= ws.maxClipSize {
		slog.Warn("invalid start or end id", "key", ws.key, "func", "getClipHandler", "start", start, "end", end, "requestedClipSize", 1+end-start, "maxClipSize", ws.maxClipSize, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	uniqueID := fmt.Sprintf("%d-%d-%d", start, end, time.Now().UnixNano())
	mergedMediaPath, err := ws.MergeRawAudio(start, end, uniqueID)
	if err != nil {
		os.Remove(mergedMediaPath)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("unable to merge raw audio", "key", ws.key, "func", "getClipHandler", "startID", start, "endID", end, "err", err)
		return
	}
	defer os.Remove(mergedMediaPath) // Delete the merged raw file when done

	mediaFilePath := filepath.Join(ws.mediaFolder, uniqueID+clipExt)

	// Note: audio has to be reencoded to m4a otherwise it will be broken. Video can be remuxed to a different container without any compatibility issues.
	if mediaType == "mp4" {
		err = FfmpegRemux(mergedMediaPath, mediaFilePath)
	} else {
		err = FfmpegConvert(mergedMediaPath, mediaFilePath)
	}
	if err != nil {
		os.Remove(mediaFilePath)
		slog.Error("unable to convert raw media to new extension", "key", ws.key, "func", "getClipHandler", "extension", clipExt, "err", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		Http500Errors.Inc()
		return
	}

	if clipExt == ".m4a" || clipExt == ".mp3" {
		TotalAudioClipped.WithLabelValues(ws.key).Inc()
		StreamAudioClipped.WithLabelValues(ws.key).Inc()
	} else if clipExt == ".mp4" {
		TotalVideoClipped.WithLabelValues(ws.key).Inc()
		StreamVideoClipped.WithLabelValues(ws.key).Inc()
	}

	if clipName == "" {
		clipName = fmt.Sprintf("%d-%d", start, end)
	}
	unixTimeInt, err := strconv.Atoi(ws.clientData.StartTime)
	unixTimeInt64 := int64(unixTimeInt)
	if err != nil {
		unixTimeInt64 = time.Now().Unix()
	}
	yymmdd := time.Unix(unixTimeInt64, 0).Format("20060102")
	attachmentName := fmt.Sprintf("%s-%s-%s", ws.key, yymmdd, clipName)

	if time.Since(processStartTime).Seconds() > 1 {
		slog.Warn("slow clip processing time", "key", ws.key, "func", "getClipHandler", "processingTimeMs", time.Since(processStartTime).Milliseconds(), "start", startStr, "end", endStr, "clipName", clipName, "mediaType", mediaType)
	}

	// use BaseName rather than Name because BaseName removes / where as Name removes anything before the last /
	// Also BaseName preserves capitalization.
	sanitizedName := sanitize.BaseName(attachmentName)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s%s\"", sanitizedName, clipExt))
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, mediaFilePath)
}
