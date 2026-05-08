package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adminReq(t *testing.T, mux *http.ServeMux, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = bytes.NewBuffer(b)
	}
	var req *http.Request
	if buf != nil {
		req = httptest.NewRequest(method, path, buf)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if key != "" {
		req.Header.Set("X-Admin-Key", key)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuth(t *testing.T) {
	_, mux, _ := setupTestApp(t, []string{"doki", "mint"})

	// No key -> 403
	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/info", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no key: status=%d want 403", rec.Code)
	}

	// Wrong key -> 403
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "wrong", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong key: status=%d want 403", rec.Code)
	}

	// Cross-channel key -> 403 (admin-mint key on doki should fail)
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-mint", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-channel key: status=%d want 403", rec.Code)
	}

	// Correct key -> 200
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("correct key: status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}

	// Unknown channel -> 404
	rec = adminReq(t, mux, http.MethodGet, "/unknown/admin/info", "admin-doki", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status=%d want 404", rec.Code)
	}
}

func TestAdminAuthDisabledChannel(t *testing.T) {
	// A channel without an admin key configured should reject all admin ops.
	db, err := InitDB(":memory:", DatabaseConfig{SkipWarmup: true})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	cfg := Config{
		Credentials: struct {
			ApiKey string `yaml:"apiKey"`
		}{ApiKey: "test"},
		Channels: []ChannelConfig{
			{Name: "noadmin", NumPastStreams: 1, AdminKey: ""}, // explicitly empty
		},
		Storage: StorageConfig{Type: "local"},
	}
	app := NewApp(cfg, db, t.TempDir(), "v", "b")
	t.Cleanup(func() { app.Close() })
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	rec := adminReq(t, mux, http.MethodGet, "/noadmin/admin/info", "anything", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled-admin channel: status=%d want 403", rec.Code)
	}
}

func TestAdminInfoAggregates(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	ctx := context.Background()
	seedExampleData(t, app, "doki")
	if err := app.UpsertIncomingStream(ctx, "doki", "https://twitch.tv/dokibird", time.Now().Unix()); err != nil {
		t.Fatalf("upsert incoming: %v", err)
	}
	if err := app.UpsertWorkerStatus(ctx, "doki", "worker-v1", "build-time", time.Now().Unix()); err != nil {
		t.Fatalf("upsert worker: %v", err)
	}

	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var info AdminInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if info.Channel != "doki" {
		t.Errorf("channel=%q want doki", info.Channel)
	}
	if info.Worker == nil || info.Worker.WorkerVersion != "worker-v1" || !info.Worker.IsActive {
		t.Errorf("worker info wrong: %+v", info.Worker)
	}
	if len(info.Streams) != 1 || info.Streams[0].StreamID != "stream-1" {
		t.Errorf("streams=%v", info.Streams)
	}
	if info.CurrentStream == nil || info.CurrentStream.StreamID != "stream-1" {
		t.Errorf("currentStream=%v", info.CurrentStream)
	}
	if len(info.IncomingURLs) != 1 || info.IncomingURLs[0] != "https://twitch.tv/dokibird" {
		t.Errorf("incomingUrls=%v", info.IncomingURLs)
	}
	if info.RestartPending {
		t.Error("expected no restart pending")
	}
	if info.Server.Version != "test-version" {
		t.Errorf("server.version=%q", info.Server.Version)
	}
}

func TestAdminIncomingAddRemove(t *testing.T) {
	_, mux, _ := setupTestApp(t, []string{"doki"})

	// Add invalid (no scheme)
	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/incoming", "admin-doki", map[string]string{"url": "twitch.tv/dokibird"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-http url: status=%d want 400", rec.Code)
	}

	// Add empty
	rec = adminReq(t, mux, http.MethodPost, "/doki/admin/incoming", "admin-doki", map[string]string{"url": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty url: status=%d want 400", rec.Code)
	}

	// Add valid
	rec = adminReq(t, mux, http.MethodPost, "/doki/admin/incoming", "admin-doki", map[string]string{"url": "https://twitch.tv/dokibird"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Verify in info
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	var info AdminInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(info.IncomingURLs) != 1 || info.IncomingURLs[0] != "https://twitch.tv/dokibird" {
		t.Errorf("after add, incoming=%v", info.IncomingURLs)
	}

	// Remove
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/incoming?url=https%3A%2F%2Ftwitch.tv%2Fdokibird", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d", rec.Code)
	}

	// Remove again -> 404
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/incoming?url=https%3A%2F%2Ftwitch.tv%2Fdokibird", "admin-doki", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete missing: status=%d want 404", rec.Code)
	}
}

func TestAdminRestart(t *testing.T) {
	_, mux, _ := setupTestApp(t, []string{"doki"})

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/restart", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restart: status=%d", rec.Code)
	}

	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	var info AdminInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !info.RestartPending {
		t.Error("expected restart pending after admin restart")
	}

	// Cancel the pending restart
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/restart", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: status=%d", rec.Code)
	}

	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.RestartPending {
		t.Error("expected no restart pending after cancel")
	}

	// Cancel again — should still be 204 (idempotent), not 404
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/restart", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("cancel when nothing pending: status=%d want 204 (idempotent)", rec.Code)
	}
}

func TestAdminDeleteStreamDataOnly(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	// Live streams cannot be deleted — deactivate before testing the happy path.
	if err := app.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Plant a fake media file under the local-storage path so we can verify it
	// is preserved when ?media is not requested.
	mediaPath := filepath.Join(app.TempDir, "doki", "stream-1", "audio", "fake.m4a")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("placeholder"), 0644); err != nil {
		t.Fatalf("write fake media: %v", err)
	}

	// Delete unknown -> 404
	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/nope", "admin-doki", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown: status=%d want 404", rec.Code)
	}

	// Default delete (no ?media param) — data-only
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// DB rows gone
	if stream, err := app.GetStreamByID(context.Background(), "doki", "stream-1"); err != nil {
		t.Fatalf("post-delete get: %v", err)
	} else if stream != nil {
		t.Error("stream still present after delete")
	}
	if transcript, err := app.GetTranscript(context.Background(), "doki", "stream-1"); err != nil {
		t.Fatalf("transcript get: %v", err)
	} else if len(transcript) != 0 {
		t.Errorf("transcript still has %d lines after delete", len(transcript))
	}

	// Media file untouched
	if _, err := os.Stat(mediaPath); os.IsNotExist(err) {
		t.Error("media file was deleted on data-only delete; should have been preserved")
	} else if err != nil {
		t.Fatalf("stat media: %v", err)
	}
}

func TestAdminDeleteStreamBroadcastsEvent(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	if err := app.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Hook a fake client into the channel so we can read what's broadcast.
	cs := app.Channels["doki"]
	client := &Client{send: make(chan WebSocketMessage, 4)}
	cs.ClientsLock.Lock()
	cs.Clients = append(cs.Clients, client)
	cs.ClientsLock.Unlock()

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case msg := <-client.send:
		if msg.Event != EventDeletedStream {
			t.Fatalf("event=%q want %q", msg.Event, EventDeletedStream)
		}
		data, ok := msg.Data.(EventDeletedStreamData)
		if !ok {
			t.Fatalf("data type=%T want EventDeletedStreamData", msg.Data)
		}
		if data.StreamID != "stream-1" {
			t.Errorf("streamID=%q want stream-1", data.StreamID)
		}
		if data.StreamTitle != "Test Stream Title" {
			t.Errorf("streamTitle=%q", data.StreamTitle)
		}
		if data.WasLive {
			t.Error("wasLive=true; we deactivated before delete")
		}
	case <-time.After(time.Second):
		t.Fatal("no deletedStream event received within 1s")
	}
}

func TestAdminDeleteStreamRejectsLive(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki") // seeds with IsLive=true

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("live delete: status=%d want 409, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Stop current stream") {
		t.Errorf("error body should mention the recommended action; got: %s", rec.Body.String())
	}

	// Stream should still exist
	stream, err := app.GetStreamByID(context.Background(), "doki", "stream-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stream == nil {
		t.Fatal("stream was deleted despite 409 response")
	}

	// After deactivating, delete works
	if err := app.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("post-deactivate delete: status=%d want 204", rec.Code)
	}
}

func TestAdminDeleteStreamWithMedia(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	if err := app.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	mediaPath := filepath.Join(app.TempDir, "doki", "stream-1", "audio", "fake.m4a")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("placeholder"), 0644); err != nil {
		t.Fatalf("write fake media: %v", err)
	}

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1?media=true", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The storage delete is fire-and-forget on a goroutine — poll for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mediaPath); os.IsNotExist(err) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Errorf("media file still present after ?media=true delete: %v", err)
	}
}

func TestAdminStop(t *testing.T) {
	_, mux, _ := setupTestApp(t, []string{"doki"})

	// Pre-populate queue with 3 entries through the admin API
	for i, u := range []string{"https://a", "https://b", "https://c"} {
		rec := adminReq(t, mux, http.MethodPost, "/doki/admin/incoming", "admin-doki", map[string]string{"url": u})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("seed %d: status=%d", i, rec.Code)
		}
	}

	// Stop
	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/stop", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop: status=%d", rec.Code)
	}

	// Queue cleared, restart pending
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	var info AdminInfoResponse
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(info.IncomingURLs) != 0 {
		t.Errorf("queue not cleared: %v", info.IncomingURLs)
	}
	if !info.RestartPending {
		t.Error("expected restart pending")
	}
}

func TestAdminUIServesPage(t *testing.T) {
	_, mux, _ := setupTestApp(t, []string{"doki"})

	rec := adminReq(t, mux, http.MethodGet, "/doki/ui", "", nil) // UI route is unauthenticated
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type=%q want text/html…", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Admin · Live Transcript", "channel-name", "X-Admin-Key"} {
		if !strings.Contains(body, want) {
			t.Errorf("UI body missing %q", want)
		}
	}

	// Unknown channel -> 404 from UI handler
	rec = adminReq(t, mux, http.MethodGet, "/unknown/ui", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel ui: status=%d want 404", rec.Code)
	}
}
