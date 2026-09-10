package livedetect

import (
	"context"
	"errors"
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

func (s *recordingSink) ActiveBroadcasts(context.Context) ([]Broadcast, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Broadcast(nil), s.active...), nil
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
	d.SeedVideo("v", "doki")
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
// broadcast ending , which also suppresses the restart re-poll.
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
// must never be adopted as healthy , that was the path by which a webhook
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
// carries a path. The callbacks must keep that prefix , a third party dials the
// public URL, not the container's , while the reconciler's ownership check
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

// A malformed identifier fails SILENTLY at the API , Helix answers an unknown
// login with an empty array, and the Data API an unknown channel with no items
// , so a typo would read as "never goes live" indefinitely. It must be caught
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
