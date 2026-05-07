package internal

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

//go:embed admin_ui.html
var adminUIFS embed.FS

// adminUIHandler serves the embedded admin UI page. The page itself is static
// — auth is enforced on the API endpoints it calls. The JS reads the channel
// from the URL and prompts for the per-channel admin key on first load.
func (app *App) adminUIHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	if _, ok := app.Channels[channelKey]; !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}
	data, err := adminUIFS.ReadFile("admin_ui.html")
	if err != nil {
		http.Error(w, "Failed to load admin UI", http.StatusInternalServerError)
		slog.Error("failed to read admin UI template", "func", "adminUIHandler", "err", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Discourage caching so a redeploy with a fresh UI is picked up immediately.
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(data); err != nil {
		slog.Error("failed to write admin UI", "func", "adminUIHandler", "err", err)
	}
}

// AdminInfoResponse is the aggregated state returned by GET /{channel}/admin/info.
type AdminInfoResponse struct {
	Channel          string        `json:"channel"`
	Worker           *WorkerStatus `json:"worker"`
	CurrentStream    *Stream       `json:"currentStream"`
	Streams          []Stream      `json:"streams"`
	IncomingURLs     []string      `json:"incomingUrls"`
	RestartPending   bool          `json:"restartPending"`
	RestartAt        int64         `json:"restartRequestedAt"`
	Server           ServerInfo    `json:"server"`
	ConnectedClients int           `json:"connectedClients"`
}

func (app *App) getAdminInfoHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	cs := app.Channels[channelKey] // existence guaranteed by middleware
	ctx := r.Context()

	worker, err := app.GetWorkerStatusByKey(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get worker status", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if worker != nil {
		// Mirror the public /status endpoint's "active in last 5 min" rule.
		worker.IsActive = time.Now().Unix()-worker.LastSeen < 300
	}

	streams, err := app.GetAllStreams(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get streams", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if streams == nil {
		streams = []Stream{}
	}

	currentStream, err := app.GetRecentStream(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get current stream", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
		return
	}

	incoming, err := app.GetIncomingStreams(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get incoming streams", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if incoming == nil {
		incoming = []string{}
	}

	restartAt, err := app.GetRestartRequest(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to get restart request", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
		return
	}

	cs.ClientsLock.Lock()
	connections := cs.ClientConnections
	cs.ClientsLock.Unlock()

	resp := AdminInfoResponse{
		Channel:          channelKey,
		Worker:           worker,
		CurrentStream:    currentStream,
		Streams:          streams,
		IncomingURLs:     incoming,
		RestartPending:   restartAt > 0,
		RestartAt:        restartAt,
		Server:           ServerInfo{Version: app.Version, BuildTime: app.BuildTime},
		ConnectedClients: connections,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to encode admin info", "key", channelKey, "func", "getAdminInfoHandler", "err", err)
	}
}

// postAdminIncomingHandler lets an admin manually add a URL to the queue.
func (app *App) postAdminIncomingHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")

	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	url := strings.TrimSpace(body.URL)
	if url == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		http.Error(w, "url must start with http:// or https://", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	if err := app.UpsertIncomingStream(r.Context(), channelKey, url, time.Now().Unix()); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to upsert incoming stream", "key", channelKey, "func", "postAdminIncomingHandler", "err", err)
		return
	}
	slog.Info("admin queued incoming stream", "key", channelKey, "func", "postAdminIncomingHandler", "url", url)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminIncomingHandler removes a URL from the queue.
func (app *App) deleteAdminIncomingHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "Missing required parameter: url", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}

	rowsAffected, err := app.DeleteIncomingStream(r.Context(), channelKey, url)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to delete incoming stream", "key", channelKey, "func", "deleteAdminIncomingHandler", "err", err)
		return
	}
	if rowsAffected == 0 {
		http.Error(w, "URL not queued", http.StatusNotFound)
		return
	}
	slog.Info("admin removed incoming stream", "key", channelKey, "func", "deleteAdminIncomingHandler", "url", url)
	w.WriteHeader(http.StatusNoContent)
}

// postAdminRestartHandler triggers a worker restart for this channel.
func (app *App) postAdminRestartHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	now := time.Now().Unix()
	if err := app.UpsertRestartRequest(r.Context(), channelKey, now); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to set restart request", "key", channelKey, "func", "postAdminRestartHandler", "err", err)
		return
	}
	slog.Info("admin requested worker restart", "key", channelKey, "func", "postAdminRestartHandler", "requestedAt", now)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminRestartHandler clears a pending restart for this channel.
// Idempotent: returns 204 whether or not anything was pending, so the UI
// doesn't need to special-case a stale view.
func (app *App) deleteAdminRestartHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	rowsAffected, err := app.DeleteRestartRequest(r.Context(), channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to clear restart request", "key", channelKey, "func", "deleteAdminRestartHandler", "err", err)
		return
	}
	slog.Info("admin cleared restart request", "key", channelKey, "func", "deleteAdminRestartHandler", "wasPending", rowsAffected > 0)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminStreamHandler removes a stream's metadata and transcript. By
// default the media files are kept — pass `?media=true` to also delete the
// stream's storage folder. Defaulting to data-only lets a local dev server
// safely "delete" streams that point at shared (e.g. R2) media without
// touching the real assets.
func (app *App) deleteAdminStreamHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "stream id required", http.StatusBadRequest)
		Http400Errors.Inc()
		return
	}
	deleteMedia := r.URL.Query().Get("media") == "true"

	stream, err := app.GetStreamByID(r.Context(), channelKey, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to look up stream", "key", channelKey, "func", "deleteAdminStreamHandler", "streamID", streamID, "err", err)
		return
	}
	if stream == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	if err := app.removeStream(r.Context(), channelKey, stream, deleteMedia); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to remove stream", "key", channelKey, "func", "deleteAdminStreamHandler", "streamID", streamID, "err", err)
		return
	}
	slog.Info("admin deleted stream", "key", channelKey, "func", "deleteAdminStreamHandler", "streamID", streamID, "wasLive", stream.IsLive, "deleteMedia", deleteMedia)
	w.WriteHeader(http.StatusNoContent)
}

// postAdminStopHandler is the compound "stop current stream" action: it
// clears the entire incoming queue and sets the restart flag, so the worker
// will drop whatever it's processing and have nothing to immediately pick up.
func (app *App) postAdminStopHandler(w http.ResponseWriter, r *http.Request) {
	channelKey := r.PathValue("channel")
	ctx := r.Context()

	cleared, err := app.ClearIncomingStreams(ctx, channelKey)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to clear incoming queue", "key", channelKey, "func", "postAdminStopHandler", "err", err)
		return
	}

	now := time.Now().Unix()
	if err := app.UpsertRestartRequest(ctx, channelKey, now); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		Http500Errors.Inc()
		slog.Error("failed to set restart request", "key", channelKey, "func", "postAdminStopHandler", "err", err)
		return
	}
	slog.Info("admin stopped current stream", "key", channelKey, "func", "postAdminStopHandler", "queueCleared", cleared, "restartAt", now)
	w.WriteHeader(http.StatusNoContent)
}

// removeStream deletes a stream's metadata and transcript from the DB and
// clears the Prometheus activation metric. If deleteMedia is true, it also
// asynchronously removes the stream's media folder from storage; otherwise
// the media files are left intact (useful for local testing against shared
// storage).
func (app *App) removeStream(ctx context.Context, channelKey string, stream *Stream, deleteMedia bool) error {
	if err := app.DeleteStream(ctx, channelKey, stream.StreamID); err != nil {
		return fmt.Errorf("delete stream: %w", err)
	}
	if err := app.DeleteTranscript(ctx, channelKey, stream.StreamID); err != nil {
		// Non-fatal; log and continue. Orphaned transcripts get caught by the
		// nightly cleanup loop anyway.
		slog.Error("failed to delete transcript", "key", channelKey, "streamID", stream.StreamID, "err", err)
	}
	if stream.StreamTitle != "" {
		ActivatedStreams.DeleteLabelValues(channelKey, stream.StreamID, stream.StreamTitle)
	}

	if !deleteMedia {
		return nil
	}

	storageKey := fmt.Sprintf("%s/%s", channelKey, stream.StreamID)
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		if err := app.Storage.DeleteFolder(context.Background(), storageKey); err != nil {
			slog.Error("failed to delete stream folder from storage", "key", channelKey, "storageKey", storageKey, "err", err)
		} else {
			slog.Info("deleted stream storage", "key", channelKey, "storageKey", storageKey)
		}
	}()
	return nil
}
