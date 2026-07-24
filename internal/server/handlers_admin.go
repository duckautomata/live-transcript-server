package server

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
)

//go:embed admin_ui.html
var adminUIFS embed.FS

// adminUIHandler serves the embedded admin UI page. The page itself is static
// — auth is enforced on the API endpoints it calls. The JS reads the channel
// from the URL and prompts for the per-channel admin key on first load.
func (app *App) adminUIHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
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
	Channel          string              `json:"channel"`
	Worker           *model.WorkerStatus `json:"worker"`
	Streams          []model.Stream      `json:"streams"`
	IncomingURLs     []string            `json:"incomingUrls"`
	RestartPending   bool                `json:"restartPending"`
	RestartAt        int64               `json:"restartRequestedAt"`
	Server           model.ServerInfo    `json:"server"`
	ConnectedClients int                 `json:"connectedClients"`
	// MembershipEnabled tells the admin UI whether to render the membership-key
	// section for this channel. True only when the archive server is configured
	// and this channel has an archive-side name mapped.
	MembershipEnabled bool `json:"membershipEnabled"`
	// DiscordBot is the gateway health of the app-wide Discord announcement
	// bot, plus whether this channel can receive its announcements.
	DiscordBot discord.BotStatus `json:"discordBot"`
}

func (app *App) getAdminInfoHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ctx := r.Context()

	worker, err := app.Store.GetWorkerStatusByKey(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to get worker status", "key", cs.Key, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if worker != nil {
		// Mirror the public /status endpoint's active-window rule.
		worker.IsActive = time.Now().Unix()-worker.LastSeen < int64(workerActiveWindow.Seconds())
	}

	streams, err := app.Store.GetAllStreams(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to get streams", "key", cs.Key, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if streams == nil {
		streams = []model.Stream{}
	}

	incoming, err := app.Store.GetIncomingStreams(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to get incoming streams", "key", cs.Key, "func", "getAdminInfoHandler", "err", err)
		return
	}
	if incoming == nil {
		incoming = []string{}
	}

	restartAt, err := app.Store.GetRestartRequest(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to get restart request", "key", cs.Key, "func", "getAdminInfoHandler", "err", err)
		return
	}

	writeJSON(w, AdminInfoResponse{
		Channel:           cs.Key,
		Worker:            worker,
		Streams:           streams,
		IncomingURLs:      incoming,
		RestartPending:    restartAt > 0,
		RestartAt:         restartAt,
		Server:            model.ServerInfo{Version: app.Version, BuildTime: app.BuildTime},
		ConnectedClients:  cs.Hub.Connections(),
		MembershipEnabled: app.membershipEnabled(cs),
		DiscordBot:        app.DiscordBot.Status(cs.Key),
	})
}

// postAdminIncomingHandler lets an admin manually add a URL to the queue.
func (app *App) postAdminIncomingHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	url := strings.TrimSpace(body.URL)
	if url == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		http.Error(w, "url must start with http:// or https://", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	if err := app.Store.UpsertIncomingStream(r.Context(), cs.Key, url, time.Now().Unix()); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to upsert incoming stream", "key", cs.Key, "func", "postAdminIncomingHandler", "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("admin queued incoming stream", "key", cs.Key, "func", "postAdminIncomingHandler", "url", url)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminIncomingHandler removes a URL from the queue.
func (app *App) deleteAdminIncomingHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "Missing required parameter: url", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	rowsAffected, err := app.Store.DeleteIncomingStream(r.Context(), cs.Key, url)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to delete incoming stream", "key", cs.Key, "func", "deleteAdminIncomingHandler", "err", err)
		return
	}
	if rowsAffected == 0 {
		http.Error(w, "URL not queued", http.StatusNotFound)
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("admin removed incoming stream", "key", cs.Key, "func", "deleteAdminIncomingHandler", "url", url)
	w.WriteHeader(http.StatusNoContent)
}

// postAdminRestartHandler triggers a worker restart for this channel.
func (app *App) postAdminRestartHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	now := time.Now().Unix()
	if err := app.Store.UpsertRestartRequest(r.Context(), cs.Key, now); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to set restart request", "key", cs.Key, "func", "postAdminRestartHandler", "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("admin requested worker restart", "key", cs.Key, "func", "postAdminRestartHandler", "requestedAt", now)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminRestartHandler clears a pending restart for this channel.
// Idempotent: returns 204 whether or not anything was pending, so the UI
// doesn't need to special-case a stale view.
func (app *App) deleteAdminRestartHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	rowsAffected, err := app.Store.DeleteRestartRequest(r.Context(), cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to clear restart request", "key", cs.Key, "func", "deleteAdminRestartHandler", "err", err)
		return
	}
	if rowsAffected > 0 {
		app.bumpAdminChange(cs.Key)
	}
	slog.Info("admin cleared restart request", "key", cs.Key, "func", "deleteAdminRestartHandler", "wasPending", rowsAffected > 0)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminStreamHandler removes a stream's metadata and transcript. By
// default the media files are kept — pass `?media=true` to also delete the
// stream's storage folder. Defaulting to data-only lets a local dev server
// safely "delete" streams that point at shared (e.g. R2) media without
// touching the real assets.
func (app *App) deleteAdminStreamHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	streamID := r.PathValue("streamID")
	if !isValidID(streamID) {
		http.Error(w, "invalid stream id", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	deleteMedia := r.URL.Query().Get("media") == "true"

	stream, err := app.Store.GetStreamByID(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to look up stream", "key", cs.Key, "func", "deleteAdminStreamHandler", "streamID", streamID, "err", err)
		return
	}
	if stream == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	if stream.IsLive {
		// Refuse: a live worker would resync via /sync after the next push and
		// resurrect the stream, causing confusing flicker for clients. Force
		// the operator to stop the stream first (Stop current stream button).
		http.Error(w, "Cannot delete a live stream. Use \"Stop current stream\" first, then delete it once the worker has deactivated it.", http.StatusConflict)
		return
	}

	if err := app.removeStream(r.Context(), cs, stream, deleteMedia); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to remove stream", "key", cs.Key, "func", "deleteAdminStreamHandler", "streamID", streamID, "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("admin deleted stream", "key", cs.Key, "func", "deleteAdminStreamHandler", "streamID", streamID, "wasLive", stream.IsLive, "deleteMedia", deleteMedia)
	w.WriteHeader(http.StatusNoContent)
}

// postAdminStopHandler is the compound "stop current stream" action: it
// clears the entire incoming queue and sets the restart flag, so the worker
// will drop whatever it's processing and have nothing to immediately pick up.
func (app *App) postAdminStopHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ctx := r.Context()

	cleared, err := app.Store.ClearIncomingStreams(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to clear incoming queue", "key", cs.Key, "func", "postAdminStopHandler", "err", err)
		return
	}

	now := time.Now().Unix()
	if err := app.Store.UpsertRestartRequest(ctx, cs.Key, now); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to set restart request", "key", cs.Key, "func", "postAdminStopHandler", "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("admin stopped current stream", "key", cs.Key, "func", "postAdminStopHandler", "queueCleared", cleared, "restartAt", now)
	w.WriteHeader(http.StatusNoContent)
}

// getAdminMembershipHandler lists the membership keys for this channel by
// proxying to the archive server. The archive-side channel name is taken from
// config (cs.MembersName), never from the request, so a channel admin can only
// ever see their own channel's keys. Side-effect-free.
func (app *App) getAdminMembershipHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.membershipEnabled(cs) {
		http.Error(w, "Membership management is disabled for this channel", http.StatusNotFound)
		return
	}

	keys, err := app.Archive.ListKeys(r.Context(), cs.MembersName)
	if err != nil {
		http.Error(w, "Archive server error", http.StatusBadGateway)
		metrics.Http500Errors.Inc()
		slog.Error("failed to list membership keys", "key", cs.Key, "membersName", cs.MembersName, "func", "getAdminMembershipHandler", "err", err)
		return
	}
	writeJSON(w, keys)
}

// postAdminMembershipHandler creates (or rotates) a membership key for this
// channel by proxying to the archive server. The archive enforces a 2-key cap
// and prunes older keys itself.
func (app *App) postAdminMembershipHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.membershipEnabled(cs) {
		http.Error(w, "Membership management is disabled for this channel", http.StatusNotFound)
		return
	}

	key, err := app.Archive.CreateKey(r.Context(), cs.MembersName)
	if err != nil {
		http.Error(w, "Archive server error", http.StatusBadGateway)
		metrics.Http500Errors.Inc()
		slog.Error("failed to create membership key", "key", cs.Key, "membersName", cs.MembersName, "func", "postAdminMembershipHandler", "err", err)
		return
	}

	// Deliberately do not log the key value.
	slog.Info("admin created membership key", "key", cs.Key, "membersName", cs.MembersName, "func", "postAdminMembershipHandler", "expiresAt", key.ExpiresAt)
	writeJSON(w, key)
}

// deleteAdminMembershipHandler deletes all membership keys for this channel by
// proxying to the archive server. The archive API has no per-key delete, so
// this removes every key for the channel.
func (app *App) deleteAdminMembershipHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	if !app.membershipEnabled(cs) {
		http.Error(w, "Membership management is disabled for this channel", http.StatusNotFound)
		return
	}

	if err := app.Archive.DeleteKeys(r.Context(), cs.MembersName); err != nil {
		http.Error(w, "Archive server error", http.StatusBadGateway)
		metrics.Http500Errors.Inc()
		slog.Error("failed to delete membership keys", "key", cs.Key, "membersName", cs.MembersName, "func", "deleteAdminMembershipHandler", "err", err)
		return
	}

	slog.Info("admin deleted all membership keys", "key", cs.Key, "membersName", cs.MembersName, "func", "deleteAdminMembershipHandler")
	w.WriteHeader(http.StatusNoContent)
}
