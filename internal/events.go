package internal

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Long-poll bounds for GET /events. The default keeps a parked request well
// under typical reverse-proxy idle timeouts.
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

// notifyWorkerEvents wakes every parked GET /events long-poll. The broadcast
// is app-wide rather than per-channel: with a handful of workers a spurious
// wakeup costs one cheap DB recheck, which is not worth per-channel signal
// plumbing.
func (app *App) notifyWorkerEvents() {
	app.eventsLock.Lock()
	close(app.eventsSignal)
	app.eventsSignal = make(chan struct{})
	app.eventsLock.Unlock()
}

// workerEventsSignal returns the channel that will be closed by the next
// notifyWorkerEvents call.
func (app *App) workerEventsSignal() <-chan struct{} {
	app.eventsLock.Lock()
	defer app.eventsLock.Unlock()
	return app.eventsSignal
}

// ReleaseEventPolls unblocks every parked GET /events request so
// http.Server.Shutdown isn't held up waiting for long polls to expire.
// Idempotent; called from main before shutting the HTTP server down.
func (app *App) ReleaseEventPolls() {
	app.eventsShutdownOnce.Do(func() { close(app.eventsShutdown) })
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

		latest, err := app.GetLatestIncomingTime(ctx, key)
		if err != nil {
			return resp, err
		}
		if latest > since {
			flags = append(flags, "incoming")
		}
		if latest > resp.Cursor {
			resp.Cursor = latest
		}

		requestedAt, err := app.GetRestartRequest(ctx, key)
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
		Http400Errors.Inc()
		return
	}
	keys := strings.Split(channelsParam, ",")
	for i := range keys {
		keys[i] = strings.TrimSpace(keys[i])
		if _, ok := app.Channels[keys[i]]; !ok {
			http.Error(w, "Channel not found: "+keys[i], http.StatusNotFound)
			Http400Errors.Inc()
			slog.Warn("invalid channel name", "func", "getEventsHandler", "key", keys[i])
			return
		}
	}

	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || since < 0 {
		since = 0
	}

	waitSeconds := defaultEventsWaitSeconds
	if v := r.URL.Query().Get("wait"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			waitSeconds = min(parsed, maxEventsWaitSeconds)
		}
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)

	for {
		// Subscribe before checking so a write that lands between the check
		// and the park still closes this signal and wakes the select below.
		signal := app.workerEventsSignal()

		resp, err := app.collectWorkerEvents(r.Context(), keys, since)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			app.report500(r, err, "failed to collect worker events", "func", "getEventsHandler")
			return
		}
		if len(resp.Events) > 0 {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				slog.Error("failed to encode events response", "func", "getEventsHandler", "err", err)
			}
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-signal:
			timer.Stop()
			// A signal was posted somewhere — loop to recheck our channels.
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-app.eventsShutdown:
			timer.Stop()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}
