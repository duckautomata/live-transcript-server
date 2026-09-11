package livedetect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"live-transcript-server/internal/config"
)

// recordingSink captures what the detector reports, so a leg can be driven
// directly without a database.
type recordingSink struct {
	mu     sync.Mutex
	live   []Broadcast
	mechs  []string
	ended  []string
	videos []VideoEvent
	active []Broadcast
}

func (s *recordingSink) ObserveLive(_ context.Context, b Broadcast, mechanism string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = append(s.live, b)
	s.mechs = append(s.mechs, mechanism)
	return nil
}

func (s *recordingSink) ObserveEnded(_ context.Context, _, broadcastID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = append(s.ended, broadcastID)
	return nil
}

func (s *recordingSink) ObserveVideo(_ context.Context, v VideoEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videos = append(s.videos, v)
	return nil
}

func (s *recordingSink) ActiveBroadcasts(context.Context) ([]Broadcast, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Broadcast(nil), s.active...), nil
}

func (s *recordingSink) videoEvents() []VideoEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]VideoEvent(nil), s.videos...)
}

func (s *recordingSink) endedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ended...)
}

func testChannels() []config.ChannelConfig {
	return []config.ChannelConfig{
		{Name: "doki", DisplayName: "Dokibird", TwitchLogin: "dokibird", YouTubeChannelId: "UCaaaaaaaaaaaaaaaaaaaaaa"},
	}
}

func newTestDetector(t *testing.T, sink Sink) *Detector {
	t.Helper()
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		Twitch:  config.LiveDetectTwitchConfig{Enabled: true, ClientId: "c", ClientSecret: "s"},
	}, testChannels(), sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestNewReturnsNilWhenDisabled(t *testing.T) {
	d, err := New(config.LiveDetectConfig{Enabled: false}, testChannels(), &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d != nil {
		t.Fatal("a disabled detector must be nil so callers need no special case")
	}
	// Every method must survive the nil receiver.
	if d.Enabled() || d.EventSubEnabled() || d.WebSubEnabled() {
		t.Error("nil detector should report everything disabled")
	}
	if err := d.Start(); err != nil {
		t.Errorf("nil Start: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if st := d.Status(); st.Enabled {
		t.Error("nil Status should report disabled")
	}
	d.HandleWebSubPush(nil)
}

// A misconfigured leg must be disabled with an explanation, never fatal: the
// transcript server is the product, and a shadow-mode observer has no business
// stopping it from booting.
func TestConfigProblemsDisableLegsWithoutFailing(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		Twitch:  config.LiveDetectTwitchConfig{Enabled: true, ClientId: "", ClientSecret: ""},
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: ""},
	}, testChannels(), &recordingSink{}, nil)

	if err == nil {
		t.Fatal("expected the problems to be reported")
	}
	if d == nil {
		t.Fatal("a detector must still be returned so the caller can carry on")
	}
	t.Cleanup(func() { d.Close() })
	if d.Enabled() {
		t.Error("no leg was usable, so nothing should be enabled")
	}
}

// EventSub needs both a public callback URL and a secret Twitch will accept.
// Missing either downgrades to polling rather than disabling Twitch entirely.
func TestEventSubDegradesToPollingWhenMisconfigured(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		// No PublicBaseURL.
		Twitch: config.LiveDetectTwitchConfig{
			Enabled: true, ClientId: "c", ClientSecret: "s",
			EventSub: true, EventSubSecret: "long-enough-secret",
		},
	}, testChannels(), &recordingSink{}, nil)
	if err == nil {
		t.Fatal("expected a reported problem")
	}
	t.Cleanup(func() { d.Close() })

	if d.EventSubEnabled() {
		t.Error("eventsub cannot run without a callback URL")
	}
	if !d.Enabled() {
		t.Error("twitch polling must survive an eventsub misconfiguration")
	}
}

func TestEventSubRejectsOutOfRangeSecret(t *testing.T) {
	for _, secret := range []string{"tooshort", string(make([]byte, EventSubSecretMaxLen+1))} {
		d, err := New(config.LiveDetectConfig{
			Enabled: true, PublicBaseURL: "https://example.test",
			Twitch: config.LiveDetectTwitchConfig{
				Enabled: true, ClientId: "c", ClientSecret: "s",
				EventSub: true, EventSubSecret: secret,
			},
		}, testChannels(), &recordingSink{}, nil)
		if err == nil {
			t.Fatalf("secret of length %d should have been rejected", len(secret))
		}
		if d.EventSubEnabled() {
			t.Errorf("eventsub must not run with a %d-character secret Twitch would reject", len(secret))
		}
		d.Close()
	}
}

// Helix responses are edge-cached and can omit a genuinely live channel from a
// successful 200. A single absence must never end a broadcast, or the ledger
// gets corrupted mid-stream.
func TestTwitchAbsenceRequiresRepetitionBeforeEnding(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	ctx := context.Background()
	logins := []string{"dokibird"}

	// Establish that a broadcast is live.
	d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{
		"dokibird": {ID: "555", UserLogin: "dokibird"},
	})

	// Absences short of the threshold must not end anything.
	for i := range twitchAbsencesBeforeEnd - 1 {
		d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{})
		if got := sink.endedIDs(); len(got) != 0 {
			t.Fatalf("ended after only %d absences: %v", i+1, got)
		}
	}

	// The threshold absence ends it.
	d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{})
	if got := sink.endedIDs(); len(got) != 1 || got[0] != "555" {
		t.Fatalf("ended = %v, want [555] once the absence is credible", got)
	}
}

// A cache miss followed by the channel reappearing must reset the counter, so
// intermittent absences never accumulate into a false end.
func TestTwitchAbsenceCounterResetsOnReappearance(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	ctx := context.Background()
	logins := []string{"dokibird"}
	live := map[string]TwitchStream{"dokibird": {ID: "555", UserLogin: "dokibird"}}

	d.reconcileTwitchAbsences(ctx, logins, live)
	for range 10 {
		d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{}) // a miss
		d.reconcileTwitchAbsences(ctx, logins, live)                      // back again
	}
	if got := sink.endedIDs(); len(got) != 0 {
		t.Fatalf("flapping absences must never end a broadcast, got %v", got)
	}
}

// Absence for a channel that was never live is unremarkable and must not
// produce an end for a broadcast that does not exist.
func TestTwitchAbsenceIgnoresNeverLiveChannel(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)

	for range twitchAbsencesBeforeEnd + 3 {
		d.reconcileTwitchAbsences(context.Background(), []string{"dokibird"}, map[string]TwitchStream{})
	}
	if got := sink.endedIDs(); len(got) != 0 {
		t.Fatalf("expected no ends, got %v", got)
	}
}

func TestKnownWebSubTopic(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://example.test",
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", WebSub: true, WebSubSecret: "s",
		},
	}, testChannels(), &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !d.KnownWebSubTopic(YouTubeTopicURL("UCaaaaaaaaaaaaaaaaaaaaaa")) {
		t.Error("a configured channel's topic must be recognised")
	}
	if d.KnownWebSubTopic("https://evil.test/feed") {
		t.Error("an arbitrary topic must not be recognised")
	}
	if d.WebSubTopicSecret(YouTubeTopicURL("UCaaaaaaaaaaaaaaaaaaaaaa")) == "" {
		t.Error("a configured topic must derive a secret")
	}
}

func TestCallbackURLsUseTheConfiguredBase(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		// A trailing slash must not produce a double slash in the callback.
		PublicBaseURL: "https://example.test/",
		Twitch: config.LiveDetectTwitchConfig{
			Enabled: true, ClientId: "c", ClientSecret: "s",
			EventSub: true, EventSubSecret: "long-enough-secret",
		},
	}, testChannels(), &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if got := d.twitchCallbackURL(); got != "https://example.test"+TwitchEventSubPath {
		t.Errorf("twitch callback = %q", got)
	}
	if got := d.webSubCallbackURL(); got != "https://example.test"+YouTubeWebSubPath {
		t.Errorf("websub callback = %q", got)
	}
}

// The poll floor exists because Twitch caches per edge server: polling faster
// returns inconsistent results, including phantom stream-id changes that read
// as restarts.
func TestTwitchPollIntervalIsClamped(t *testing.T) {
	sink := &recordingSink{}
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		Twitch:  config.LiveDetectTwitchConfig{Enabled: true, ClientId: "c", ClientSecret: "s", PollSeconds: 5},
	}, testChannels(), sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if got := d.twitchPollInterval().Seconds(); got < minTwitchPollSeconds {
		t.Fatalf("poll interval = %vs, must be clamped to at least %ds", got, minTwitchPollSeconds)
	}
}

// A restart mid-broadcast must resume watching what the ledger still believes
// is live. Without this the in-memory watchlist comes back empty, the id only
// returns on the next discovery pass, and until then nothing notices the
// broadcast ending - which also suppresses the restart re-poll.
func TestReseedFromLedgerRestoresWatchlist(t *testing.T) {
	sink := &recordingSink{active: []Broadcast{
		{Platform: PlatformYouTube, ChannelKey: "doki", ID: "vid-live"},
		// Twitch has no per-video watchlist; its polling leg re-derives state
		// from Helix every cycle, so this row must be ignored rather than
		// seeded as if it were a YouTube video.
		{Platform: PlatformTwitch, ChannelKey: "doki", ID: "555"},
	}}

	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, testChannels(), sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	d.reseedFromLedger()

	if !d.watch.Known("vid-live") {
		t.Error("a still-live YouTube broadcast must be restored to the watchlist")
	}
	if d.watch.Known("555") {
		t.Error("a Twitch broadcast has no video id and must not be seeded")
	}
	if got := d.watch.Size(); got != 1 {
		t.Errorf("watchlist size = %d, want 1", got)
	}
}

// A ledger read failure must not stop the detector from starting: losing the
// re-seed costs one discovery pass, while failing to start costs everything.
func TestReseedSurvivesASinkError(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, testChannels(), failingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	d.reseedFromLedger() // must not panic
	if got := d.watch.Size(); got != 0 {
		t.Errorf("watchlist size = %d, want 0", got)
	}
}

// failingSink makes every ledger read fail.
type failingSink struct{}

func (failingSink) ObserveLive(context.Context, Broadcast, string) error { return errFailingSink }
func (failingSink) ObserveEnded(context.Context, string, string) error   { return errFailingSink }
func (failingSink) ObserveVideo(context.Context, VideoEvent) error       { return errFailingSink }
func (failingSink) ActiveBroadcasts(context.Context) ([]Broadcast, error) {
	return nil, errFailingSink
}

var errFailingSink = errors.New("sink unavailable")

// A changed stream id on a channel that is still present is a restart: the
// ingest dropped past the reconnect grace window and Twitch minted a new id.
// Overwriting it silently would leave the old broadcast live in the ledger
// forever.
func TestTwitchIDChangeEndsTheOldBroadcast(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	ctx := context.Background()
	logins := []string{"dokibird"}

	d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{
		"dokibird": {ID: "first", UserLogin: "dokibird"},
	})
	if got := sink.endedIDs(); len(got) != 0 {
		t.Fatalf("nothing should have ended yet, got %v", got)
	}

	d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{
		"dokibird": {ID: "second", UserLogin: "dokibird"},
	})
	if got := sink.endedIDs(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("ended = %v, want [first]: a restart must end the broadcast it replaced", got)
	}

	// The same id repeating is not a restart.
	d.reconcileTwitchAbsences(ctx, logins, map[string]TwitchStream{
		"dokibird": {ID: "second", UserLogin: "dokibird"},
	})
	if got := sink.endedIDs(); len(got) != 1 {
		t.Fatalf("an unchanged id must not end anything, got %v", got)
	}
}

// A stale subscription that is not in the enabled state cannot deliver, so it
// must never be adopted as healthy - that was the path by which a webhook
// blocked at the edge looked fine forever.
func TestPendingAndYoung(t *testing.T) {
	now := base

	fresh := EventSubSubscription{
		Status: EventSubStatusVerificationPending, CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if !pendingAndYoung(fresh, now) {
		t.Error("a subscription created a minute ago is still mid-handshake")
	}

	stuck := EventSubSubscription{
		Status: EventSubStatusVerificationPending, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	if pendingAndYoung(stuck, now) {
		t.Error("a subscription pending for an hour will never verify and must be recreated")
	}

	enabled := EventSubSubscription{Status: EventSubStatusEnabled}
	if pendingAndYoung(enabled, now) {
		t.Error("an enabled subscription is not pending")
	}

	// An unreadable timestamp must be treated as NOT young, so the
	// subscription is recreated rather than trusted indefinitely.
	unparseable := EventSubSubscription{Status: EventSubStatusVerificationPending, CreatedAt: "not-a-time"}
	if pendingAndYoung(unparseable, now) {
		t.Error("an unparseable created_at must not read as young")
	}
}

// A leg that only runs every few hours must not be judged by the 15-minute
// staleness rule, and a push leg is legitimately silent when nobody streams.
func TestStalenessWindowFollowsEachLegsCadence(t *testing.T) {
	d := newTestDetector(t, &recordingSink{})

	if got := d.staleAfter(MechanismYouTubeAudit); got < 3*searchAuditInterval {
		t.Errorf("audit stale window = %v, too short for a %v cadence", got, searchAuditInterval)
	}
	if got := d.staleAfter(MechanismTwitchEventSub); got != 0 {
		t.Errorf("a push leg must never be judged stale, got %v", got)
	}
	if got := d.staleAfter(MechanismYouTubeWebSub); got != 0 {
		t.Errorf("a push leg must never be judged stale, got %v", got)
	}
	// A frequent leg still gets the configured floor rather than 3x a few seconds.
	if got := d.staleAfter(MechanismTwitchPoll); got < time.Duration(d.staleAlertMinutes())*time.Minute {
		t.Errorf("poll leg stale window = %v, below the configured floor", got)
	}
}

// The real deployment is served under a path prefix that the reverse proxy
// strips (api.duck-automata.com/live/* -> this container), so publicBaseUrl
// carries a path. The callbacks must keep that prefix - a third party dials the
// public URL, not the container's - while the reconciler's ownership check
// still resolves to the bare host.
func TestCallbackURLsSurviveAPathPrefixedBase(t *testing.T) {
	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://api.duck-automata.com/live",
		Twitch: config.LiveDetectTwitchConfig{
			Enabled: true, ClientId: "c", ClientSecret: "s",
			EventSub: true, EventSubSecret: "long-enough-secret-value",
		},
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", WebSub: true, WebSubSecret: "s",
		},
	}, testChannels(), &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if got, want := d.twitchCallbackURL(), "https://api.duck-automata.com/live/livedetect/twitch/eventsub"; got != want {
		t.Errorf("twitch callback = %q, want %q", got, want)
	}
	if got, want := d.webSubCallbackURL(), "https://api.duck-automata.com/live/livedetect/youtube/websub"; got != want {
		t.Errorf("websub callback = %q, want %q", got, want)
	}
	// Subscription cleanup compares hosts, not full URLs, so a path prefix
	// must not stop us recognising our own subscriptions.
	if got := callbackHost(d.twitchCallbackURL()); got != "api.duck-automata.com" {
		t.Errorf("callbackHost = %q, want the bare host", got)
	}
	if !d.EventSubEnabled() || !d.WebSubEnabled() {
		t.Error("a path-prefixed base must not disable either push leg")
	}
}

// Which platforms a channel is watched on is decided per channel by which
// identifiers it carries. A Twitch-only and a YouTube-only channel must be able
// to coexist, each watched on exactly one platform.
func TestPerChannelPlatformTargeting(t *testing.T) {
	channels := []config.ChannelConfig{
		{Name: "both", TwitchLogin: "bothlogin", YouTubeChannelId: "UCaaaaaaaaaaaaaaaaaaaaaa"},
		{Name: "twitchonly", TwitchLogin: "twitchonlylogin"},
		{Name: "youtubeonly", YouTubeChannelId: "UCbbbbbbbbbbbbbbbbbbbbbb"},
		{Name: "neither"},
	}

	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://example.test",
		Twitch:        config.LiveDetectTwitchConfig{Enabled: true, ClientId: "c", ClientSecret: "s"},
		YouTube:       config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, channels, &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if got, want := d.watchSummary(), "both=twitch+youtube twitchonly=twitch youtubeonly=youtube"; got != want {
		t.Errorf("watchSummary = %q, want %q", got, want)
	}
	if _, ok := d.twitchTargets["twitchonlylogin"]; !ok {
		t.Error("a twitch-only channel must be polled on twitch")
	}
	if _, ok := d.ytTargets["UCbbbbbbbbbbbbbbbbbbbbbb"]; !ok {
		t.Error("a youtube-only channel must be watched on youtube")
	}
	if len(d.twitchTargets) != 2 || len(d.ytTargets) != 2 {
		t.Errorf("targets = %d twitch / %d youtube, want 2 and 2", len(d.twitchTargets), len(d.ytTargets))
	}
}

// A malformed identifier fails SILENTLY at the API - Helix answers an unknown
// login with an empty array, and the Data API an unknown channel with no items
// - so a typo would read as "never goes live" indefinitely. It must be caught
// at construction and reported, while leaving every other channel working.
func TestMalformedIdentifiersAreRejectedPerChannel(t *testing.T) {
	channels := []config.ChannelConfig{
		{Name: "good", TwitchLogin: "goodlogin", YouTubeChannelId: "UCaaaaaaaaaaaaaaaaaaaaaa"},
		{Name: "badyt", YouTubeChannelId: "youtube.com/@someone"},
		{Name: "shortyt", YouTubeChannelId: "UCtooshort"},
		{Name: "badtwitch", TwitchLogin: "not a login"},
	}

	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://example.test",
		Twitch:        config.LiveDetectTwitchConfig{Enabled: true, ClientId: "c", ClientSecret: "s"},
		YouTube:       config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, channels, &recordingSink{}, nil)
	if err == nil {
		t.Fatal("malformed identifiers must be reported")
	}
	t.Cleanup(func() { d.Close() })

	// The good channel keeps working; only the broken ones are dropped.
	if got, want := d.watchSummary(), "good=twitch+youtube"; got != want {
		t.Errorf("watchSummary = %q, want %q", got, want)
	}
	for _, msg := range []string{"badyt", "shortyt", "badtwitch"} {
		if !strings.Contains(err.Error(), msg) {
			t.Errorf("the reported problems do not name %q: %v", msg, err)
		}
	}
}

func TestYouTubeChannelIDPattern(t *testing.T) {
	valid := []string{"UCaaaaaaaaaaaaaaaaaaaaaa", "UC-_0123456789abcdefghij"}
	invalid := []string{
		"", "UC", "UCtooshort", "UCaaaaaaaaaaaaaaaaaaaaaaa", // one char too long
		"XXaaaaaaaaaaaaaaaaaaaaaa", // wrong prefix
		"UCaaaaaaaaaaaaaaaaaaaaa!", // bad character
		"https://www.youtube.com/channel/UCaaaaaaaaaaaaaaaaaaaaaa",
		"@dokibird",
	}
	for _, v := range valid {
		if !ytChannelIDPattern.MatchString(v) {
			t.Errorf("%q should be a valid channel id", v)
		}
	}
	for _, v := range invalid {
		if ytChannelIDPattern.MatchString(v) {
			t.Errorf("%q should be rejected", v)
		}
	}
}

func TestValidTwitchLogin(t *testing.T) {
	for _, v := range []string{"doki", "dokibird", "mint_fantome", "abcd", "a1_2"} {
		if !validTwitchLogin(v) {
			t.Errorf("%q should be a valid login", v)
		}
	}
	for _, v := range []string{"", "abc", "not a login", "has-dash", "UPPER", "@doki",
		"https://twitch.tv/dokibird", strings.Repeat("a", 26)} {
		if validTwitchLogin(v) {
			t.Errorf("%q should be rejected", v)
		}
	}
}

// The EventSub delivery path had no direct coverage, which is how a nil map in
// handleStreamOnline reached this far. It panicked inside a held mutex, so the
// blast radius was the whole Twitch side: the poll leg would block forever on
// its next cycle and shutdown would hang.
func TestHandleStreamOnlineRecordsTheLiveBroadcast(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)

	env := TwitchEnvelope{
		Subscription: EventSubSubscription{
			Type:      EventSubTypeStreamOnline,
			Condition: map[string]string{"broadcaster_user_id": "1234"},
		},
		Event: []byte(`{"id":"999","broadcaster_user_id":"1234","broadcaster_user_login":"dokibird","type":"live","started_at":"2026-01-01T19:00:00Z"}`),
	}
	d.HandleEventSubNotification(context.Background(), env, time.Now())

	sink.mu.Lock()
	got := len(sink.live)
	sink.mu.Unlock()
	if got != 1 {
		t.Fatalf("observed %d broadcasts, want 1", got)
	}

	// The mutex must be free afterwards: if the handler leaked it, every later
	// poll cycle and every later webhook would block on it forever.
	done := make(chan struct{})
	go func() {
		d.reconcileTwitchAbsences(context.Background(), []string{"dokibird"}, map[string]TwitchStream{
			"dokibird": {ID: "999", UserLogin: "dokibird"},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the poll leg blocked; the eventsub handler leaked twitchAbsentMu")
	}
}

// An offline that Twitch sent BEFORE we started tracking the current broadcast
// arrived out of order and must not end the restart that superseded it.
func TestOutOfOrderOfflineDoesNotEndTheRestart(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	ctx := context.Background()

	// The restart goes live now.
	online := TwitchEnvelope{
		Subscription: EventSubSubscription{
			Type: EventSubTypeStreamOnline, Condition: map[string]string{"broadcaster_user_id": "1234"},
		},
		Event: []byte(`{"id":"restart","broadcaster_user_id":"1234","broadcaster_user_login":"dokibird","type":"live","started_at":"2026-01-01T19:00:00Z"}`),
	}
	d.HandleEventSubNotification(ctx, online, time.Now())

	// A stream.offline for the PREVIOUS broadcast, sent before the restart,
	// lands afterwards.
	offline := TwitchEnvelope{
		Subscription: EventSubSubscription{
			Type: EventSubTypeStreamOffline, Condition: map[string]string{"broadcaster_user_id": "1234"},
		},
		Event: []byte(`{"broadcaster_user_id":"1234","broadcaster_user_login":"dokibird"}`),
	}
	d.HandleEventSubNotification(ctx, offline, time.Now().Add(-time.Minute))

	if got := sink.endedIDs(); len(got) != 0 {
		t.Fatalf("ended %v; a late offline must not end the broadcast that replaced it", got)
	}
}

// A well-ordered offline still ends the broadcast it refers to.
func TestInOrderOfflineEndsTheBroadcast(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	ctx := context.Background()

	d.HandleEventSubNotification(ctx, TwitchEnvelope{
		Subscription: EventSubSubscription{
			Type: EventSubTypeStreamOnline, Condition: map[string]string{"broadcaster_user_id": "1234"},
		},
		Event: []byte(`{"id":"live1","broadcaster_user_id":"1234","broadcaster_user_login":"dokibird","type":"live","started_at":"2026-01-01T19:00:00Z"}`),
	}, time.Now().Add(-time.Hour))

	d.HandleEventSubNotification(ctx, TwitchEnvelope{
		Subscription: EventSubSubscription{
			Type: EventSubTypeStreamOffline, Condition: map[string]string{"broadcaster_user_id": "1234"},
		},
		Event: []byte(`{"broadcaster_user_id":"1234","broadcaster_user_login":"dokibird"}`),
	}, time.Now())

	if got := sink.endedIDs(); len(got) != 1 || got[0] != "live1" {
		t.Fatalf("ended = %v, want [live1]", got)
	}
}

// A Twitch broadcast that ended while the process was down must be recoverable.
// Without re-seeding the tracked login, the absence path bails on an empty
// twitchLiveID, ended_at stays zero forever, and the row is handed back on
// every restart and never pruned.
func TestReseedRestoresTwitchTracking(t *testing.T) {
	sink := &recordingSink{active: []Broadcast{
		{Platform: PlatformTwitch, ChannelKey: "doki", ID: "was-live-before-restart"},
	}}
	d := newTestDetector(t, sink)

	d.reseedFromLedger()

	// It is now tracked, so three absent polls can conclude it ended.
	for range twitchAbsencesBeforeEnd {
		d.reconcileTwitchAbsences(context.Background(), []string{"dokibird"}, map[string]TwitchStream{})
	}
	if got := sink.endedIDs(); len(got) != 1 || got[0] != "was-live-before-restart" {
		t.Fatalf("ended = %v, want the pre-restart broadcast to be closable", got)
	}
}

func TestTwitchLoginFor(t *testing.T) {
	d := newTestDetector(t, &recordingSink{})
	if got := d.twitchLoginFor("doki"); got != "dokibird" {
		t.Errorf("twitchLoginFor(doki) = %q, want dokibird", got)
	}
	if got := d.twitchLoginFor("nosuchchannel"); got != "" {
		t.Errorf("an unknown channel must map to no login, got %q", got)
	}
}

// Discovery is the cost that scales with channel count and is paid whether or
// not anyone streams. At a high channel count it can eat the whole budget
// before a single broadcast, so the projection must warn rather than let that
// be discovered as an unexplained staleness alert hours later.
func TestQuotaProjectionSuggestsASaferCadence(t *testing.T) {
	channels := make([]config.ChannelConfig, 0, 6)
	for i := range 6 {
		channels = append(channels, config.ChannelConfig{
			Name:             "ch" + string(rune('a'+i)),
			YouTubeChannelId: "UC" + string(rune('a'+i)) + "aaaaaaaaaaaaaaaaaaaaa",
		})
	}

	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", DiscoverySeconds: 120, DailyUnitBudget: 8000,
		},
	}, channels, &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if len(d.ytTargets) != 6 {
		t.Fatalf("expected 6 youtube targets, got %d", len(d.ytTargets))
	}
	// 6 channels x 720 cycles/day = 4320 units, which is over half of 8000.
	cycles := int(24 * time.Hour / d.ytDiscoveryInterval())
	if got := len(d.ytTargets) * cycles; got != 4320 {
		t.Fatalf("projected discovery = %d units/day, expected 4320", got)
	}
	// The projection must not panic and must run on a nil alerts client.
	d.logQuotaProjection()
}

// The probe's whole job is telling "our handler answered" from "something at
// the edge answered". A decoy page carries a plausible status and no marker.
func TestProbeDistinguishesHandlerFromInterception(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantReached bool
	}{
		{
			name: "our handler answered",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(HeaderCallbackMarker, "1")
				w.WriteHeader(http.StatusForbidden) // the unsigned-probe rejection
			},
			wantReached: true,
		},
		{
			name: "bot challenge serves a decoy page with a 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cf-Ray", "a38c3b6dde1e054d")
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "<html>decoy</html>")
			},
			wantReached: false,
		},
		{
			name: "edge blocks outright",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cf-Ray", "deadbeefdeadbeef")
				w.WriteHeader(http.StatusForbidden)
			},
			wantReached: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			d := newTestDetector(t, &recordingSink{})
			reached, detail := d.probeOne(srv.URL)
			if reached != tc.wantReached {
				t.Fatalf("reached = %v, want %v (detail: %s)", reached, tc.wantReached, detail)
			}
			if !reached && !strings.Contains(detail, "no handler marker") {
				t.Errorf("detail should name the missing marker, got %q", detail)
			}
		})
	}
}

// An unreachable probe target is INCONCLUSIVE, not proof of interception: the
// request has to leave the container, reach the edge and hairpin back, and that
// can fail for reasons that have nothing to do with bot protection. Crying wolf
// here would train the operator to ignore the one alert that matters.
func TestProbeTreatsTransportFailureAsInconclusive(t *testing.T) {
	d := newTestDetector(t, &recordingSink{})
	reached, detail := d.probeOne("http://127.0.0.1:1/livedetect/twitch/eventsub")

	if !reached {
		t.Fatalf("a transport failure must not be reported as interception (detail: %s)", detail)
	}
	if !strings.Contains(detail, "inconclusive") {
		t.Errorf("detail should say inconclusive, got %q", detail)
	}
}

func TestWebSubBackoffGrowsAndCaps(t *testing.T) {
	got := nextWebSubBackoff(0, 0)
	if got != 5*time.Minute {
		t.Fatalf("first backoff without a hub hint = %v, want 5m", got)
	}
	// It must climb, so a persistently sick hub is not hammered...
	for _, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		got = nextWebSubBackoff(got, 0)
		if got != want {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	// ...but it must cap well under the 12h renewal cadence, or a transient
	// outage silently costs most of a day of push coverage.
	got = nextWebSubBackoff(got, 0)
	if got != time.Hour {
		t.Fatalf("backoff = %v, want the 1h cap", got)
	}
	if got = nextWebSubBackoff(got, 0); got != time.Hour {
		t.Fatalf("backoff = %v, want it to stay capped", got)
	}
	if got >= webSubRenewInterval {
		t.Fatalf("a retry backoff of %v is no better than waiting for the normal cadence", got)
	}
}

// Google's hub repeats a static "retry in 2 minutes" however long it has been
// unwell, so obeying it literally means retrying every two minutes forever
// against a service that is explicitly overloaded. It is a floor, not the
// whole answer.
func TestWebSubBackoffEscalatesPastAStaticRetryAfter(t *testing.T) {
	const hint = 2 * time.Minute

	// The first delay takes the hub at its word rather than waiting our longer
	// default, so a genuinely brief overload costs only what it should.
	got := nextWebSubBackoff(0, hint)
	if got != hint {
		t.Fatalf("first delay = %v, want the hub's %v", got, hint)
	}
	// Consecutive failures escalate anyway.
	for _, want := range []time.Duration{4 * time.Minute, 8 * time.Minute, 16 * time.Minute} {
		got = nextWebSubBackoff(got, hint)
		if got != want {
			t.Fatalf("backoff = %v, want %v; a static hint must not pin the retry rate", got, want)
		}
	}
	// And the hub's floor is still respected if our schedule would go sooner.
	if got := nextWebSubBackoff(0, 45*time.Minute); got != 45*time.Minute {
		t.Fatalf("backoff = %v, want the hub's longer floor honoured", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 10, 6, 44, 56, 0, time.UTC)

	// The form Google's hub actually sends.
	if got := parseRetryAfter("120", now); got != 2*time.Minute {
		t.Errorf("delay-seconds form: got %v, want 2m", got)
	}
	// The HTTP-date form the spec also permits.
	if got := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got < 80*time.Second || got > 90*time.Second {
		t.Errorf("http-date form: got %v, want about 90s", got)
	}
	// Absent, unparseable, or already elapsed all mean "no guidance".
	for _, v := range []string{"", "   ", "soon", "0", "-5", now.Add(-time.Hour).Format(http.TimeFormat)} {
		if got := parseRetryAfter(v, now); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", v, got)
		}
	}
}

// The hub knows when it will be well; a short overload should not cost us the
// full invented backoff. But its guidance still has to be bounded.
func TestClampHubRetry(t *testing.T) {
	if got := clampHubRetry(2 * time.Minute); got != 2*time.Minute {
		t.Errorf("a sane value must pass through, got %v", got)
	}
	if got := clampHubRetry(time.Second); got != 30*time.Second {
		t.Errorf("a tiny value must be floored, got %v", got)
	}
	if got := clampHubRetry(48 * time.Hour); got != time.Hour {
		t.Errorf("an absurd value must be capped, got %v", got)
	}
	if got := clampHubRetry(0); got != 0 {
		t.Errorf("no guidance must stay zero so the backoff is used, got %v", got)
	}
}

// The hub answers an overload with 503 + Retry-After rather than a transport
// failure, so that has to survive as structured data rather than a string.
func TestWebSubHTTPErrorCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewWebSubClient("secret")
	c.HubURL = srv.URL

	err := c.Subscribe(context.Background(), "UCaaaaaaaaaaaaaaaaaaaaaa", "https://example.test/cb")
	if err == nil {
		t.Fatal("expected an error for a 503")
	}
	var httpErr *WebSubHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error %v is not a *WebSubHTTPError; the retry hint would be lost", err)
	}
	if httpErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", httpErr.Status)
	}
	if httpErr.RetryAfter != 2*time.Minute {
		t.Errorf("retryAfter = %v, want 2m", httpErr.RetryAfter)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("the message should mention the retry hint, got %q", err.Error())
	}
}

// A partial failure must retry only what failed. Re-subscribing a channel that
// already succeeded achieves nothing and adds load to a hub that has just asked
// us to back off.
func TestWebSubRetriesOnlyTheFailedChannels(t *testing.T) {
	var mu sync.Mutex
	var attempts []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		topic := r.Form.Get("hub.topic")
		mu.Lock()
		attempts = append(attempts, topic)
		mu.Unlock()

		// One channel succeeds; the rest are refused with a retry hint.
		if strings.Contains(topic, "UCgood") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://example.test",
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", WebSub: true, WebSubSecret: "s",
		},
	}, []config.ChannelConfig{
		{Name: "good", YouTubeChannelId: "UCgoodaaaaaaaaaaaaaaaaaa"},
		{Name: "bad", YouTubeChannelId: "UCbadaaaaaaaaaaaaaaaaaaa"},
	}, &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	d.websub.HubURL = srv.URL

	failed, retryAfter := d.webSubRenewOnce(nil)
	if len(failed) != 1 || !strings.Contains(failed[0], "UCbad") {
		t.Fatalf("failed = %v, want just the failing channel", failed)
	}
	if retryAfter != 2*time.Minute {
		t.Errorf("retryAfter = %v, want the hub's 2m", retryAfter)
	}

	// The retry pass touches only the failure.
	mu.Lock()
	attempts = nil
	mu.Unlock()

	d.webSubRenewOnce(failed)

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 {
		t.Fatalf("retry made %d requests, want 1 (only the failed channel)", len(attempts))
	}
	if !strings.Contains(attempts[0], "UCbad") {
		t.Errorf("retry hit %q, want the failed channel", attempts[0])
	}
}

// Shutdown cancels whatever call is in flight. That is an expected consequence
// of stopping, not a fault - reporting it produces a burst of ERROR lines and
// can fire a Discord alert about a server that is merely exiting.
func TestShutdownCancellationIsNotAFailure(t *testing.T) {
	d := newTestDetector(t, &recordingSink{})

	d.recordFailure(MechanismTwitchPoll, errors.New("before shutdown"))
	d.mu.Lock()
	before := d.health[MechanismTwitchPoll].failures
	d.mu.Unlock()
	if before != 1 {
		t.Fatalf("a real failure should count, got %d", before)
	}

	d.cancel() // as Close does

	for range 10 {
		d.recordFailure(MechanismTwitchPoll, errors.New("context canceled"))
	}
	d.mu.Lock()
	after := d.health[MechanismTwitchPoll].failures
	alerted := d.health[MechanismTwitchPoll].alerted
	d.mu.Unlock()

	if after != before {
		t.Errorf("failures went %d -> %d during shutdown; cancellation must not count", before, after)
	}
	if alerted {
		t.Error("shutdown must never raise an alert")
	}
}

// The hub is overloaded as a whole, not per channel. Once several in a row have
// refused, working through the rest adds load to a service that has already
// asked us to stop and delays our own backoff by 20s a channel.
func TestWebSubPassAbortsWhenTheHubRefusesEverything(t *testing.T) {
	var mu sync.Mutex
	requests := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	var channels []config.ChannelConfig
	for i := range 6 {
		channels = append(channels, config.ChannelConfig{
			Name:             "ch" + string(rune('a'+i)),
			YouTubeChannelId: "UC" + string(rune('a'+i)) + "aaaaaaaaaaaaaaaaaaaaa",
		})
	}
	d, err := New(config.LiveDetectConfig{
		Enabled:       true,
		PublicBaseURL: "https://example.test",
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", WebSub: true, WebSubSecret: "s",
		},
	}, channels, &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	d.websub.HubURL = srv.URL

	failed, retryAfter := d.webSubRenewOnce(nil)

	mu.Lock()
	got := requests
	mu.Unlock()

	if got != webSubAbortAfterConsecutiveFailures {
		t.Errorf("made %d requests, want %d before giving up on the pass",
			got, webSubAbortAfterConsecutiveFailures)
	}
	// Everything still needs retrying, including what was never attempted.
	if len(failed) != len(channels) {
		t.Errorf("failed = %d channels, want all %d marked for retry", len(failed), len(channels))
	}
	if retryAfter != 2*time.Minute {
		t.Errorf("retryAfter = %v, want the hub's 2m", retryAfter)
	}
}

// A safety net you cannot see is indistinguishable from one that never ran.
// The audit only speaks every three hours, so the quiet case is exactly the one
// that has to be confirmable.
func TestSearchAuditReportsEvenWhenItFindsNothing(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// No live broadcasts: the normal, quiet case.
		w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)

	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{
			Enabled: true, ApiKey: "k", SearchAudit: true,
		},
	}, testChannels(), &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	d.youtube.BaseURL = srv.URL

	d.searchAuditOnce()

	if calls != 1 {
		t.Fatalf("made %d search calls, want 1 per configured channel", calls)
	}
	// It must draw on the separate search bucket, never the unit budget.
	snap := d.gov.Snapshot(time.Now())
	if snap.SearchSpent != 1 {
		t.Errorf("searchSpent = %d, want 1", snap.SearchSpent)
	}
	if snap.UnitsSpent != 0 {
		t.Errorf("unitsSpent = %d; the audit must not touch the shared unit budget", snap.UnitsSpent)
	}
	// And a clean pass marks the leg healthy rather than leaving it "idle".
	d.mu.Lock()
	lastSuccess := d.health[MechanismYouTubeAudit].lastSuccess
	d.mu.Unlock()
	if lastSuccess.IsZero() {
		t.Error("a successful audit must record leg health")
	}
}
