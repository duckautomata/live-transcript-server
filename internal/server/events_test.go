package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// pollEvents performs a GET /events request and decodes the response when the
// status is 200. body is nil for non-200 responses.
func pollEvents(t *testing.T, mux *http.ServeMux, apiKey, channels string, since int64, wait int) (int, *WorkerEventsResponse) {
	t.Helper()
	url := fmt.Sprintf("/events?channels=%s&since=%d&wait=%d", channels, since, wait)
	req, _ := http.NewRequest("GET", url, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return rr.Code, nil
	}
	var resp WorkerEventsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode events response: %v", err)
	}
	return rr.Code, &resp
}

func TestEvents_AuthAndValidation(t *testing.T) {
	key := "events-auth"
	app, mux := setupTestApp(t, []string{key})

	// No API key
	code, _ := pollEvents(t, mux, "", key, 0, 0)
	if code != http.StatusForbidden {
		t.Errorf("expected 403 without api key, got %d", code)
	}

	// Missing channels param
	req, _ := http.NewRequest("GET", "/events", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without channels param, got %d", rr.Code)
	}

	// Unknown channel
	code, _ = pollEvents(t, mux, app.ApiKey, key+",nope", 0, 0)
	if code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown channel, got %d", code)
	}
}

func TestEvents_IncomingEdgeTriggered(t *testing.T) {
	key := "events-incoming"
	app, mux := setupTestApp(t, []string{key})

	// Nothing pending: immediate 204 with wait=0.
	code, _ := pollEvents(t, mux, app.ApiKey, key, 0, 0)
	if code != http.StatusNoContent {
		t.Fatalf("expected 204 with empty state, got %d", code)
	}

	// Queue a URL through the admin endpoint.
	body := strings.NewReader(`{"url": "https://twitch.tv/foo"}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/admin/incoming", key), body)
	req.Header.Set("X-Admin-Key", "admin-"+key)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to queue incoming url: %d %s", rr.Code, rr.Body.String())
	}

	// Reported once...
	code, resp := pollEvents(t, mux, app.ApiKey, key, 0, 0)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with queued url, got %d", code)
	}
	if !slices.Contains(resp.Events[key], "incoming") {
		t.Errorf("expected incoming flag, got %v", resp.Events)
	}
	if resp.Cursor <= 0 {
		t.Errorf("expected cursor to advance, got %d", resp.Cursor)
	}

	// ...then suppressed once the cursor is echoed back, even though the URL
	// is still queued.
	code, _ = pollEvents(t, mux, app.ApiKey, key, resp.Cursor, 0)
	if code != http.StatusNoContent {
		t.Errorf("expected 204 after cursor advanced, got %d", code)
	}
}

func TestEvents_RestartLevelTriggered(t *testing.T) {
	key := "events-restart"
	app, mux := setupTestApp(t, []string{key})

	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/admin/restart", key), nil)
	req.Header.Set("X-Admin-Key", "admin-"+key)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to request restart: %d", rr.Code)
	}

	code, resp := pollEvents(t, mux, app.ApiKey, key, 0, 0)
	if code != http.StatusOK || !slices.Contains(resp.Events[key], "restart") {
		t.Fatalf("expected restart flag, got %d %v", code, resp)
	}

	// Level-triggered: still reported with an advanced cursor until acked.
	code, resp = pollEvents(t, mux, app.ApiKey, key, resp.Cursor, 0)
	if code != http.StatusOK || !slices.Contains(resp.Events[key], "restart") {
		t.Fatalf("expected restart flag to persist until ack, got %d %v", code, resp)
	}

	// Worker ack clears it.
	req, _ = http.NewRequest("DELETE", fmt.Sprintf("/%s/restart", key), nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to ack restart: %d", rr.Code)
	}

	code, _ = pollEvents(t, mux, app.ApiKey, key, resp.Cursor, 0)
	if code != http.StatusNoContent {
		t.Errorf("expected 204 after restart ack, got %d", code)
	}
}

func TestEvents_WakesParkedPoll(t *testing.T) {
	keyA, keyB := "events-wake-a", "events-wake-b"
	app, mux := setupTestApp(t, []string{keyA, keyB})

	type result struct {
		code int
		resp WorkerEventsResponse
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("GET", fmt.Sprintf("/events?channels=%s,%s&wait=10", keyA, keyB), nil)
		req.Header.Set("X-API-Key", app.ApiKey)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		var resp WorkerEventsResponse
		if rr.Code == http.StatusOK {
			json.NewDecoder(rr.Body).Decode(&resp)
		}
		done <- result{rr.Code, resp}
	}()

	// Give the poll time to park, then queue a URL on channel B.
	time.Sleep(200 * time.Millisecond)
	body := strings.NewReader(`{"url": "https://twitch.tv/bar"}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/admin/incoming", keyB), body)
	req.Header.Set("X-Admin-Key", "admin-"+keyB)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to queue incoming url: %d", rr.Code)
	}

	select {
	case res := <-done:
		if res.code != http.StatusOK {
			t.Fatalf("expected 200 from woken poll, got %d", res.code)
		}
		if !slices.Contains(res.resp.Events[keyB], "incoming") {
			t.Errorf("expected incoming flag on %s, got %v", keyB, res.resp.Events)
		}
		if _, ok := res.resp.Events[keyA]; ok {
			t.Errorf("expected no events for %s, got %v", keyA, res.resp.Events)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked poll was not woken by the incoming post")
	}
}

func TestEvents_ShutdownReleasesParkedPoll(t *testing.T) {
	key := "events-shutdown"
	app, mux := setupTestApp(t, []string{key})

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", fmt.Sprintf("/events?channels=%s&wait=10", key), nil)
		req.Header.Set("X-API-Key", app.ApiKey)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		done <- rr.Code
	}()

	time.Sleep(200 * time.Millisecond)
	app.Notifier.Release()

	select {
	case code := <-done:
		if code != http.StatusNoContent {
			t.Errorf("expected 204 from released poll, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked poll was not released on shutdown")
	}
}

// pollAdmin performs a GET /{channel}/admin/poll request. counter is -100 for
// non-200 responses.
func pollAdmin(t *testing.T, mux *http.ServeMux, adminKey, channel string, since int64, wait int) (int, int64) {
	t.Helper()
	url := fmt.Sprintf("/%s/admin/poll?since=%d&wait=%d", channel, since, wait)
	req, _ := http.NewRequest("GET", url, nil)
	if adminKey != "" {
		req.Header.Set("X-Admin-Key", adminKey)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return rr.Code, -100
	}
	var resp AdminPollResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode admin poll response: %v", err)
	}
	return rr.Code, resp.Counter
}

func TestAdminPoll_FirstPollAndTimeout(t *testing.T) {
	key := "adminpoll-basic"
	_, mux := setupTestApp(t, []string{key})

	// No admin key -> 403.
	code, _ := pollAdmin(t, mux, "", key, -1, 0)
	if code != http.StatusForbidden {
		t.Errorf("expected 403 without admin key, got %d", code)
	}

	// First poll (since=-1) answers immediately with the current counter.
	code, counter := pollAdmin(t, mux, "admin-"+key, key, -1, 0)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on first poll, got %d", code)
	}
	if counter <= 0 {
		t.Fatalf("expected clock-seeded counter, got %d", counter)
	}

	// Nothing changed: same since times out with 204.
	code, _ = pollAdmin(t, mux, "admin-"+key, key, counter, 0)
	if code != http.StatusNoContent {
		t.Errorf("expected 204 when counter unchanged, got %d", code)
	}
}

func TestAdminPoll_BumpsOnAdminAndWorkerActions(t *testing.T) {
	key := "adminpoll-bumps"
	app, mux := setupTestApp(t, []string{key})

	_, counter := pollAdmin(t, mux, "admin-"+key, key, -1, 0)

	// Admin queues a URL -> counter advances.
	body := strings.NewReader(`{"url": "https://twitch.tv/foo"}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/admin/incoming", key), body)
	req.Header.Set("X-Admin-Key", "admin-"+key)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to queue incoming url: %d", rr.Code)
	}
	code, next := pollAdmin(t, mux, "admin-"+key, key, counter, 0)
	if code != http.StatusOK || next <= counter {
		t.Fatalf("expected advanced counter after admin action, got %d %d->%d", code, counter, next)
	}
	counter = next

	// Worker removes the URL -> counter advances again (the page shows the queue).
	req, _ = http.NewRequest("DELETE", fmt.Sprintf("/%s/incoming?url=%s", key, "https://twitch.tv/foo"), nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("worker failed to delete incoming url: %d", rr.Code)
	}
	code, next = pollAdmin(t, mux, "admin-"+key, key, counter, 0)
	if code != http.StatusOK || next <= counter {
		t.Fatalf("expected advanced counter after worker ack, got %d %d->%d", code, counter, next)
	}
}

func TestAdminPoll_WakesParkedPoll(t *testing.T) {
	key := "adminpoll-wake"
	_, mux := setupTestApp(t, []string{key})

	_, counter := pollAdmin(t, mux, "admin-"+key, key, -1, 0)

	type result struct {
		code    int
		counter int64
	}
	done := make(chan result, 1)
	go func() {
		code, next := pollAdmin(t, mux, "admin-"+key, key, counter, 10)
		done <- result{code, next}
	}()

	time.Sleep(200 * time.Millisecond)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/admin/restart", key), nil)
	req.Header.Set("X-Admin-Key", "admin-"+key)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("failed to request restart: %d", rr.Code)
	}

	select {
	case res := <-done:
		if res.code != http.StatusOK || res.counter <= counter {
			t.Errorf("expected woken poll with advanced counter, got %d %d->%d", res.code, counter, res.counter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked admin poll was not woken by the restart post")
	}
}
