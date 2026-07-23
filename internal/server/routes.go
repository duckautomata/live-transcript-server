package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"regexp"

	"live-transcript-server/internal/metrics"
)

// validIDPattern matches the identifier format used for channel keys, stream
// IDs, clip IDs, and file IDs (YouTube and Twitch IDs and shortuuid values all
// fit). It excludes '/', '.', and every other character that could enable path
// traversal when the value is interpolated into a storage key or filesystem
// path.
var validIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// isValidID reports whether s is safe to interpolate into storage keys and file
// paths. Empty and over-long values are rejected.
func isValidID(s string) bool {
	return len(s) > 0 && len(s) <= 128 && validIDPattern.MatchString(s)
}

// RegisterRoutes registers every endpoint on mux. Routes are grouped by
// audience — worker (X-API-Key), admin (X-Admin-Key), public — and each
// audience maps to one handlers_*.go file. A new endpoint is one handler in
// the matching file plus one line here.
func (app *App) RegisterRoutes(mux *http.ServeMux) {
	slog.Info("registering endpoints", "func", "RegisterRoutes")

	// Worker routes (X-API-Key)
	mux.HandleFunc("POST /{channel}/activate", app.apiKeyMiddleware(app.withChannel(app.activateHandler)))
	mux.HandleFunc("POST /{channel}/deactivate", app.apiKeyMiddleware(app.withChannel(app.deactivateHandler)))
	mux.HandleFunc("POST /{channel}/sync", app.apiKeyMiddleware(app.withChannel(app.syncHandler)))
	mux.HandleFunc("POST /{channel}/line/{streamID}", app.apiKeyMiddleware(app.withChannel(app.lineHandler)))
	mux.HandleFunc("POST /{channel}/media/{streamID}/{id}", app.apiKeyMiddleware(app.withChannel(app.mediaHandler)))
	mux.HandleFunc("GET /{channel}/statuscheck", app.apiKeyMiddleware(app.withChannel(app.statuscheckHandler)))
	mux.HandleFunc("POST /status", app.apiKeyMiddleware(app.workerStatusHandler))
	mux.HandleFunc("GET /events", app.apiKeyMiddleware(app.getEventsHandler))
	mux.HandleFunc("GET /{channel}/incoming", app.apiKeyMiddleware(app.withChannel(app.getIncomingHandler)))
	mux.HandleFunc("DELETE /{channel}/incoming", app.apiKeyMiddleware(app.withChannel(app.deleteIncomingHandler)))
	mux.HandleFunc("POST /{channel}/restart", app.apiKeyMiddleware(app.withChannel(app.postRestartHandler)))
	mux.HandleFunc("GET /{channel}/restart", app.apiKeyMiddleware(app.withChannel(app.getRestartHandler)))
	mux.HandleFunc("DELETE /{channel}/restart", app.apiKeyMiddleware(app.withChannel(app.deleteRestartHandler)))

	// Admin UI + admin-key protected routes (X-Admin-Key, per channel)
	mux.HandleFunc("GET /{channel}/ui", app.withChannel(app.adminUIHandler))
	mux.HandleFunc("GET /{channel}/admin/info", app.withAdminChannel(app.getAdminInfoHandler))
	mux.HandleFunc("GET /{channel}/admin/poll", app.withAdminChannel(app.adminPollHandler))
	mux.HandleFunc("POST /{channel}/admin/incoming", app.withAdminChannel(app.postAdminIncomingHandler))
	mux.HandleFunc("DELETE /{channel}/admin/incoming", app.withAdminChannel(app.deleteAdminIncomingHandler))
	mux.HandleFunc("POST /{channel}/admin/restart", app.withAdminChannel(app.postAdminRestartHandler))
	mux.HandleFunc("DELETE /{channel}/admin/restart", app.withAdminChannel(app.deleteAdminRestartHandler))
	mux.HandleFunc("DELETE /{channel}/admin/stream/{streamID}", app.withAdminChannel(app.deleteAdminStreamHandler))
	mux.HandleFunc("POST /{channel}/admin/stop", app.withAdminChannel(app.postAdminStopHandler))
	mux.HandleFunc("GET /{channel}/admin/membership", app.withAdminChannel(app.getAdminMembershipHandler))
	mux.HandleFunc("POST /{channel}/admin/membership", app.withAdminChannel(app.postAdminMembershipHandler))
	mux.HandleFunc("DELETE /{channel}/admin/membership", app.withAdminChannel(app.deleteAdminMembershipHandler))

	// Public routes
	mux.HandleFunc("GET /status", app.getStatusHandler)
	mux.HandleFunc("GET /{channel}/websocket", app.withChannel(app.wsHandler))
	mux.HandleFunc("GET /{channel}/stream/{streamID}/{type}/{filename}", app.withChannel(app.streamHandler))
	mux.HandleFunc("GET /{channel}/download/{streamID}/{type}/{filename}", app.withChannel(app.downloadHandler))
	mux.HandleFunc("GET /{channel}/frame/{streamID}/{filename}", app.withChannel(app.getFrameHandler))
	mux.HandleFunc("GET /{channel}/transcript/{streamID}", app.withChannel(app.getTranscriptHandler))
	mux.HandleFunc("POST /{channel}/clip", app.withChannel(app.postClipHandler))
	mux.HandleFunc("POST /{channel}/trim", app.withChannel(app.postTrimHandler))
}

// channelHandler is an http.HandlerFunc that additionally receives the
// resolved channel.
type channelHandler func(w http.ResponseWriter, r *http.Request, cs *ChannelState)

// withChannel resolves the {channel} path value once — 404, metric, and log
// on failure — and passes the channel state to the handler. Every handler on
// a /{channel}/... route uses this instead of repeating the lookup.
func (app *App) withChannel(h channelHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("channel")
		cs, ok := app.Channels[key]
		if !ok {
			http.Error(w, "Channel not found", http.StatusNotFound)
			metrics.Http400Errors.Inc()
			slog.Warn("invalid channel name", "key", key, "path", r.URL.Path)
			return
		}
		h(w, r, cs)
	}
}

// apiKeyMiddleware enforces the shared worker API key from the X-API-Key
// header.
func (app *App) apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if app.ApiKey == "" {
			// Fail closed: never serve a protected route when no key is set.
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			slog.Error("rejecting protected request: no API key configured", "func", "apiKeyMiddleware", "path", r.URL.Path)
			return
		}
		key := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(app.ApiKey)) != 1 {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// withAdminChannel resolves the channel, then enforces its per-channel admin
// key (configured in channels[].adminKey) from the X-Admin-Key header. A
// channel without an admin key configured rejects all admin operations.
func (app *App) withAdminChannel(h channelHandler) http.HandlerFunc {
	return app.withChannel(func(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
		if cs.AdminKey == "" {
			http.Error(w, "Admin operations are disabled for this channel", http.StatusForbidden)
			return
		}
		provided := r.Header.Get("X-Admin-Key")
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cs.AdminKey)) != 1 {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h(w, r, cs)
	})
}

// CorsMiddleware adds permissive CORS headers to all responses and answers
// preflight OPTIONS requests.
func CorsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-Admin-Key, Authorization")
		// Expose headers so clients can read them (e.g. filename from Content-Disposition)
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
		// Cache the preflight response for 24 hours to reduce OPTIONS requests
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Preflight OPTIONS requests are answered here so the browser can
		// check permissions before sending the actual request.
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
