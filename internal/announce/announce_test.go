package announce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// notifNow is the dispatcher clock in every test, so log rows and timestamps
// are exact.
var notifNow = time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

// notifTokenTail is the distinctive part of every test webhook token. Every
// test that inspects a log row, error or result asserts it is absent.
const notifTokenTail = "SeCrEtWebhookToken_abcdefghij0123456789"

const (
	notifWebhookA        = "100000000000000001"
	notifWebhookB        = "100000000000000002"
	notifWebhookC        = "100000000000000003"
	notifWebhookOperator = "900000000000000009"
)

func notifToken(id string) string { return notifTokenTail + "_" + id }

func notifWebhookURL(id string) string {
	return "https://discord.com/api/webhooks/" + id + "/" + notifToken(id)
}

func notifChannel() Channel {
	return Channel{Key: "doki", DisplayName: "Dokibird", TwitchLogin: "dokibird"}
}

func notifLivePayload() Payload {
	return Payload{
		Trigger:      TriggerLive,
		Platform:     PlatformYouTube,
		ChannelKey:   "doki",
		ID:           "vid123",
		URL:          "https://www.youtube.com/watch?v=vid123",
		Title:        "Big stream <today>",
		EventTime:    notifNow.Add(-30 * time.Second),
		DetectedAt:   notifNow,
		Mechanism:    "youtube-state-poll",
		SawScheduled: true,
	}
}

func notifRenderContext() RenderContext {
	return RenderContext{
		Payload:       notifLivePayload(),
		Channel:       notifChannel(),
		TranscriptURL: "https://lt.example/doki/",
	}
}

// notifEvent builds a sendable rule with the default look plus a role ping in
// the message body.
func notifEvent(id int64, name string, triggers []Trigger, urls ...string) model.NotificationEvent {
	ev := DefaultEvent()
	ev.ID = id
	ev.ChannelKey = "doki"
	ev.Name = name
	ev.Triggers = nil
	for _, t := range triggers {
		ev.Triggers = append(ev.Triggers, string(t))
	}
	ev.Webhooks = model.WebhooksFromURLs(urls...)
	ev.Content = "<@&555555555555555555> {channel} - {headline}"
	return ev
}

// ---------------------------------------------------------------------------
// A fake Discord: an httptest server reached through a transport that rewrites
// discord.com URLs, so the sender's host validation stays in force and no test
// can ever reach the real thing.
// ---------------------------------------------------------------------------

type notifRewriteTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (rt *notifRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if host != "discord.com" && !strings.HasSuffix(host, ".discord.com") && !strings.HasSuffix(host, "discordapp.com") {
		return nil, fmt.Errorf("refusing to send to unexpected host %q", host)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = rt.target.Host
	return rt.inner.RoundTrip(clone)
}

// notifErrTransport fails every request with a fixed error, the way a dead
// network does.
type notifErrTransport struct {
	err   error
	calls int
	mu    sync.Mutex
}

func (t *notifErrTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return nil, t.err
}

var notifWebhookPath = regexp.MustCompile(`^/api/(?:v\d+/)?webhooks/(\d+)/([^/]+)$`)

type notifReply struct {
	status  int
	body    string
	headers map[string]string
}

type notifCall struct {
	WebhookID string
	Token     string
	Query     url.Values
	Header    http.Header
	Body      map[string]any
	Raw       string
}

type notifDiscord struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []notifCall
	script map[string][]notifReply
}

func newNotifDiscord(t *testing.T) *notifDiscord {
	t.Helper()
	d := &notifDiscord{script: map[string][]notifReply{}}
	d.srv = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *notifDiscord) client() *http.Client {
	target, err := url.Parse(d.srv.URL)
	if err != nil {
		panic(err)
	}
	return &http.Client{Transport: &notifRewriteTransport{target: target, inner: d.srv.Client().Transport}}
}

// reply queues canned answers for one webhook id; once the queue is drained
// the webhook answers 200 again.
func (d *notifDiscord) reply(id string, replies ...notifReply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.script[id] = append(d.script[id], replies...)
}

func (d *notifDiscord) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m := notifWebhookPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, "not a webhook path", http.StatusNotFound)
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	call := notifCall{
		WebhookID: m[1],
		Token:     m[2],
		Query:     r.URL.Query(),
		Header:    r.Header.Clone(),
		Body:      body,
		Raw:       string(raw),
	}
	d.mu.Lock()
	d.calls = append(d.calls, call)
	rep := notifReply{status: http.StatusOK, body: `{"id":"1"}`}
	if q := d.script[m[1]]; len(q) > 0 {
		rep = q[0]
		d.script[m[1]] = q[1:]
	}
	d.mu.Unlock()
	for k, v := range rep.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (d *notifDiscord) allCalls() []notifCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *notifDiscord) callsTo(id string) []notifCall {
	var out []notifCall
	for _, c := range d.allCalls() {
		if c.WebhookID == id {
			out = append(out, c)
		}
	}
	return out
}

func notifParse(t *testing.T, c notifCall) []string {
	t.Helper()
	am, ok := c.Body["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions missing from body %s", c.Raw)
	}
	raw, ok := am["parse"].([]any)
	if !ok {
		t.Fatalf("allowed_mentions.parse missing or not a list in body %s", c.Raw)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

func notifEmbed(t *testing.T, c notifCall) map[string]any {
	t.Helper()
	embeds, ok := c.Body["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("expected exactly one embed in body %s", c.Raw)
	}
	e, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed is not an object in body %s", c.Raw)
	}
	return e
}

func notifFooter(t *testing.T, c notifCall) string {
	t.Helper()
	f, _ := notifEmbed(t, c)["footer"].(map[string]any)
	text, _ := f["text"].(string)
	return text
}

// notifSleepRecorder makes a sender's rate-limit waits instant and records
// what it would have waited.
func notifSleepRecorder(s *Sender) *[]time.Duration {
	sleeps := &[]time.Duration{}
	s.sleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	return sleeps
}

// ---------------------------------------------------------------------------
// A fake Store.
// ---------------------------------------------------------------------------

type notifClaim struct {
	allowed   bool
	remaining int64
}

type notifResult struct {
	ID        int64
	Delivered bool
	ErrText   string
	Now       int64
}

type notifStore struct {
	mu        sync.Mutex
	events    map[string][]model.NotificationEvent
	listErr   error
	listPanic bool
	claims    map[int64]notifClaim
	claimErr  error
	claimed   []int64
	results   []notifResult
	logs      []model.NotificationLogEntry
	logErr    error
}

var _ Store = (*notifStore)(nil)

func newNotifStore(events ...model.NotificationEvent) *notifStore {
	s := &notifStore{events: map[string][]model.NotificationEvent{}, claims: map[int64]notifClaim{}}
	for _, ev := range events {
		s.events[ev.ChannelKey] = append(s.events[ev.ChannelKey], ev)
	}
	return s
}

func (s *notifStore) ListNotificationEvents(_ context.Context, channelKey string) ([]model.NotificationEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listPanic {
		panic("store exploded")
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.events[channelKey]), nil
}

func (s *notifStore) ClaimNotificationSend(_ context.Context, id int64, _ string, _ int64) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed = append(s.claimed, id)
	if s.claimErr != nil {
		return false, 0, s.claimErr
	}
	if c, ok := s.claims[id]; ok {
		return c.allowed, c.remaining, nil
	}
	return true, 0, nil
}

func (s *notifStore) RecordNotificationResult(_ context.Context, id int64, delivered bool, errText string, now int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, notifResult{ID: id, Delivered: delivered, ErrText: errText, Now: now})
	return nil
}

func (s *notifStore) InsertNotificationLog(_ context.Context, e model.NotificationLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logErr != nil {
		return s.logErr
	}
	e.ID = int64(len(s.logs) + 1)
	s.logs = append(s.logs, e)
	return nil
}

func (s *notifStore) logEntries() []model.NotificationLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.logs)
}

func (s *notifStore) resultEntries() []notifResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.results)
}

func (s *notifStore) claimedIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.claimed)
}

func notifWaitForLogs(t *testing.T, s *notifStore, n int) []model.NotificationLogEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		logs := s.logEntries()
		if len(logs) >= n {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d log entries, have %d", n, len(logs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newNotifDispatcher wires a dispatcher to the fake store and fake Discord.
// Deliveries run synchronously unless the override installs a Background, so
// a test can assert right after Dispatch returns.
func newNotifDispatcher(t *testing.T, store *notifStore, discord *notifDiscord, override func(*Config)) *Dispatcher {
	t.Helper()
	cfg := Config{
		Store:             store,
		Channels:          map[string]Channel{"doki": notifChannel()},
		TranscriptBaseURL: "https://lt.example/",
		Version:           "1.2.3",
		HTTPClient:        discord.client(),
		Background:        func(fn func()) bool { fn(); return true },
	}
	if override != nil {
		override(&cfg)
	}
	d := New(cfg)
	notifSleepRecorder(d.sender)
	d.now = func() time.Time { return notifNow }
	return d
}

func notifAssertNoToken(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, notifTokenTail) {
		t.Errorf("%s leaks the webhook token: %q", what, s)
	}
}

func notifAssertProblem(t *testing.T, ve *ValidationError, want string) {
	t.Helper()
	if !slices.Contains(ve.Problems, want) {
		t.Errorf("missing problem %q in %q", want, ve.Problems)
	}
}

func notifValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if !IsValidationError(err) {
		t.Error("IsValidationError should report true")
	}
	return ve
}

// ---------------------------------------------------------------------------
// Triggers, platforms and thumbnails
// ---------------------------------------------------------------------------

func TestTriggerTableAndLabels(t *testing.T) {
	for _, info := range Triggers {
		if !KnownTrigger(string(info.ID)) {
			t.Errorf("KnownTrigger(%q) = false", info.ID)
		}
		if info.ID.Headline() != info.Headline {
			t.Errorf("%s: Headline() = %q, want %q", info.ID, info.ID.Headline(), info.Headline)
		}
		if info.ID.Label() != info.Label {
			t.Errorf("%s: Label() = %q, want %q", info.ID, info.ID.Label(), info.Label)
		}
	}
	if KnownTrigger("bogus") || KnownTrigger("") || KnownTrigger("Live") {
		t.Error("KnownTrigger accepted an unknown or uncanonical trigger")
	}
	if got := Trigger("bogus").Headline(); got != "Update" {
		t.Errorf("unknown trigger headline = %q, want Update", got)
	}
	if got := Trigger("bogus").Label(); got != "bogus" {
		t.Errorf("unknown trigger label = %q, want the raw id", got)
	}
	want := map[Trigger]string{TriggerLive: "Stream Started", TriggerScheduled: "Stream Scheduled", TriggerUpload: "New Video", TriggerShort: "New Short"}
	for tr, headline := range want {
		if tr.Headline() != headline {
			t.Errorf("%s headline = %q, want %q", tr, tr.Headline(), headline)
		}
	}
	if PlatformLabel(PlatformYouTube) != "YouTube" || PlatformLabel(PlatformTwitch) != "Twitch" || PlatformLabel("kick") != "kick" {
		t.Error("PlatformLabel mapping is wrong")
	}
}

func TestPayloadThumbnailURL(t *testing.T) {
	ch := notifChannel()
	yt := Payload{Platform: PlatformYouTube, ID: "vid123"}
	if got, want := yt.ThumbnailURL(ch), "https://i.ytimg.com/vi/vid123/maxresdefault.jpg"; got != want {
		t.Errorf("youtube thumbnail = %q, want %q", got, want)
	}
	if got := (Payload{Platform: PlatformYouTube}).ThumbnailURL(ch); got != "" {
		t.Errorf("youtube thumbnail without id = %q, want empty", got)
	}

	tw := Payload{Platform: PlatformTwitch, ID: "42", DetectedAt: notifNow}
	want := fmt.Sprintf("https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg?t=%d", notifNow.Unix())
	if got := tw.ThumbnailURL(ch); got != want {
		t.Errorf("twitch thumbnail = %q, want %q", got, want)
	}
	if got := (Payload{Platform: PlatformTwitch}).ThumbnailURL(ch); got != "https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg" {
		t.Errorf("twitch thumbnail without DetectedAt should carry no cache-buster, got %q", got)
	}
	if got := tw.ThumbnailURL(Channel{DisplayName: "DokiBird"}); !strings.Contains(got, "live_user_dokibird-") {
		t.Errorf("twitch thumbnail should fall back to the lowercased display name, got %q", got)
	}
	if got := (Payload{Platform: "kick", ID: "x"}).ThumbnailURL(ch); got != "" {
		t.Errorf("unknown platform thumbnail = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Expand / Render
// ---------------------------------------------------------------------------

func TestRenderContextValuesEveryPlaceholder(t *testing.T) {
	rc := notifRenderContext()
	unix := rc.Payload.EventTime.Unix()
	want := map[string]string{
		"{channel}":    "Dokibird",
		"{title}":      "Big stream <today>",
		"{url}":        "https://www.youtube.com/watch?v=vid123",
		"{platform}":   "YouTube",
		"{headline}":   "Stream Started",
		"{time}":       fmt.Sprintf("<t:%d:R>", unix),
		"{timeFull}":   fmt.Sprintf("<t:%d:F>", unix),
		"{transcript}": "https://lt.example/doki/",
		"{thumbnail}":  "https://i.ytimg.com/vi/vid123/maxresdefault.jpg",
		"{trigger}":    "live",
		"{id}":         "vid123",
	}
	got := rc.values()
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q", name, got[name], w)
		}
	}
	// The documented list and the implementation must agree exactly.
	for _, ph := range Placeholders {
		if _, ok := got[ph.Name]; !ok {
			t.Errorf("documented placeholder %s has no value", ph.Name)
		}
	}
	for name := range got {
		if !slices.ContainsFunc(Placeholders, func(p Placeholder) bool { return p.Name == name }) {
			t.Errorf("placeholder %s is implemented but not documented", name)
		}
	}

	// And every one of them expands through Render, in one template.
	var names, values []string
	for _, ph := range Placeholders {
		names = append(names, ph.Name)
		values = append(values, want[ph.Name])
	}
	msg := Render(model.NotificationEvent{Content: strings.Join(names, "|")}, rc)
	if msg.Content != strings.Join(values, "|") {
		t.Errorf("rendered content = %q, want %q", msg.Content, strings.Join(values, "|"))
	}
	if msg.Embed != nil {
		t.Error("embed should be nil when EmbedEnabled is false")
	}
}

func TestRenderContextChannelFallsBackToKey(t *testing.T) {
	rc := notifRenderContext()
	rc.Channel = Channel{}
	if got := rc.values()["{channel}"]; got != "doki" {
		t.Errorf("{channel} with no display name = %q, want the key", got)
	}
}

func TestExpandLeavesUnknownPlaceholdersIntact(t *testing.T) {
	values := notifRenderContext().values()
	in := "{nope} {channel} {} {Channel} {123} {{title}} { title } {channel"
	want := "{nope} Dokibird {} {Channel} {123} {Big stream <today>} { title } {channel"
	if got := Expand(in, values); got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
	if got := Expand("", values); got != "" {
		t.Errorf("Expand(\"\") = %q", got)
	}
}

func TestRenderZeroEventTime(t *testing.T) {
	rc := notifRenderContext()
	rc.Payload.EventTime = time.Time{}
	ev := DefaultEvent()
	ev.Content = "[{time}][{timeFull}]"
	ev.Embed.Timestamp = true
	msg := Render(ev, rc)
	if msg.Content != "[][]" {
		t.Errorf("time placeholders with zero EventTime = %q, want empty", msg.Content)
	}
	if msg.Embed == nil {
		t.Fatal("expected an embed")
	}
	if _, ok := msg.Embed["timestamp"]; ok {
		t.Error("embed must carry no timestamp when EventTime is zero")
	}

	// With a time, the timestamp is RFC3339 in UTC and only when asked for.
	rc = notifRenderContext()
	msg = Render(ev, rc)
	if got := msg.Embed["timestamp"]; got != rc.Payload.EventTime.UTC().Format(time.RFC3339) {
		t.Errorf("timestamp = %v, want %s", got, rc.Payload.EventTime.UTC().Format(time.RFC3339))
	}
	ev.Embed.Timestamp = false
	if _, ok := Render(ev, rc).Embed["timestamp"]; ok {
		t.Error("embed must carry no timestamp when the template disables it")
	}
}

func TestRenderDefaultLook(t *testing.T) {
	rc := notifRenderContext()
	msg := Render(DefaultEvent(), rc)
	if msg.Content != "" {
		t.Errorf("default content = %q, want empty", msg.Content)
	}
	e := msg.Embed
	if e == nil {
		t.Fatal("default event must render an embed")
	}
	if e["title"] != "Dokibird's Stream Started" {
		t.Errorf("title = %v", e["title"])
	}
	desc, _ := e["description"].(string)
	for _, want := range []string{"**Big stream <today>**", "[Open on YouTube](https://www.youtube.com/watch?v=vid123)", "[Transcript](https://lt.example/doki/)"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q lacks %q", desc, want)
		}
	}
	if e["url"] != rc.Payload.URL {
		t.Errorf("url = %v", e["url"])
	}
	if e["color"] != 0x2ECC71 {
		t.Errorf("color = %v, want %d", e["color"], 0x2ECC71)
	}
	img, _ := e["image"].(map[string]string)
	if img["url"] != "https://i.ytimg.com/vi/vid123/maxresdefault.jpg" {
		t.Errorf("image = %v", e["image"])
	}
	if _, ok := e["footer"]; ok {
		t.Error("default look has no footer")
	}
	if _, ok := e["timestamp"]; !ok {
		t.Error("default look carries the event timestamp")
	}
	if msg.IsEmpty() {
		t.Error("default rendering is not empty")
	}
}

func TestRenderFooterOverride(t *testing.T) {
	rc := notifRenderContext()
	rc.FooterOverride = "diag {trigger}"
	ev := DefaultEvent()
	ev.Embed.Footer = "public footer"
	msg := Render(ev, rc)
	f, _ := msg.Embed["footer"].(map[string]string)
	if f["text"] != "diag live" {
		t.Errorf("footer = %v, want the expanded override", msg.Embed["footer"])
	}
	rc.FooterOverride = ""
	msg = Render(ev, rc)
	f, _ = msg.Embed["footer"].(map[string]string)
	if f["text"] != "public footer" {
		t.Errorf("footer = %v, want the template footer", msg.Embed["footer"])
	}
}

func TestRenderEmbedDisabled(t *testing.T) {
	rc := notifRenderContext()
	ev := DefaultEvent()
	ev.EmbedEnabled = false
	ev.Content = "{channel} is live: {url}"
	msg := Render(ev, rc)
	if msg.Embed != nil {
		t.Errorf("embed = %v, want nil", msg.Embed)
	}
	if msg.Content != "Dokibird is live: https://www.youtube.com/watch?v=vid123" {
		t.Errorf("content = %q", msg.Content)
	}
	ev.Content = "   "
	if msg := Render(ev, rc); !msg.IsEmpty() {
		t.Error("whitespace content with no embed is empty")
	}
}

func TestRenderTruncatesAtDiscordLimits(t *testing.T) {
	rc := notifRenderContext()
	runes := func(s string) int { return utf8.RuneCountInString(s) }

	ev := DefaultEvent()
	ev.Content = strings.Repeat("é", MaxContentLength+500) // multi-byte: limits are runes, not bytes
	ev.Embed.Title = strings.Repeat("t", MaxEmbedTitle+50)
	ev.Embed.Description = strings.Repeat("d", MaxEmbedDescription+1000)
	ev.Embed.Footer = strings.Repeat("f", MaxEmbedFooter+1)
	msg := Render(ev, rc)

	if n := runes(msg.Content); n != MaxContentLength || !strings.HasSuffix(msg.Content, "…") {
		t.Errorf("content: %d runes, suffix %q; want %d runes ending in an ellipsis", n, msg.Content[len(msg.Content)-3:], MaxContentLength)
	}
	title, _ := msg.Embed["title"].(string)
	if n := runes(title); n != MaxEmbedTitle || !strings.HasSuffix(title, "…") {
		t.Errorf("title: %d runes, want %d ending in an ellipsis", n, MaxEmbedTitle)
	}
	footer, _ := msg.Embed["footer"].(map[string]string)
	if n := runes(footer["text"]); n != MaxEmbedFooter {
		t.Errorf("footer: %d runes, want %d", n, MaxEmbedFooter)
	}
	// Per-field limits sum to 6400; the combined 6000 limit trims the
	// description by the overflow.
	desc, _ := msg.Embed["description"].(string)
	wantDesc := MaxEmbedDescription - (MaxEmbedTitle + MaxEmbedDescription + MaxEmbedFooter - MaxEmbedTotal)
	if n := runes(desc); n != wantDesc || !strings.HasSuffix(desc, "…") {
		t.Errorf("description: %d runes, want %d ending in an ellipsis", n, wantDesc)
	}
	if total := runes(title) + runes(desc) + runes(footer["text"]); total != MaxEmbedTotal {
		t.Errorf("embed total = %d, want %d", total, MaxEmbedTotal)
	}

	// Exactly at the limit is left alone.
	ev = DefaultEvent()
	ev.Content = strings.Repeat("a", MaxContentLength)
	if msg := Render(ev, rc); msg.Content != ev.Content {
		t.Error("content exactly at the limit must not be altered")
	}
}

func TestRenderExpandURLDropsNonHTTPValues(t *testing.T) {
	rc := notifRenderContext()
	ev := DefaultEvent()
	ev.Embed.URL = "{title}"                  // expands to prose, not a URL
	ev.Embed.Image = "javascript:alert(1)"    // wrong scheme
	ev.Embed.Thumbnail = "   {transcript}   " // whitespace around a real URL is fine
	msg := Render(ev, rc)
	if _, ok := msg.Embed["url"]; ok {
		t.Errorf("url should be dropped when the template expands to a non-URL, got %v", msg.Embed["url"])
	}
	if _, ok := msg.Embed["image"]; ok {
		t.Errorf("image with a javascript: scheme must be dropped, got %v", msg.Embed["image"])
	}
	thumb, _ := msg.Embed["thumbnail"].(map[string]string)
	if thumb["url"] != "https://lt.example/doki/" {
		t.Errorf("thumbnail = %v, want the trimmed transcript URL", msg.Embed["thumbnail"])
	}

	// A platform with no preview image renders {thumbnail} empty, and the
	// image field simply disappears rather than becoming an empty object.
	rc.Payload.Platform = "kick"
	msg = Render(DefaultEvent(), rc)
	if _, ok := msg.Embed["image"]; ok {
		t.Errorf("image should be omitted when {thumbnail} is empty, got %v", msg.Embed["image"])
	}

	// Plain http is allowed; an absurdly long URL is not.
	ev = DefaultEvent()
	ev.Embed.Image = "http://insecure.example/a.png"
	ev.Embed.URL = "https://x.example/?t={title}"
	rc = notifRenderContext()
	rc.Payload.Title = strings.Repeat("x", 3000)
	msg = Render(ev, rc)
	img, _ := msg.Embed["image"].(map[string]string)
	if img["url"] != "http://insecure.example/a.png" {
		t.Errorf("http image should be kept, got %v", msg.Embed["image"])
	}
	if _, ok := msg.Embed["url"]; ok {
		t.Error("a URL over 2048 bytes must be dropped")
	}

	if got := expandURL("{title}", map[string]string{"{title}": "https://ok.example/x"}); got != "https://ok.example/x" {
		t.Errorf("expandURL = %q", got)
	}
	if got := expandURL("ftp://x.example/", nil); got != "" {
		t.Errorf("expandURL(ftp) = %q, want empty", got)
	}
}

func TestRenderEmbedWithNoVisibleFieldsIsEmpty(t *testing.T) {
	// Every visible field expands to nothing; only color, url and timestamp
	// remain. Discord answers "Cannot send an empty message" for such an
	// embed, so the rendering should count as empty.
	rc := notifRenderContext()
	rc.TranscriptURL = ""
	rc.Payload.Platform = "kick"
	ev := DefaultEvent()
	ev.Embed.Title = "{transcript}"
	ev.Embed.Description = "{transcript}"
	msg := Render(ev, rc)
	if !msg.IsEmpty() {
		t.Errorf("an embed with nothing visible must render as empty, got %v", msg.Embed)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"", 3, ""},
		{"日本語テキスト", 4, "日本語…"},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.max); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestParseColor(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"#2ECC71", 0x2ECC71, true},
		{"2ECC71", 0x2ECC71, true},
		{"#2ecc71", 0x2ECC71, true},
		{"#000000", 0, true},
		{"#FFFFFF", 0xFFFFFF, true},
		{"#ffffff  ", 0xFFFFFF, true},
		{"", 0, false},
		{"#", 0, false},
		{"#12345", 0, false},
		{"#1234567", 0, false},
		{"#GGGGGG", 0, false},
		{"blue", 0, false},
		{"##FFFFFF", 0, false},
		{"#FF FF FF", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseColor(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseColor(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// Normalize
// ---------------------------------------------------------------------------

func notifValidEvent() model.NotificationEvent {
	ev := DefaultEvent()
	ev.Name = "Go live"
	ev.Webhooks = model.WebhooksFromURLs(notifWebhookURL(notifWebhookA))
	ev.Triggers = []string{string(TriggerLive)}
	return ev
}

func TestNormalizeAcceptsDefaultRule(t *testing.T) {
	ev := notifValidEvent()
	if err := Normalize(&ev); err != nil {
		t.Fatalf("the default rule with one webhook must be valid: %v", err)
	}
	if ev.Embed.Color != DefaultEmbedColor {
		t.Errorf("color = %q, want %q", ev.Embed.Color, DefaultEmbedColor)
	}
	if !IsValidationError(&ValidationError{}) || IsValidationError(errors.New("x")) || IsValidationError(nil) {
		t.Error("IsValidationError misclassifies")
	}
	if got := (&ValidationError{Problems: []string{"a", "b"}}).Error(); got != "a; b" {
		t.Errorf("Error() = %q", got)
	}
}

func TestNormalizeTrimsAndDedupes(t *testing.T) {
	a, b := notifWebhookURL(notifWebhookA), notifWebhookURL(notifWebhookB)
	ev := notifValidEvent()
	ev.Name = "  Go live  "
	ev.Webhooks = model.WebhooksFromURLs("  "+a+" ", a, "", "\t", b, b+"\n")
	ev.Triggers = []string{" Live ", "live", "", "UPLOAD", "upload", "Short"}
	ev.Embed.Title = "  {channel}  "
	ev.Embed.Description = "desc\n\n  "
	ev.Embed.URL = " {url} "
	ev.Embed.Image = " {thumbnail} "
	ev.Embed.Thumbnail = " https://img.example/t.png "
	ev.Embed.Footer = " foot "
	if err := Normalize(&ev); err != nil {
		t.Fatalf("unexpected problems: %v", err)
	}
	if ev.Name != "Go live" {
		t.Errorf("name = %q", ev.Name)
	}
	if !slices.Equal(ev.WebhookURLs(), []string{a, b}) {
		t.Errorf("webhooks = %q, want [%s %s]", ev.WebhookURLs(), MaskWebhookURL(a), MaskWebhookURL(b))
	}
	if !slices.Equal(ev.Triggers, []string{"live", "upload", "short"}) {
		t.Errorf("triggers = %q", ev.Triggers)
	}
	e := ev.Embed
	if e.Title != "{channel}" || e.Description != "desc" || e.URL != "{url}" || e.Image != "{thumbnail}" ||
		e.Thumbnail != "https://img.example/t.png" || e.Footer != "foot" {
		t.Errorf("embed fields were not trimmed: %+v", e)
	}
}

func TestNormalizeEmptyListsBecomeNonNil(t *testing.T) {
	ev := model.NotificationEvent{Name: "x", EmbedEnabled: true, Embed: DefaultEmbed()}
	err := Normalize(&ev)
	notifValidationError(t, err)
	if ev.Webhooks == nil || ev.Triggers == nil {
		t.Error("nil slices must become empty slices so JSON renders [] not null")
	}
}

func TestNormalizeReportsEveryProblemAtOnce(t *testing.T) {
	ev := model.NotificationEvent{
		Name:         "   ",
		Webhooks:     model.WebhooksFromURLs("https://evil.example/api/webhooks/123456789012345678/" + notifToken("evil")),
		Triggers:     []string{"bogus"},
		Content:      strings.Repeat("c", MaxContentLength+1),
		EmbedEnabled: true,
		Embed: model.EmbedTemplate{
			Title:       strings.Repeat("t", MaxEmbedTitle+1),
			Description: strings.Repeat("d", MaxEmbedDescription+1),
			Footer:      strings.Repeat("f", MaxEmbedFooter+1),
			Color:       "blue",
			URL:         "ftp://x.example/",
			Image:       "https://img.example/" + strings.Repeat("i", MaxTemplateURLLength),
			Thumbnail:   "not a url",
		},
		CooldownSeconds: -1,
	}
	ve := notifValidationError(t, Normalize(&ev))
	for _, want := range []string{
		"name is required",
		"https://evil.example/… is not a Discord webhook URL (expected https://discord.com/api/webhooks/…)",
		"at least one Discord webhook URL is required",
		`unknown trigger "bogus"`,
		"pick at least one trigger",
		fmt.Sprintf("message must be at most %d characters", MaxContentLength),
		fmt.Sprintf("embed title must be at most %d characters", MaxEmbedTitle),
		fmt.Sprintf("embed description must be at most %d characters", MaxEmbedDescription),
		fmt.Sprintf("embed footer must be at most %d characters", MaxEmbedFooter),
		"embed color must look like #RRGGBB",
		"embed link must be a URL or a placeholder like {url}",
		"embed image URL is too long",
		"embed thumbnail must be a URL or a placeholder like {url}",
		"minimum time between notifications cannot be negative",
	} {
		notifAssertProblem(t, ve, want)
	}
	if len(ve.Problems) != 14 {
		t.Errorf("got %d problems, want 14: %q", len(ve.Problems), ve.Problems)
	}
	notifAssertNoToken(t, "validation error", ve.Error())
	if len(ev.WebhookURLs()) != 0 {
		t.Errorf("the rejected URL must not survive in the rule: %q", ev.WebhookURLs())
	}
}

func TestNormalizeRejectsNonDiscordWebhooks(t *testing.T) {
	good := notifWebhookURL(notifWebhookA)
	for _, bad := range []string{
		"http://discord.com/api/webhooks/123456789012345678/" + notifToken("x"),
		"https://discord.com.evil.example/api/webhooks/123456789012345678/" + notifToken("x"),
		"https://example.com/hook",
		"https://discord.com/api/webhooks/123456789012345678/" + notifToken("x") + "?wait=true",
		"discord.com/api/webhooks/123456789012345678/" + notifToken("x"),
	} {
		ev := notifValidEvent()
		ev.Webhooks = model.WebhooksFromURLs(bad, good)
		ve := notifValidationError(t, Normalize(&ev))
		found := false
		for _, p := range ve.Problems {
			if strings.Contains(p, "is not a Discord webhook URL") {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no rejection in %q", bad, ve.Problems)
		}
		if slices.Contains(ve.Problems, "at least one Discord webhook URL is required") {
			t.Errorf("%q: the valid URL alongside it should satisfy the minimum", bad)
		}
		if !slices.Equal(ev.WebhookURLs(), []string{good}) {
			t.Errorf("%q: webhooks = %q, want only the valid one", bad, ev.WebhookURLs())
		}
		notifAssertNoToken(t, "problem for "+MaskWebhookURL(bad), ve.Error())
	}
}

func TestNormalizeWebhookCount(t *testing.T) {
	urls := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, notifWebhookURL(fmt.Sprintf("2000000000000000%02d", i)))
		}
		return out
	}
	ev := notifValidEvent()
	ev.Webhooks = model.WebhooksFromURLs(urls(MaxWebhooksPerEvent)...)
	if err := Normalize(&ev); err != nil {
		t.Errorf("%d webhooks should be allowed: %v", MaxWebhooksPerEvent, err)
	}
	ev = notifValidEvent()
	ev.Webhooks = model.WebhooksFromURLs(urls(MaxWebhooksPerEvent + 1)...)
	notifAssertProblem(t, notifValidationError(t, Normalize(&ev)), fmt.Sprintf("at most %d webhook URLs per event", MaxWebhooksPerEvent))
	// Duplicates collapse before the count.
	ev = notifValidEvent()
	ev.Webhooks = model.WebhooksFromURLs(append(urls(MaxWebhooksPerEvent), urls(MaxWebhooksPerEvent)...)...)
	if err := Normalize(&ev); err != nil {
		t.Errorf("duplicates should be deduped before counting: %v", err)
	}
	if len(ev.WebhookURLs()) != MaxWebhooksPerEvent {
		t.Errorf("deduped to %d, want %d", len(ev.WebhookURLs()), MaxWebhooksPerEvent)
	}
}

func TestNormalizeCooldownBounds(t *testing.T) {
	for _, c := range []struct {
		secs int64
		want string
	}{
		{0, ""},
		{60, ""},
		{MaxCooldownSeconds, ""},
		{-1, "minimum time between notifications cannot be negative"},
		{MaxCooldownSeconds + 1, "minimum time between notifications must be at most 7 days"},
	} {
		ev := notifValidEvent()
		ev.CooldownSeconds = c.secs
		err := Normalize(&ev)
		if c.want == "" {
			if err != nil {
				t.Errorf("cooldown %d: unexpected %v", c.secs, err)
			}
			continue
		}
		notifAssertProblem(t, notifValidationError(t, err), c.want)
	}
}

func TestNormalizeCanonicalizesColor(t *testing.T) {
	for in, want := range map[string]string{
		"2ecc71":    "#2ECC71",
		"#abcdef":   "#ABCDEF",
		" #AbCdEf ": "#ABCDEF",
		"#123456":   "#123456",
		"":          "",
		"   ":       "",
	} {
		ev := notifValidEvent()
		ev.Embed.Color = in
		if err := Normalize(&ev); err != nil {
			t.Errorf("color %q: %v", in, err)
		}
		if ev.Embed.Color != want {
			t.Errorf("color %q normalized to %q, want %q", in, ev.Embed.Color, want)
		}
	}
	ev := notifValidEvent()
	ev.Embed.Color = "#12345G"
	notifAssertProblem(t, notifValidationError(t, Normalize(&ev)), "embed color must look like #RRGGBB")
}

func TestNormalizeNameLength(t *testing.T) {
	ev := notifValidEvent()
	ev.Name = strings.Repeat("n", MaxEventNameLength)
	if err := Normalize(&ev); err != nil {
		t.Errorf("name at the limit: %v", err)
	}
	ev.Name = strings.Repeat("n", MaxEventNameLength+1)
	notifAssertProblem(t, notifValidationError(t, Normalize(&ev)), fmt.Sprintf("name must be at most %d characters", MaxEventNameLength))
}

func TestNormalizeEmptyMessageRules(t *testing.T) {
	// Embed enabled but nothing visible.
	ev := notifValidEvent()
	ev.Content = "hello"
	ev.Embed = model.EmbedTemplate{Color: "#FFFFFF", URL: "{url}", Timestamp: true}
	notifAssertProblem(t, notifValidationError(t, Normalize(&ev)), "the embed is enabled but empty - give it a title or description, or disable it")

	// Embed disabled and no content.
	ev = notifValidEvent()
	ev.EmbedEnabled = false
	ev.Content = "  \n "
	notifAssertProblem(t, notifValidationError(t, Normalize(&ev)), "the message is empty: add message text or enable the embed")

	// Embed disabled: its limits are not enforced and content alone is fine.
	ev = notifValidEvent()
	ev.EmbedEnabled = false
	ev.Content = "just text"
	ev.Embed.Title = strings.Repeat("t", MaxEmbedTitle+100)
	ev.Embed.Color = "blue"
	if err := Normalize(&ev); err != nil {
		t.Errorf("a disabled embed must not be validated: %v", err)
	}

	// A placeholder is accepted where a URL is expected.
	ev = notifValidEvent()
	ev.Embed.URL = "{url}"
	ev.Embed.Image = "{thumbnail}"
	ev.Embed.Thumbnail = "https://img.example/t.png"
	if err := Normalize(&ev); err != nil {
		t.Errorf("placeholders in URL fields: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Webhook URL validation and masking
// ---------------------------------------------------------------------------

func TestValidWebhookURL(t *testing.T) {
	token := notifToken("x")
	valid := []string{
		"https://discord.com/api/webhooks/123456789012345678/" + token,
		"https://ptb.discord.com/api/webhooks/123456789012345678/" + token,
		"https://canary.discord.com/api/webhooks/123456789012345678/" + token,
		"https://discordapp.com/api/webhooks/123456789012345678/" + token,
		"https://ptb.discordapp.com/api/webhooks/123456789012345678/" + token,
		"https://canary.discordapp.com/api/webhooks/123456789012345678/" + token,
		"https://discord.com/api/v10/webhooks/123456789012345678/" + token,
		"https://discord.com/api/v9/webhooks/123456789012345678/" + token,
		"https://discord.com/api/webhooks/12345/" + token,                                 // shortest id
		"https://discord.com/api/webhooks/123456789012345678/" + strings.Repeat("a", 20),  // shortest token
		"https://discord.com/api/webhooks/123456789012345678/" + strings.Repeat("Z", 200), // longest token
		"https://discord.com/api/webhooks/123456789012345678/abc-DEF_012345678901234",
		"  https://discord.com/api/webhooks/123456789012345678/" + token + "\n", // trimmed
	}
	for _, u := range valid {
		if !ValidWebhookURL(u) {
			t.Errorf("ValidWebhookURL(%q) = false, want true", u)
		}
		if got := WebhookID(u); got == "" {
			t.Errorf("WebhookID(%q) = empty", u)
		}
	}
	invalid := []string{
		"",
		"http://discord.com/api/webhooks/123456789012345678/" + token,
		"https://www.discord.com/api/webhooks/123456789012345678/" + token,
		"https://discord.com.evil.example/api/webhooks/123456789012345678/" + token,
		"https://evil.example/api/webhooks/123456789012345678/" + token,
		"https://discord.gg/api/webhooks/123456789012345678/" + token,
		"https://discord.com/api/webhooks/1234/" + token,            // id too short
		"https://discord.com/api/webhooks/123456789012345678/short", // token too short
		"https://discord.com/api/webhooks/123456789012345678/" + strings.Repeat("a", 201),
		"https://discord.com/api/webhooks/123456789012345678/" + token + "/extra",
		"https://discord.com/api/webhooks/123456789012345678/" + token + "?wait=true",
		"https://discord.com/api/webhooks/123456789012345678/" + token + "#frag",
		"https://discord.com/api/webhooks/123456789012345678/tok en_abcdefghijklmnop",
		"https://discord.com/api/webhooks/123456789012345678/tok.en_abcdefghijklmnop",
		"https://discord.com/api/webhooks/abc/" + token,
		"https://discord.com/api/webhooks//" + token,
		"https://discord.com/api/vX/webhooks/123456789012345678/" + token,
		"https://discord.com/webhooks/123456789012345678/" + token,
		"https://discord.com/api/webhooks/123456789012345678/" + token + "\x00",
	}
	for _, u := range invalid {
		if ValidWebhookURL(u) {
			t.Errorf("ValidWebhookURL(%q) = true, want false", u)
		}
		if got := WebhookID(u); got != "" {
			t.Errorf("WebhookID(%q) = %q, want empty", u, got)
		}
	}
	if got := WebhookID(notifWebhookURL(notifWebhookA)); got != notifWebhookA {
		t.Errorf("WebhookID = %q, want %s", got, notifWebhookA)
	}
}

func TestMaskWebhookURL(t *testing.T) {
	token := notifToken("m")
	cases := []struct {
		in   string
		want string
	}{
		{"https://discord.com/api/webhooks/123456789012345678/" + token, "webhook 123456789012345678/••••"},
		{"https://discord.com/api/v10/webhooks/123456789012345678/" + token, "webhook 123456789012345678/••••"},
		{"https://ptb.discord.com/api/webhooks/123456789012345678/" + token, "webhook 123456789012345678/••••"},
		{"https://canary.discordapp.com/api/webhooks/123456789012345678/" + token, "webhook 123456789012345678/••••"},
		{"  https://discord.com/api/webhooks/123456789012345678/" + token + "  ", "webhook 123456789012345678/••••"},
		// Not a webhook: scheme and host only, never the path.
		{"https://evil.example/api/webhooks/123456789012345678/" + token, "https://evil.example/…"},
		{"http://discord.com/api/webhooks/123456789012345678/" + token, "http://discord.com/…"},
		{"https://discord.com/api/webhooks/123456789012345678/" + token + "?wait=true", "https://discord.com/…"},
		{"https://evil.example?" + token, "https://evil.example/…"},
		{"https://evil.example#" + token, "https://evil.example/…"},
		{"https://evil.example", "https://evil.example/…"},
		// No scheme: at most a short prefix.
		{"abc", "abc"},
		{"abcdefghijkl", "abcdefghijkl"},
		{"abcdefghijklm", "abcdefghijkl…"},
		{"discord.com/api/webhooks/123456789012345678/" + token, "discord.com/…"},
		{"", ""},
	}
	for _, c := range cases {
		got := MaskWebhookURL(c.in)
		if got != c.want {
			t.Errorf("MaskWebhookURL(%q) = %q, want %q", c.in, got, c.want)
		}
		notifAssertNoToken(t, "masked "+c.want, got)
	}
}

// ---------------------------------------------------------------------------
// Sender
// ---------------------------------------------------------------------------

func notifSender(t *testing.T) (*Sender, *notifDiscord, *[]time.Duration) {
	t.Helper()
	discord := newNotifDiscord(t)
	s := NewSender(discord.client())
	return s, discord, notifSleepRecorder(s)
}

func notifMessage() Message {
	const content = "<@&555555555555555555> @everyone hello"
	return Message{
		Content:  content,
		Embed:    map[string]any{"title": "T", "description": "D", "color": 0x2ECC71},
		Mentions: MentionPolicy(content),
	}
}

// notifAllowedMentions returns a posted body's allowed_mentions object.
func notifAllowedMentions(t *testing.T, c notifCall) map[string]any {
	t.Helper()
	am, ok := c.Body["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions missing from body %s", c.Raw)
	}
	return am
}

// notifIDList reads a string list (roles/users) out of allowed_mentions.
func notifIDList(am map[string]any, key string) []string {
	raw, _ := am[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func notifDeliveryError(t *testing.T, err error) *DeliveryError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var de *DeliveryError
	if !errors.As(err, &de) {
		t.Fatalf("expected *DeliveryError, got %T: %v", err, err)
	}
	notifAssertNoToken(t, "error", err.Error())
	notifAssertNoToken(t, "DeliveryError.Webhook", de.Webhook)
	notifAssertNoToken(t, "DeliveryError.Reason", de.Reason)
	return de
}

func TestSenderPostsRenderedMessageWithRealMentions(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := discord.allCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d requests, want 1", len(calls))
	}
	c := calls[0]
	if c.WebhookID != notifWebhookA || c.Token != notifToken(notifWebhookA) {
		t.Errorf("posted to %s/%s, want the rule's webhook", c.WebhookID, c.Token)
	}
	if c.Query.Get("wait") != "true" {
		t.Errorf("query = %v, want wait=true", c.Query)
	}
	if ct := c.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ua := c.Header.Get("User-Agent"); !strings.Contains(ua, "live-transcript-server") {
		t.Errorf("User-Agent = %q", ua)
	}
	// The policy comes from the template: the role it names, and @everyone
	// because the template itself says so - never a blanket parse list.
	if got := notifParse(t, c); !slices.Equal(got, []string{"everyone"}) {
		t.Errorf("allowed_mentions.parse = %v, want [everyone] (the template says @everyone)", got)
	}
	if got := notifIDList(notifAllowedMentions(t, c), "roles"); !slices.Equal(got, []string{"555555555555555555"}) {
		t.Errorf("allowed_mentions.roles = %v, want the role the template names", got)
	}
	if c.Body["content"] != notifMessage().Content {
		t.Errorf("content = %v", c.Body["content"])
	}
	e := notifEmbed(t, c)
	if e["title"] != "T" || e["description"] != "D" || e["color"] != float64(0x2ECC71) {
		t.Errorf("embed = %v", e)
	}
	// Nothing but the announcement goes on the wire.
	for k := range c.Body {
		if k != "content" && k != "embeds" && k != "allowed_mentions" {
			t.Errorf("unexpected key %q in webhook body", k)
		}
	}
	if len(*sleeps) != 0 {
		t.Errorf("a 2xx must not sleep, slept %v", *sleeps)
	}
}

func TestSenderSuppressedMentionsSendEmptyParseList(t *testing.T) {
	s, discord, _ := notifSender(t)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	c := discord.allCalls()[0]
	if got := notifParse(t, c); len(got) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want []", got)
	}
	// An absent parse key would mean "ping everything"; it must be an
	// explicit empty list.
	if !strings.Contains(c.Raw, `"parse":[]`) {
		t.Errorf("raw body must carry \"parse\":[], got %s", c.Raw)
	}
	// The mention text is still rendered - Discord shows it without pinging.
	if got, _ := c.Body["content"].(string); !strings.Contains(got, "<@&555555555555555555>") {
		t.Errorf("content should keep the mention text, got %q", got)
	}
}

func TestSenderContentOnlyAndEmbedOnly(t *testing.T) {
	s, discord, _ := notifSender(t)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), Message{Content: "just text"}, false); err != nil {
		t.Fatalf("content only: %v", err)
	}
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), Message{Content: "  ", Embed: map[string]any{"title": "x"}}, false); err != nil {
		t.Fatalf("embed only: %v", err)
	}
	calls := discord.allCalls()
	if _, ok := calls[0].Body["embeds"]; ok {
		t.Error("content-only message must not carry an embeds key")
	}
	if _, ok := calls[1].Body["content"]; ok {
		t.Error("blank content must be omitted, not sent as whitespace")
	}
}

func TestSenderRefusesBadInputWithoutRequests(t *testing.T) {
	s, discord, _ := notifSender(t)
	bad := "https://evil.example/api/webhooks/123456789012345678/" + notifToken("evil")
	de := notifDeliveryError(t, s.Send(context.Background(), bad, notifMessage(), false))
	if de.Reason != "not a Discord webhook URL" || de.Status != 0 || de.Webhook != "https://evil.example/…" {
		t.Errorf("DeliveryError = %+v", de)
	}
	de = notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), Message{Content: "  "}, false))
	if de.Reason != "message is empty" {
		t.Errorf("DeliveryError = %+v", de)
	}
	if n := len(discord.allCalls()); n != 0 {
		t.Errorf("%d requests were made, want none", n)
	}
	if got := de.Error(); got != "webhook 100000000000000001/••••: message is empty" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&DeliveryError{Webhook: "w", Status: 404, Reason: "r"}).Error(); got != "w: HTTP 404 r" {
		t.Errorf("Error() with status = %q", got)
	}
}

func TestSenderRetriesRateLimitUsingRetryAfterBody(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	discord.reply(notifWebhookA,
		notifReply{status: http.StatusTooManyRequests, body: `{"message":"You are being rate limited.","retry_after":0.05,"global":false}`},
		notifReply{status: http.StatusOK, body: `{"id":"1"}`},
	)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false); err != nil {
		t.Fatalf("Send after one 429 should succeed: %v", err)
	}
	if n := len(discord.allCalls()); n != 2 {
		t.Errorf("got %d requests, want 2", n)
	}
	if !slices.Equal(*sleeps, []time.Duration{50 * time.Millisecond}) {
		t.Errorf("sleeps = %v, want [50ms] from the retry_after body", *sleeps)
	}
}

func TestSenderRateLimitFallsBackToHeaderThenDefault(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	discord.reply(notifWebhookA,
		notifReply{status: http.StatusTooManyRequests, body: "", headers: map[string]string{"Retry-After": "2"}},
		notifReply{status: http.StatusTooManyRequests, body: "not json"},
		notifReply{status: http.StatusOK, body: `{"id":"1"}`},
	)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !slices.Equal(*sleeps, []time.Duration{2 * time.Second, time.Second}) {
		t.Errorf("sleeps = %v, want [2s 1s] (header, then the default)", *sleeps)
	}
	if n := len(discord.allCalls()); n != 3 {
		t.Errorf("got %d requests, want 3", n)
	}
}

func TestSenderGivesUpOnHugeRetryAfter(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	discord.reply(notifWebhookA, notifReply{status: http.StatusTooManyRequests, body: `{"retry_after":600.0}`})
	de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != http.StatusTooManyRequests || !strings.Contains(de.Reason, "gave up") || !strings.Contains(de.Reason, "10m0s") {
		t.Errorf("DeliveryError = %+v, want a 429 that gave up after 10m0s", de)
	}
	if n := len(discord.allCalls()); n != 1 {
		t.Errorf("got %d requests, want 1 (no retry)", n)
	}
	if len(*sleeps) != 0 {
		t.Errorf("must not wait out a huge limit, slept %v", *sleeps)
	}
}

func TestSenderPersistentRateLimitFailsAfterMaxAttempts(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	for i := 0; i < maxAttempts+2; i++ {
		discord.reply(notifWebhookA, notifReply{status: http.StatusTooManyRequests, body: `{"retry_after":0.01}`})
	}
	de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != http.StatusTooManyRequests || de.Reason != "rate limited" {
		t.Errorf("DeliveryError = %+v", de)
	}
	if n := len(discord.allCalls()); n != maxAttempts {
		t.Errorf("got %d requests, want %d", n, maxAttempts)
	}
	if len(*sleeps) != maxAttempts-1 {
		t.Errorf("slept %d times for %d attempts; the final attempt must not wait out a limit no retry follows", len(*sleeps), maxAttempts)
	}
}

func TestSenderRetriesServerErrorThenSucceeds(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	discord.reply(notifWebhookA,
		notifReply{status: http.StatusBadGateway, body: "<html>bad gateway</html>"},
		notifReply{status: http.StatusOK, body: `{"id":"1"}`},
	)
	if err := s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false); err != nil {
		t.Fatalf("Send after one 502 should succeed: %v", err)
	}
	if n := len(discord.allCalls()); n != 2 {
		t.Errorf("got %d requests, want 2", n)
	}
	if !slices.Equal(*sleeps, []time.Duration{serverErrorBackoff}) {
		t.Errorf("sleeps = %v, want [%v]", *sleeps, serverErrorBackoff)
	}
}

func TestSenderPersistentServerErrorFails(t *testing.T) {
	s, discord, sleeps := notifSender(t)
	for i := 0; i < maxAttempts+2; i++ {
		discord.reply(notifWebhookA, notifReply{status: http.StatusInternalServerError, body: `{"message":"Internal Server Error","code":0}`})
	}
	de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != http.StatusInternalServerError || de.Reason != "Internal Server Error" {
		t.Errorf("DeliveryError = %+v", de)
	}
	if n := len(discord.allCalls()); n != maxAttempts {
		t.Errorf("got %d requests, want %d", n, maxAttempts)
	}
	if len(*sleeps) != maxAttempts-1 {
		t.Errorf("slept %d times for %d attempts; the final 5xx must not back off when no retry follows", len(*sleeps), maxAttempts)
	}
}

func TestSenderDeletedWebhookFailsImmediately(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		s, discord, sleeps := notifSender(t)
		discord.reply(notifWebhookA, notifReply{status: status, body: `{"message":"Unknown Webhook","code":10015}`})
		de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
		if de.Status != status || !strings.Contains(de.Reason, "no longer exists") {
			t.Errorf("%d: DeliveryError = %+v", status, de)
		}
		if !strings.Contains(de.Error(), "no longer exists") || !strings.Contains(de.Error(), "webhook "+notifWebhookA+"/••••") {
			t.Errorf("%d: Error() = %q", status, de.Error())
		}
		if n := len(discord.allCalls()); n != 1 {
			t.Errorf("%d: got %d requests, want 1 (no retry)", status, n)
		}
		if len(*sleeps) != 0 {
			t.Errorf("%d: slept %v", status, *sleeps)
		}
	}
}

func TestSenderOtherClientErrors(t *testing.T) {
	s, discord, _ := notifSender(t)
	discord.reply(notifWebhookA, notifReply{status: http.StatusBadRequest, body: `{"message":"Invalid Form Body","code":50035}`})
	de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != 400 || de.Reason != "Discord rejected the message: Invalid Form Body" {
		t.Errorf("400: %+v", de)
	}
	discord.reply(notifWebhookA, notifReply{status: http.StatusForbidden, body: `{}`})
	de = notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != 403 || de.Reason != "webhook is not allowed to post here" {
		t.Errorf("403: %+v", de)
	}
	if n := len(discord.allCalls()); n != 2 {
		t.Errorf("got %d requests, want 2 (no retries on 4xx)", n)
	}
}

func TestSenderTransportErrorNeverLeaksToken(t *testing.T) {
	tr := &notifErrTransport{err: errors.New("dial tcp 162.159.1.1:443: connection refused")}
	s := NewSender(&http.Client{Transport: tr})
	sleeps := notifSleepRecorder(s)
	de := notifDeliveryError(t, s.Send(context.Background(), notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != 0 {
		t.Errorf("transport failures have no status, got %d", de.Status)
	}
	if want := "request failed: dial tcp 162.159.1.1:443: connection refused"; de.Reason != want {
		t.Errorf("Reason = %q, want %q", de.Reason, want)
	}
	if strings.Contains(de.Error(), "discord.com") {
		t.Errorf("the request URL must be scrubbed from the error: %q", de.Error())
	}
	if tr.calls != maxAttempts {
		t.Errorf("transport called %d times, want %d", tr.calls, maxAttempts)
	}
	if len(*sleeps) != maxAttempts-1 {
		t.Errorf("slept %d times, want %d (between attempts only)", len(*sleeps), maxAttempts-1)
	}
}

func TestSenderStopsOnCancelledContext(t *testing.T) {
	s, discord, _ := notifSender(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	discord.reply(notifWebhookA,
		notifReply{status: http.StatusTooManyRequests, body: `{"retry_after":0.05}`},
		notifReply{status: http.StatusOK, body: `{"id":"1"}`},
	)
	de := notifDeliveryError(t, s.Send(ctx, notifWebhookURL(notifWebhookA), notifMessage(), false))
	if de.Status != http.StatusTooManyRequests {
		t.Errorf("expected the last 429 to be reported, got %+v", de)
	}
	if n := len(discord.allCalls()); n != 1 {
		t.Errorf("got %d requests, want 1 (no retry after cancellation)", n)
	}
}

func TestScrubURLErrorAndHelpers(t *testing.T) {
	uerr := &url.Error{Op: "Post", URL: notifWebhookURL(notifWebhookA), Err: errors.New("EOF")}
	if got := scrubURLError(uerr).Error(); got != "request failed: EOF" {
		t.Errorf("scrubURLError(url.Error) = %q", got)
	}
	notifAssertNoToken(t, "scrubbed", scrubURLError(uerr).Error())
	if got := scrubURLError(errors.New("plain")).Error(); got != "request failed" {
		t.Errorf("scrubURLError(plain) = %q", got)
	}

	if got := describeDiscordError(http.StatusBadGateway, nil); got != "Bad Gateway" {
		t.Errorf("describe 502 = %q", got)
	}
	if got := describeDiscordError(http.StatusServiceUnavailable, []byte(`{"message":"`+strings.Repeat("m", 300)+`"}`)); utf8.RuneCountInString(got) != 200 {
		t.Errorf("long Discord messages are truncated to 200 runes, got %d", utf8.RuneCountInString(got))
	}
	if got := describeDiscordError(http.StatusBadRequest, []byte(`garbage`)); got != "Discord rejected the message" {
		t.Errorf("describe 400 without a message = %q", got)
	}
	if got := describeDiscordError(http.StatusTooManyRequests, nil); got != "rate limited" {
		t.Errorf("describe 429 = %q", got)
	}

	h := http.Header{}
	if got := parseRetryAfter(h, []byte(`{"retry_after":1.5}`)); got != 1500*time.Millisecond {
		t.Errorf("retry_after body = %v", got)
	}
	h.Set("Retry-After", "3")
	if got := parseRetryAfter(h, []byte(`{"retry_after":1.5}`)); got != 1500*time.Millisecond {
		t.Errorf("body should win over header, got %v", got)
	}
	if got := parseRetryAfter(h, nil); got != 3*time.Second {
		t.Errorf("header fallback = %v", got)
	}
	h.Set("Retry-After", "nope")
	if got := parseRetryAfter(h, []byte(`{"retry_after":0}`)); got != 0 {
		t.Errorf("no usable hint = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher
// ---------------------------------------------------------------------------

func TestDispatcherNilReceiverIsSafe(t *testing.T) {
	var d *Dispatcher
	d.Dispatch(notifLivePayload()) // must not panic
	d.SetHTTPClient(nil)
	if ch := d.ChannelInfo("doki"); ch.Key != "doki" || ch.DisplayName != "doki" {
		t.Errorf("nil ChannelInfo = %+v", ch)
	}
	if d.TranscriptURL("doki") != "" || d.OperatorFeedConfigured() {
		t.Error("nil dispatcher has no transcript URL or operator feed")
	}
}

func TestDispatcherChannelAndTranscriptHelpers(t *testing.T) {
	store := newNotifStore()
	d := newNotifDispatcher(t, store, newNotifDiscord(t), nil)
	if got := d.TranscriptURL("doki"); got != "https://lt.example/doki/" {
		t.Errorf("TranscriptURL = %q (trailing slash on the base should be trimmed)", got)
	}
	if ch := d.ChannelInfo("doki"); ch != notifChannel() {
		t.Errorf("ChannelInfo = %+v", ch)
	}
	if ch := d.ChannelInfo("other"); ch.Key != "other" || ch.DisplayName != "other" {
		t.Errorf("unknown channel should fall back to its key, got %+v", ch)
	}
	if d.OperatorFeedConfigured() {
		t.Error("no operator webhook was configured")
	}
	bare := New(Config{Store: store})
	if bare.TranscriptURL("doki") != "" || bare.ChannelInfo("doki").DisplayName != "doki" {
		t.Error("a dispatcher with no base URL or channels must still answer sensibly")
	}
	if bare.background == nil || bare.channels == nil || bare.sender == nil {
		t.Error("New must fill in defaults")
	}
}

func TestDispatcherPreviewUsesChannelPresentation(t *testing.T) {
	d := newNotifDispatcher(t, newNotifStore(), newNotifDiscord(t), nil)
	ev := notifEvent(1, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	p := notifLivePayload()
	got := d.Preview(ev, p)
	want := Render(ev, RenderContext{Payload: p, Channel: notifChannel(), TranscriptURL: "https://lt.example/doki/"})
	if got.Content != want.Content || fmt.Sprint(got.Embed) != fmt.Sprint(want.Embed) {
		t.Errorf("Preview = %+v, want %+v", got, want)
	}
	if got.Content != "<@&555555555555555555> Dokibird - Stream Started" {
		t.Errorf("content = %q", got.Content)
	}
}

func TestDispatchDeliversOnlyMatchingEnabledRules(t *testing.T) {
	live := notifEvent(1, "live rule", []Trigger{TriggerLive, TriggerScheduled}, notifWebhookURL(notifWebhookA))
	upload := notifEvent(2, "upload rule", []Trigger{TriggerUpload}, notifWebhookURL(notifWebhookB))
	disabled := notifEvent(3, "disabled rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookC))
	disabled.Enabled = false
	other := notifEvent(4, "other channel", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookC))
	other.ChannelKey = "mint"
	store := newNotifStore(live, upload, disabled, other)
	discord := newNotifDiscord(t)
	var logged []string
	d := newNotifDispatcher(t, store, discord, func(c *Config) {
		c.OnLogged = func(key string) { logged = append(logged, key) }
	})

	sentBefore := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSent))
	d.Dispatch(notifLivePayload())

	calls := discord.allCalls()
	if len(calls) != 1 || calls[0].WebhookID != notifWebhookA {
		t.Fatalf("expected exactly one POST to webhook A, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	// A real announcement pings exactly the role the template names.
	if got := notifIDList(notifAllowedMentions(t, c), "roles"); !slices.Equal(got, []string{"555555555555555555"}) {
		t.Errorf("a real announcement must ping the template's role: roles = %v", got)
	}
	if got := notifParse(t, c); len(got) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want [] (the template has no @everyone)", got)
	}
	if c.Body["content"] != "<@&555555555555555555> Dokibird - Stream Started" {
		t.Errorf("content = %v", c.Body["content"])
	}
	if _, ok := notifEmbed(t, c)["footer"]; ok {
		t.Error("an audience announcement must not carry the operator diagnostics footer")
	}

	logs := store.logEntries()
	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want 1: %+v", len(logs), logs)
	}
	e := logs[0]
	want := model.NotificationLogEntry{
		ID: 1, ChannelKey: "doki", EventID: 1, EventName: "live rule", Trigger: "live", Platform: "youtube",
		BroadcastID: "vid123", Title: "Big stream <today>", URL: "https://www.youtube.com/watch?v=vid123",
		Status: model.NotificationStatusSent, Detail: "", Webhooks: 1, Delivered: 1, SentAt: notifNow.Unix(),
	}
	if e != want {
		t.Errorf("log entry = %+v\nwant      %+v", e, want)
	}
	if !slices.Equal(store.claimedIDs(), []int64{1}) {
		t.Errorf("claimed = %v, want only rule 1", store.claimedIDs())
	}
	results := store.resultEntries()
	if len(results) != 1 || results[0] != (notifResult{ID: 1, Delivered: true, ErrText: "", Now: notifNow.Unix()}) {
		t.Errorf("results = %+v", results)
	}
	if !slices.Equal(logged, []string{"doki"}) {
		t.Errorf("OnLogged = %v, want [doki]", logged)
	}
	if got := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSent)); got != sentBefore+1 {
		t.Errorf("sent counter went %v -> %v, want +1", sentBefore, got)
	}

	// The upload rule fires for an upload, and nothing else does.
	d.Dispatch(Payload{Trigger: TriggerUpload, Platform: PlatformYouTube, ChannelKey: "doki", ID: "up1", URL: "https://www.youtube.com/watch?v=up1", Title: "Upload", EventTime: notifNow})
	calls = discord.allCalls()
	if len(calls) != 2 || calls[1].WebhookID != notifWebhookB {
		t.Fatalf("expected a second POST to webhook B, got %+v", calls)
	}
	if got := calls[1].Body["content"]; got != "<@&555555555555555555> Dokibird - New Video" {
		t.Errorf("upload content = %v", got)
	}
	if got := notifEmbed(t, calls[1])["title"]; got != "Dokibird's New Video" {
		t.Errorf("upload embed title = %v", got)
	}
}

func TestDispatchCooldownSuppressed(t *testing.T) {
	ev := notifEvent(7, "cool", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.CooldownSeconds = 600
	store := newNotifStore(ev)
	store.claims[7] = notifClaim{allowed: false, remaining: 125}
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)

	before := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSuppressed))
	d.Dispatch(notifLivePayload())

	if n := len(discord.allCalls()); n != 0 {
		t.Fatalf("a suppressed rule must not POST, got %d requests", n)
	}
	logs := store.logEntries()
	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want 1", len(logs))
	}
	e := logs[0]
	if e.Status != model.NotificationStatusSuppressed || e.EventID != 7 || e.Webhooks != 1 || e.Delivered != 0 {
		t.Errorf("log entry = %+v", e)
	}
	if e.Detail != "within the minimum gap between Going live notifications (2m5s left)" {
		t.Errorf("detail = %q", e.Detail)
	}
	if len(store.resultEntries()) != 0 {
		t.Error("a suppressed send must not touch the delivery trail")
	}
	if got := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSuppressed)); got != before+1 {
		t.Errorf("suppressed counter went %v -> %v, want +1", before, got)
	}
}

func TestDispatchPartialDelivery(t *testing.T) {
	ev := notifEvent(5, "two hooks", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA), notifWebhookURL(notifWebhookB))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	discord.reply(notifWebhookB, notifReply{status: http.StatusNotFound, body: `{"message":"Unknown Webhook","code":10015}`})
	d := newNotifDispatcher(t, store, discord, nil)

	d.Dispatch(notifLivePayload())

	if a, b := discord.callsTo(notifWebhookA), discord.callsTo(notifWebhookB); len(a) != 1 || len(b) != 1 {
		t.Fatalf("calls: A=%d B=%d, want 1 each", len(a), len(b))
	}
	logs := store.logEntries()
	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want 1", len(logs))
	}
	e := logs[0]
	if e.Status != model.NotificationStatusPartial || e.Webhooks != 2 || e.Delivered != 1 {
		t.Errorf("log entry = %+v", e)
	}
	wantDetail := "webhook " + notifWebhookB + "/••••: HTTP 404 webhook no longer exists (deleted, or the token is wrong)"
	if e.Detail != wantDetail {
		t.Errorf("detail = %q, want %q", e.Detail, wantDetail)
	}
	notifAssertNoToken(t, "log detail", e.Detail)
	results := store.resultEntries()
	if len(results) != 1 || !results[0].Delivered || results[0].ErrText != wantDetail || results[0].ID != 5 {
		t.Errorf("results = %+v", results)
	}
	notifAssertNoToken(t, "recorded error", results[0].ErrText)
}

func TestDispatchAllWebhooksFailed(t *testing.T) {
	ev := notifEvent(6, "dead hooks", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA), notifWebhookURL(notifWebhookB))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	discord.reply(notifWebhookA, notifReply{status: http.StatusNotFound, body: `{"message":"Unknown Webhook","code":10015}`})
	discord.reply(notifWebhookB, notifReply{status: http.StatusBadRequest, body: `{"message":"Invalid Form Body","code":50035}`})
	d := newNotifDispatcher(t, store, discord, nil)

	before := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusFailed))
	d.Dispatch(notifLivePayload())

	logs := store.logEntries()
	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want 1", len(logs))
	}
	e := logs[0]
	if e.Status != model.NotificationStatusFailed || e.Webhooks != 2 || e.Delivered != 0 {
		t.Errorf("log entry = %+v", e)
	}
	for _, want := range []string{
		"webhook " + notifWebhookA + "/••••: HTTP 404 webhook no longer exists",
		"webhook " + notifWebhookB + "/••••: HTTP 400 Discord rejected the message: Invalid Form Body",
	} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail %q lacks %q", e.Detail, want)
		}
	}
	if strings.Count(e.Detail, "; ") != 1 {
		t.Errorf("failures should be joined with '; ': %q", e.Detail)
	}
	notifAssertNoToken(t, "log detail", e.Detail)
	results := store.resultEntries()
	if len(results) != 1 || results[0].Delivered || results[0].ErrText != e.Detail {
		t.Errorf("results = %+v", results)
	}
	if got := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusFailed)); got != before+1 {
		t.Errorf("failed counter went %v -> %v, want +1", before, got)
	}
}

func TestDispatchEmptyRenderIsRecordedNotSent(t *testing.T) {
	ev := notifEvent(8, "blank", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.EmbedEnabled = false
	ev.Content = "   "
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)

	d.Dispatch(notifLivePayload())

	if n := len(discord.allCalls()); n != 0 {
		t.Errorf("an empty rendering must not be POSTed, got %d requests", n)
	}
	logs := store.logEntries()
	if len(logs) != 1 || logs[0].Status != model.NotificationStatusFailed || logs[0].Detail != "the template rendered to an empty message" {
		t.Errorf("logs = %+v", logs)
	}
	results := store.resultEntries()
	if len(results) != 1 || results[0].Delivered || results[0].ErrText != "the template rendered to an empty message" {
		t.Errorf("results = %+v", results)
	}
}

func TestDispatchOperatorFeed(t *testing.T) {
	ev := notifEvent(9, "audience", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	operator := notifWebhookURL(notifWebhookOperator)
	d := newNotifDispatcher(t, store, discord, func(c *Config) { c.OperatorWebhookURL = operator })
	if !d.OperatorFeedConfigured() {
		t.Fatal("operator feed should be configured")
	}

	d.Dispatch(notifLivePayload())

	op := discord.callsTo(notifWebhookOperator)
	if len(op) != 1 {
		t.Fatalf("operator feed got %d posts, want 1", len(op))
	}
	if got := notifParse(t, op[0]); len(got) != 0 {
		t.Errorf("operator feed must never ping: parse = %v", got)
	}
	if _, ok := op[0].Body["content"]; ok {
		t.Errorf("operator feed uses the default look, which has no content: %v", op[0].Body["content"])
	}
	footer := notifFooter(t, op[0])
	for _, want := range []string{"live detection", "via youtube-state-poll", "delay 30s", "watched as scheduled", "v1.2.3"} {
		if !strings.Contains(footer, want) {
			t.Errorf("operator footer %q lacks %q", footer, want)
		}
	}
	if got := notifEmbed(t, op[0])["title"]; got != "Dokibird's Stream Started" {
		t.Errorf("operator embed title = %v", got)
	}
	// The operator diagnostics stay out of the audience post, and the
	// operator post is not a logged notification.
	aud := discord.callsTo(notifWebhookA)
	if len(aud) != 1 {
		t.Fatalf("audience webhook got %d posts, want 1", len(aud))
	}
	if f := notifFooter(t, aud[0]); f != "" {
		t.Errorf("audience post carries the operator footer %q", f)
	}
	// The audience post keeps the pings its template names; the operator
	// post pings nobody.
	if got := notifIDList(notifAllowedMentions(t, aud[0]), "roles"); len(got) == 0 {
		t.Error("audience post must keep the template's role mentions")
	}
	if got := notifAllowedMentions(t, op[0]); len(notifIDList(got, "roles")) != 0 || len(notifParse(t, op[0])) != 0 {
		t.Errorf("operator post must ping nobody, got %v", got)
	}
	if logs := store.logEntries(); len(logs) != 1 || logs[0].EventID != 9 {
		t.Errorf("only the audience rule is logged, got %+v", logs)
	}

	// The feed posts even when no rule matches, and even with no rules.
	d.Dispatch(Payload{Trigger: TriggerShort, Platform: PlatformYouTube, ChannelKey: "doki", ID: "s1", URL: "https://www.youtube.com/shorts/s1", Title: "Short", EventTime: notifNow, Mechanism: "youtube-uploads-poll"})
	op = discord.callsTo(notifWebhookOperator)
	if len(op) != 2 {
		t.Fatalf("operator feed got %d posts, want 2", len(op))
	}
	footer = notifFooter(t, op[1])
	if !strings.Contains(footer, "via youtube-uploads-poll") || strings.Contains(footer, "delay") {
		t.Errorf("non-live footer = %q: wants the mechanism and no delay", footer)
	}
	if n := len(discord.callsTo(notifWebhookA)); n != 1 {
		t.Errorf("a short must not reach the live-only audience rule, got %d posts", n)
	}
}

func TestDispatchOperatorFeedDisabledWhenUnset(t *testing.T) {
	store := newNotifStore()
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)
	d.Dispatch(notifLivePayload())
	if n := len(discord.allCalls()); n != 0 {
		t.Errorf("no operator webhook: got %d requests, want none", n)
	}
}

func TestOperatorFooterVariants(t *testing.T) {
	d := newNotifDispatcher(t, newNotifStore(), newNotifDiscord(t), func(c *Config) { c.Version = "v2.0.0" })
	p := notifLivePayload()
	p.SawScheduled = false
	if got := d.operatorFooter(p); got != "live detection · via youtube-state-poll · delay 30s · found by discovery · v2.0.0" {
		t.Errorf("discovery footer = %q", got)
	}
	p.SawScheduled = true
	p.DetectedAt = p.EventTime.Add(-5 * time.Second)
	if got := d.operatorFooter(p); got != "live detection · via youtube-state-poll · detected 5s before the reported start · watched as scheduled · v2.0.0" {
		t.Errorf("early footer = %q", got)
	}
	p = notifLivePayload()
	p.Platform = PlatformTwitch
	p.Mechanism = "twitch-eventsub"
	if got := d.operatorFooter(p); got != "live detection · via twitch-eventsub · delay 30s · v2.0.0" {
		t.Errorf("twitch footer = %q", got)
	}
	p.EventTime = time.Time{}
	if got := d.operatorFooter(p); got != "live detection · via twitch-eventsub · v2.0.0" {
		t.Errorf("no-start-time footer = %q", got)
	}
	p.Mechanism = ""
	if got := d.operatorFooter(p); got != "live detection · v2.0.0" {
		t.Errorf("bare footer = %q", got)
	}
	bare := New(Config{Store: newNotifStore()})
	if got := bare.operatorFooter(Payload{Trigger: TriggerUpload}); got != "live detection" {
		t.Errorf("no-version footer = %q", got)
	}
}

func TestDispatchToleratesStoreFailures(t *testing.T) {
	ev := notifEvent(10, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))

	// Listing fails: nothing is sent, nothing panics.
	store := newNotifStore(ev)
	store.listErr = errors.New("db locked")
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)
	d.Dispatch(notifLivePayload())
	if n := len(discord.allCalls()); n != 0 {
		t.Errorf("list error: %d requests, want none", n)
	}

	// Claiming fails: the rule is skipped, not sent unclaimed.
	store = newNotifStore(ev)
	store.claimErr = errors.New("db locked")
	d = newNotifDispatcher(t, store, discord, nil)
	d.Dispatch(notifLivePayload())
	if n := len(discord.allCalls()); n != 0 {
		t.Errorf("claim error: %d requests, want none", n)
	}
	if n := len(store.logEntries()); n != 0 {
		t.Errorf("claim error: %d log entries, want none", n)
	}

	// The log insert fails: the send still happened and OnLogged is not fired.
	store = newNotifStore(ev)
	store.logErr = errors.New("disk full")
	fired := false
	d = newNotifDispatcher(t, store, discord, func(c *Config) { c.OnLogged = func(string) { fired = true } })
	d.Dispatch(notifLivePayload())
	if n := len(discord.allCalls()); n != 1 {
		t.Errorf("log error: %d requests, want 1", n)
	}
	if fired {
		t.Error("OnLogged must not fire when the log row was not written")
	}

	// A panicking store is recovered.
	store = newNotifStore(ev)
	store.listPanic = true
	d = newNotifDispatcher(t, store, discord, nil)
	d.Dispatch(notifLivePayload()) // must not propagate
}

// When no background task can be started (the process is shutting down) the
// delivery runs inline on the caller's goroutine rather than being dropped:
// the caller has already taken the durable ledger claim, so a dropped
// announcement would never be retried.
func TestDispatchRunsInlineWhenBackgroundRefuses(t *testing.T) {
	ev := notifEvent(11, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, func(c *Config) {
		c.Background = func(func()) bool { return false }
	})
	d.Dispatch(notifLivePayload())
	// No waiting: with Background refusing, everything ran before Dispatch returned.
	if n := len(discord.allCalls()); n != 1 {
		t.Errorf("shutting down: %d requests, want 1 delivered inline", n)
	}
	if logs := store.logEntries(); len(logs) != 1 || logs[0].Status != model.NotificationStatusSent {
		t.Errorf("shutting down: log entries = %+v, want one sent entry", logs)
	}
}

// Once the shutdown context is cancelled, an in-flight delivery is cut short
// after the grace period instead of holding shutdown for a full rule timeout.
func TestDispatchShutdownCancelsAfterGrace(t *testing.T) {
	ev := notifEvent(13, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	shutdown, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	d := newNotifDispatcher(t, store, discord, func(c *Config) {
		c.Shutdown = shutdown
	})

	ctx, cancel := d.deliveryContext(time.Hour)
	defer cancel()
	cancelShutdown()
	select {
	case <-ctx.Done():
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatal("delivery context was not cancelled after the shutdown grace period")
	}
}

func TestDispatchRunsInBackgroundByDefault(t *testing.T) {
	ev := notifEvent(12, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, func(c *Config) { c.Background = nil })
	d.Dispatch(notifLivePayload())
	logs := notifWaitForLogs(t, store, 1)
	if logs[0].Status != model.NotificationStatusSent {
		t.Errorf("log = %+v", logs[0])
	}
	if n := len(discord.callsTo(notifWebhookA)); n != 1 {
		t.Errorf("got %d posts, want 1", n)
	}
}

func TestSetHTTPClientRedirectsPosts(t *testing.T) {
	ev := notifEvent(13, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	first := newNotifDiscord(t)
	second := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, first, nil)
	d.SetHTTPClient(second.client())
	notifSleepRecorder(d.sender)
	d.Dispatch(notifLivePayload())
	if n := len(first.allCalls()); n != 0 {
		t.Errorf("old client still used: %d requests", n)
	}
	if n := len(second.allCalls()); n != 1 {
		t.Errorf("new client: %d requests, want 1", n)
	}
}

func TestSendTestSuppressesMentionsAndLogsTest(t *testing.T) {
	store := newNotifStore()
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)
	// An unsaved draft (ID 0) with a webhook that is not in the rule's list.
	ev := notifEvent(0, "draft", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.Content = "<@&555555555555555555> @everyone {channel} {headline}"
	p := SamplePayload(TriggerLive, notifChannel(), nil, notifNow)

	sentBefore := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSent))
	res := d.SendTest(context.Background(), ev, p, model.Webhook{URL: notifWebhookURL(notifWebhookB)})

	if !res.OK || res.Error != "" || res.Delivered != 1 {
		t.Errorf("TestResult = %+v", res)
	}
	if res.Webhook != "webhook "+notifWebhookB+"/••••" {
		t.Errorf("result webhook = %q, want masked", res.Webhook)
	}
	notifAssertNoToken(t, "TestResult", fmt.Sprintf("%+v", res))

	calls := discord.allCalls()
	if len(calls) != 1 || calls[0].WebhookID != notifWebhookB {
		t.Fatalf("expected one POST to webhook B, got %+v", calls)
	}
	if got := notifParse(t, calls[0]); len(got) != 0 {
		t.Errorf("test send must suppress every mention: parse = %v", got)
	}
	if got, _ := calls[0].Body["content"].(string); got != "<@&555555555555555555> @everyone Dokibird Stream Started" {
		t.Errorf("content = %q", got)
	}

	logs := store.logEntries()
	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want 1", len(logs))
	}
	e := logs[0]
	if e.Status != model.NotificationStatusTest || e.EventID != 0 || e.EventName != "draft" || e.Trigger != "live" ||
		e.Webhooks != 1 || e.Delivered != 1 || e.SentAt != notifNow.Unix() || e.ChannelKey != "doki" || e.BroadcastID != SampleVideoID {
		t.Errorf("log entry = %+v", e)
	}
	if e.Detail != "test sent to webhook "+notifWebhookB+"/•••• with pings suppressed" {
		t.Errorf("detail = %q", e.Detail)
	}
	notifAssertNoToken(t, "log detail", e.Detail)
	if len(store.resultEntries()) != 0 || len(store.claimedIDs()) != 0 {
		t.Error("a test send must not claim the cooldown or touch the delivery trail")
	}
	if got := testutil.ToFloat64(metrics.Announcements.WithLabelValues("doki", "live", model.NotificationStatusSent)); got != sentBefore {
		t.Errorf("test sends must not count as announcements, counter went %v -> %v", sentBefore, got)
	}
}

func TestSendTestFailures(t *testing.T) {
	store := newNotifStore()
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)
	ev := notifEvent(3, "saved", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	p := SamplePayload(TriggerLive, notifChannel(), nil, notifNow)

	// Deleted webhook.
	discord.reply(notifWebhookA, notifReply{status: http.StatusNotFound, body: `{"message":"Unknown Webhook","code":10015}`})
	res := d.SendTest(context.Background(), ev, p, model.Webhook{URL: notifWebhookURL(notifWebhookA)})
	if res.OK || res.Delivered != 0 || !strings.Contains(res.Error, "no longer exists") || !strings.Contains(res.Error, "HTTP 404") {
		t.Errorf("TestResult = %+v", res)
	}
	notifAssertNoToken(t, "TestResult.Error", res.Error)

	// Not a Discord URL: refused before any request.
	res = d.SendTest(context.Background(), ev, p, model.Webhook{URL: "https://evil.example/api/webhooks/123456789012345678/" + notifToken("evil")})
	if res.OK || res.Error != "https://evil.example/…: not a Discord webhook URL" || res.Webhook != "https://evil.example/…" {
		t.Errorf("TestResult = %+v", res)
	}

	// Empty rendering: refused before any request.
	blank := ev
	blank.EmbedEnabled = false
	blank.Content = ""
	res = d.SendTest(context.Background(), blank, p, model.Webhook{URL: notifWebhookURL(notifWebhookB)})
	if res.OK || res.Error != "the template rendered to an empty message" {
		t.Errorf("TestResult = %+v", res)
	}

	if n := len(discord.allCalls()); n != 1 {
		t.Errorf("got %d requests, want only the 404 attempt", n)
	}
	logs := store.logEntries()
	if len(logs) != 3 {
		t.Fatalf("got %d log entries, want 3", len(logs))
	}
	for i, e := range logs {
		if e.Status != model.NotificationStatusTest || e.Delivered != 0 || e.Webhooks != 1 || e.EventID != 3 {
			t.Errorf("log[%d] = %+v", i, e)
		}
		if !strings.HasPrefix(e.Detail, "test failed: ") {
			t.Errorf("log[%d] detail = %q", i, e.Detail)
		}
		notifAssertNoToken(t, fmt.Sprintf("log[%d] detail", i), e.Detail)
	}
	if !strings.Contains(logs[0].Detail, "no longer exists") || !strings.Contains(logs[1].Detail, "not a Discord webhook URL") || !strings.Contains(logs[2].Detail, "empty message") {
		t.Errorf("details = %q / %q / %q", logs[0].Detail, logs[1].Detail, logs[2].Detail)
	}
}

// ---------------------------------------------------------------------------
// SamplePayload
// ---------------------------------------------------------------------------

func TestSamplePayload(t *testing.T) {
	ch := notifChannel()
	live := SamplePayload(TriggerLive, ch, nil, notifNow)
	if live.Trigger != TriggerLive || live.Platform != PlatformYouTube || live.ChannelKey != "doki" || live.ID != SampleVideoID ||
		live.URL != "https://www.youtube.com/watch?v="+SampleVideoID || !live.EventTime.Equal(notifNow) || !live.DetectedAt.Equal(notifNow) || live.Mechanism == "" {
		t.Errorf("live sample = %+v", live)
	}
	sched := SamplePayload(TriggerScheduled, ch, nil, notifNow)
	if !sched.EventTime.Equal(notifNow.Add(2*time.Hour)) || sched.Title != SampleVideoTitle {
		t.Errorf("scheduled sample = %+v", sched)
	}
	if up := SamplePayload(TriggerUpload, ch, nil, notifNow); up.Title != SampleVideoTitle || up.Trigger != TriggerUpload {
		t.Errorf("upload sample = %+v", up)
	}
	short := SamplePayload(TriggerShort, ch, nil, notifNow)
	if short.Title != SampleVideoTitle || short.URL != "https://www.youtube.com/shorts/"+SampleVideoID {
		t.Errorf("short sample = %+v", short)
	}
	// Every sample renders a full default embed with a thumbnail.
	for _, tr := range []Trigger{TriggerLive, TriggerScheduled, TriggerUpload, TriggerShort} {
		msg := Render(DefaultEvent(), RenderContext{Payload: SamplePayload(tr, ch, nil, notifNow), Channel: ch, TranscriptURL: "https://lt.example/doki/"})
		if msg.IsEmpty() || msg.Embed["image"] == nil || msg.Embed["timestamp"] == nil {
			t.Errorf("%s sample renders incompletely: %+v", tr, msg.Embed)
		}
	}

	// A recent YouTube detection lends its id, url, title and mechanism.
	recent := &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real1", URL: "https://www.youtube.com/watch?v=real1", Title: "Real title", Mechanism: "youtube-callback"}
	got := SamplePayload(TriggerUpload, ch, recent, notifNow)
	if got.ID != "real1" || got.URL != recent.URL || got.Title != "Real title" || got.Mechanism != "youtube-callback" || got.Platform != PlatformYouTube || got.Trigger != TriggerUpload {
		t.Errorf("sample from recent = %+v", got)
	}
	// A recent detection with no title keeps the stand-in title.
	got = SamplePayload(TriggerLive, ch, &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real2", URL: "u"}, notifNow)
	if got.ID != "real2" || got.Title == "" || got.Mechanism != "youtube-state-poll" {
		t.Errorf("sample from untitled recent = %+v", got)
	}
	// An empty id is ignored.
	got = SamplePayload(TriggerLive, ch, &model.DetectedBroadcast{Platform: PlatformYouTube, Title: "ignored"}, notifNow)
	if got.ID != SampleVideoID || got.Title == "ignored" {
		t.Errorf("sample from id-less recent = %+v", got)
	}

	// A Twitch detection illustrates a live trigger, but never a YouTube-only one.
	tw := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "42", URL: "https://twitch.tv/dokibird", Title: "Twitch title", Mechanism: "twitch-eventsub"}
	got = SamplePayload(TriggerLive, ch, tw, notifNow)
	if got.Platform != PlatformTwitch || got.ID != "42" || got.URL != tw.URL || got.Title != "Twitch title" || got.Mechanism != "twitch-eventsub" {
		t.Errorf("twitch live sample = %+v", got)
	}
	if !strings.Contains(got.ThumbnailURL(ch), "live_user_dokibird") {
		t.Errorf("twitch live sample thumbnail = %q", got.ThumbnailURL(ch))
	}
	for _, tr := range []Trigger{TriggerScheduled, TriggerUpload, TriggerShort} {
		got = SamplePayload(tr, ch, tw, notifNow)
		if got.Platform != PlatformYouTube || got.ID != SampleVideoID || got.Title == "Twitch title" || !strings.Contains(got.URL, "youtube.com") {
			t.Errorf("%s sample from a Twitch detection must keep the YouTube stand-in, got %+v", tr, got)
		}
	}
}

// The mention policy is derived from the TEMPLATE, never the rendered text:
// a title that happens to contain @everyone or a role mention is shown to the
// audience but notifies nobody, while the roles the admin wrote in do ping.
func TestMentionPolicyComesFromTemplateNotFromTitle(t *testing.T) {
	policy := MentionPolicy("<@&111> <@&222> <@!333> <@444> hi <@&111>")
	if got := notifIDListFromPolicy(policy, "roles"); !slices.Equal(got, []string{"111", "222"}) {
		t.Errorf("roles = %v, want [111 222] deduped in order", got)
	}
	if got := notifIDListFromPolicy(policy, "users"); !slices.Equal(got, []string{"333", "444"}) {
		t.Errorf("users = %v, want [333 444]", got)
	}
	if parse, _ := policy["parse"].([]string); len(parse) != 0 {
		t.Errorf("parse = %v, want [] without @everyone in the template", parse)
	}
	if parse, _ := MentionPolicy("@here folks")["parse"].([]string); !slices.Equal(parse, []string{"everyone"}) {
		t.Errorf("parse = %v, want [everyone] when the template says @here", parse)
	}
	if got := MentionPolicy("plain text"); len(got) != 1 || len(got["parse"].([]string)) != 0 {
		t.Errorf("a template without mentions must produce parse:[] only, got %v", got)
	}

	// End to end: the payload's title carries mention syntax; the rule's
	// template names one role. Only that role is allowed.
	ev := notifEvent(1, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.Content = "<@&555555555555555555> {title}"
	p := notifLivePayload()
	p.Title = "@everyone <@&999999999999999999> come watch"
	msg := Render(ev, RenderContext{Payload: p, Channel: notifChannel()})
	if !strings.Contains(msg.Content, "@everyone <@&999999999999999999>") {
		t.Fatalf("content = %q, the title must still be rendered verbatim", msg.Content)
	}
	if got := notifIDListFromPolicy(msg.Mentions, "roles"); !slices.Equal(got, []string{"555555555555555555"}) {
		t.Errorf("roles = %v, want only the template's role", got)
	}
	if parse, _ := msg.Mentions["parse"].([]string); len(parse) != 0 {
		t.Errorf("parse = %v, want [] - the @everyone came from the title", parse)
	}
}

// notifIDListFromPolicy reads a []string id list from an in-memory policy.
func notifIDListFromPolicy(policy map[string]any, key string) []string {
	list, _ := policy[key].([]string)
	return list
}

// A title that arrives empty renders as "(untitled)" rather than leaving the
// default "**{title}**" line as four bare asterisks.
func TestRenderEmptyTitleFallsBack(t *testing.T) {
	ev := DefaultEvent()
	p := notifLivePayload()
	p.Title = "   "
	msg := Render(ev, RenderContext{Payload: p, Channel: notifChannel()})
	desc, _ := msg.Embed["description"].(string)
	if !strings.Contains(desc, "**(untitled)**") {
		t.Errorf("description = %q, want the (untitled) fallback", desc)
	}
}

// A forum-channel target carries ?thread_id=; it is accepted, masked like any
// other webhook, and the thread survives alongside wait=true on the wire.
func TestSenderKeepsThreadIDOnForumWebhooks(t *testing.T) {
	withThread := notifWebhookURL(notifWebhookA) + "?thread_id=123456789012345678"
	if !ValidWebhookURL(withThread) {
		t.Fatal("a thread_id query must be accepted")
	}
	for _, bad := range []string{
		notifWebhookURL(notifWebhookA) + "?thread_id=abc",
		notifWebhookURL(notifWebhookA) + "?wait=true",
		notifWebhookURL(notifWebhookA) + "?thread_id=123456789012345678&x=1",
	} {
		if ValidWebhookURL(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if got := MaskWebhookURL(withThread); strings.Contains(got, notifToken(notifWebhookA)) || !strings.Contains(got, notifWebhookA) {
		t.Errorf("mask = %q", got)
	}

	s, discord, _ := notifSender(t)
	if err := s.Send(context.Background(), withThread, notifMessage(), false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	c := discord.allCalls()[0]
	if c.Query.Get("thread_id") != "123456789012345678" || c.Query.Get("wait") != "true" {
		t.Errorf("query = %v, want both thread_id and wait=true", c.Query)
	}
}
