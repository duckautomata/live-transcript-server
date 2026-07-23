package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIncomingEndpoints(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ctx := context.Background()

	// Initially empty
	req := httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET empty: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"urls\":[]}\n" {
		t.Errorf("GET empty body=%q want empty url list", got)
	}

	// Insert via direct DB call (bot path)
	if err := app.Store.UpsertIncomingStream(ctx, "doki", "https://twitch.tv/dokibird", 1700000000); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := app.Store.UpsertIncomingStream(ctx, "doki", "https://www.youtube.com/watch?v=abc", 1700000010); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// GET returns both URLs in received_at order
	req = httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "{\"urls\":[\"https://twitch.tv/dokibird\",\"https://www.youtube.com/watch?v=abc\"]}\n"
	if rec.Body.String() != want {
		t.Errorf("GET body=%q want %q", rec.Body.String(), want)
	}

	// GET should be idempotent — call again, same result
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doki/incoming", nil).WithContext(ctx))
	// (no api key — should fail auth)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without api key, got %d", rec.Code)
	}

	// DELETE one URL
	delReq := httptest.NewRequest(http.MethodDelete, "/doki/incoming?url=https://twitch.tv/dokibird", nil)
	delReq.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, delReq)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// DELETE same URL again -> 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, delReq)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE missing: status=%d want 404", rec.Code)
	}

	// GET shows only the remaining url
	req = httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	want = "{\"urls\":[\"https://www.youtube.com/watch?v=abc\"]}\n"
	if rec.Body.String() != want {
		t.Errorf("GET after delete body=%q want %q", rec.Body.String(), want)
	}

	// DELETE without ?url -> 400
	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodDelete, "/doki/incoming", nil)
	bad.Header.Set("X-API-Key", app.ApiKey)
	mux.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE no url: status=%d want 400", rec.Code)
	}

	// Unknown channel -> 404
	rec = httptest.NewRecorder()
	bad = httptest.NewRequest(http.MethodGet, "/unknown/incoming", nil)
	bad.Header.Set("X-API-Key", app.ApiKey)
	mux.ServeHTTP(rec, bad)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status=%d want 404", rec.Code)
	}
}

func TestRestartEndpoints(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	do := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-API-Key", app.ApiKey)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Initially: no restart pending
	rec := do(http.MethodGet, "/doki/restart")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET initial: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var status RestartStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Pending || status.RequestedAt != 0 {
		t.Errorf("expected no pending restart, got %+v", status)
	}

	// POST -> sets pending
	beforePost := time.Now().Unix()
	rec = do(http.MethodPost, "/doki/restart")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST: status=%d body=%s", rec.Code, rec.Body.String())
	}
	afterPost := time.Now().Unix()

	rec = do(http.MethodGet, "/doki/restart")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after post: status=%d", rec.Code)
	}
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.Pending {
		t.Error("expected pending=true after POST")
	}
	if status.RequestedAt < beforePost || status.RequestedAt > afterPost {
		t.Errorf("requestedAt=%d not in [%d, %d]", status.RequestedAt, beforePost, afterPost)
	}
	firstTimestamp := status.RequestedAt

	// POST again -> updates timestamp (idempotent re-request)
	time.Sleep(1100 * time.Millisecond) // ensure unix-second tick
	rec = do(http.MethodPost, "/doki/restart")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-POST: status=%d", rec.Code)
	}
	rec = do(http.MethodGet, "/doki/restart")
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.RequestedAt <= firstTimestamp {
		t.Errorf("expected requestedAt to advance after re-POST, first=%d second=%d", firstTimestamp, status.RequestedAt)
	}

	// DELETE -> clears
	rec = do(http.MethodDelete, "/doki/restart")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/doki/restart")
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Pending || status.RequestedAt != 0 {
		t.Errorf("expected no pending restart after DELETE, got %+v", status)
	}

	// DELETE again -> 404 (no pending)
	rec = do(http.MethodDelete, "/doki/restart")
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE no-op: status=%d want 404", rec.Code)
	}

	// Unknown channel -> 404
	rec = do(http.MethodPost, "/unknown/restart")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel POST: status=%d want 404", rec.Code)
	}

	// Unauthenticated -> 403
	req := httptest.NewRequest(http.MethodPost, "/doki/restart", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no api key: status=%d want 403", rec.Code)
	}
}

func TestCleanupExpiredIncomingStreams(t *testing.T) {
	app, _ := setupTestApp(t, []string{"doki"})
	ctx := context.Background()

	// Two URLs at different ages
	if err := app.Store.UpsertIncomingStream(ctx, "doki", "old", 100); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := app.Store.UpsertIncomingStream(ctx, "doki", "fresh", 1000); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	removed, err := app.Store.CleanupExpiredIncomingStreams(ctx, 500)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d want 1", removed)
	}

	urls, err := app.Store.GetIncomingStreams(ctx, "doki")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(urls) != 1 || urls[0] != "fresh" {
		t.Errorf("urls=%v want [fresh]", urls)
	}
}
