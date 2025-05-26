package internal

import (
	"encoding/base64"
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

func (w *WebSocketServer) Initialize(handle func(string, func(http.ResponseWriter, *http.Request))) {
	err := os.MkdirAll(w.mediaFolder, 0755)
	if err != nil {
		slog.Error("cannot create media folder", "key", w.key, "func", "Initialize", "err", err)
	}

	data, err := w.archive.FileToClientData()
	if err != nil {
		slog.Error("cannot read in gob archive", "key", w.key, "func", "Initialize", "err", err)
	} else {
		slog.Info("read in state from file", "key", w.key, "func", "Initialize")
		w.clientData = data
	}

	slog.Info("creating endpoints", "key", w.key, "func", "Initialize")
	handle(fmt.Sprintf("/ws/%s", w.key), w.wsHandler)
	handle(fmt.Sprintf("/%s/activate", w.key), w.activateHandler)
	handle(fmt.Sprintf("/%s/deactivate", w.key), w.deactivateHandler)
	handle(fmt.Sprintf("/%s/upload", w.key), w.uploadHandler)
	handle(fmt.Sprintf("/%s/update", w.key), w.updateHandler)
	handle(fmt.Sprintf("/%s/statuscheck", w.key), w.statuscheckHandler)

	handle(fmt.Sprintf("/%s/audio", w.key), w.getAudioHandler)
	handle(fmt.Sprintf("/%s/clip", w.key), w.getClipHandler)

	slog.Info("starting save loop in go routine", "key", w.key, "func", "Initialize")
	go w.saveDataLoop()
}

func (w *WebSocketServer) activateStream(activeId string, activeTitle string, startTime string, mediaType string) bool {
	isNewStream := false
	w.streamLock.Lock()
	msg := ""
	w.clientData.ActiveTitle = activeTitle
	w.clientData.StartTime = startTime
	if w.clientData.ActiveID == activeId {
		// Stream is already active, only send status event if isLive is false
		if !w.clientData.IsLive {
			w.clientData.IsLive = true
			msg = fmt.Sprintf("![]status\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.IsLive)
			slog.Debug("stream is already active and isLive is false, sending status event", "key", w.key, "func", "activateStream", "activeID", activeId)
		} else {
			slog.Debug("stream is already active and isLive is already true, skipping status event", "key", w.key, "func", "activateStream", "activeID", activeId)
		}
	} else {
		w.clientData.ActiveID = activeId
		w.clientData.IsLive = true
		w.clientData.MediaType = mediaType
		isNewStream = true
		// New stream is active, clear local state and send newstream event
		msg = fmt.Sprintf("![]newstream\n%s\n%s\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.StartTime, w.clientData.MediaType, w.clientData.IsLive)
		slog.Debug("received new stream id, clearing local state and sending newstream event", "key", w.key, "func", "activateStream", "activeID", activeId)
	}
	w.streamLock.Unlock()

	if isNewStream {
		w.transcriptLock.Lock()
		w.clientData.Transcript = make([]Line, 0)
		w.transcriptLock.Unlock()
		w.ResetAudioFile()
	}

	if msg != "" {
		w.broadcast([]byte(msg))
		return true
	} else {
		return false
	}
}

func (w *WebSocketServer) deactivateStream(activeId string) bool {
	w.streamLock.Lock()
	msg := ""
	if w.clientData.ActiveID == activeId {
		w.clientData.IsLive = false
		msg = fmt.Sprintf("![]status\n%s\n%s\n%v", w.clientData.ActiveID, w.clientData.ActiveTitle, w.clientData.IsLive)
		slog.Debug("deactivating stream", "key", w.key, "func", "deactivateStream", "activeID", activeId)
	}
	w.streamLock.Unlock()

	if msg != "" {
		w.broadcast([]byte(msg))
		return true
	} else {
		return false
	}
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

func (ws *WebSocketServer) basicAuth(r *http.Request) (string, string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		slog.Debug("authHeader is empty", "key", ws.key, "func", "basicAuth")
		return "", "", false
	}

	if !strings.HasPrefix(authHeader, "Basic ") {
		slog.Debug("authHeader does not have the correct prefix", "key", ws.key, "func", "basicAuth", "authHeader", authHeader)
		return "", "", false
	}

	token, _ := base64.StdEncoding.DecodeString(authHeader[6:])
	parts := strings.SplitN(string(token), ":", 2)
	if len(parts) != 2 {
		slog.Debug("authHeader is not in the correct format", "key", ws.key, "func", "basicAuth", "authHeader", authHeader)
		return "", "", false
	}

	return parts[0], parts[1], true
}

func (ws *WebSocketServer) verify(w http.ResponseWriter, r *http.Request) bool {
	username, password, ok := ws.basicAuth(r)
	if !ok {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return false
	}

	if username != ws.username || password != ws.password {
		slog.Debug("incorrect username and password", "key", ws.key, "func", "verify", "requestUsername", username, "requestPassword", password)
		http.Error(w, "404 page not found", http.StatusNotFound)
		return false
	}

	return true
}

func (ws *WebSocketServer) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if !ws.verify(w, r) {
		return
	}

	// Decode the JSON data from the request body
	decoder := json.NewDecoder(r.Body)
	var data ClientData
	if err := decoder.Decode(&data); err != nil {
		http.Error(w, "Error decoding JSON data", http.StatusBadRequest)
		slog.Error("unable to decode JSON data", "key", ws.key, "func", "uploadHandler", "err", err)
		return
	}

	// Very susecptiale to deadlock. Pt 2
	ws.clientsLock.Lock()
	ws.streamLock.Lock()
	ws.transcriptLock.Lock()
	ws.clientData = &data
	ws.transcriptLock.Unlock()
	ws.streamLock.Unlock()
	ws.clientsLock.Unlock()

	ws.refreshAll()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON UploadData data received and processed successfully"))
	slog.Info("successfully received and processed worker's state", "key", ws.key, "func", "uploadHandler")
}

func (ws *WebSocketServer) updateHandler(w http.ResponseWriter, r *http.Request) {
	if !ws.verify(w, r) {
		return
	}

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
		slog.Error("unable to decode JSON data", "key", ws.key, "func", "updateHandler", "err", err)
		return
	}

	ws.transcriptLock.Lock()
	if len(ws.clientData.Transcript) > 0 {
		// If the next ID does not match us with our current data, we return a reqeust to send us entire state
		lastID := ws.clientData.Transcript[len(ws.clientData.Transcript)-1].ID
		if data.NewLine.ID-1 != lastID {
			ws.transcriptLock.Unlock()
			http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
			slog.Warn("current last id is not one behind new line id. Requesting worker to send current state.", "key", ws.key, "func", "updateHandler", "lastID", lastID, "newLineID", data.NewLine.ID)
			return
		}
	} else if data.NewLine.ID > 0 {
		// Our state is empty, but we got an ID that is greater than 0. Meaning we are missing some data.
		ws.transcriptLock.Unlock()
		http.Error(w, "Server out of sync. Send current state.", http.StatusConflict)
		slog.Warn("current state is empty but we got an id > 0. Requesting worker to send current state.", "key", ws.key, "func", "updateHandler", "newLineID", data.NewLine.ID)
		return
	}
	// else, our state is empty and we received a 0 id. So all is good.
	ws.clientData.Transcript = append(ws.clientData.Transcript, data.NewLine)
	ws.transcriptLock.Unlock()

	if ws.clientData.MediaType == "none" {
		ws.refreshAll()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("JSON Line data received and processed successfully"))
		return
	}

	// Only process raw data if media type is not none.
	rawFile, fileErr := ws.RawB64ToFile(data.RawB64Data, data.NewLine.ID, "raw")
	mp3File := ChangeExtension(rawFile, ".mp3")
	convertError := FfmpegConvert(rawFile, mp3File)
	ws.refreshAll()

	if fileErr != nil {
		http.Error(w, "Unable to save raw media to file.", http.StatusInternalServerError)
		slog.Error("unable to save raw media to file.", "key", ws.key, "func", "updateHandler", "err", fileErr)
		return
	}

	if convertError != nil {
		os.Remove(rawFile)
		os.Remove(mp3File)
		http.Error(w, "Unable to convert raw media to mp3.", http.StatusInternalServerError)
		slog.Error("unable to convert raw media to mp3.", "key", ws.key, "func", "updateHandler", "err", fileErr)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("JSON Line data received and processed successfully"))
}

func (ws *WebSocketServer) activateHandler(w http.ResponseWriter, r *http.Request) {
	if !ws.verify(w, r) {
		return
	}

	// Parse the query parameters
	query := r.URL.Query()
	streamID := query.Get("id")
	title := query.Get("title")
	startTime := query.Get("startTime")
	mediaType := query.Get("mediaType")

	// Check if the required parameters are present
	if streamID == "" || title == "" || startTime == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		slog.Warn("invalid parameters", "key", ws.key, "func", "activateHandler", "streamID", streamID, "title", title, "startTime", startTime)
		return
	}

	activated := ws.activateStream(streamID, title, startTime, mediaType)

	if activated {
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully activated", ws.key))
		slog.Info("activated stream", "key", ws.key, "func", "activateHandler", "streamID", streamID, "mediaType", mediaType)
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream is already activated", ws.key))
		slog.Info("id already activated", "key", ws.key, "func", "activateHandler", "streamID", streamID)
	}
}

func (ws *WebSocketServer) deactivateHandler(w http.ResponseWriter, r *http.Request) {
	if !ws.verify(w, r) {
		return
	}

	// Parse the query parameters
	query := r.URL.Query()
	streamID := query.Get("id")

	// Check if the required parameters are present
	if streamID == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		slog.Warn("invalid parameters, streamID is empty", "key", ws.key, "func", "deactivateHandler")
		return
	}

	deactivated := ws.deactivateStream(streamID)

	if deactivated {
		w.WriteHeader(http.StatusOK)
		w.Write(fmt.Appendf(nil, "%s stream successfully deactivated", ws.key))
	} else {
		w.WriteHeader(http.StatusAlreadyReported)
		w.Write(fmt.Appendf(nil, "%s stream was not deactivated", ws.key))
	}
}

func (ws *WebSocketServer) statuscheckHandler(w http.ResponseWriter, r *http.Request) {
	if !ws.verify(w, r) {
		return
	}

	ws.clientsLock.Lock()
	size := ws.clientConnections
	ws.clientsLock.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write(fmt.Appendf(nil, "Current number of clients: %d", size))
}

func (ws *WebSocketServer) getAudioHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		slog.Warn("invalid request. Method is not a GET", "key", ws.key, "func", "getAudioHandler", "method", r.Method)
		return
	}

	if ws.clientData.MediaType == "none" {
		http.Error(w, "Audio download is disabled for this stream", http.StatusMethodNotAllowed)
		slog.Warn("cannot retrieve audio. Media type is none", "key", ws.key, "func", "getAudioHandler")
		return
	}

	// Extract the ID from the query parameter
	query := r.URL.Query()
	idStr := query.Get("id")
	stream := query.Get("stream")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		slog.Warn("unable to convert id to int", "key", ws.key, "func", "getAudioHandler", "id", idStr, "err", err)
		return
	}

	filePath := filepath.Join(ws.mediaFolder, fmt.Sprintf("%d.mp3", id))

	// Check if the file exists
	_, err = os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "No audio found", http.StatusNotFound)
			slog.Warn("no audio file found for the requested id", "key", ws.key, "func", "getAudioHandler", "id", id)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		slog.Error("unable to check audio file", "key", ws.key, "func", "getAudioHandler", "id", id, "err", err)
		return
	}

	// Enable Content-Disposition to have the browser automatically download the audio
	if stream != "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_%d.mp3\"", ws.clientData.ActiveID, id))
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeFile(w, r, filePath)
}

func (ws *WebSocketServer) getClipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusMethodNotAllowed)
		slog.Warn("invalid request. Method is not a GET", "key", ws.key, "func", "getClipHandler", "method", r.Method)
		return
	}

	if ws.clientData.MediaType == "none" {
		http.Error(w, "Clipping is disabled for this stream", http.StatusMethodNotAllowed)
		slog.Warn("cannot clip media. Media type is none", "key", ws.key, "func", "getClipHandler")
		return
	}

	// Extract the ID from the query parameter
	query := r.URL.Query()
	startStr := query.Get("start")
	endStr := query.Get("end")
	clipName := strings.TrimSpace(query.Get("name"))
	mediaType := strings.TrimSpace(query.Get("type"))
	start, err := strconv.Atoi(startStr)
	end, err2 := strconv.Atoi(endStr)
	clipExt := ".mp3"
	contentType := "audio/mpeg"
	if mediaType == "mp4" && ws.clientData.MediaType == "video" {
		clipExt = ".mp4"
		contentType = "video/mp4"
	}

	if mediaType == "mp4" && ws.clientData.MediaType != "video" {
		http.Error(w, "Video clipping is disabled for this stream", http.StatusMethodNotAllowed)
		slog.Warn("cannot clip mp4. Media type is not 'video'", "key", ws.key, "func", "getClipHandler", "mediaType", ws.clientData.MediaType)
		return
	}

	if err != nil {
		slog.Warn("unable to convert start id to int", "key", ws.key, "func", "getClipHandler", "start", startStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if err2 != nil {
		slog.Warn("unable to convert end id to int", "key", ws.key, "func", "getClipHandler", "end", endStr, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if start < 0 || end <= start || end-start >= ws.maxClipSize {
		slog.Warn("invalid start or end id", "key", ws.key, "func", "getClipHandler", "start", start, "end", end, "requestedClipSize", 1+end-start, "maxClipSize", ws.maxClipSize, "err", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	mediaFilePath := filepath.Join(ws.mediaFolder, fmt.Sprintf("%d-%d%s", start, end, clipExt))
	mergedMediaPath, alreadyConverted, err := ws.MergeRawAudio(start, end, clipExt)
	if err != nil {
		os.Remove(mediaFilePath)
		os.Remove(mergedMediaPath)
		http.Error(w, "Server error", http.StatusInternalServerError)
		slog.Error("unable to merge raw audio", "key", ws.key, "func", "getClipHandler", "startID", start, "endID", end, "err", err)
		return
	}

	// do an aditional step of converting raw to clipExt since we only send out clipExt files for maximum compatibility.
	if !alreadyConverted {
		// Note: audio has to be reencoded to mp3 otherwise it will be broken. Video can be remuxed to a different container without any compatibility issues.
		if mediaType == "mp4" {
			err = FfmpegRemux(mergedMediaPath, mediaFilePath)
		} else {
			err = FfmpegConvert(mergedMediaPath, mediaFilePath)
		}
		if err != nil {
			os.Remove(mediaFilePath)
			os.Remove(mergedMediaPath)
			slog.Error("unable to convert raw media to new extension", "key", ws.key, "func", "getClipHandler", "extension", clipExt, "err", err)
			http.Error(w, "Server error", http.StatusInternalServerError)
			return
		}
		err = os.Remove(mergedMediaPath)
		if err != nil {
			slog.Error("unable to remove temp merged raw file", "key", ws.key, "func", "getClipHandler", "err", err)
		}

		mergedMediaPath = mediaFilePath
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
	// use BaseName rather than Name because BaseName removes / where as Name removes anything before the last /
	// Also BaseName preserves capitalization.
	sanitizedName := sanitize.BaseName(attachmentName)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s%s\"", sanitizedName, clipExt))
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, mergedMediaPath)
}
