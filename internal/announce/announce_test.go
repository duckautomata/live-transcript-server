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
	"strconv"
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
		Description:  "Come hang out while we play something new",
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
	// Twitch serves a "404" placeholder for an offline channel, so an ended
	// broadcast offers no image; a YouTube thumbnail outlives the stream.
	ended := tw
	ended.Ended = true
	if got := ended.ThumbnailURL(ch); got != "" {
		t.Errorf("ended twitch thumbnail = %q, want none", got)
	}
	if got := (Payload{Platform: PlatformYouTube, ID: "abc", Ended: true}).ThumbnailURL(ch); got != "https://i.ytimg.com/vi/abc/maxresdefault.jpg" {
		t.Errorf("ended youtube thumbnail = %q, want it kept", got)
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
		"{channel}":     "Dokibird",
		"{title}":       "Big stream <today>",
		"{description}": "Come hang out while we play something new",
		"{game}":        "", // the payload is a YouTube one; see TestGameIsTheTwitchCategory
		"{url}":         "https://www.youtube.com/watch?v=vid123",
		"{platform}":    "YouTube",
		"{headline}":    "Stream Started",
		"{time}":        fmt.Sprintf("<t:%d:R>", unix),
		"{timeShort}":   fmt.Sprintf("<t:%d:t>", unix),
		"{timeFull}":    fmt.Sprintf("<t:%d:F>", unix),
		"{transcript}":  "https://lt.example/doki/",
		"{thumbnail}":   "https://i.ytimg.com/vi/vid123/maxresdefault.jpg",
		"{trigger}":     "live",
		"{id}":          "vid123",
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
	ev.Content = "at [{time}][{timeFull}]"
	ev.Embed.Timestamp = true
	msg := Render(ev, rc)
	if msg.Content != "at [][]" {
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

	// Embed disabled: content alone is fine, but the embed fields are still
	// bounded - what is stored must never exceed what could be sent.
	ev = notifValidEvent()
	ev.EmbedEnabled = false
	ev.Content = "just text"
	if err := Normalize(&ev); err != nil {
		t.Errorf("a disabled embed with sane fields must pass: %v", err)
	}
	ev.Embed.Title = strings.Repeat("t", MaxEmbedTitle+100)
	ev.Embed.Color = "blue"
	verr := notifValidationError(t, Normalize(&ev))
	notifAssertProblem(t, verr, fmt.Sprintf("embed title must be at most %d characters", MaxEmbedTitle))
	notifAssertProblem(t, verr, "embed color must look like #RRGGBB")

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
	p := SamplePayload(TriggerLive, notifChannel(), notifNow)

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
	p := SamplePayload(TriggerLive, notifChannel(), notifNow)

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
	live := SamplePayload(TriggerLive, ch, notifNow)
	if live.Trigger != TriggerLive || live.Platform != PlatformYouTube || live.ChannelKey != "doki" || live.ID != SampleVideoID ||
		live.URL != "https://www.youtube.com/watch?v="+SampleVideoID || !live.EventTime.Equal(notifNow) || !live.DetectedAt.Equal(notifNow) || live.Mechanism == "" {
		t.Errorf("live sample = %+v", live)
	}
	if !live.IsSample() {
		t.Error("the stand-in must identify itself as such")
	}
	sched := SamplePayload(TriggerScheduled, ch, notifNow)
	if !sched.EventTime.Equal(notifNow.Add(2*time.Hour)) || sched.Title != SampleVideoTitle {
		t.Errorf("scheduled sample = %+v", sched)
	}
	if up := SamplePayload(TriggerUpload, ch, notifNow); up.Title != SampleVideoTitle || up.Trigger != TriggerUpload {
		t.Errorf("upload sample = %+v", up)
	}
	short := SamplePayload(TriggerShort, ch, notifNow)
	if short.Title != SampleVideoTitle || short.URL != "https://www.youtube.com/shorts/"+SampleVideoID {
		t.Errorf("short sample = %+v", short)
	}
	// Every sample renders a full default embed with a thumbnail.
	for _, tr := range []Trigger{TriggerLive, TriggerScheduled, TriggerUpload, TriggerShort} {
		msg := Render(DefaultEvent(), RenderContext{Payload: SamplePayload(tr, ch, notifNow), Channel: ch, TranscriptURL: "https://lt.example/doki/"})
		if msg.IsEmpty() || msg.Embed["image"] == nil || msg.Embed["timestamp"] == nil {
			t.Errorf("%s sample renders incompletely: %+v", tr, msg.Embed)
		}
	}
}

// A preview is built from the channel's own detections and nothing else: a
// detail the detection lacks stays blank rather than being borrowed from the
// stand-in video, and a channel with nothing yet previews blank.
func TestPreviewPayloadNeverInvents(t *testing.T) {
	ch := notifChannel()

	t.Run("nothing detected", func(t *testing.T) {
		for _, tr := range []Trigger{TriggerLive, TriggerScheduled, TriggerUpload, TriggerShort} {
			got := PreviewPayload(tr, ch, nil, nil, notifNow)
			if got.Trigger != tr || got.ChannelKey != "doki" || got.ID != "" || got.URL != "" || got.Title != "" || !got.EventTime.IsZero() || got.IsSample() {
				t.Errorf("%s preview with nothing detected = %+v, want only trigger and channel", tr, got)
			}
			if tr != TriggerLive && got.Platform != PlatformYouTube {
				t.Errorf("%s is YouTube-only, got platform %q", tr, got.Platform)
			}
			if tr == TriggerLive && got.Platform != "" {
				t.Errorf("live preview with nothing detected has platform %q, want blank", got.Platform)
			}
		}
		// Id-less rows count as nothing.
		got := PreviewPayload(TriggerLive, ch, &model.DetectedBroadcast{Platform: PlatformYouTube, Title: "ignored"}, nil, notifNow)
		if got.ID != "" || got.Title != "" {
			t.Errorf("preview from an id-less detection = %+v", got)
		}
		got = PreviewPayload(TriggerUpload, ch, nil, &model.DetectedVideo{Title: "ignored"}, notifNow)
		if got.ID != "" || got.Title != "" {
			t.Errorf("preview from an id-less video = %+v", got)
		}
	})

	t.Run("live from the latest broadcast, blanks kept blank", func(t *testing.T) {
		// A Twitch broadcast EventSub claimed without a title, since ended.
		tw := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "42", URL: "https://twitch.tv/dokibird", Mechanism: "twitch-eventsub", StartedAt: notifNow.Add(-time.Hour).Unix(), EndedAt: notifNow.Unix()}
		got := PreviewPayload(TriggerLive, ch, tw, nil, notifNow)
		if got.Platform != PlatformTwitch || got.ID != "42" || got.URL != tw.URL || got.Mechanism != "twitch-eventsub" || !got.EventTime.Equal(time.Unix(tw.StartedAt, 0)) {
			t.Errorf("twitch live preview = %+v", got)
		}
		if got.Title != "" {
			t.Errorf("title = %q, want blank: the detection had none", got.Title)
		}
		if !got.Ended || got.ThumbnailURL(ch) != "" {
			t.Errorf("an ended Twitch broadcast has no preview image, got ended=%v thumbnail=%q", got.Ended, got.ThumbnailURL(ch))
		}
		msg := Render(DefaultEvent(), RenderContext{Payload: got, Channel: ch, TranscriptURL: "https://lt.example/doki/"})
		if msg.Embed["image"] != nil {
			t.Errorf("rendered embed carries an image for an ended Twitch stream: %v", msg.Embed["image"])
		}
		if desc, _ := msg.Embed["description"].(string); strings.Contains(desc, "*") || strings.Contains(desc, SampleVideoTitle) {
			t.Errorf("description = %q, want no title line at all", desc)
		}

		// Still live: the preview image is offered, cache-busted by now.
		tw.EndedAt = 0
		got = PreviewPayload(TriggerLive, ch, tw, nil, notifNow)
		if got.Ended || !strings.Contains(got.ThumbnailURL(ch), "live_user_dokibird-1280x720.jpg?t="+strconv.FormatInt(notifNow.Unix(), 10)) {
			t.Errorf("live Twitch preview thumbnail = %q (ended=%v)", got.ThumbnailURL(ch), got.Ended)
		}

		// No start time reported: the time stays unknown rather than "now".
		yt := &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real1", URL: "https://www.youtube.com/watch?v=real1", Title: "Real title", Mechanism: "youtube-callback"}
		got = PreviewPayload(TriggerLive, ch, yt, nil, notifNow)
		if got.ID != "real1" || got.Title != "Real title" || !got.EventTime.IsZero() || got.ThumbnailURL(ch) != "https://i.ytimg.com/vi/real1/maxresdefault.jpg" {
			t.Errorf("youtube live preview = %+v", got)
		}
	})

	t.Run("other triggers from the latest video of that kind", func(t *testing.T) {
		v := &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "short1", Kind: "short", URL: "https://www.youtube.com/shorts/short1", Title: "A short", PublishedAt: notifNow.Add(-time.Hour).Unix(), ScheduledAt: 0}
		got := PreviewPayload(TriggerShort, ch, nil, v, notifNow)
		if got.Platform != PlatformYouTube || got.ID != "short1" || got.URL != v.URL || got.Title != "A short" || !got.EventTime.Equal(time.Unix(v.PublishedAt, 0)) {
			t.Errorf("short preview = %+v", got)
		}
		s := &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "sched1", Kind: "scheduled", URL: "https://www.youtube.com/watch?v=sched1", Title: "Soon", PublishedAt: notifNow.Unix(), ScheduledAt: notifNow.Add(3 * time.Hour).Unix()}
		got = PreviewPayload(TriggerScheduled, ch, nil, s, notifNow)
		if !got.EventTime.Equal(time.Unix(s.ScheduledAt, 0)) {
			t.Errorf("scheduled preview time = %v, want the scheduled start", got.EventTime)
		}
		// A video with no publish time keeps the time blank.
		got = PreviewPayload(TriggerUpload, ch, nil, &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "u1", Kind: "upload", URL: "x"}, notifNow)
		if !got.EventTime.IsZero() || got.Title != "" {
			t.Errorf("upload preview without a publish time = %+v", got)
		}

		// With no video of that kind, the latest YouTube broadcast stands in
		// (it is a video too), but a Twitch one never does.
		yt := &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real1", URL: "https://www.youtube.com/watch?v=real1", Title: "Real title", Mechanism: "youtube-callback", StartedAt: notifNow.Unix()}
		got = PreviewPayload(TriggerUpload, ch, yt, nil, notifNow)
		if got.Platform != PlatformYouTube || got.ID != "real1" || got.Title != "Real title" || !got.EventTime.IsZero() {
			t.Errorf("upload preview from a broadcast = %+v", got)
		}
		tw := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "42", URL: "https://twitch.tv/dokibird", Title: "Twitch title"}
		for _, tr := range []Trigger{TriggerScheduled, TriggerUpload, TriggerShort} {
			got = PreviewPayload(tr, ch, tw, nil, notifNow)
			if got.Platform != PlatformYouTube || got.ID != "" || got.Title != "" || got.URL != "" {
				t.Errorf("%s preview from a Twitch broadcast = %+v, want blank", tr, got)
			}
		}
		// The video of the right kind wins over the broadcast.
		got = PreviewPayload(TriggerUpload, ch, yt, &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "u2", Kind: "upload", URL: "y", Title: "The upload"}, notifNow)
		if got.ID != "u2" || got.Title != "The upload" {
			t.Errorf("upload preview should prefer the upload, got %+v", got)
		}
	})
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

// A title that arrives empty renders as nothing: not "(untitled)", not the
// default "**{title}**" line as four bare asterisks. The emphasis around the
// empty placeholder goes with it.
func TestRenderEmptyTitleRendersBlank(t *testing.T) {
	ev := DefaultEvent()
	p := notifLivePayload()
	p.Title = "   "
	msg := Render(ev, RenderContext{Payload: p, Channel: notifChannel(), TranscriptURL: "https://lt.example/doki/"})
	desc, _ := msg.Embed["description"].(string)
	if strings.Contains(desc, "untitled") || strings.Contains(desc, "*") {
		t.Errorf("description = %q, want the title line gone", desc)
	}
	if !strings.HasPrefix(desc, "[Open on YouTube]") {
		t.Errorf("description = %q, want it to start at the links", desc)
	}
}

// An empty placeholder takes matching emphasis with it; everything else
// around a placeholder is preserved exactly, empty or not.
func TestExpandCollapsesEmphasisAroundEmptyValues(t *testing.T) {
	values := map[string]string{"{title}": "", "{channel}": "Dokibird", "{time}": ""}
	cases := map[string]string{
		"**{title}**":                 "",
		"***{title}***":               "",
		"*{title}*":                   "",
		"__{title}__":                 "",
		"_{title}_":                   "",
		"~~{title}~~":                 "",
		"`{title}`":                   "",
		"**{title}**\n\n[Open]":       "\n\n[Open]",
		"**{channel}**":               "**Dokibird**",
		"**{channel}'s {title}**":     "**Dokibird's **",
		"**{title}*":                  "***",
		"** {title} **":               "**  **",
		"{channel}_{title}":           "Dokibird_",
		"**{nope}**":                  "**{nope}**",
		"**{title}** **{channel}**":   " **Dokibird**",
		"live {time} **{title}** now": "live   now",
	}
	for in, want := range cases {
		if got := Expand(in, values); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
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

// ---------------------------------------------------------------------------
// {description}: cleaning, line counts, line removal, the elastic fit
// ---------------------------------------------------------------------------

// {timeShort} is the same moment as {time}, as a clock time, and like the
// other time placeholders it renders empty when the platform reported none.
func TestTimeShortPlaceholder(t *testing.T) {
	rc := notifRenderContext()
	want := fmt.Sprintf("<t:%d:t>", rc.Payload.EventTime.Unix())
	if got := Render(model.NotificationEvent{Content: "at {timeShort}"}, rc).Content; got != "at "+want {
		t.Errorf("content = %q, want %q", got, "at "+want)
	}
	rc.Payload.EventTime = time.Time{}
	if got := Render(model.NotificationEvent{Content: "at [{timeShort}]"}, rc).Content; got != "at []" {
		t.Errorf("content with zero EventTime = %q, want the placeholder empty", got)
	}
}

// A description is the creator's text, not the author's, and arrives in every
// shape. It is normalised once so that lines can be counted, "the first line"
// is never an invisible one, and none of its markdown can reach past it into
// the author's own message.
func TestCleanDescription(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"only whitespace", "   \n\t\n", ""},
		{"CRLF is one break", "a\r\nb", "a\nb"},
		{"lone CR", "a\rb", "a\nb"},
		{"unicode line and paragraph separators", "a\u2028b\u2029c", "a\nb\nc"},
		{"tab becomes a space", "a\tb", "a b"},
		{"NUL and other control characters go", "a\x00b\x07c\x1bd", "abcd"},
		{"trailing space trimmed", "a   \nb \t", "a\nb"},
		{"indentation kept", "intro\n   indented", "intro\n   indented"},
		{"a zero-width-space line is blank", "a\n\u200B\nb", "a\n\nb"},
		{"a hangul-filler first line is not the first line", "\u3164\nfirst", "first"},
		{"every invisible at once", "\u200B\u200C\u200D\u2060\uFEFF\u2800\u3164 \nreal", "real"},
		{"leading and trailing blanks dropped", "\n\n a\n\n", " a"},
		{"a run of blanks becomes one", "a\n\n\n\nb", "a\n\nb"},
		{"a run of mixed blanks becomes one", "a\n \n\u200B\n\nb", "a\n\nb"},
		{"block quote guarded", ">>> the rest", "\u200B>>> the rest"},
		{"indented block quote guarded after the indent", "  >>>x", "  \u200B>>>x"},
		{"heading guarded", "# Big", "\u200B# Big"},
		{"second-level heading guarded", "## Big", "\u200B## Big"},
		{"third-level heading guarded", "### Big", "\u200B### Big"},
		{"subtext guarded", "-# small", "\u200B-# small"},
		{"four hashes is not a heading", "#### nope", "#### nope"},
		{"a hashtag is left alone", "#hashtag #another", "#hashtag #another"},
		{"a one-line quote is harmless", "> quote", "> quote"},
		{"two angle brackets are not a block quote", ">> x", ">> x"},
		{"a dash-hash without the space is nothing", "-#x", "-#x"},
		{"a guarded line mid-way", "intro\n# Links\nhttps://example.test", "intro\n\u200B# Links\nhttps://example.test"},
	}
	for _, c := range cases {
		if got := cleanDescription(c.in); got != c.want {
			t.Errorf("%s: cleanDescription(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		// Cleaning what is already clean changes nothing.
		if again := cleanDescription(c.want); again != c.want {
			t.Errorf("%s: cleaning twice = %q, want %q", c.name, again, c.want)
		}
	}

	// And it is what {description} expands to.
	rc := notifRenderContext()
	rc.Payload.Description = "\r\n  pitch  \r\n\r\n\r\n>>> quoted\r\n"
	if got := rc.values()["{description}"]; got != "  pitch\n\n\u200B>>> quoted" {
		t.Errorf("{description} = %q, want the cleaned text", got)
	}
}

// The number is how many lines of TEXT: a blank line between two kept lines is
// kept but never counted, never left dangling at the end, and a cut between
// lines gets no ellipsis because nothing was cut mid-way.
func TestFirstLines(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"A\n\nB\nC", 1, "A"},
		{"A\n\nB\nC", 2, "A\n\nB"},
		{"A\n\nB\nC", 3, "A\n\nB\nC"},
		{"A\n\nB\nC", 9, "A\n\nB\nC"},
		{"A\nB\n\nC", 2, "A\nB"},
		{"only", 1, "only"},
		{"only", 99, "only"},
		{"", 3, ""},
	}
	for _, c := range cases {
		if got := firstLines(c.in, c.n); got != c.want {
			t.Errorf("firstLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// "First line" of a description written as one enormous paragraph must be a
// teaser, not a wall: a line over the cap is cut at a word, and nothing
// follows the ellipsis. Bare {description} has no such cap.
func TestFirstLinesCapsALongLine(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 100)) // 499 runes
	got := firstLines(long+"\nnext", 2)
	if n := utf8.RuneCountInString(got); n > MaxDescriptionLineRunes || n < MaxDescriptionLineRunes-5 {
		t.Errorf("capped line is %d runes, want just under %d", n, MaxDescriptionLineRunes)
	}
	if !strings.HasSuffix(got, "word…") || strings.Contains(got, "next") || strings.Contains(got, "\n") {
		t.Errorf("firstLines = %q, want it to end at the cut, on a whole word", got)
	}

	// The cap stops the output wherever the long line falls.
	got = firstLines("short\n"+long+"\nthird", 3)
	if !strings.HasPrefix(got, "short\nword ") || !strings.HasSuffix(got, "…") || strings.Contains(got, "third") {
		t.Errorf("firstLines = %q, want the short line, the cut line, and nothing after it", got)
	}

	// A long line that is one enormous link cuts down to nothing, and a bare
	// ellipsis is not a line: the output ends on the line before it.
	hugeLink := "https://example.com/" + strings.Repeat("a", 400)
	if got := firstLines("short\n"+hugeLink+"\nthird", 3); got != "short" {
		t.Errorf("firstLines = %q, want just the short line", got)
	}
	if got := firstLines(hugeLink+"\nnext", 1); got != "" {
		t.Errorf("firstLines = %q, want nothing rather than a lone ellipsis", got)
	}

	// A line exactly at the cap is left alone, and the count goes on.
	exact := strings.Repeat("x", MaxDescriptionLineRunes)
	if got := firstLines(exact+"\nnext", 2); got != exact+"\nnext" {
		t.Errorf("a line exactly at the cap was altered: %d runes", utf8.RuneCountInString(got))
	}

	// All of it means all of it.
	if got := Expand("{description}", map[string]string{"{description}": long}); got != long {
		t.Errorf("bare {description} was shortened to %d runes", utf8.RuneCountInString(got))
	}
}

// {description:N} is the first N lines, {description} all of it. Anything
// else with a colon in it is a typo, and a typo is left exactly as written so
// that it shows in the preview.
func TestExpandLineCounts(t *testing.T) {
	values := map[string]string{
		"{description}": "L1\nL2\n\nL3\nL4\nL5\nL6\nL7\nL8\nL9",
		"{title}":       "T",
		"{url}":         "https://x.example",
	}
	cases := map[string]string{
		"{description}":        values["{description}"],
		"{description:1}":      "L1",
		"{description:3}":      "L1\nL2\n\nL3",
		"{description:07}":     "L1\nL2\n\nL3\nL4\nL5\nL6\nL7",
		"{description:99}":     values["{description}"],
		"a {description:1} b":  "a L1 b",
		"**{description:1}**":  "**L1**",
		"{description:1}{url}": "L1https://x.example",

		// Left as written.
		"{description:0}":     "{description:0}",
		"{description:00}":    "{description:00}",
		"{description:}":      "{description:}",
		"{description:abc}":   "{description:abc}",
		"{description:100}":   "{description:100}",
		"{description:-1}":    "{description:-1}",
		"{description:1.5}":   "{description:1.5}",
		"{Description:1}":     "{Description:1}",
		"{description :1}":    "{description :1}",
		"{description: 1}":    "{description: 1}",
		"{description:1:2}":   "{description:1:2}",
		"**{description:0}**": "**{description:0}**",

		// Only a placeholder that declares lines takes a count.
		"{title:2}": "{title:2}",
		"{url:1}":   "{url:1}",
		"{nope:3}":  "{nope:3}",
	}
	for in, want := range cases {
		if got := Expand(in, values); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}

	// With no description the emphasis goes with it, whichever form was used;
	// Expand itself never removes a line of the template.
	values["{description}"] = ""
	for in, want := range map[string]string{
		"**{description:1}**":         "",
		"__{description}__":           "",
		"x\n{description:1}\n\ny":     "x\n\n\ny",
		"x\n**{description:3}**\n\ny": "x\n\n\ny",
	} {
		if got := Expand(in, values); got != want {
			t.Errorf("Expand(%q) with no description = %q, want %q", in, got, want)
		}
	}
	if got := Expand("{description:1} {title}", nil); got != "{description:1} {title}" {
		t.Errorf("Expand with a nil table = %q, want the template back", got)
	}
}

// The editor builds {name:N} from the vocabulary it is served, so what is
// served and what the parser accepts must be the same thing: every placeholder
// that offers a line chooser expands in every form the chooser can write.
func TestLineAwarePlaceholdersMatchTheParser(t *testing.T) {
	count := 0
	for _, p := range Placeholders {
		if p.Lines == nil {
			continue
		}
		count++
		if p.Lines.Default < 1 || p.Lines.Default > p.Lines.Max || p.Lines.Max > 99 {
			t.Errorf("%s: lines = %+v, want 1 <= default <= max <= 99", p.Name, *p.Lines)
		}
		values := map[string]string{p.Name: "one\ntwo\nthree"}
		stem := strings.TrimSuffix(p.Name, "}")
		for _, token := range []string{
			p.Name,
			stem + ":1}",
			fmt.Sprintf("%s:%d}", stem, p.Lines.Default),
			fmt.Sprintf("%s:%d}", stem, p.Lines.Max),
		} {
			if got := Expand(token, values); got == token {
				t.Errorf("%s is offered by the editor but does not expand", token)
			}
		}
		for _, token := range []string{stem + ":0}", stem + ":100}"} {
			if got := Expand(token, values); got != token {
				t.Errorf("Expand(%q) = %q, want it left as written", token, got)
			}
		}
	}
	if count != 1 {
		t.Errorf("%d placeholders take a line count, want exactly 1 ({description})", count)
	}
}

// A line whose placeholders all came up blank, and that has no words left on
// it, is removed - so one template reads well on the platform that lacks the
// value. A line that still says something stays, and so does everything the
// author typed without a placeholder.
func TestExpandLinesRemovesBlankPlaceholderLines(t *testing.T) {
	x := expander{values: map[string]string{"{title}": "T", "{description}": "", "{channel}": "Dokibird"}, descCap: noCap}
	cases := []struct{ name, in, want string }{
		{"the twitch case", "**{title}**\n\n{description:1}\n\n[Open]", "**T**\n\n[Open]"},
		{"emoji prefix", "🎬 {description:1}\nnext", "next"},
		{"bullet prefix", "- {description:1}\nnext", "next"},
		{"quote prefix", "> {description:1}\nnext", "next"},
		{"custom emoji prefix", "<:yt:123456> {description:1}\nnext", "next"},
		{"animated emoji prefix", "<a:live:99> {description:1}\nnext", "next"},
		{"shortcode prefix", ":video_game: {description:1}\nnext", "next"},
		{"a time is not a shortcode", "10:30:45 {description:1}\nnext", "10:30:45 \nnext"},
		{"emphasis", "**{description}**\nnext", "next"},
		{"the last line", "first\n{description:3}", "first"},
		{"every line", "{description:1}\n\n{description:3}\n\nend", "end"},
		{"all of it gone", "{description}", ""},
		{"words keep the line", "About: {description:1}\nnext", "About: \nnext"},
		{"digits keep the line", "<@&123> {description:1}\nnext", "<@&123> \nnext"},
		{"a filled placeholder keeps the line", "{title} {description:1}", "T "},
		{"an unknown placeholder is not a blank one", "{nope}\nnext", "{nope}\nnext"},
		{"a typo is not a blank one", "{description:0}\nnext", "{description:0}\nnext"},
		{"a separator without placeholders stays", "---\n{title}\n***", "---\nT\n***"},
		{"author blank lines untouched", "a\n\n\nb\n", "a\n\n\nb\n"},
		{"one gap absorbed below", "top\n\n{description:1}\n\nend", "top\n\nend"},
		{"no gap to absorb: text above", "top\n{description:1}\n\nend", "top\n\nend"},
		{"no gap to absorb: text below", "top\n\n{description:1}\nend", "top\n\nend"},
		{"only one gap per removed line", "top\n\n{description:1}\n\n\nend", "top\n\n\nend"},
	}
	for _, c := range cases {
		if got := x.expandLines(c.in); got != c.want {
			t.Errorf("%s: expandLines(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// End to end, the same rule on both platforms: the description line is there
// for a YouTube video and simply is not for a Twitch stream, in the message
// and in the embed alike.
func TestRenderDropsTheDescriptionLineOnTwitch(t *testing.T) {
	ev := DefaultEvent()
	ev.Content = "{channel} is live!\n> {description:1}\n{url}"
	ev.Embed.Description = "**{title}**\n\n{description:1}\n\n[Open on {platform}]({url})"

	yt := notifRenderContext()
	yt.Payload.Description = "The pitch.\nMore."
	msg := Render(ev, yt)
	if want := "Dokibird is live!\n> The pitch.\nhttps://www.youtube.com/watch?v=vid123"; msg.Content != want {
		t.Errorf("youtube content = %q, want %q", msg.Content, want)
	}
	if want := "**Big stream <today>**\n\nThe pitch.\n\n[Open on YouTube](https://www.youtube.com/watch?v=vid123)"; msg.Embed["description"] != want {
		t.Errorf("youtube embed description = %q, want %q", msg.Embed["description"], want)
	}

	tw := notifRenderContext()
	tw.Payload.Platform = PlatformTwitch
	tw.Payload.Description = ""
	tw.Payload.URL = "https://twitch.tv/dokibird"
	msg = Render(ev, tw)
	if want := "Dokibird is live!\nhttps://twitch.tv/dokibird"; msg.Content != want {
		t.Errorf("twitch content = %q, want %q", msg.Content, want)
	}
	if want := "**Big stream <today>**\n\n[Open on Twitch](https://twitch.tv/dokibird)"; msg.Embed["description"] != want {
		t.Errorf("twitch embed description = %q, want %q", msg.Embed["description"], want)
	}
	if msg.Shortened {
		t.Error("a removed line is not a shortened description")
	}
}

// A role ping next to a title that came up blank keeps its line - it has
// digits on it - and keeps pinging: the policy is read from the template.
func TestRenderKeepsAMentionLineWhoseTitleIsBlank(t *testing.T) {
	ev := notifEvent(1, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.Content = "<@&1> {title}\n{url}"
	p := notifLivePayload()
	p.Title = ""
	msg := Render(ev, RenderContext{Payload: p, Channel: notifChannel()})
	if want := "<@&1> \nhttps://www.youtube.com/watch?v=vid123"; msg.Content != want {
		t.Errorf("content = %q, want %q", msg.Content, want)
	}
	if got := notifIDListFromPolicy(msg.Mentions, "roles"); !slices.Equal(got, []string{"1"}) {
		t.Errorf("roles = %v, want the template's role", got)
	}
}

// cutAtWord ends on a whole word where it reasonably can, and never inside a
// link: a cut URL is still clickable, and goes somewhere else.
func TestCutAtWord(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("a", 100)
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello world", 0, ""},
		{"hello world", -3, ""},
		{"hello world", 1, "…"},
		{"hello", 5, "hello"},
		{"hello world", 11, "hello world"},
		{"hello world", 10, "hello…"},
		{"hello world", 8, "hello…"},
		{"hello world", 6, "hello…"},
		{"hello world", 3, "…"},
		{"one\ntwo three", 9, "one\ntwo…"},
		// Text without spaces is one enormous word; dropping all of it would
		// keep nothing, so past 40 runes the cut falls inside it.
		{strings.Repeat("あ", 100), 50, strings.Repeat("あ", 49) + "…"},
		{"see " + strings.Repeat("あ", 30) + " end", 20, "see…"},
		// A link is never cut into, whatever its length.
		{"see " + longURL + " end", 60, "see…"},
		{"see <" + longURL + "> end", 60, "see…"},
		{"see [link](" + longURL + ") end", 80, "see…"},
		{longURL, 60, "…"},
		// Nor does the ellipsis touch one: "https://x…" is a working link to
		// somewhere else. It gets a space, or the link goes when there is no
		// room for that space.
		{"Twitter: https://twitter.com/x Merch store", 35, "Twitter: https://twitter.com/x …"},
		{"Twitter: https://twitter.com/x\nMerch store", 35, "Twitter: https://twitter.com/x …"},
		{"Twitter: https://twitter.com/x Merch", 32, "Twitter: https://twitter.com/x …"},
		{"Twitter: https://twitter.com/x Merch", 31, "Twitter:…"},
		{"Twitter: <https://twitter.com/x> Merch store", 37, "Twitter: <https://twitter.com/x> …"},
	}
	for _, c := range cases {
		if got := cutAtWord(c.in, c.max); got != c.want {
			t.Errorf("cutAtWord(%.30q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// The elastic fit binary-searches on the cap, which is only valid if a larger
// cap never produces a shorter result. Checked at every cap over a text with
// long links, a wrapped link, a long plain word and a long word that turns
// into a link part-way through - together with the promises that the result
// fits and that no link is ever left half-cut.
func TestCutAtWordIsMonotoneAndNeverCutsALink(t *testing.T) {
	urls := []string{
		"https://example.com/a/very/long/path/that/goes/on/and/on/for/more/than/forty/runes/easily?q=1",
		"https://example.org/another/really/long/wrapped/link/that/also/exceeds/forty/runes",
		"https://example.net/glued/to/a/long/prefix",
	}
	text := "Intro words here " + urls[0] + " then <" + urls[1] + "> and a " +
		strings.Repeat("verylongword", 8) + " and " + strings.Repeat("prefix", 9) + urls[2] + " end\nsecond line"
	total := utf8.RuneCountInString(text)

	prev := 0
	for max := 0; max <= total+3; max++ {
		out := cutAtWord(text, max)
		n := utf8.RuneCountInString(out)
		if n > max {
			t.Fatalf("max %d: result is %d runes", max, n)
		}
		if n < prev {
			t.Fatalf("max %d: result shrank from %d to %d runes: %q", max, prev, n, out)
		}
		prev = n

		if fusedEllipsis.MatchString(out) {
			t.Fatalf("max %d: the ellipsis is part of a link: %q", max, out)
		}
		body := strings.TrimSuffix(out, "…")
		if !strings.HasPrefix(text, body) {
			t.Fatalf("max %d: %q is not a prefix of the text", max, out)
		}
		for _, u := range urls {
			at := strings.Index(text, u)
			if len(body) > at && len(body) < at+len(u) {
				t.Fatalf("max %d: cut inside %s: %q", max, u, out)
			}
		}
	}
	if cutAtWord(text, total) != text {
		t.Error("a text that fits must come back unchanged")
	}
}

// fusedEllipsis matches a link that runs straight into an ellipsis.
var fusedEllipsis = regexp.MustCompile(`https?://\S*…`)

// notifLongDescription is a description of about n runes of ordinary words.
func notifLongDescription(n int) string {
	return strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", n/18+1))[:n]
}

// The description is the elastic part of a message; the author's text is not.
// With the usual "title, description, links" layout a 5000-character
// description must not push the links off the end: the description is what is
// shortened, as far as needed and no further.
func TestRenderShortensTheDescriptionNotTheAuthorsText(t *testing.T) {
	const links = "[Open on YouTube](https://www.youtube.com/watch?v=vid123) | [Transcript](https://lt.example/doki/)"
	ev := DefaultEvent()
	ev.Embed.Description = "**{title}**\n\n{description}\n\n[Open on {platform}]({url}) | [Transcript]({transcript})"
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(5000)

	msg := Render(ev, rc)
	desc, _ := msg.Embed["description"].(string)
	n := utf8.RuneCountInString(desc)
	if n > MaxEmbedDescription || n < MaxEmbedDescription-20 {
		t.Errorf("description is %d runes, want it to fill the %d available", n, MaxEmbedDescription)
	}
	if !strings.HasPrefix(desc, "**Big stream <today>**\n\nlorem ipsum") {
		t.Errorf("description starts %.60q, want the title line then the description", desc)
	}
	if !strings.HasSuffix(desc, "…\n\n"+links) {
		t.Errorf("description ends %q, want the shortened description and then the links intact", desc[len(desc)-140:])
	}
	if !msg.Shortened {
		t.Error("Shortened = false, want the preview to be told")
	}

	// A description that fits is not touched, and nobody is told anything.
	rc.Payload.Description = notifLongDescription(500)
	msg = Render(ev, rc)
	if desc, _ := msg.Embed["description"].(string); msg.Shortened || strings.Contains(desc, "…") || !strings.HasSuffix(desc, links) {
		t.Errorf("a fitting description was shortened: shortened=%v, %q", msg.Shortened, desc)
	}
}

// A field that overflows with no description to give is cut at its end,
// exactly as it always was: this is not the description's doing, so the
// preview is not told it was.
func TestRenderOverflowWithoutADescriptionIsUnchanged(t *testing.T) {
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(5000)
	ev := DefaultEvent()

	// The template does not use the placeholder.
	ev.Embed.Description = strings.Repeat("d", 5000) + " {title}"
	msg := Render(ev, rc)
	want := truncate(strings.TrimSpace(Expand(ev.Embed.Description, rc.values())), MaxEmbedDescription)
	if msg.Embed["description"] != want {
		t.Error("an overflowing template without {description} must render as before")
	}
	if msg.Shortened {
		t.Error("Shortened = true for a template that does not use the description")
	}

	// It does, but there is nothing in it.
	rc.Payload.Description = ""
	ev.Embed.Description = "{description}\n" + strings.Repeat("d", 5000)
	msg = Render(ev, rc)
	if desc, _ := msg.Embed["description"].(string); utf8.RuneCountInString(desc) != MaxEmbedDescription || !strings.HasSuffix(desc, "d…") {
		t.Errorf("description = %d runes, want the old tail cut at %d", utf8.RuneCountInString(desc), MaxEmbedDescription)
	}
	if msg.Shortened {
		t.Error("Shortened = true with no description to shorten")
	}
}

// Every use of the description gives way by the same amount.
func TestRenderShortensEveryUseOfTheDescription(t *testing.T) {
	ev := DefaultEvent()
	ev.Embed.Description = "{description}\n--\n{description}\n\nEND"
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(3000)

	msg := Render(ev, rc)
	desc, _ := msg.Embed["description"].(string)
	n := utf8.RuneCountInString(desc)
	if n > MaxEmbedDescription || n < MaxEmbedDescription-40 {
		t.Errorf("description is %d runes, want it to fill the %d available", n, MaxEmbedDescription)
	}
	first, second, ok := strings.Cut(strings.TrimSuffix(desc, "\n\nEND"), "\n--\n")
	if !ok || first != second || !strings.HasSuffix(first, "…") {
		t.Errorf("the two uses differ or are not shortened: %d and %d runes", utf8.RuneCountInString(first), utf8.RuneCountInString(second))
	}
	if !strings.HasSuffix(desc, "\n\nEND") || !msg.Shortened {
		t.Errorf("want the author's END kept and Shortened set; shortened=%v, tail %q", msg.Shortened, desc[len(desc)-20:])
	}
}

// The same in the message body, whose limit is 2000.
func TestRenderShortensTheDescriptionInContent(t *testing.T) {
	ev := model.NotificationEvent{Content: "{channel} is live!\n{description}\nWatch: {url}"}
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(5000)

	msg := Render(ev, rc)
	n := utf8.RuneCountInString(msg.Content)
	if n > MaxContentLength || n < MaxContentLength-20 {
		t.Errorf("content is %d runes, want it to fill the %d available", n, MaxContentLength)
	}
	if !strings.HasPrefix(msg.Content, "Dokibird is live!\nlorem") || !strings.HasSuffix(msg.Content, "…\nWatch: https://www.youtube.com/watch?v=vid123") {
		t.Errorf("content = %.40q ... %q", msg.Content, msg.Content[len(msg.Content)-60:])
	}
	if !msg.Shortened {
		t.Error("Shortened = false")
	}
}

// The embed's combined limit is tighter than the sum of its fields. That
// squeeze comes out of the video description too, never out of the author's
// closing line.
func TestRenderFitsTheDescriptionIntoTheEmbedTotal(t *testing.T) {
	ev := DefaultEvent()
	ev.Embed.Title = strings.Repeat("t", MaxEmbedTitle)
	ev.Embed.Footer = strings.Repeat("f", MaxEmbedFooter)
	ev.Embed.Description = "{description}\n\nEND"
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(5000)

	msg := Render(ev, rc)
	desc, _ := msg.Embed["description"].(string)
	room := MaxEmbedTotal - MaxEmbedTitle - MaxEmbedFooter
	if n := utf8.RuneCountInString(desc); n > room || n < room-20 {
		t.Errorf("description is %d runes, want it to fill the %d the title and footer left", n, room)
	}
	if !strings.HasSuffix(desc, "…\n\nEND") || !msg.Shortened {
		t.Errorf("want the description shortened and END kept; shortened=%v, tail %q", msg.Shortened, desc[len(desc)-20:])
	}
}

// When there is room for less than a phrase, a stub of description is noise:
// it goes altogether, and its line with it. And when the author's own text
// does not fit even then, the old tail cut is all that is left.
func TestRenderDropsADescriptionThereIsNoRoomFor(t *testing.T) {
	rc := notifRenderContext()
	rc.Payload.Description = notifLongDescription(5000)
	ev := DefaultEvent()

	// Room for ten runes of description.
	filler := strings.Repeat("x", MaxEmbedDescription-len("\n\nEND")-10)
	ev.Embed.Description = filler + "\n{description}\nEND"
	msg := Render(ev, rc)
	if want := filler + "\nEND"; msg.Embed["description"] != want {
		desc, _ := msg.Embed["description"].(string)
		t.Errorf("description = %d runes ending %q, want the filler and END with no description stub", utf8.RuneCountInString(desc), desc[len(desc)-30:])
	}
	if !msg.DescriptionDropped || msg.Shortened {
		t.Errorf("dropped=%v shortened=%v, want the description reported as dropped, not as shortened", msg.DescriptionDropped, msg.Shortened)
	}

	// Room for exactly the minimum keeps it.
	filler = strings.Repeat("x", MaxEmbedDescription-len("\n\nEND")-minDescriptionCap)
	ev.Embed.Description = filler + "\n{description}\nEND"
	msg = Render(ev, rc)
	if desc, _ := msg.Embed["description"].(string); !strings.Contains(desc, "\nlorem ipsum") || !strings.HasSuffix(desc, "…\nEND") {
		t.Errorf("description ends %q, want a short description kept", desc[len(desc)-40:])
	}

	// No room even without it.
	ev.Embed.Description = strings.Repeat("x", 5000) + "\n{description}"
	msg = Render(ev, rc)
	if want := truncate(strings.Repeat("x", 5000), MaxEmbedDescription); msg.Embed["description"] != want {
		t.Error("an author's text that overflows on its own must be cut at the limit")
	}
	if !msg.DescriptionDropped || msg.Shortened {
		t.Errorf("dropped=%v shortened=%v, want dropped: the author's own text was cut, and the editor must not claim it was kept", msg.DescriptionDropped, msg.Shortened)
	}

	// A description that opens with a link longer than the room there is cuts
	// down to a bare ellipsis at every cap. That is no description either: it
	// goes, line and all, rather than leaving a stray "…" between the author's
	// lines.
	rc.Payload.Description = "https://example.com/" + strings.Repeat("a", 200) + " and the rest of it"
	filler = strings.Repeat("x", MaxEmbedDescription-len("\n\nEND")-100)
	ev.Embed.Description = filler + "\n{description}\nEND"
	msg = Render(ev, rc)
	if want := filler + "\nEND"; msg.Embed["description"] != want {
		desc, _ := msg.Embed["description"].(string)
		t.Errorf("description ends %q, want no stray ellipsis line", desc[len(desc)-30:])
	}
	if !msg.DescriptionDropped || msg.Shortened {
		t.Errorf("dropped=%v shortened=%v, want dropped", msg.DescriptionDropped, msg.Shortened)
	}
}

// Discord unfurls every bare link in a message body into a preview card, and
// the author cannot edit the creator's links - so in the content, and only
// there, they are wrapped in <>. Embed text does not unfurl and stays raw.
func TestRenderWrapsDescriptionLinksInContentOnly(t *testing.T) {
	const raw = "Merch: https://example.com/merch, and (https://x.example/a). " +
		"Wiki https://en.wikipedia.org/wiki/Foo_(bar) done <https://already.example/w> [site](https://site.example/p)"
	const wrapped = "Merch: <https://example.com/merch>, and (<https://x.example/a>). " +
		"Wiki <https://en.wikipedia.org/wiki/Foo_(bar)> done <https://already.example/w> [site](<https://site.example/p>)"
	ev := DefaultEvent()
	ev.Content = "{description}"
	ev.Embed.Description = "{description}"
	rc := notifRenderContext()
	rc.Payload.Description = raw

	msg := Render(ev, rc)
	if msg.Content != wrapped {
		t.Errorf("content = %q\nwant      %q", msg.Content, wrapped)
	}
	if msg.Embed["description"] != raw {
		t.Errorf("embed description = %q, want the links untouched", msg.Embed["description"])
	}

	// The first-lines form is wrapped the same way, and the author's own
	// links are never touched.
	ev.Content = "{description:1} https://mine.example/x {url}"
	rc.Payload.Description = "Watch https://example.com/live now!\nsecond line"
	if got, want := Render(ev, rc).Content, "Watch <https://example.com/live> now! https://mine.example/x https://www.youtube.com/watch?v=vid123"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// What counts as the link, and what is the sentence around it.
func TestAngleWrapURLs(t *testing.T) {
	cases := map[string]string{
		"":                                        "",
		"no links here":                           "no links here",
		"https://a.example/x":                     "<https://a.example/x>",
		"http://plain.example":                    "<http://plain.example>",
		"Go to https://a.example/x.":              "Go to <https://a.example/x>.",
		"Really https://a.example/x!?":            "Really <https://a.example/x>!?",
		"a https://a.example/x, b":                "a <https://a.example/x>, b",
		"a https://a.example/x; b":                "a <https://a.example/x>; b",
		"see: https://a.example/x:":               "see: <https://a.example/x>:",
		`"https://a.example/q"`:                   `"<https://a.example/q>"`,
		"'https://a.example/q'":                   "'<https://a.example/q>'",
		"(https://a.example/x)":                   "(<https://a.example/x>)",
		"(see https://a.example/x).":              "(see <https://a.example/x>).",
		"https://a.example/x)":                    "<https://a.example/x>)",
		"https://a.example/wiki/Foo_(bar)":        "<https://a.example/wiki/Foo_(bar)>",
		"(https://a.example/wiki/Foo_(bar))":      "(<https://a.example/wiki/Foo_(bar)>)",
		"[label](https://a.example/x)":            "[label](<https://a.example/x>)",
		"<https://a.example/x>":                   "<https://a.example/x>",
		"[label](<https://a.example/x>)":          "[label](<https://a.example/x>)",
		"https://a.example/1 https://a.example/2": "<https://a.example/1> <https://a.example/2>",
		"one\nhttps://a.example/x\ntwo":           "one\n<https://a.example/x>\ntwo",
		"ftp://a.example/x":                       "ftp://a.example/x",
		// Every kind of space ends a link, not only the ASCII ones: Japanese
		// text follows a link with U+3000, and pasted text with a no-break one.
		"https://a.example/x　フォローしてね": "<https://a.example/x>　フォローしてね",
		"https://a.example/x next":    "<https://a.example/x> next",
		"https://...":                 "https://...",
	}
	for in, want := range cases {
		if got := angleWrapURLs(in); got != want {
			t.Errorf("angleWrapURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

// A description is platform text like a title: whatever mention syntax it
// carries is shown and pings nobody.
func TestMentionPolicyIgnoresTheDescription(t *testing.T) {
	ev := notifEvent(1, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	ev.Content = "<@&555555555555555555> {description}"
	ev.Embed.Description = "{description:1}"
	p := notifLivePayload()
	p.Description = "@everyone <@&5> <@77> come watch\n@here too"
	msg := Render(ev, RenderContext{Payload: p, Channel: notifChannel()})
	if !strings.Contains(msg.Content, "@everyone <@&5> <@77> come watch\n@here too") {
		t.Fatalf("content = %q, the description must still be rendered verbatim", msg.Content)
	}
	if got := notifIDListFromPolicy(msg.Mentions, "roles"); !slices.Equal(got, []string{"555555555555555555"}) {
		t.Errorf("roles = %v, want only the template's role", got)
	}
	if got := notifIDListFromPolicy(msg.Mentions, "users"); len(got) != 0 {
		t.Errorf("users = %v, want none - the user mention came from the description", got)
	}
	if parse, _ := msg.Mentions["parse"].([]string); len(parse) != 0 {
		t.Errorf("parse = %v, want [] - the @everyone came from the description", parse)
	}
}

// Previews and test sends render the description the ledger recorded, from
// whichever row stands in for the trigger - and never an invented one.
func TestPreviewPayloadCarriesTheDescription(t *testing.T) {
	ch := notifChannel()
	yt := &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real1", URL: "https://www.youtube.com/watch?v=real1", Title: "Real title", Description: "live pitch\n\nlive links"}

	if got := PreviewPayload(TriggerLive, ch, yt, nil, notifNow); got.Description != yt.Description {
		t.Errorf("live preview description = %q, want the broadcast's", got.Description)
	}
	for _, tr := range []Trigger{TriggerScheduled, TriggerUpload, TriggerShort} {
		v := &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "v-" + string(tr), Kind: string(tr), URL: "x", Description: "about the " + string(tr)}
		if got := PreviewPayload(tr, ch, yt, v, notifNow); got.Description != v.Description {
			t.Errorf("%s preview description = %q, want the video's", tr, got.Description)
		}
		// With no video of that kind the YouTube broadcast stands in, and
		// brings its own description.
		if got := PreviewPayload(tr, ch, yt, nil, notifNow); got.ID != "real1" || got.Description != yt.Description {
			t.Errorf("%s preview borrowed from a broadcast = %+v, want its description too", tr, got)
		}
	}

	// A Twitch stream has none, and the preview that dresses an ended Twitch
	// stream up as live gets no example description to go with its title.
	tw := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "42", URL: "https://twitch.tv/dokibird", EndedAt: notifNow.Unix()}
	if got := PreviewPayload(TriggerLive, ch, tw, nil, notifNow); got.Description != "" {
		t.Errorf("twitch preview description = %q, want none", got.Description)
	}
	if got := PreviewPayload(TriggerLive, ch, nil, nil, notifNow); got.Description != "" {
		t.Errorf("preview with nothing detected has description %q", got.Description)
	}
}

// The stand-in video has a description shaped like a real one, so the three
// ways of including it look different while working on the editor offline.
func TestSamplePayloadCarriesADescription(t *testing.T) {
	for _, tr := range []Trigger{TriggerLive, TriggerScheduled, TriggerUpload, TriggerShort} {
		if got := SamplePayload(tr, notifChannel(), notifNow).Description; got != SampleVideoDescription {
			t.Errorf("%s sample description = %q", tr, got)
		}
	}
	if cleanDescription(SampleVideoDescription) != SampleVideoDescription {
		t.Error("the sample description should already be clean, so what the editor shows is what was written")
	}
	values := map[string]string{"{description}": SampleVideoDescription}
	one, three, all := Expand("{description:1}", values), Expand("{description:3}", values), Expand("{description}", values)
	if one == three || three == all || strings.Contains(one, "\n") {
		t.Errorf("first line, first three lines and all of it must differ:\n%q\n%q\n%q", one, three, all)
	}
	if !strings.Contains(all, "\n\n") || strings.Count(all, "https://") < 2 {
		t.Error("the sample description needs a blank line and a couple of bare links")
	}
}

// UsesPlaceholder answers "would this rule show that placeholder" with the
// parser's own grammar, so the preview handler never has to know it.
func TestUsesPlaceholder(t *testing.T) {
	content := func(s string) model.NotificationEvent { return model.NotificationEvent{Content: s} }
	embed := func(enabled bool, e model.EmbedTemplate) model.NotificationEvent {
		return model.NotificationEvent{EmbedEnabled: enabled, Embed: e}
	}
	cases := []struct {
		what string
		ev   model.NotificationEvent
		name string
		want bool
	}{
		{"bare", content("x {description} y"), "{description}", true},
		{"with a count", content("{description:3}"), "{description}", true},
		{"two digits", content("{description:12}"), "{description}", true},
		{"inside emphasis", content("**{description:1}**"), "{description}", true},
		{"among others", content("{title}\n> {description:1}\n{url}"), "{description}", true},
		{"a count of zero is a typo", content("{description:0}"), "{description}", false},
		{"three digits is a typo", content("{description:100}"), "{description}", false},
		{"wrong case", content("{Description}"), "{description}", false},
		{"a longer name", content("{descriptions}"), "{description}", false},
		{"no braces", content("description"), "{description}", false},
		{"absent", content("{title} {url}"), "{description}", false},
		{"empty rule", model.NotificationEvent{}, "{description}", false},
		{"embed title", embed(true, model.EmbedTemplate{Title: "{description:1}"}), "{description}", true},
		{"embed description", embed(true, model.EmbedTemplate{Description: "a\n{description}"}), "{description}", true},
		{"embed footer", embed(true, model.EmbedTemplate{Footer: "{description:2}"}), "{description}", true},
		{"embed off", embed(false, model.EmbedTemplate{Title: "{description}", Description: "{description}", Footer: "{description}"}), "{description}", false},
		{"url fields are not text", embed(true, model.EmbedTemplate{URL: "{description}", Image: "{description}", Thumbnail: "{description}"}), "{description}", false},
		{"another placeholder", content("{title}"), "{title}", true},
		{"a count on one that takes none", content("{title:2}"), "{title}", false},
		{"the default rule has no description", DefaultEvent(), "{description}", false},
		{"the default rule has a title", DefaultEvent(), "{title}", true},
	}
	for _, c := range cases {
		if got := UsesPlaceholder(c.ev, c.name); got != c.want {
			t.Errorf("%s: UsesPlaceholder(%s) = %v, want %v", c.what, c.name, got, c.want)
		}
	}
}

// {game} is the Twitch category and nothing else. It renders on a Twitch
// payload, takes no line count, and on YouTube it is blank - so a line it
// stood alone on goes, the way a {description} line does on Twitch.
func TestGameIsTheTwitchCategory(t *testing.T) {
	rc := notifRenderContext()
	rc.Payload.Platform = PlatformTwitch
	rc.Payload.Description = ""
	rc.Payload.Game = "  Just Chatting "
	if got := rc.values()["{game}"]; got != "Just Chatting" {
		t.Errorf("{game} = %q, want the category, trimmed", got)
	}

	ev := model.NotificationEvent{Content: "{channel} is live\n🎮 {game}\n- {game:1}"}
	if got, want := Render(ev, rc).Content, "Dokibird is live\n🎮 Just Chatting\n- {game:1}"; got != want {
		t.Errorf("twitch content = %q, want %q", got, want)
	}
	if !UsesPlaceholder(ev, "{game}") {
		t.Error("UsesPlaceholder does not see {game}")
	}

	// The same template on YouTube: no category, and nothing stands in for it.
	ev.Content = "{channel} is live\n🎮 {game}\n**{game}**\nPlaying: {game}"
	if got, want := Render(ev, notifRenderContext()).Content, "Dokibird is live\nPlaying:"; got != want {
		t.Errorf("youtube content = %q, want %q", got, want)
	}

	// A category that reads like a ping pings nobody.
	rc.Payload.Game = "@everyone <@&5>"
	msg := Render(model.NotificationEvent{Content: "{game}"}, rc)
	if parse, _ := msg.Mentions["parse"].([]string); len(parse) != 0 || len(notifIDListFromPolicy(msg.Mentions, "roles")) != 0 {
		t.Errorf("mentions = %v, want none - they came from the category", msg.Mentions)
	}
}

// Previews and test sends render the category the ledger recorded for a live
// Twitch broadcast. The video triggers are YouTube-only and never get one,
// whichever row stands in for them.
func TestPreviewPayloadCarriesTheGame(t *testing.T) {
	ch := notifChannel()
	tw := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "42", URL: "https://twitch.tv/dokibird", Title: "Real title", Game: "Minecraft"}
	if got := PreviewPayload(TriggerLive, ch, tw, nil, notifNow); got.Game != "Minecraft" {
		t.Errorf("live preview game = %q, want the broadcast's", got.Game)
	}

	// An ended Twitch stream claimed without one gets no example to go with
	// its example title.
	bare := &model.DetectedBroadcast{Platform: PlatformTwitch, BroadcastID: "43", URL: "https://twitch.tv/dokibird", EndedAt: notifNow.Unix()}
	if got := PreviewPayload(TriggerLive, ch, bare, nil, notifNow); got.Game != "" {
		t.Errorf("preview game for a row without one = %q, want none", got.Game)
	}

	yt := &model.DetectedBroadcast{Platform: PlatformYouTube, BroadcastID: "real1", URL: "https://www.youtube.com/watch?v=real1", Game: "must never appear"}
	for _, tr := range []Trigger{TriggerScheduled, TriggerUpload, TriggerShort} {
		v := &model.DetectedVideo{Platform: PlatformYouTube, VideoID: "v1", Kind: string(tr), URL: "x"}
		if got := PreviewPayload(tr, ch, yt, v, notifNow); got.Game != "" {
			t.Errorf("%s preview game = %q, want none", tr, got.Game)
		}
		if got := PreviewPayload(tr, ch, yt, nil, notifNow); got.Game != "" {
			t.Errorf("%s preview borrowed from a broadcast has game %q, want none", tr, got.Game)
		}
	}
	for _, tr := range []Trigger{TriggerLive, TriggerScheduled, TriggerUpload, TriggerShort} {
		if got := SamplePayload(tr, ch, notifNow).Game; got != "" {
			t.Errorf("%s sample game = %q; the stand-in is a YouTube video", tr, got)
		}
	}
}
