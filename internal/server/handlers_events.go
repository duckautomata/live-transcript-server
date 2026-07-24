package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/notify"
)

// Long-poll bounds for GET /events and GET /{channel}/admin/poll. The default
// keeps a parked request well under typical reverse-proxy idle timeouts.
const (
	defaultEventsWaitSeconds = 25
	maxEventsWaitSeconds     = 60
)

// WorkerEventsResponse is returned by GET /events. Events maps a channel key
// to the names of its pending signals ("incoming", "restart"). Cursor is a
// high-water mark the worker echoes back via ?since= on its next poll.
type WorkerEventsResponse struct {
	Cursor int64               `json:"cursor"`
	Events map[string][]string `json:"events"`
}

// AdminPollResponse is returned by GET /{channel}/admin/poll. Counter is the
// channel's current admin change counter; the page echoes it back via ?since=
// so the poll only answers when something it displays has changed.
type AdminPollResponse struct {
	Counter int64 `json:"counter"`
}

// collectWorkerEvents gathers the pending signals for the given channels.
//
// "restart" is level-triggered: it is reported for as long as the restart row
// exists. The worker's DELETE /{channel}/restart ack removes the row, so the
// flag clears itself and cannot spin the long poll.
//
// "incoming" is edge-triggered on received_at: queued URLs stay on the server
// until their stream ends, so reporting "queue non-empty" would turn the long
// poll into a hot loop for the whole stream. Instead the flag fires when a
// URL's received_at is newer than the worker's cursor, and the cursor
// advances past it in the same response. received_at has second granularity,
// so a URL queued in the same second the cursor last advanced can be missed;
// the worker's fallback /incoming refresh covers that case.
func (app *App) collectWorkerEvents(ctx context.Context, keys []string, since int64) (WorkerEventsResponse, error) {
	resp := WorkerEventsResponse{Cursor: since, Events: make(map[string][]string)}
	for _, key := range keys {
		var flags []string

		latest, err := app.Store.GetLatestIncomingTime(ctx, key)
		if err != nil {
			return resp, err
		}
		if latest > since {
			flags = append(flags, "incoming")
		}
		if latest > resp.Cursor {
			resp.Cursor = latest
		}

		requestedAt, err := app.Store.GetRestartRequest(ctx, key)
		if err != nil {
			return resp, err
		}
		if requestedAt > 0 {
			flags = append(flags, "restart")
		}

		if len(flags) > 0 {
			resp.Events[key] = flags
		}
	}
	return resp, nil
}

// parseWaitSeconds reads the `wait` query parameter, clamped to
// [0, maxEventsWaitSeconds], defaulting to defaultEventsWaitSeconds.
func parseWaitSeconds(r *http.Request) time.Duration {
	waitSeconds := defaultEventsWaitSeconds
	if v := r.URL.Query().Get("wait"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			waitSeconds = min(parsed, maxEventsWaitSeconds)
		}
	}
	return time.Duration(waitSeconds) * time.Second
}

// adminPollHandler is the admin page's long-poll notification endpoint:
// GET /{channel}/admin/poll?since=<counter>&wait=<seconds>.
// It answers with the channel's current change counter as soon as it differs
// from `since` (immediately for a first poll or after a server restart, since
// counters are seeded from the clock), and otherwise parks until a bump or
// the wait elapses (204 No Content). The page then fetches /admin/info as
// usual — this endpoint only says when, never what.
func (app *App) adminPollHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		since = -1
	}
	deadline := time.Now().Add(parseWaitSeconds(r))

	var current int64
	outcome := app.Notifier.Wait(r.Context(), deadline, func() bool {
		current = cs.AdminChangeCounter.Load()
		return current != since
	})

	switch outcome {
	case notify.Ready:
		writeJSON(w, AdminPollResponse{Counter: current})
	case notify.Timeout, notify.Released:
		w.WriteHeader(http.StatusNoContent)
	case notify.Canceled:
		// Client went away; nothing to write.
	}
}

// getEventsHandler is the worker's long-poll notification endpoint:
// GET /events?channels=a,b&since=<cursor>&wait=<seconds>.
// It answers immediately when any listed channel has a pending signal and
// otherwise parks until one is posted or `wait` elapses (204 No Content).
// The GET /{channel}/incoming and /{channel}/restart endpoints remain the
// source of truth — this endpoint only tells the worker to go read them.
func (app *App) getEventsHandler(w http.ResponseWriter, r *http.Request) {
	channelsParam := r.URL.Query().Get("channels")
	if channelsParam == "" {
		http.Error(w, "Missing required parameter: channels", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}
	keys := strings.Split(channelsParam, ",")
	for i := range keys {
		keys[i] = strings.TrimSpace(keys[i])
		if _, ok := app.Channels[keys[i]]; !ok {
			http.Error(w, "Channel not found: "+keys[i], http.StatusNotFound)
			metrics.Http400Errors.Inc()
			slog.Warn("invalid channel name", "func", "getEventsHandler", "key", keys[i])
			return
		}
	}

	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || since < 0 {
		since = 0
	}

	deadline := time.Now().Add(parseWaitSeconds(r))

	var resp WorkerEventsResponse
	var collectErr error
	outcome := app.Notifier.Wait(r.Context(), deadline, func() bool {
		resp, collectErr = app.collectWorkerEvents(r.Context(), keys, since)
		// An error ends the poll immediately so we can answer with a 500.
		return collectErr != nil || len(resp.Events) > 0
	})

	if collectErr != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		app.report500(r, collectErr, "failed to collect worker events", "func", "getEventsHandler")
		return
	}

	switch outcome {
	case notify.Ready:
		writeJSON(w, resp)
	case notify.Timeout, notify.Released:
		w.WriteHeader(http.StatusNoContent)
	case notify.Canceled:
		// Client went away; nothing to write.
	}
}
