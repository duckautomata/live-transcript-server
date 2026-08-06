package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/store"
	"live-transcript-server/internal/ws"

	"github.com/gorilla/websocket"
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
	_, mux := setupTestApp(t, []string{"doki", "mint"})

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
	st, err := store.Open(":memory:", config.DatabaseConfig{SkipWarmup: true})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	cfg := config.Config{
		Credentials: config.Credentials{ApiKey: "test"},
		Channels: []config.ChannelConfig{
			{Name: "noadmin", NumPastStreams: 1, AdminKey: ""}, // explicitly empty
		},
		Storage: config.StorageConfig{Type: "local"},
	}
	app, err := NewApp(cfg, st, t.TempDir(), "v", "b")
	if err != nil {
		t.Fatalf("construct app: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	rec := adminReq(t, mux, http.MethodGet, "/noadmin/admin/info", "anything", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled-admin channel: status=%d want 403", rec.Code)
	}
}

func TestAdminInfoAggregates(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ctx := context.Background()
	seedExampleData(t, app, "doki")
	if err := app.Store.UpsertIncomingStream(ctx, "doki", "https://twitch.tv/dokibird", time.Now().Unix()); err != nil {
		t.Fatalf("upsert incoming: %v", err)
	}
	if err := app.Store.UpsertWorkerStatus(ctx, "doki", "worker-v1", "build-time", time.Now().Unix()); err != nil {
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
	if len(info.IncomingURLs) != 1 || info.IncomingURLs[0] != "https://twitch.tv/dokibird" {
		t.Errorf("incomingUrls=%v", info.IncomingURLs)
	}
	if info.RestartPending {
		t.Error("expected no restart pending")
	}
	if info.Server.Version != "test-version" {
		t.Errorf("server.version=%q", info.Server.Version)
	}
	// The test app has no bot token, so the status must degrade to "off"
	// rather than panicking on the nil bot.
	if info.DiscordBot.State != discord.BotStateOff {
		t.Errorf("discordBot.state=%q want %q", info.DiscordBot.State, discord.BotStateOff)
	}
}

func TestAdminIncomingAddRemove(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})

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
	_, mux := setupTestApp(t, []string{"doki"})

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
	app, mux := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	// Live streams cannot be deleted — deactivate before testing the happy path.
	if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
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
	if stream, err := app.Store.GetStreamByID(context.Background(), "doki", "stream-1"); err != nil {
		t.Fatalf("post-delete get: %v", err)
	} else if stream != nil {
		t.Error("stream still present after delete")
	}
	if transcript, err := app.Store.GetTranscript(context.Background(), "doki", "stream-1"); err != nil {
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
	app, mux := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Connect a real websocket client so we can read what's broadcast.
	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/doki/websocket"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	// Consume the initial sync message.
	var initMsg ws.Message
	if err := conn.ReadJSON(&initMsg); err != nil {
		t.Fatalf("read initial sync: %v", err)
	}

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	conn.SetReadDeadline(time.Now().Add(time.Second))
	var msg ws.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("no deletedStream event received within 1s: %v", err)
	}
	if msg.Event != ws.EventDeletedStream {
		t.Fatalf("event=%q want %q", msg.Event, ws.EventDeletedStream)
	}
	data, ok := msg.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type=%T want map", msg.Data)
	}
	if data["streamId"] != "stream-1" {
		t.Errorf("streamID=%v want stream-1", data["streamId"])
	}
	if data["streamTitle"] != "Test Stream Title" {
		t.Errorf("streamTitle=%v", data["streamTitle"])
	}
	if data["wasLive"] == true {
		t.Error("wasLive=true; we deactivated before delete")
	}
}

func TestAdminDeleteStreamRejectsLive(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki") // seeds with IsLive=true

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("live delete: status=%d want 409, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Stop current stream") {
		t.Errorf("error body should mention the recommended action; got: %s", rec.Body.String())
	}

	// Stream should still exist
	stream, err := app.Store.GetStreamByID(context.Background(), "doki", "stream-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stream == nil {
		t.Fatal("stream was deleted despite 409 response")
	}

	// After deactivating, delete works
	if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	rec = adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("post-deactivate delete: status=%d want 204", rec.Code)
	}
}

func TestAdminDeleteStreamWithMedia(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
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
	_, mux := setupTestApp(t, []string{"doki"})

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
	_, mux := setupTestApp(t, []string{"doki"})

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

// captureAdminWebhook routes the app's admin audit posts to a test webhook and
// returns a channel of the decoded payloads. Posts are sent from a goroutine,
// so tests must receive from the channel rather than assert immediately.
func captureAdminWebhook(t *testing.T, app *App) chan map[string]any {
	t.Helper()
	payloads := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode admin webhook payload: %v", err)
		}
		payloads <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	app.Discord.AdminWebhookURL = srv.URL
	return payloads
}

// waitAdminAudit returns the embed title and its fields as a name->value map.
func waitAdminAudit(t *testing.T, payloads chan map[string]any) (string, map[string]string) {
	t.Helper()
	var payload map[string]any
	select {
	case payload = <-payloads:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for admin audit webhook")
	}

	embeds, ok := payload["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("payload embeds = %#v, want exactly one embed", payload["embeds"])
	}
	embed, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed = %#v, want object", embeds[0])
	}
	title, _ := embed["title"].(string)

	rawFields, ok := embed["fields"].([]any)
	if !ok {
		t.Fatalf("embed fields = %#v, want array", embed["fields"])
	}
	fields := make(map[string]string, len(rawFields))
	for _, rf := range rawFields {
		f, ok := rf.(map[string]any)
		if !ok {
			t.Fatalf("field = %#v, want object", rf)
		}
		name, _ := f["name"].(string)
		value, _ := f["value"].(string)
		fields[name] = value
	}
	return title, fields
}

func expectNoAdminAudit(t *testing.T, payloads chan map[string]any) {
	t.Helper()
	select {
	case p := <-payloads:
		t.Fatalf("unexpected admin audit webhook: %#v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAdminActionsNotifyDiscord(t *testing.T) {
	const queuedURL = "https://www.youtube.com/watch?v=abc123"

	tests := []struct {
		name       string
		setup      func(t *testing.T, app *App)
		method     string
		path       string
		body       any
		wantTitle  string
		wantFields map[string]string
	}{
		{
			name:      "queue add",
			method:    http.MethodPost,
			path:      "/doki/admin/incoming",
			body:      map[string]string{"url": queuedURL},
			wantTitle: "Admin: Queued incoming stream",
			wantFields: map[string]string{
				"Channel Key": "doki",
				"URL":         queuedURL,
				"Endpoint":    "POST /doki/admin/incoming",
			},
		},
		{
			name: "queue remove",
			setup: func(t *testing.T, app *App) {
				if err := app.Store.UpsertIncomingStream(context.Background(), "doki", queuedURL, time.Now().Unix()); err != nil {
					t.Fatalf("seed incoming: %v", err)
				}
			},
			method:    http.MethodDelete,
			path:      "/doki/admin/incoming?url=" + url.QueryEscape(queuedURL),
			wantTitle: "Admin: Removed queued stream",
			wantFields: map[string]string{
				"Channel Key": "doki",
				"URL":         queuedURL,
			},
		},
		{
			name:       "restart request",
			method:     http.MethodPost,
			path:       "/doki/admin/restart",
			wantTitle:  "Admin: Requested worker restart",
			wantFields: map[string]string{"Channel Key": "doki"},
		},
		{
			name:      "restart clear",
			method:    http.MethodDelete,
			path:      "/doki/admin/restart",
			wantTitle: "Admin: Cleared restart request",
			wantFields: map[string]string{
				"Channel Key": "doki",
				"Was Pending": "no",
			},
		},
		{
			name:      "stop current stream",
			method:    http.MethodPost,
			path:      "/doki/admin/stop",
			wantTitle: "Admin: Stopped current stream",
			wantFields: map[string]string{
				"Channel Key":         "doki",
				"Queued URLs Cleared": "0",
				"Restart Requested":   "yes",
			},
		},
		{
			name: "delete stream",
			setup: func(t *testing.T, app *App) {
				seedExampleData(t, app, "doki")
				if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-1", false); err != nil {
					t.Fatalf("deactivate: %v", err)
				}
			},
			method:    http.MethodDelete,
			path:      "/doki/admin/stream/stream-1",
			wantTitle: "Admin: Deleted stream",
			wantFields: map[string]string{
				"Channel Key":   "doki",
				"Stream ID":     "stream-1",
				"Media Deleted": "no",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, mux := setupTestApp(t, []string{"doki"})
			if tc.setup != nil {
				tc.setup(t, app)
			}
			// Capture after setup so seeding never posts an audit itself.
			payloads := captureAdminWebhook(t, app)

			rec := adminReq(t, mux, tc.method, tc.path, "admin-doki", tc.body)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			title, fields := waitAdminAudit(t, payloads)
			if title != tc.wantTitle {
				t.Errorf("title=%q want %q", title, tc.wantTitle)
			}
			for name, want := range tc.wantFields {
				if got := fields[name]; got != want {
					t.Errorf("field %q=%q want %q", name, got, want)
				}
			}
			if fields["Source IP"] == "" {
				t.Error("missing Source IP field")
			}
		})
	}
}

func TestAdminReadsAndFailuresDoNotNotifyDiscord(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	seedExampleData(t, app, "doki")
	payloads := captureAdminWebhook(t, app)

	// Read-only endpoints: the admin page polls these constantly.
	if rec := adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil); rec.Code != http.StatusOK {
		t.Fatalf("info: status=%d", rec.Code)
	}
	if rec := adminReq(t, mux, http.MethodGet, "/doki/ui", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("ui: status=%d", rec.Code)
	}

	// Rejected and failed operations: nothing was done, so nothing is audited.
	if rec := adminReq(t, mux, http.MethodPost, "/doki/admin/restart", "wrong-key", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("bad key: status=%d want 403", rec.Code)
	}
	if rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/nope", "admin-doki", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown stream: status=%d want 404", rec.Code)
	}
	// stream-1 is still live, so this is refused with a 409.
	if rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-1", "admin-doki", nil); rec.Code != http.StatusConflict {
		t.Fatalf("live stream: status=%d want 409", rec.Code)
	}

	expectNoAdminAudit(t, payloads)
}
