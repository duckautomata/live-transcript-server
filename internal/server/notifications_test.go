package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// Every webhook URL a test hands to the server has this shape. The token is
// the secret: it must never show up in a response, a log line, an audit
// record or an error, so it is deliberately distinctive and grep-able.
const (
	notifWebhookID    = "123456789012345678"
	notifWebhookToken = "SecretTokenAbcdefghijklmnopqrstuvwxyz012" // 40 alphanumerics
	notifWebhookURL   = "https://discord.com/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken
	notifAdminKey     = "admin-doki"
	notifBase         = "/doki/admin/notifications"
	notifRoleMention  = "<@&111222333444555666>"
)

// notifWebhookFor builds a Discord webhook URL with a distinct id, so a test
// with several rules can tell from the request path which rule was posted.
func notifWebhookFor(id string) string {
	return "https://discord.com/api/webhooks/" + id + "/" + notifWebhookToken
}

// notifRewriteTransport sends every request to the test webhook server no
// matter which host it was addressed to. The announce sender refuses anything
// but a discord.com URL, so this is the only way to observe a delivery - and
// it is also what guarantees no test can ever reach the real Discord.
type notifRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t notifRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = t.target.Scheme
	r.URL.Host = t.target.Host
	r.Host = t.target.Host
	return t.base.RoundTrip(r)
}

// notifPost is one request the fake Discord received.
type notifPost struct {
	Path  string
	Query url.Values
	Body  map[string]any
}

// notifWebhookServer stands in for Discord's webhook endpoint. Every post is
// pushed onto posts; status, when non-zero, is the HTTP status it answers
// with instead of 200.
type notifWebhookServer struct {
	posts  chan notifPost
	status atomic.Int32
}

// newNotifWebhookServer starts the fake Discord and points the app's
// announcer at it. It must be installed BEFORE anything can dispatch.
func newNotifWebhookServer(t *testing.T, app *App) *notifWebhookServer {
	t.Helper()
	ws := &notifWebhookServer{posts: make(chan notifPost, 32)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook server: decode body: %v", err)
		}
		ws.posts <- notifPost{Path: r.URL.Path, Query: r.URL.Query(), Body: body}
		if s := ws.status.Load(); s != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(s))
			_, _ = w.Write([]byte(`{"message":"Unknown Webhook","code":10015}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	app.Announcer.SetHTTPClient(&http.Client{
		Transport: notifRewriteTransport{target: target, base: srv.Client().Transport},
		Timeout:   10 * time.Second,
	})
	return ws
}

// next returns the next post, failing after a bounded wait.
func (ws *notifWebhookServer) next(t *testing.T) notifPost {
	t.Helper()
	select {
	case p := <-ws.posts:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a webhook post")
	}
	return notifPost{}
}

// none asserts nothing is waiting. Callers must first have waited for
// whatever proves the dispatch in question has finished (its log entry), so
// this never needs a sleep.
func (ws *notifWebhookServer) none(t *testing.T) {
	t.Helper()
	select {
	case p := <-ws.posts:
		t.Fatalf("unexpected webhook post to %s: %#v", p.Path, p.Body)
	default:
	}
}

// notifRule is a valid rule with a role ping in its content and the default
// embed, on the standard webhook unless others are given.
func notifRule(name string, triggers []string, webhooks ...string) model.NotificationEvent {
	if len(webhooks) == 0 {
		webhooks = []string{notifWebhookURL}
	}
	return model.NotificationEvent{
		Name:            name,
		Enabled:         true,
		Webhooks:        model.WebhooksFromURLs(webhooks...),
		Triggers:        triggers,
		Content:         notifRoleMention + " {channel} · {headline}: {url} {time}",
		EmbedEnabled:    true,
		Embed:           announce.DefaultEmbed(),
		CooldownSeconds: 1800,
	}
}

func notifDecode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func notifCreate(t *testing.T, mux *http.ServeMux, channel string, ev model.NotificationEvent) model.NotificationEvent {
	t.Helper()
	rec := adminReq(t, mux, http.MethodPost, "/"+channel+"/admin/notifications", "admin-"+channel, ev)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q: status=%d body=%s", ev.Name, rec.Code, rec.Body.String())
	}
	var got model.NotificationEvent
	notifDecode(t, rec, &got)
	return got
}

func notifGet(t *testing.T, mux *http.ServeMux, channel string) NotificationsResponse {
	t.Helper()
	rec := adminReq(t, mux, http.MethodGet, "/"+channel+"/admin/notifications", "admin-"+channel, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET notifications: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp NotificationsResponse
	notifDecode(t, rec, &resp)
	return resp
}

// notifFind returns the rule with the given id from a GET response.
func notifFind(t *testing.T, resp NotificationsResponse, id int64) model.NotificationEvent {
	t.Helper()
	for _, ev := range resp.Events {
		if ev.ID == id {
			return ev
		}
	}
	t.Fatalf("rule %d not in response (have %d rules)", id, len(resp.Events))
	return model.NotificationEvent{}
}

// notifRawReq sends a body verbatim, for malformed-JSON cases adminReq cannot
// express.
func notifRawReq(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", notifAdminKey)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// notifWaitLog waits until the channel's notification log holds exactly n
// entries (newest first). Log rows are the last thing a dispatch writes, so
// their arrival is the proof a background delivery has finished.
func notifWaitLog(t *testing.T, app *App, n int) []model.NotificationLogEntry {
	t.Helper()
	var entries []model.NotificationLogEntry
	waitFor(t, 3*time.Second, fmt.Sprintf("%d notification log entries", n), func() bool {
		var err error
		entries, err = app.Store.ListNotificationLog(context.Background(), "doki", 50)
		if err != nil {
			t.Fatalf("list notification log: %v", err)
		}
		return len(entries) >= n
	})
	if len(entries) != n {
		t.Fatalf("got %d log entries, want exactly %d: %+v", len(entries), n, entries)
	}
	return entries
}

// notifDetectApp is setupDetectApp with an admin key on the channel, so rules
// can be created through the real handlers, plus the fake Discord installed
// before anything can dispatch.
func notifDetectApp(t *testing.T) (*App, *http.ServeMux, *notifWebhookServer) {
	t.Helper()
	app, mux := setupDetectApp(t)
	app.Channels["doki"].AdminKey = notifAdminKey
	return app, mux, newNotifWebhookServer(t, app)
}

func notifEmbed(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	embeds, _ := body["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("embeds = %#v, want exactly one", body["embeds"])
	}
	e, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed = %#v, want object", embeds[0])
	}
	return e
}

func notifParse(t *testing.T, body map[string]any) []string {
	t.Helper()
	am, ok := body["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("no allowed_mentions in %#v", body)
	}
	raw, ok := am["parse"].([]any)
	if !ok {
		t.Fatalf("allowed_mentions.parse = %#v, want an array", am["parse"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// notifNested reads a string at a key path of a decoded JSON object.
func notifNested(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	s, _ := cur.(string)
	return s
}

// notifLogBuffer is a goroutine-safe sink for captured slog output.
type notifLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *notifLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *notifLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// notifCaptureLogs routes the process's slog output into a buffer for the
// rest of the test. The package runs its tests sequentially, so swapping the
// default logger cannot disturb another test.
func notifCaptureLogs(t *testing.T) *notifLogBuffer {
	t.Helper()
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	buf := &notifLogBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestNotificationsRoutesRequireAdminKey(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})
	ws := newNotifWebhookServer(t, app)

	rule := notifRule("Auth", []string{"live"})
	draft := notificationDraftRequest{Event: rule, Trigger: "live", WebhookURL: notifWebhookURL}
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, notifBase, nil},
		{http.MethodPost, notifBase, rule},
		{http.MethodPost, notifBase + "/preview", draft},
		{http.MethodPost, notifBase + "/test", draft},
		{http.MethodPut, notifBase + "/1", rule},
		{http.MethodDelete, notifBase + "/1", nil},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			for _, key := range []string{"", "wrong", "admin-mint"} {
				rec := adminReq(t, mux, rt.method, rt.path, key, rt.body)
				if rec.Code != http.StatusForbidden {
					t.Errorf("key %q: status=%d want 403 (body %q)", key, rec.Code, rec.Body.String())
				}
			}
			other := strings.Replace(rt.path, "/doki/", "/nobody/", 1)
			if rec := adminReq(t, mux, rt.method, other, notifAdminKey, rt.body); rec.Code != http.StatusNotFound {
				t.Errorf("unknown channel: status=%d want 404", rec.Code)
			}
		})
	}

	// Nothing got through: no rule, no log row, no post.
	resp := notifGet(t, mux, "doki")
	if len(resp.Events) != 0 || len(resp.Log) != 0 {
		t.Fatalf("rejected requests left state behind: %d events, %d log rows", len(resp.Events), len(resp.Log))
	}
	ws.none(t)
}

// ---------------------------------------------------------------------------
// GET
// ---------------------------------------------------------------------------

func TestNotificationsGetReturnsVocabularyAndEmptyLists(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	rec := adminReq(t, mux, http.MethodGet, notifBase, notifAdminKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q want application/json", ct)
	}

	// The lists must be [] and never null: the page iterates them blindly.
	var raw map[string]json.RawMessage
	notifDecode(t, rec, &raw)
	for _, key := range []string{"events", "log", "videos"} {
		if got := strings.TrimSpace(string(raw[key])); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}

	var resp NotificationsResponse
	notifDecode(t, rec, &resp)

	wantTriggers := []announce.Trigger{announce.TriggerLive, announce.TriggerScheduled, announce.TriggerUpload, announce.TriggerShort}
	if len(resp.Triggers) != len(wantTriggers) {
		t.Fatalf("triggers = %+v, want %d entries", resp.Triggers, len(wantTriggers))
	}
	for i, want := range wantTriggers {
		got := resp.Triggers[i]
		if got.ID != want || got.Label == "" || got.Headline == "" || got.Description == "" || len(got.Platforms) == 0 {
			t.Errorf("trigger[%d] = %+v, want id %q with label/headline/description/platforms", i, got, want)
		}
	}

	if len(resp.Placeholders) != len(announce.Placeholders) {
		t.Fatalf("placeholders = %d, want %d", len(resp.Placeholders), len(announce.Placeholders))
	}
	for i, want := range announce.Placeholders {
		if resp.Placeholders[i].Name != want.Name || resp.Placeholders[i].Description == "" {
			t.Errorf("placeholder[%d] = %+v, want %q", i, resp.Placeholders[i], want.Name)
		}
	}

	d := resp.Defaults
	if !d.EmbedEnabled || d.Content != "" || d.CooldownSeconds != defaultCooldownSeconds {
		t.Errorf("defaults = %+v", d)
	}
	if d.Embed != announce.DefaultEmbed() {
		t.Errorf("default embed = %+v, want %+v", d.Embed, announce.DefaultEmbed())
	}
	if d.Embed.Color != "#2ECC71" || d.Embed.Image != "{thumbnail}" || !d.Embed.Timestamp {
		t.Errorf("default embed does not mirror NotifyStreamStart: %+v", d.Embed)
	}

	wantLimits := NotificationLimits{
		Name: 80, Content: 2000, EmbedTitle: 256, EmbedDescription: 4096, EmbedFooter: 2048,
		Webhooks: 10, CooldownSeconds: 7 * 24 * 60 * 60,
	}
	if resp.Limits != wantLimits {
		t.Errorf("limits = %+v, want %+v", resp.Limits, wantLimits)
	}

	if resp.ChannelName != "doki" {
		t.Errorf("channelName = %q, want doki (falls back to the key)", resp.ChannelName)
	}
	if !strings.HasPrefix(resp.TranscriptURL, "https://") || !strings.HasSuffix(resp.TranscriptURL, "/doki/") {
		t.Errorf("transcriptUrl = %q", resp.TranscriptURL)
	}
	if resp.TranscriptURL != app.Announcer.TranscriptURL("doki") {
		t.Errorf("transcriptUrl = %q, want %q", resp.TranscriptURL, app.Announcer.TranscriptURL("doki"))
	}
	if resp.OperatorFeed {
		t.Error("operatorFeed must be false with no detectWebhookUrl configured")
	}
	if resp.QueueIncoming {
		t.Error("queueIncoming must be false by default")
	}
}

// ---------------------------------------------------------------------------
// POST (create)
// ---------------------------------------------------------------------------

func TestNotificationsCreateRule(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	ev := notifRule("Go live", []string{"Live", " upload ", "live"}, notifWebhookURL, "  "+notifWebhookURL+"  ")
	ev.Embed.Color = "2ecc71"
	rec := adminReq(t, mux, http.MethodPost, notifBase, notifAdminKey, ev)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q", ct)
	}
	var got model.NotificationEvent
	notifDecode(t, rec, &got)

	if got.ID <= 0 {
		t.Fatalf("id = %d, want a positive id", got.ID)
	}
	if got.Name != "Go live" || got.ChannelKey != "doki" || !got.Enabled || got.CooldownSeconds != 1800 {
		t.Errorf("echoed rule = %+v", got)
	}
	if want := []string{"live", "upload"}; strings.Join(got.Triggers, ",") != strings.Join(want, ",") {
		t.Errorf("triggers = %v, want %v (lowercased, trimmed, deduped, order kept)", got.Triggers, want)
	}
	if len(got.WebhookURLs()) != 1 || got.WebhookURLs()[0] != notifWebhookURL {
		t.Errorf("webhookUrls = %v, want the one trimmed, deduped URL", got.WebhookURLs())
	}
	if got.Embed.Color != "#2ECC71" {
		t.Errorf("color = %q, want canonical #2ECC71", got.Embed.Color)
	}
	if got.CreatedAt <= 0 || got.UpdatedAt != got.CreatedAt {
		t.Errorf("createdAt=%d updatedAt=%d", got.CreatedAt, got.UpdatedAt)
	}
	if got.SentCount != 0 || got.LastSentAt != 0 || got.LastError != "" {
		t.Errorf("a new rule must have an empty delivery trail: %+v", got)
	}

	resp := notifGet(t, mux, "doki")
	if len(resp.Events) != 1 {
		t.Fatalf("GET lists %d rules, want 1", len(resp.Events))
	}
	listed := resp.Events[0]
	if listed.ID != got.ID || listed.Name != got.Name || strings.Join(listed.Triggers, ",") != "live,upload" ||
		len(listed.WebhookURLs()) != 1 || listed.WebhookURLs()[0] != notifWebhookURL {
		t.Errorf("listed rule = %+v, want what was created", listed)
	}

	stored, err := app.Store.GetNotificationEvent(context.Background(), "doki", got.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored rule: %v %v", stored, err)
	}
}

func TestNotificationsCreateRejectsInvalidRules(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})

	evilURL := "https://evil.example.com/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken
	plainURL := "http://discord.com/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken
	shortURL := "https://discord.com/api/webhooks/" + notifWebhookID + "/nope-not-a-token"

	cases := []struct {
		name   string
		mutate func(*model.NotificationEvent)
		want   string   // substring of the 400 body
		absent []string // must not appear anywhere in the body
	}{
		{
			name:   "webhook on another host",
			mutate: func(ev *model.NotificationEvent) { ev.Webhooks = model.WebhooksFromURLs(evilURL) },
			want:   "not a Discord webhook URL",
			absent: []string{notifWebhookToken, evilURL, "/api/webhooks/" + notifWebhookID},
		},
		{
			name:   "webhook over plain http",
			mutate: func(ev *model.NotificationEvent) { ev.Webhooks = model.WebhooksFromURLs(plainURL) },
			want:   "not a Discord webhook URL",
			absent: []string{notifWebhookToken, plainURL},
		},
		{
			name:   "malformed Discord webhook",
			mutate: func(ev *model.NotificationEvent) { ev.Webhooks = model.WebhooksFromURLs(shortURL) },
			want:   "not a Discord webhook URL",
			absent: []string{"nope-not-a-token", shortURL},
		},
		{
			name:   "no webhooks",
			mutate: func(ev *model.NotificationEvent) { ev.Webhooks = nil },
			want:   "at least one Discord webhook URL is required",
		},
		{
			name:   "blank webhooks only",
			mutate: func(ev *model.NotificationEvent) { ev.Webhooks = model.WebhooksFromURLs("", "   ") },
			want:   "at least one Discord webhook URL is required",
		},
		{
			name:   "no triggers",
			mutate: func(ev *model.NotificationEvent) { ev.Triggers = nil },
			want:   "pick at least one trigger",
		},
		{
			name:   "blank triggers only",
			mutate: func(ev *model.NotificationEvent) { ev.Triggers = []string{"", " "} },
			want:   "pick at least one trigger",
		},
		{
			name:   "unknown trigger",
			mutate: func(ev *model.NotificationEvent) { ev.Triggers = []string{"premiere"} },
			want:   `unknown trigger "premiere"`,
		},
		{
			name:   "empty name",
			mutate: func(ev *model.NotificationEvent) { ev.Name = "   " },
			want:   "name is required",
		},
		{
			name:   "negative cooldown",
			mutate: func(ev *model.NotificationEvent) { ev.CooldownSeconds = -1 },
			want:   "cannot be negative",
		},
		{
			name:   "cooldown over a week",
			mutate: func(ev *model.NotificationEvent) { ev.CooldownSeconds = announce.MaxCooldownSeconds + 1 },
			want:   "at most 7 days",
		},
		{
			name:   "embed enabled but empty",
			mutate: func(ev *model.NotificationEvent) { ev.Embed = model.EmbedTemplate{} },
			want:   "embed is enabled but empty",
		},
		{
			name: "nothing to send",
			mutate: func(ev *model.NotificationEvent) {
				ev.EmbedEnabled = false
				ev.Content = ""
			},
			want: "the message is empty",
		},
		{
			name:   "bad embed color",
			mutate: func(ev *model.NotificationEvent) { ev.Embed.Color = "green" },
			want:   "embed color must look like #RRGGBB",
		},
		{
			name:   "embed image is not a URL",
			mutate: func(ev *model.NotificationEvent) { ev.Embed.Image = "picture.png" },
			want:   "embed image must be a URL or a placeholder",
		},
		{
			name:   "content over the Discord limit",
			mutate: func(ev *model.NotificationEvent) { ev.Content = strings.Repeat("x", announce.MaxContentLength+1) },
			want:   "message must be at most 2000 characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := notifRule("Bad", []string{"live"})
			tc.mutate(&ev)
			rec := adminReq(t, mux, http.MethodPost, notifBase, notifAdminKey, ev)
			body := rec.Body.String()
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 (body %q)", rec.Code, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("body %q does not mention %q", body, tc.want)
			}
			for _, s := range tc.absent {
				if strings.Contains(body, s) {
					t.Errorf("body %q leaks %q", body, s)
				}
			}
		})
	}

	// Several problems are reported together, so the admin fixes them in one
	// round trip.
	multi := notifRule("", nil, evilURL)
	rec := adminReq(t, mux, http.MethodPost, notifBase, notifAdminKey, multi)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-problem rule: status=%d", rec.Code)
	}
	for _, want := range []string{"name is required", "pick at least one trigger", "not a Discord webhook URL"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("multi-problem body %q lacks %q", rec.Body.String(), want)
		}
	}

	if rec := notifRawReq(t, mux, http.MethodPost, notifBase, `{"name": "broken"`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d want 400", rec.Code)
	}
	if rec := notifRawReq(t, mux, http.MethodPost, notifBase, `[]`); rec.Code != http.StatusBadRequest {
		t.Errorf("JSON of the wrong shape: status=%d want 400", rec.Code)
	}

	if resp := notifGet(t, mux, "doki"); len(resp.Events) != 0 {
		t.Fatalf("rejected rules were persisted: %+v", resp.Events)
	}
}

func TestNotificationsBodyCannotSetProtectedFields(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})

	ev := notifRule("Forged", []string{"live"})
	ev.ID = 999
	ev.ChannelKey = "mint"
	ev.SentCount = 42
	ev.LastSentAt = 1234
	ev.LastError = "forged error"
	ev.LastErrorAt = 5
	ev.CreatedAt = 1
	ev.UpdatedAt = 1

	got := notifCreate(t, mux, "doki", ev)
	if got.ID == 999 || got.ID <= 0 {
		t.Errorf("id = %d, want a server-assigned id", got.ID)
	}
	if got.ChannelKey != "doki" {
		t.Errorf("channelKey = %q, want the route's channel", got.ChannelKey)
	}
	if got.SentCount != 0 || got.LastSentAt != 0 || got.LastError != "" || got.LastErrorAt != 0 {
		t.Errorf("delivery trail taken from the body: %+v", got)
	}
	if got.CreatedAt <= 1 || got.UpdatedAt <= 1 {
		t.Errorf("timestamps taken from the body: created=%d updated=%d", got.CreatedAt, got.UpdatedAt)
	}

	if resp := notifGet(t, mux, "mint"); len(resp.Events) != 0 {
		t.Errorf("rule written into mint: %+v", resp.Events)
	}
	ctx := context.Background()
	if r, _ := app.Store.GetNotificationEvent(ctx, "mint", got.ID); r != nil {
		t.Errorf("rule readable under mint: %+v", r)
	}
	stored, err := app.Store.GetNotificationEvent(ctx, "doki", got.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored rule: %v %v", stored, err)
	}
	if stored.SentCount != 0 || stored.LastSentAt != 0 || stored.LastError != "" {
		t.Errorf("stored trail taken from the body: %+v", stored)
	}
}

// ---------------------------------------------------------------------------
// PUT / DELETE
// ---------------------------------------------------------------------------

func TestNotificationsUpdateKeepsDeliveryTrail(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ctx := context.Background()
	created := notifCreate(t, mux, "doki", notifRule("Original", []string{"live"}))

	// A delivery, the way the dispatcher records one.
	sentAt := time.Now().Unix() - 60
	allowed, _, err := app.Store.ClaimNotificationSend(ctx, created.ID, "live", sentAt)
	if err != nil || !allowed {
		t.Fatalf("claim: allowed=%v err=%v", allowed, err)
	}
	if err := app.Store.RecordNotificationResult(ctx, created.ID, true, "", sentAt); err != nil {
		t.Fatalf("record: %v", err)
	}

	edit := created
	edit.Name = "  Renamed  "
	edit.Triggers = []string{"Upload", "short", "upload"}
	edit.Enabled = false
	edit.CooldownSeconds = 60
	edit.Content = "{channel} posted {url}"
	edit.Webhooks = model.WebhooksFromURLs(notifWebhookFor("999999999999999999"))
	// The body's trail and identity are noise: the editor round-trips whatever
	// it last saw, and a hostile body must not be able to reset or move a rule.
	edit.SentCount = 0
	edit.LastSentAt = 0
	edit.ChannelKey = "mint"
	edit.ID = 424242
	edit.CreatedAt = 7

	rec := adminReq(t, mux, http.MethodPut, fmt.Sprintf("%s/%d", notifBase, created.ID), notifAdminKey, edit)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got model.NotificationEvent
	notifDecode(t, rec, &got)

	if got.ID != created.ID || got.ChannelKey != "doki" {
		t.Errorf("identity changed: id=%d channel=%q", got.ID, got.ChannelKey)
	}
	if got.Name != "Renamed" || got.Enabled || got.CooldownSeconds != 60 || got.Content != "{channel} posted {url}" {
		t.Errorf("edit not applied: %+v", got)
	}
	if strings.Join(got.Triggers, ",") != "upload,short" {
		t.Errorf("triggers = %v, want normalized upload,short", got.Triggers)
	}
	if len(got.WebhookURLs()) != 1 || got.WebhookURLs()[0] != notifWebhookFor("999999999999999999") {
		t.Errorf("webhooks = %v", got.WebhookURLs())
	}
	if got.SentCount != 1 || got.LastSentAt != sentAt {
		t.Errorf("delivery trail lost on edit: sentCount=%d lastSentAt=%d (want 1, %d)", got.SentCount, got.LastSentAt, sentAt)
	}
	if got.CreatedAt != created.CreatedAt {
		t.Errorf("createdAt = %d, want the original %d", got.CreatedAt, created.CreatedAt)
	}
	if got.UpdatedAt < created.UpdatedAt {
		t.Errorf("updatedAt = %d went backwards from %d", got.UpdatedAt, created.UpdatedAt)
	}

	listed := notifFind(t, notifGet(t, mux, "doki"), created.ID)
	if listed.Name != "Renamed" || listed.SentCount != 1 || listed.LastSentAt != sentAt || listed.Enabled {
		t.Errorf("GET after edit = %+v", listed)
	}
}

func TestNotificationsUpdateAndDeleteErrors(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	created := notifCreate(t, mux, "doki", notifRule("Keep me", []string{"live"}))
	valid := notifRule("Valid", []string{"live"})

	if rec := adminReq(t, mux, http.MethodPut, notifBase+"/9999", notifAdminKey, valid); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown id: status=%d want 404", rec.Code)
	}
	for _, id := range []string{"abc", "0", "-5", "1.5", "1e3", " 1"} {
		if rec := adminReq(t, mux, http.MethodPut, notifBase+"/"+url.PathEscape(id), notifAdminKey, valid); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT id %q: status=%d want 400", id, rec.Code)
		}
		if rec := adminReq(t, mux, http.MethodDelete, notifBase+"/"+url.PathEscape(id), notifAdminKey, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE id %q: status=%d want 400", id, rec.Code)
		}
	}
	if rec := adminReq(t, mux, http.MethodDelete, notifBase+"/9999", notifAdminKey, nil); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown id: status=%d want 404", rec.Code)
	}

	// An invalid edit of a real rule is refused and leaves the rule alone.
	broken := notifRule("Broken", nil)
	rec := adminReq(t, mux, http.MethodPut, fmt.Sprintf("%s/%d", notifBase, created.ID), notifAdminKey, broken)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "pick at least one trigger") {
		t.Errorf("PUT invalid rule: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec := notifRawReq(t, mux, http.MethodPut, fmt.Sprintf("%s/%d", notifBase, created.ID), `{"name":`); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT malformed JSON: status=%d want 400", rec.Code)
	}

	resp := notifGet(t, mux, "doki")
	if len(resp.Events) != 1 || resp.Events[0].Name != "Keep me" {
		t.Fatalf("rule disturbed by failed requests: %+v", resp.Events)
	}
}

func TestNotificationsDeleteRule(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	created := notifCreate(t, mux, "doki", notifRule("Doomed", []string{"live"}))
	path := fmt.Sprintf("%s/%d", notifBase, created.ID)

	rec := adminReq(t, mux, http.MethodDelete, path, notifAdminKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 with a body: %q", rec.Body.String())
	}

	if rec := adminReq(t, mux, http.MethodDelete, path, notifAdminKey, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE: status=%d want 404", rec.Code)
	}
	if rec := adminReq(t, mux, http.MethodPut, path, notifAdminKey, notifRule("Ghost", []string{"live"})); rec.Code != http.StatusNotFound {
		t.Errorf("PUT after delete: status=%d want 404", rec.Code)
	}
	if resp := notifGet(t, mux, "doki"); len(resp.Events) != 0 {
		t.Errorf("GET after delete lists %+v", resp.Events)
	}
	if r, _ := app.Store.GetNotificationEvent(context.Background(), "doki", created.ID); r != nil {
		t.Errorf("rule still stored: %+v", r)
	}
}

func TestNotificationsRulesAreScopedToTheirChannel(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki", "mint"})

	dokiRule := notifCreate(t, mux, "doki", notifRule("Doki only", []string{"live"}))
	mintRule := notifCreate(t, mux, "mint", notifRule("Mint only", []string{"upload"}, notifWebhookFor("555555555555555555")))
	if dokiRule.ID == mintRule.ID {
		t.Fatalf("two rules share id %d", dokiRule.ID)
	}
	if dokiRule.ChannelKey != "doki" || mintRule.ChannelKey != "mint" {
		t.Fatalf("channels: %q %q", dokiRule.ChannelKey, mintRule.ChannelKey)
	}

	if resp := notifGet(t, mux, "doki"); len(resp.Events) != 1 || resp.Events[0].ID != dokiRule.ID {
		t.Errorf("doki sees %+v", resp.Events)
	}
	if resp := notifGet(t, mux, "mint"); len(resp.Events) != 1 || resp.Events[0].ID != mintRule.ID {
		t.Errorf("mint sees %+v", resp.Events)
	}

	// mint's admin, on mint's own routes, cannot touch doki's rule by id.
	crossPath := fmt.Sprintf("/mint/admin/notifications/%d", dokiRule.ID)
	if rec := adminReq(t, mux, http.MethodPut, crossPath, "admin-mint", notifRule("Hijacked", []string{"live"})); rec.Code != http.StatusNotFound {
		t.Errorf("cross-channel PUT: status=%d want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if rec := adminReq(t, mux, http.MethodDelete, crossPath, "admin-mint", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-channel DELETE: status=%d want 404", rec.Code)
	}
	// ...and mint's key on doki's routes is refused outright.
	if rec := adminReq(t, mux, http.MethodGet, notifBase, "admin-mint", nil); rec.Code != http.StatusForbidden {
		t.Errorf("mint key on doki route: status=%d want 403", rec.Code)
	}

	still := notifFind(t, notifGet(t, mux, "doki"), dokiRule.ID)
	if still.Name != "Doki only" {
		t.Errorf("doki's rule was altered from mint: %+v", still)
	}
}

// ---------------------------------------------------------------------------
// Preview
// ---------------------------------------------------------------------------

func TestNotificationsPreviewRendersDraft(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ws := newNotifWebhookServer(t, app)
	transcript := app.Announcer.TranscriptURL("doki")

	draft := model.NotificationEvent{
		Name:         "Draft",
		Enabled:      true,
		Triggers:     []string{"upload", "short"},
		Content:      notifRoleMention + " {channel} — {headline}: {url} [{platform}/{trigger}/{id}] {time} {nope}",
		EmbedEnabled: true,
		Embed:        announce.DefaultEmbed(),
	}
	draft.Embed.Footer = "{transcript}"
	// No webhooks: a preview is for a rule that may not be sendable yet.

	preview := func(t *testing.T, req notificationDraftRequest) (NotificationPreviewResponse, *httptest.ResponseRecorder) {
		t.Helper()
		rec := adminReq(t, mux, http.MethodPost, notifBase+"/preview", notifAdminKey, req)
		var resp NotificationPreviewResponse
		if rec.Code == http.StatusOK {
			notifDecode(t, rec, &resp)
		}
		return resp, rec
	}

	resp, rec := preview(t, notificationDraftRequest{Event: draft, Trigger: "upload"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Trigger != "upload" {
		t.Errorf("trigger = %q", resp.Trigger)
	}

	sampleTime, _ := resp.Sample["eventTime"].(float64)
	if sampleTime <= 0 {
		t.Fatalf("sample = %#v, want an eventTime", resp.Sample)
	}
	for k, want := range map[string]string{
		"platform": "youtube",
		"id":       announce.SampleVideoID,
		"url":      "https://www.youtube.com/watch?v=" + announce.SampleVideoID,
		"title":    announce.SampleVideoTitle,
	} {
		if got, _ := resp.Sample[k].(string); got != want {
			t.Errorf("sample[%s] = %q, want %q", k, got, want)
		}
	}

	wantContent := fmt.Sprintf("%s doki — New Video: https://www.youtube.com/watch?v="+announce.SampleVideoID+" [YouTube/upload/"+announce.SampleVideoID+"] <t:%d:R> {nope}",
		notifRoleMention, int64(sampleTime))
	if resp.Content != wantContent {
		t.Errorf("content =\n%q\nwant\n%q", resp.Content, wantContent)
	}

	e := resp.Embed
	if e == nil {
		t.Fatal("embed missing from preview")
	}
	if got := notifNested(e, "title"); got != "doki's New Video" {
		t.Errorf("embed.title = %q", got)
	}
	desc := notifNested(e, "description")
	for _, want := range []string{"**" + announce.SampleVideoTitle + "**", "[Open on YouTube](https://www.youtube.com/watch?v=" + announce.SampleVideoID + ")", "[Transcript](" + transcript + ")"} {
		if !strings.Contains(desc, want) {
			t.Errorf("embed.description %q lacks %q", desc, want)
		}
	}
	if got := notifNested(e, "url"); got != "https://www.youtube.com/watch?v="+announce.SampleVideoID {
		t.Errorf("embed.url = %q", got)
	}
	if got, _ := e["color"].(float64); int(got) != 0x2ECC71 {
		t.Errorf("embed.color = %v, want %d", e["color"], 0x2ECC71)
	}
	if got := notifNested(e, "image", "url"); got != "https://i.ytimg.com/vi/"+announce.SampleVideoID+"/maxresdefault.jpg" {
		t.Errorf("embed.image.url = %q", got)
	}
	if got := notifNested(e, "footer", "text"); got != transcript {
		t.Errorf("embed.footer.text = %q, want %q", got, transcript)
	}
	if got := notifNested(e, "timestamp"); got != time.Unix(int64(sampleTime), 0).UTC().Format(time.RFC3339) {
		t.Errorf("embed.timestamp = %q", got)
	}

	t.Run("each trigger has its own headline and sample", func(t *testing.T) {
		cases := []struct {
			trigger, headline, title, url string
		}{
			{"live", "Stream Started", announce.SampleVideoTitle, "https://www.youtube.com/watch?v=" + announce.SampleVideoID},
			{"scheduled", "Stream Scheduled", announce.SampleVideoTitle, "https://www.youtube.com/watch?v=" + announce.SampleVideoID},
			{"upload", "New Video", announce.SampleVideoTitle, "https://www.youtube.com/watch?v=" + announce.SampleVideoID},
			{"short", "New Short", announce.SampleVideoTitle, "https://www.youtube.com/shorts/" + announce.SampleVideoID},
		}
		for _, tc := range cases {
			resp, rec := preview(t, notificationDraftRequest{Event: draft, Trigger: strings.ToUpper(tc.trigger)})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", tc.trigger, rec.Code, rec.Body.String())
			}
			if resp.Trigger != tc.trigger {
				t.Errorf("trigger %q echoed as %q (want case-insensitive)", tc.trigger, resp.Trigger)
			}
			if !strings.Contains(resp.Content, "doki — "+tc.headline+": "+tc.url) {
				t.Errorf("%s: content %q lacks headline %q / url %q", tc.trigger, resp.Content, tc.headline, tc.url)
			}
			if got := notifNested(resp.Embed, "title"); got != "doki's "+tc.headline {
				t.Errorf("%s: embed.title = %q", tc.trigger, got)
			}
			if got, _ := resp.Sample["title"].(string); got != tc.title {
				t.Errorf("%s: sample title = %q, want %q", tc.trigger, got, tc.title)
			}
			if tc.trigger == "scheduled" {
				at, _ := resp.Sample["eventTime"].(float64)
				if time.Unix(int64(at), 0).Before(time.Now().Add(time.Hour)) {
					t.Errorf("scheduled sample eventTime %v is not in the future", time.Unix(int64(at), 0))
				}
			}
		}
	})

	t.Run("trigger defaults to the draft's first trigger", func(t *testing.T) {
		resp, rec := preview(t, notificationDraftRequest{Event: draft})
		if rec.Code != http.StatusOK || resp.Trigger != "upload" {
			t.Errorf("status=%d trigger=%q, want 200 upload", rec.Code, resp.Trigger)
		}
		bare := draft
		bare.Triggers = nil
		resp, rec = preview(t, notificationDraftRequest{Event: bare})
		if rec.Code != http.StatusOK || resp.Trigger != "live" {
			t.Errorf("no triggers at all: status=%d trigger=%q, want 200 live", rec.Code, resp.Trigger)
		}
	})

	t.Run("a half-finished draft still previews", func(t *testing.T) {
		half := model.NotificationEvent{Content: "hello {channel}", Embed: model.EmbedTemplate{}}
		resp, rec := preview(t, notificationDraftRequest{Event: half, Trigger: "live"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if resp.Content != "hello doki" {
			t.Errorf("content = %q", resp.Content)
		}
		if resp.Embed != nil {
			t.Errorf("embed = %#v, want none when the embed is disabled", resp.Embed)
		}
	})

	t.Run("unknown trigger and bad JSON are 400", func(t *testing.T) {
		if _, rec := preview(t, notificationDraftRequest{Event: draft, Trigger: "premiere"}); rec.Code != http.StatusBadRequest {
			t.Errorf("unknown trigger: status=%d", rec.Code)
		}
		if rec := notifRawReq(t, mux, http.MethodPost, notifBase+"/preview", `{"event":`); rec.Code != http.StatusBadRequest {
			t.Errorf("malformed JSON: status=%d", rec.Code)
		}
	})

	t.Run("the sample reuses the latest live detection", func(t *testing.T) {
		err := app.ObserveLive(context.Background(), livedetect.Broadcast{
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "realvid01",
			URL: livedetect.YouTubeWatchURL("realvid01"), Title: "An actual stream", StartedAt: time.Now(),
		}, livedetect.MechanismYouTubeState)
		if err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		resp, rec := preview(t, notificationDraftRequest{Event: draft, Trigger: "live"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if id, _ := resp.Sample["id"].(string); id != "realvid01" {
			t.Errorf("sample id = %q, want the detected broadcast", id)
		}
		if title, _ := resp.Sample["title"].(string); title != "An actual stream" {
			t.Errorf("sample title = %q", title)
		}
		if got := notifNested(resp.Embed, "image", "url"); got != "https://i.ytimg.com/vi/realvid01/maxresdefault.jpg" {
			t.Errorf("embed.image.url = %q, want the real thumbnail", got)
		}
	})

	// A preview is read-only: nothing is posted, logged or saved.
	ws.none(t)
	after := notifGet(t, mux, "doki")
	if len(after.Events) != 0 || len(after.Log) != 0 {
		t.Errorf("preview left state behind: %d events, %d log rows", len(after.Events), len(after.Log))
	}
}

// ---------------------------------------------------------------------------
// Test send
// ---------------------------------------------------------------------------

func TestNotificationsTestSendRejectsNonDiscordURL(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ws := newNotifWebhookServer(t, app)
	draft := notifRule("Draft", []string{"live"})

	bad := []string{
		"",
		"   ",
		"https://example.com/hook",
		"https://evil.example.com/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken,
		"http://discord.com/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken,
		"https://discord.com/api/webhooks/" + notifWebhookID + "/nope-not-a-token",
		"https://discord.com.evil.example/api/webhooks/" + notifWebhookID + "/" + notifWebhookToken,
	}
	for _, target := range bad {
		rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey,
			notificationDraftRequest{Event: draft, Trigger: "live", WebhookURL: target})
		body := rec.Body.String()
		if rec.Code != http.StatusBadRequest {
			t.Errorf("webhookUrl %q: status=%d want 400 (body %q)", target, rec.Code, body)
		}
		if !strings.Contains(body, "Discord webhook URL") {
			t.Errorf("webhookUrl %q: body %q does not explain the requirement", target, body)
		}
		if strings.Contains(body, notifWebhookToken) || strings.Contains(body, "nope-not-a-token") {
			t.Errorf("webhookUrl %q: body %q echoes the token", target, body)
		}
	}

	ws.none(t)
	if resp := notifGet(t, mux, "doki"); len(resp.Log) != 0 {
		t.Errorf("refused tests were logged: %+v", resp.Log)
	}
}

func TestNotificationsTestSendDeliversWithMentionsSuppressed(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ws := newNotifWebhookServer(t, app)
	audits := captureAdminWebhook(t, app)

	// The draft's own webhook and the test target differ: a test goes only
	// where the admin pointed it, never to the rule's audience.
	draft := notifRule("Draft", []string{"live", "upload"})
	const targetID = "222222222222222222"
	target := notifWebhookFor(targetID)

	rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey,
		notificationDraftRequest{Event: draft, Trigger: "upload", WebhookURL: target})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res announce.TestResult
	notifDecode(t, rec, &res)
	if !res.OK || res.Error != "" {
		t.Errorf("result = %+v, want ok", res)
	}
	if res.Webhook != "webhook "+targetID+"/••••" {
		t.Errorf("result.webhook = %q, want the masked form", res.Webhook)
	}
	if strings.Contains(rec.Body.String(), notifWebhookToken) {
		t.Errorf("response leaks the token: %s", rec.Body.String())
	}

	post := ws.next(t)
	if post.Path != "/api/webhooks/"+targetID+"/"+notifWebhookToken {
		t.Errorf("posted to %q, want the test target", post.Path)
	}
	if post.Query.Get("wait") != "true" {
		t.Errorf("query = %v, want wait=true so Discord reports a rejected embed", post.Query)
	}
	if parse := notifParse(t, post.Body); len(parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want [] (every ping suppressed)", parse)
	}
	content, _ := post.Body["content"].(string)
	if !strings.Contains(content, notifRoleMention) {
		t.Errorf("content %q should still SHOW the role mention (Discord suppresses it via allowed_mentions)", content)
	}
	if !strings.Contains(content, "doki · New Video: https://www.youtube.com/watch?v="+announce.SampleVideoID) {
		t.Errorf("content = %q, want the upload sample rendered", content)
	}
	if got := notifNested(notifEmbed(t, post.Body), "title"); got != "doki's New Video" {
		t.Errorf("embed.title = %q", got)
	}
	ws.none(t)

	entries := notifWaitLog(t, app, 1)
	e := entries[0]
	if e.Status != model.NotificationStatusTest || e.EventName != "Draft" || e.Trigger != "upload" ||
		e.Webhooks != 1 || e.Delivered != 1 || e.Platform != "youtube" || e.BroadcastID != announce.SampleVideoID {
		t.Errorf("log entry = %+v", e)
	}
	if !strings.Contains(e.Detail, "webhook "+targetID+"/••••") || !strings.Contains(e.Detail, "pings suppressed") {
		t.Errorf("log detail = %q", e.Detail)
	}
	if strings.Contains(e.Detail, notifWebhookToken) {
		t.Errorf("log detail leaks the token: %q", e.Detail)
	}
	if e.EventID != 0 {
		t.Errorf("an unsaved draft has no event id, got %d", e.EventID)
	}
	if resp := notifGet(t, mux, "doki"); len(resp.Log) != 1 || resp.Log[0].Status != model.NotificationStatusTest {
		t.Errorf("GET log = %+v", resp.Log)
	}

	title, fields := waitAdminAudit(t, audits)
	if title != "Admin: Sent notification test" {
		t.Errorf("audit title = %q", title)
	}
	if fields["Webhook"] != "webhook "+targetID+"/••••" || fields["Delivered"] != "yes" ||
		fields["Trigger"] != "upload" || fields["Event"] != "Draft" || fields["Channel Key"] != "doki" {
		t.Errorf("audit fields = %v", fields)
	}
	for name, v := range fields {
		if strings.Contains(v, notifWebhookToken) {
			t.Errorf("audit field %q leaks the token: %q", name, v)
		}
	}
}

func TestNotificationsTestSendFailureNeverLeaksToken(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ws := newNotifWebhookServer(t, app)
	audits := captureAdminWebhook(t, app)
	ws.status.Store(http.StatusNotFound)

	rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey,
		notificationDraftRequest{Event: notifRule("Draft", []string{"live"}), Trigger: "live", WebhookURL: notifWebhookURL})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s (the request succeeded; only the delivery failed)", rec.Code, rec.Body.String())
	}
	var res announce.TestResult
	notifDecode(t, rec, &res)
	if res.OK {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if !strings.Contains(res.Error, "HTTP 404") || !strings.Contains(res.Error, "webhook "+notifWebhookID+"/••••") {
		t.Errorf("error = %q, want the status and the masked webhook", res.Error)
	}
	if strings.Contains(rec.Body.String(), notifWebhookToken) {
		t.Errorf("response leaks the token: %s", rec.Body.String())
	}

	// A 404 is final: one attempt, no retry loop.
	ws.next(t)
	ws.none(t)

	e := notifWaitLog(t, app, 1)[0]
	if e.Status != model.NotificationStatusTest || e.Delivered != 0 || e.Webhooks != 1 {
		t.Errorf("log entry = %+v", e)
	}
	if !strings.HasPrefix(e.Detail, "test failed:") || strings.Contains(e.Detail, notifWebhookToken) {
		t.Errorf("log detail = %q", e.Detail)
	}

	_, fields := waitAdminAudit(t, audits)
	if fields["Delivered"] != "no" || !strings.Contains(fields["Result"], "HTTP 404") {
		t.Errorf("audit fields = %v", fields)
	}
	for name, v := range fields {
		if strings.Contains(v, notifWebhookToken) {
			t.Errorf("audit field %q leaks the token: %q", name, v)
		}
	}
}

func TestNotificationsTestSendEmptyTemplateIsRefusedBeforePosting(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ws := newNotifWebhookServer(t, app)

	draft := notifRule("Empty", []string{"live"})
	draft.EmbedEnabled = false
	draft.Content = "   "
	rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey,
		notificationDraftRequest{Event: draft, Trigger: "live", WebhookURL: notifWebhookURL})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res announce.TestResult
	notifDecode(t, rec, &res)
	if res.OK || !strings.Contains(res.Error, "empty message") {
		t.Errorf("result = %+v, want the empty-template failure", res)
	}
	ws.none(t)

	e := notifWaitLog(t, app, 1)[0]
	if e.Status != model.NotificationStatusTest || e.Delivered != 0 || !strings.Contains(e.Detail, "empty message") {
		t.Errorf("log entry = %+v", e)
	}
}

// ---------------------------------------------------------------------------
// Sink: ObserveLive
// ---------------------------------------------------------------------------

func TestObserveLiveQueuesForWorkerOnlyWhenEnabled(t *testing.T) {
	app, _, ws := notifDetectApp(t)
	ctx := context.Background()
	const streamURL = "https://twitch.tv/dokibird"

	if app.QueueIncoming {
		t.Fatal("setupDetectApp must start in shadow mode")
	}
	broadcast := func(id string) livedetect.Broadcast {
		return livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: id, URL: streamURL,
			Title: "Queue test", StartedAt: time.Now(),
		}
	}

	if err := app.ObserveLive(ctx, broadcast("q-off"), livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (shadow): %v", err)
	}
	if urls, _ := app.Store.GetIncomingStreams(ctx, "doki"); len(urls) != 0 {
		t.Fatalf("shadow mode queued %v for the worker", urls)
	}
	if det, _ := app.Store.GetDetection(ctx, "twitch", "q-off"); det == nil {
		t.Fatal("shadow mode must still record the detection")
	}

	app.QueueIncoming = true
	if err := app.ObserveLive(ctx, broadcast("q-on"), livedetect.MechanismTwitchEventSub); err != nil {
		t.Fatalf("ObserveLive (queueing): %v", err)
	}
	urls, err := app.Store.GetIncomingStreams(ctx, "doki")
	if err != nil {
		t.Fatalf("incoming: %v", err)
	}
	if len(urls) != 1 || urls[0] != streamURL {
		t.Fatalf("incoming = %v, want exactly [%s]", urls, streamURL)
	}

	// The same broadcast seen again is a ledger no-op, not a second queue entry.
	if err := app.ObserveLive(ctx, broadcast("q-on"), livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (replay): %v", err)
	}
	if urls, _ := app.Store.GetIncomingStreams(ctx, "doki"); len(urls) != 1 {
		t.Errorf("replayed detection changed the queue: %v", urls)
	}

	// An unknown channel is refused before anything is queued.
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "nope", ID: "q-x", URL: "https://twitch.tv/nope",
	}, livedetect.MechanismTwitchPoll); err == nil {
		t.Error("unknown channel must be rejected even when queueing is on")
	}
	if urls, _ := app.Store.GetIncomingStreams(ctx, "nope"); len(urls) != 0 {
		t.Errorf("unknown channel queued %v", urls)
	}

	// Queueing is the only worker-facing effect: the streams table is untouched
	// and, with no rules and no operator feed, nothing is posted anywhere.
	if s, _ := app.Store.GetRecentStream(ctx, "doki"); s != nil {
		t.Error("detection must never write to the streams table")
	}
	ws.none(t)
}

func TestObserveLiveDispatchesToMatchingEnabledRules(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	ctx := context.Background()

	// Rules that must NOT fire are created first, so that by the time the
	// matching rule's post arrives they have provably been evaluated and
	// skipped - no sleeping to "check nothing else happened".
	uploadOnly := notifCreate(t, mux, "doki", notifRule("Uploads", []string{"upload"}, notifWebhookFor("111111111111111111")))
	off := notifRule("Disabled", []string{"live"}, notifWebhookFor("222222222222222222"))
	off.Enabled = false
	disabled := notifCreate(t, mux, "doki", off)
	const liveID = "333333333333333333"
	live := notifCreate(t, mux, "doki", notifRule("Go live", []string{"live"}, notifWebhookFor(liveID)))

	started := time.Now().Add(-5 * time.Second).Truncate(time.Second)
	err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "77001",
		URL: "https://twitch.tv/dokibird", Title: "Playing games", StartedAt: started,
	}, livedetect.MechanismTwitchEventSub)
	if err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	post := ws.next(t)
	if post.Path != "/api/webhooks/"+liveID+"/"+notifWebhookToken {
		t.Fatalf("posted to %q, want the enabled live rule's webhook", post.Path)
	}
	if post.Query.Get("wait") != "true" {
		t.Errorf("query = %v, want wait=true", post.Query)
	}
	// A real announcement pings exactly the role its template names, and
	// nothing else: the policy is derived from the template, not parsed from
	// the rendered text.
	if parse := notifParse(t, post.Body); len(parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want [] (the template has no @everyone)", parse)
	}
	am, _ := post.Body["allowed_mentions"].(map[string]any)
	roles, _ := am["roles"].([]any)
	if len(roles) != 1 || roles[0] != strings.Trim(notifRoleMention, "<@&>") {
		t.Errorf("allowed_mentions.roles = %v, want the template's role only", roles)
	}
	wantContent := fmt.Sprintf("%s Dokibird · Stream Started: https://twitch.tv/dokibird <t:%d:R>", notifRoleMention, started.Unix())
	if content, _ := post.Body["content"].(string); content != wantContent {
		t.Errorf("content =\n%q\nwant\n%q", content, wantContent)
	}
	e := notifEmbed(t, post.Body)
	if got := notifNested(e, "title"); got != "Dokibird's Stream Started" {
		t.Errorf("embed.title = %q", got)
	}
	desc := notifNested(e, "description")
	if !strings.Contains(desc, "**Playing games**") || !strings.Contains(desc, "[Open on Twitch](https://twitch.tv/dokibird)") {
		t.Errorf("embed.description = %q", desc)
	}
	if got := notifNested(e, "url"); got != "https://twitch.tv/dokibird" {
		t.Errorf("embed.url = %q", got)
	}
	if got, _ := e["color"].(float64); int(got) != 0x2ECC71 {
		t.Errorf("embed.color = %v", e["color"])
	}
	if got := notifNested(e, "image", "url"); !strings.HasPrefix(got, "https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg?t=") {
		t.Errorf("embed.image.url = %q, want the cache-busted Twitch preview", got)
	}
	if got := notifNested(e, "timestamp"); got != started.UTC().Format(time.RFC3339) {
		t.Errorf("embed.timestamp = %q, want %q", got, started.UTC().Format(time.RFC3339))
	}

	entries := notifWaitLog(t, app, 1)
	sent := entries[0]
	if sent.Status != model.NotificationStatusSent || sent.EventID != live.ID || sent.EventName != "Go live" ||
		sent.Trigger != "live" || sent.Platform != "twitch" || sent.BroadcastID != "77001" ||
		sent.Title != "Playing games" || sent.URL != "https://twitch.tv/dokibird" ||
		sent.Webhooks != 1 || sent.Delivered != 1 || sent.Detail != "" {
		t.Errorf("log entry = %+v", sent)
	}
	ws.none(t)

	resp := notifGet(t, mux, "doki")
	if got := notifFind(t, resp, live.ID); got.SentCount != 1 || got.LastSentAt == 0 || got.LastError != "" {
		t.Errorf("live rule trail = sentCount %d lastSentAt %d lastError %q", got.SentCount, got.LastSentAt, got.LastError)
	}
	for _, id := range []int64{uploadOnly.ID, disabled.ID} {
		if got := notifFind(t, resp, id); got.SentCount != 0 || got.LastSentAt != 0 {
			t.Errorf("rule %d (%s) was touched: %+v", id, got.Name, got)
		}
	}
	if len(resp.Log) != 1 {
		t.Errorf("GET log has %d rows, want 1", len(resp.Log))
	}

	// A restart minutes later is a brand-new broadcast id, and the cooldown
	// is exactly what keeps it from pinging everyone twice.
	err = app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "77002",
		URL: "https://twitch.tv/dokibird", Title: "Playing games (restarted)", StartedAt: time.Now(),
	}, livedetect.MechanismTwitchPoll)
	if err != nil {
		t.Fatalf("ObserveLive (restart): %v", err)
	}
	entries = notifWaitLog(t, app, 2)
	suppressed := entries[0]
	if suppressed.Status != model.NotificationStatusSuppressed || suppressed.EventID != live.ID ||
		suppressed.BroadcastID != "77002" || suppressed.Delivered != 0 {
		t.Errorf("restart log entry = %+v, want suppressed", suppressed)
	}
	if !strings.Contains(suppressed.Detail, "minimum gap") {
		t.Errorf("suppressed detail = %q", suppressed.Detail)
	}
	ws.none(t)
	if got := notifFind(t, notifGet(t, mux, "doki"), live.ID); got.SentCount != 1 {
		t.Errorf("suppressed send counted: sentCount = %d", got.SentCount)
	}
}

// ---------------------------------------------------------------------------
// Sink: ObserveVideo
// ---------------------------------------------------------------------------

func TestObserveVideoDedupesOnLedgerAndDispatches(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	ctx := context.Background()

	const hookID = "444444444444444444"
	rule := notifRule("Videos", []string{"upload", "short", "scheduled"}, notifWebhookFor(hookID))
	rule.CooldownSeconds = 0 // two different observations in a row must both go out
	created := notifCreate(t, mux, "doki", rule)

	published := time.Now().Add(-time.Minute).Truncate(time.Second)
	upload := livedetect.VideoEvent{
		Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki",
		ID: "vid-1", URL: livedetect.YouTubeWatchURL("vid-1"), Title: "A new video", PublishedAt: published,
	}
	if err := app.ObserveVideo(ctx, upload); err != nil {
		t.Fatalf("ObserveVideo: %v", err)
	}

	post := ws.next(t)
	if post.Path != "/api/webhooks/"+hookID+"/"+notifWebhookToken {
		t.Errorf("posted to %q", post.Path)
	}
	wantContent := fmt.Sprintf("%s Dokibird · New Video: %s <t:%d:R>", notifRoleMention, livedetect.YouTubeWatchURL("vid-1"), published.Unix())
	if content, _ := post.Body["content"].(string); content != wantContent {
		t.Errorf("content =\n%q\nwant\n%q", content, wantContent)
	}
	e := notifEmbed(t, post.Body)
	if got := notifNested(e, "title"); got != "Dokibird's New Video" {
		t.Errorf("embed.title = %q", got)
	}
	if got := notifNested(e, "image", "url"); got != "https://i.ytimg.com/vi/vid-1/maxresdefault.jpg" {
		t.Errorf("embed.image.url = %q", got)
	}
	if got := notifNested(e, "timestamp"); got != published.UTC().Format(time.RFC3339) {
		t.Errorf("embed.timestamp = %q, want the publish time", got)
	}
	entry := notifWaitLog(t, app, 1)[0]
	if entry.Status != model.NotificationStatusSent || entry.EventID != created.ID || entry.Trigger != "upload" ||
		entry.Platform != "youtube" || entry.BroadcastID != "vid-1" || entry.Delivered != 1 {
		t.Errorf("log entry = %+v", entry)
	}

	// The poller re-derives the same upload on every cycle; the ledger makes
	// the replay a synchronous no-op, so no dispatch can be in flight.
	if err := app.ObserveVideo(ctx, upload); err != nil {
		t.Fatalf("ObserveVideo (replay): %v", err)
	}
	ws.none(t)
	if entries, _ := app.Store.ListNotificationLog(ctx, "doki", 50); len(entries) != 1 {
		t.Errorf("replay produced a log entry: %+v", entries)
	}
	rows, err := app.Store.GetRecentVideoDetections(ctx, "doki", 20)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(rows) != 1 || rows[0].VideoID != "vid-1" || rows[0].Kind != livedetect.VideoUpload ||
		rows[0].PublishedAt != published.Unix() || rows[0].ScheduledAt != 0 || rows[0].Title != "A new video" {
		t.Errorf("ledger = %+v", rows)
	}

	// The same id in a different kind is a different observation: a video is
	// legitimately "scheduled" first and then something else later.
	scheduledAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := app.ObserveVideo(ctx, livedetect.VideoEvent{
		Kind: livedetect.VideoScheduled, Platform: livedetect.PlatformYouTube, ChannelKey: "doki",
		ID: "vid-1", URL: livedetect.YouTubeWatchURL("vid-1"), Title: "Waiting room",
		PublishedAt: published, ScheduledAt: scheduledAt,
	}); err != nil {
		t.Fatalf("ObserveVideo (scheduled): %v", err)
	}
	post = ws.next(t)
	wantContent = fmt.Sprintf("%s Dokibird · Stream Scheduled: %s <t:%d:R>", notifRoleMention, livedetect.YouTubeWatchURL("vid-1"), scheduledAt.Unix())
	if content, _ := post.Body["content"].(string); content != wantContent {
		t.Errorf("scheduled content =\n%q\nwant\n%q (the event time is the scheduled start)", content, wantContent)
	}
	if got := notifNested(notifEmbed(t, post.Body), "timestamp"); got != scheduledAt.UTC().Format(time.RFC3339) {
		t.Errorf("scheduled embed.timestamp = %q, want the scheduled start", got)
	}
	entries := notifWaitLog(t, app, 2)
	if entries[0].Trigger != "scheduled" || entries[0].Status != model.NotificationStatusSent {
		t.Errorf("scheduled log entry = %+v", entries[0])
	}
	ws.none(t)

	rows, _ = app.Store.GetRecentVideoDetections(ctx, "doki", 20)
	kinds := map[string]int64{}
	for _, r := range rows {
		kinds[r.Kind] = r.ScheduledAt
	}
	if len(rows) != 2 || kinds[livedetect.VideoScheduled] != scheduledAt.Unix() || kinds[livedetect.VideoUpload] != 0 {
		t.Errorf("ledger = %+v", rows)
	}

	resp := notifGet(t, mux, "doki")
	if len(resp.Videos) != 2 {
		t.Errorf("GET videos = %+v, want both observations", resp.Videos)
	}
	if got := notifFind(t, resp, created.ID); got.SentCount != 2 {
		t.Errorf("sentCount = %d, want 2", got.SentCount)
	}
}

func TestObserveVideoRejectsUnknownKindsAndChannels(t *testing.T) {
	app, _, ws := notifDetectApp(t)
	ctx := context.Background()

	base := livedetect.VideoEvent{
		Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki",
		ID: "vid-x", URL: livedetect.YouTubeWatchURL("vid-x"), Title: "x", PublishedAt: time.Now(),
	}
	cases := []struct {
		name   string
		mutate func(*livedetect.VideoEvent)
	}{
		{"live is not a video kind", func(v *livedetect.VideoEvent) { v.Kind = "live" }},
		{"unknown kind", func(v *livedetect.VideoEvent) { v.Kind = "premiere" }},
		{"empty kind", func(v *livedetect.VideoEvent) { v.Kind = "" }},
		{"kind is case-sensitive", func(v *livedetect.VideoEvent) { v.Kind = "Upload" }},
		{"unknown channel", func(v *livedetect.VideoEvent) { v.ChannelKey = "nope" }},
		{"empty channel", func(v *livedetect.VideoEvent) { v.ChannelKey = "" }},
		{"empty id", func(v *livedetect.VideoEvent) { v.ID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.mutate(&v)
			if err := app.ObserveVideo(ctx, v); err == nil {
				t.Errorf("ObserveVideo(%+v) = nil, want an error", v)
			}
		})
	}

	for _, ch := range []string{"doki", "nope", ""} {
		if rows, _ := app.Store.GetRecentVideoDetections(ctx, ch, 20); len(rows) != 0 {
			t.Errorf("rejected events reached the ledger for %q: %+v", ch, rows)
		}
	}
	if entries, _ := app.Store.ListNotificationLog(ctx, "doki", 50); len(entries) != 0 {
		t.Errorf("rejected events were logged: %+v", entries)
	}
	ws.none(t)
}

// ---------------------------------------------------------------------------
// Secrecy: the webhook token must never leave the rule
// ---------------------------------------------------------------------------

func TestNotificationsNeverLogWebhookToken(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	ctx := context.Background()
	audits := captureAdminWebhook(t, app)
	logs := notifCaptureLogs(t)

	// Every path that handles a webhook URL, success and failure alike.
	rule := notifRule("Secret", []string{"live"})
	rule.CooldownSeconds = 0
	created := notifCreate(t, mux, "doki", rule) // audit: created

	edit := created
	edit.Name = "Secret (edited)"
	if rec := adminReq(t, mux, http.MethodPut, fmt.Sprintf("%s/%d", notifBase, created.ID), notifAdminKey, edit); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	} // audit: edited

	testReq := notificationDraftRequest{Event: edit, Trigger: "live", WebhookURL: notifWebhookURL}
	if rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey, testReq); rec.Code != http.StatusOK {
		t.Fatalf("test: status=%d", rec.Code)
	} // audit: test ok
	ws.next(t)

	ws.status.Store(http.StatusNotFound)
	if rec := adminReq(t, mux, http.MethodPost, notifBase+"/test", notifAdminKey, testReq); rec.Code != http.StatusOK {
		t.Fatalf("test (failing): status=%d", rec.Code)
	} // audit: test failed
	ws.next(t)
	ws.status.Store(0)

	observe := func(id string) {
		t.Helper()
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: id,
			URL: "https://twitch.tv/dokibird", Title: "Secret stream", StartedAt: time.Now(),
		}, livedetect.MechanismTwitchEventSub); err != nil {
			t.Fatalf("ObserveLive(%s): %v", id, err)
		}
	}
	observe("s-ok")
	ws.next(t)
	notifWaitLog(t, app, 3)

	ws.status.Store(http.StatusNotFound)
	observe("s-fail")
	ws.next(t)
	entries := notifWaitLog(t, app, 4)
	ws.status.Store(0)

	failed := entries[0]
	if failed.Status != model.NotificationStatusFailed || failed.Delivered != 0 {
		t.Errorf("failed dispatch log entry = %+v", failed)
	}
	got := notifFind(t, notifGet(t, mux, "doki"), created.ID)
	if got.LastError == "" || got.LastErrorAt == 0 || !strings.Contains(got.LastError, "webhook "+notifWebhookID+"/••••") {
		t.Errorf("rule lastError = %q lastErrorAt = %d, want the masked failure recorded", got.LastError, got.LastErrorAt)
	}
	if strings.Contains(got.LastError, notifWebhookToken) {
		t.Errorf("rule lastError leaks the token: %q", got.LastError)
	}
	if got.SentCount != 1 {
		t.Errorf("sentCount = %d, want 1 (the failed send does not count)", got.SentCount)
	}

	if rec := adminReq(t, mux, http.MethodDelete, fmt.Sprintf("%s/%d", notifBase, created.ID), notifAdminKey, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d", rec.Code)
	} // audit: deleted
	ws.none(t)

	for _, e := range entries {
		if strings.Contains(e.Detail, notifWebhookToken) {
			t.Errorf("log entry %d (%s) leaks the token: %q", e.ID, e.Status, e.Detail)
		}
	}

	// Five audit records: created, edited, test, test, deleted. Each must carry
	// the masked form and never the token.
	titles := map[string]bool{}
	for range 5 {
		select {
		case p := <-audits:
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), notifWebhookToken) {
				t.Errorf("audit record leaks the token: %s", raw)
			}
			embeds, _ := p["embeds"].([]any)
			if len(embeds) == 1 {
				if em, ok := embeds[0].(map[string]any); ok {
					title, _ := em["title"].(string)
					titles[title] = true
				}
			}
			if !strings.Contains(string(raw), "webhook "+notifWebhookID+"/••••") {
				t.Errorf("audit record does not identify the webhook by its masked form: %s", raw)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for admin audit records")
		}
	}
	for _, want := range []string{"Admin: Created notification event", "Admin: Edited notification event",
		"Admin: Sent notification test", "Admin: Deleted notification event"} {
		if !titles[want] {
			t.Errorf("no audit record titled %q (have %v)", want, titles)
		}
	}

	out := logs.String()
	if !strings.Contains(out, "admin created notification event") || !strings.Contains(out, "announcement dispatched") {
		t.Fatalf("log capture did not see the notification code paths:\n%s", out)
	}
	if strings.Contains(out, notifWebhookToken) {
		idx := strings.Index(out, notifWebhookToken)
		lo, hi := max(0, idx-200), min(len(out), idx+len(notifWebhookToken)+50)
		t.Errorf("the webhook token appears in the process log:\n...%s...", out[lo:hi])
	}
	if !strings.Contains(out, "webhook "+notifWebhookID+"/••••") {
		t.Errorf("logs never mention the masked webhook; expected the delivery failure to be logged in masked form:\n%s", out)
	}
}

// Guard against an unused-import slip if a helper above is trimmed.
var _ io.Writer = (*notifLogBuffer)(nil)
