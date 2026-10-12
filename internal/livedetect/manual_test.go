package livedetect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Video ids for the manual-check tests. A check validates the id's shape, so
// unlike the discovery tests' ids these are real-looking 11-character ones.
const (
	manualDokiVideo  = "dokiVideo01"
	manualMintVideo  = "secondVid01"
	manualStranger   = "strangerV01"
	manualStrangerCh = "UCzzzzzzzzzzzzzzzzzzzzzz"
)

// manualTestDetector builds a YouTube-only detector for the two members
// channels against baseURL and starts the discovery loop, with its own first
// pass pushed an hour out so only the test's requests drive it.
func manualTestDetector(t *testing.T, sink Sink, baseURL string) *Detector {
	t.Helper()
	d := membersDetector(t, sink, baseURL)
	d.discoveryStartDelay = time.Hour
	d.spawn(d.runYouTubeDiscovery)
	deadline := time.Now().Add(5 * time.Second)
	for !d.ytDiscoveryRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the discovery loop never started")
		}
		time.Sleep(time.Millisecond)
	}
	return d
}

// gatedAPI serves api, but holds every request at a gate until the test opens
// it, so a test can catch the discovery loop in the middle of a pass. arrivals
// counts requests that reached the gate.
type gatedAPI struct {
	URL      string
	gate     chan struct{}
	arrivals atomic.Int32
}

func newGatedAPI(t *testing.T, api *membersFakeAPI) *gatedAPI {
	t.Helper()
	g := &gatedAPI{gate: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.arrivals.Add(1)
		select {
		case <-g.gate:
		case <-r.Context().Done():
			return
		}
		api.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	// Registered after the server, so it runs before srv.Close: a test that
	// fails with the gate shut must not leave Close waiting on held requests.
	t.Cleanup(g.open)
	g.URL = srv.URL
	return g
}

func (g *gatedAPI) open() {
	select {
	case <-g.gate:
	default:
		close(g.gate)
	}
}

func (g *gatedAPI) waitArrivals(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.arrivals.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d requests reached the API, want %d", g.arrivals.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// dokiVideo is a videos.list item on doki's channel.
func dokiVideo(id, lbc string, started time.Time) YTVideo {
	now := time.Now()
	return membersBroadcast(id, lbc, now.Add(-24*time.Hour), now.Add(-3*time.Hour), started)
}

// The property everything else rests on: a request is served by the discovery
// goroutine, so it waits for a pass in progress instead of running beside it.
// Run where the HTTP request arrived, a check would read and write
// ytMembersAbsent concurrently with discovery, and a concurrent map write
// kills the process.
func TestManualRequestsRunOnTheDiscoveryGoroutine(t *testing.T) {
	api, _ := newMembersFakeAPI(t)
	api.setPlaylist(UploadsPlaylistID(membersDoki), "fresh-upload")
	api.setVideo(dokiVideo(manualDokiVideo, "upcoming", time.Time{}))
	g := newGatedAPI(t, api)
	d := manualTestDetector(t, &recordingSink{}, g.URL)

	type passResult struct {
		summary DiscoverySummary
		err     error
	}
	passDone := make(chan passResult, 1)
	go func() {
		s, err := d.RunDiscoveryNow(context.Background(), "doki")
		passDone <- passResult{s, err}
	}()
	// The pass is now held inside its first playlist read.
	g.waitArrivals(t, 1)

	type checkResult struct {
		check VideoCheck
		err   error
	}
	checkDone := make(chan checkResult, 1)
	go func() {
		c, err := d.CheckYouTubeVideo(context.Background(), "doki", manualDokiVideo)
		checkDone <- checkResult{c, err}
	}()
	select {
	case r := <-checkDone:
		t.Fatalf("the check finished (%+v) while a discovery pass held the discovery goroutine", r)
	case <-time.After(300 * time.Millisecond):
	}
	if n := g.arrivals.Load(); n != 1 {
		t.Fatalf("%d requests reached the API, want only the held one: the check ran beside the pass", n)
	}

	g.open()
	var pass passResult
	select {
	case pass = <-passDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the manual pass never finished")
	}
	if pass.err != nil {
		t.Fatalf("RunDiscoveryNow: %v", pass.err)
	}
	if got := pass.summary.Found["doki"]; !slices.Equal(got, []string{"fresh-upload"}) {
		t.Errorf("pass found %v for doki, want [fresh-upload]", got)
	}

	var check checkResult
	select {
	case check = <-checkDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the check never finished after the pass released the goroutine")
	}
	if check.err != nil {
		t.Fatalf("CheckYouTubeVideo: %v", check.err)
	}
	if !check.check.Tracked || !d.watch.Known(manualDokiVideo) {
		t.Error("the check must leave the video on the watchlist")
	}
}

// A caller is bounded by its own context even while the loop is busy, so an
// admin who closes the page does not leave a handler parked for 45 seconds.
func TestManualRequestRespectsTheCallersContext(t *testing.T) {
	api, _ := newMembersFakeAPI(t)
	g := newGatedAPI(t, api)
	d := manualTestDetector(t, &recordingSink{}, g.URL)

	go d.RunDiscoveryNow(context.Background(), "doki")
	g.waitArrivals(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s for a 100ms deadline", waited)
	}
}

// Without a running discovery loop there is nobody to serve a request, and the
// caller must hear so at once rather than wait out the timeout.
func TestManualControlsAnswerAtOnceWithoutTheLeg(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, d *Detector, want error) {
		t.Helper()
		start := time.Now()
		if _, err := d.RunDiscoveryNow(ctx, "doki"); !errors.Is(err, want) {
			t.Errorf("RunDiscoveryNow err = %v, want %v", err, want)
		}
		if _, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo); !errors.Is(err, want) {
			t.Errorf("CheckYouTubeVideo err = %v, want %v", err, want)
		}
		if waited := time.Since(start); waited > time.Second {
			t.Errorf("took %s to refuse; a missing leg must answer immediately", waited)
		}
	}

	t.Run("detection disabled", func(t *testing.T) {
		var d *Detector
		check(t, d, ErrDetectionDisabled)
		if d.WatchesYouTube("doki") {
			t.Error("a nil detector watches nothing")
		}
	})
	t.Run("twitch only", func(t *testing.T) {
		d := newTestDetector(t, &recordingSink{})
		check(t, d, ErrYouTubeNotRunning)
		if d.WatchesYouTube("doki") {
			t.Error("a channel is not watched on YouTube while the YouTube leg is off")
		}
	})
	t.Run("youtube leg not started", func(t *testing.T) {
		_, baseURL := newMembersFakeAPI(t)
		d := membersDetector(t, &recordingSink{}, baseURL)
		check(t, d, ErrYouTubeNotRunning)
		if !d.WatchesYouTube("doki") {
			t.Error("doki has a youtubeChannelId and the leg is enabled")
		}
	})
	t.Run("after shutdown", func(t *testing.T) {
		_, baseURL := newMembersFakeAPI(t)
		d := manualTestDetector(t, &recordingSink{}, baseURL)
		d.Close()
		check(t, d, ErrYouTubeNotRunning)
	})
}

// Input the detector can reject on its own is refused before anything is
// queued or counted against the day's checks.
func TestCheckVideoRejectsBadInputWithoutSpending(t *testing.T) {
	_, baseURL := newMembersFakeAPI(t)
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	ctx := context.Background()

	for _, id := range []string{"", "short", "dokiVideo01x", "doki/ideo01", "https://youtu.be/dokiVideo01"} {
		if _, err := d.CheckYouTubeVideo(ctx, "doki", id); !errors.Is(err, ErrInvalidVideoID) {
			t.Errorf("CheckYouTubeVideo(%q) err = %v, want ErrInvalidVideoID", id, err)
		}
	}
	if _, err := d.CheckYouTubeVideo(ctx, "not-a-channel", manualDokiVideo); !errors.Is(err, ErrNoYouTubeChannel) {
		t.Errorf("unknown channel err = %v, want ErrNoYouTubeChannel", err)
	}
	d.manualMu.Lock()
	counted := len(d.manualChecks)
	d.manualMu.Unlock()
	if counted != 0 {
		t.Errorf("manualChecks = %v, want rejected input left uncounted", d.manualChecks)
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 0 {
		t.Errorf("unitsSpent = %d, want 0", spent)
	}
}

// The cross-channel guard. Seeded under doki, a mint stream would be announced
// - and, with queueIncoming, queued - as doki's, because the state poll trusts
// the key an id was seeded under.
func TestCheckVideoRejectsAnotherChannelsVideo(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), manualMintVideo, manualStranger)
	mint := dokiVideo(manualMintVideo, "live", time.Now())
	mint.Snippet.ChannelID = membersMint
	api.setVideo(mint)
	stranger := dokiVideo(manualStranger, "live", time.Now())
	stranger.Snippet.ChannelID = manualStrangerCh
	api.setVideo(stranger)

	d := manualTestDetector(t, &recordingSink{}, baseURL)
	ctx := context.Background()

	cases := []struct {
		id, wantChannelID, wantOwner string
	}{
		{manualMintVideo, membersMint, "mint"},
		{manualStranger, manualStrangerCh, ""},
	}
	for _, tc := range cases {
		_, err := d.CheckYouTubeVideo(ctx, "doki", tc.id)
		var other *OtherChannelError
		if !errors.As(err, &other) {
			t.Fatalf("%s: err = %v, want *OtherChannelError", tc.id, err)
		}
		if other.ChannelID != tc.wantChannelID || other.ChannelKey != tc.wantOwner {
			t.Errorf("%s: refusal = %+v, want channel %s owned by %q", tc.id, other, tc.wantChannelID, tc.wantOwner)
		}
		if strings.Contains(other.Error(), "mint") {
			t.Errorf("message %q names another channel's key", other.Error())
		}
		if d.watch.Known(tc.id) {
			t.Errorf("%s: another channel's video was seeded", tc.id)
		}
	}
	if n := api.count(MembersOnlyPlaylistID(membersDoki)); n != 0 {
		t.Errorf("members-only playlist read %d times; a refused check must stop before it", n)
	}
	// The same video checked from its own channel is fine.
	if c, err := d.CheckYouTubeVideo(ctx, "mint", manualMintVideo); err != nil || !c.Tracked {
		t.Errorf("checked from its own channel: %+v, %v", c, err)
	}
	if got := d.watch.ChannelOf(manualMintVideo); got != "mint" {
		t.Errorf("ChannelOf = %q, want mint", got)
	}
}

func TestCheckVideoNotReturnedIsNotFound(t *testing.T) {
	_, baseURL := newMembersFakeAPI(t)
	d := manualTestDetector(t, &recordingSink{}, baseURL)

	_, err := d.CheckYouTubeVideo(context.Background(), "doki", manualDokiVideo)
	if !errors.Is(err, ErrVideoNotFound) {
		t.Fatalf("err = %v, want ErrVideoNotFound", err)
	}
	if d.watch.Known(manualDokiVideo) {
		t.Error("a video YouTube does not return must not be seeded")
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 1 {
		t.Errorf("unitsSpent = %d, want the one videos.list call", spent)
	}
}

// The incident path end to end: an id hidden in both negative caches is
// checked, comes out of them, goes onto the watchlist due now, and the next
// state poll claims the go-live the ordinary way.
func TestCheckVideoClearsNegativeCachesThenSeeds(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	started := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	api.setVideo(dokiVideo(manualDokiVideo, "live", started))

	d := manualTestDetector(t, sink, baseURL)
	now := time.Now()
	d.watch.mu.Lock()
	d.watch.retired[manualDokiVideo] = now
	d.watch.retiredMembersOnly[manualDokiVideo] = now
	d.watch.mu.Unlock()

	c, err := d.CheckYouTubeVideo(context.Background(), "doki", manualDokiVideo)
	if err != nil {
		t.Fatalf("CheckYouTubeVideo: %v", err)
	}
	if !c.Unretired || c.WasTracked || !c.Tracked {
		t.Errorf("check = %+v, want unretired, newly tracked", c)
	}
	if c.State != "live" || c.ActualStart != started.Unix() || c.ChannelKey != "doki" {
		t.Errorf("check = %+v, want live since %d on doki", c, started.Unix())
	}
	if c.URL != YouTubeWatchURL(manualDokiVideo) || c.Title == "" {
		t.Errorf("check = %+v, want the watch URL and title", c)
	}
	if c.ChecksRemaining != manualChecksPerDay-1 {
		t.Errorf("checksRemaining = %d, want %d", c.ChecksRemaining, manualChecksPerDay-1)
	}
	if d.watch.Retired(manualDokiVideo, time.Now()) || d.watch.RetiredMembersOnly(manualDokiVideo, time.Now()) {
		t.Error("the check must clear both negative caches")
	}
	if due := d.watch.Due(time.Now()); !slices.Contains(due, manualDokiVideo) {
		t.Errorf("due = %v, want the checked video polled immediately", due)
	}

	d.youtubeStateOnce()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.live) != 1 || sink.live[0].ID != manualDokiVideo || sink.live[0].ChannelKey != "doki" {
		t.Fatalf("live = %+v, want the checked video claimed for doki", sink.live)
	}
	// The state poll made the claim, but a video already live when the
	// operator checked it is a miss the operator rescued, and is booked as
	// one rather than as a two-hour state-poll detection.
	if sink.mechs[0] != MechanismYouTubeManual {
		t.Errorf("mechanism = %q, want %q", sink.mechs[0], MechanismYouTubeManual)
	}
}

// Members-only awareness, mirroring discovery: an id on the members-only
// playlist is seeded members-only, so its go-live is not queued for a worker
// that refuses it; a channel without that playlist is backed off exactly as
// discovery backs it off.
func TestCheckVideoSeedsMembersOnly(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "older", manualDokiVideo)
	api.setVideo(dokiVideo(manualDokiVideo, "live", time.Now().Add(-time.Minute)))
	mint := dokiVideo(manualMintVideo, "upcoming", time.Time{})
	mint.Snippet.ChannelID = membersMint
	api.setVideo(mint)
	// mint has no members-only playlist: the fake answers 404.

	d := manualTestDetector(t, sink, baseURL)
	ctx := context.Background()

	c, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo)
	if err != nil {
		t.Fatalf("doki: %v", err)
	}
	if !c.MembersOnly || !c.MembersOnlyChecked {
		t.Errorf("doki check = %+v, want members-only, checked", c)
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 2 {
		t.Errorf("unitsSpent = %d, want videos.list plus the members-only read", spent)
	}
	d.youtubeStateOnce()
	sink.mu.Lock()
	if len(sink.live) != 1 || !sink.live[0].MembersOnly {
		t.Errorf("live = %+v, want the go-live reported as members-only", sink.live)
	}
	sink.mu.Unlock()

	for i := range 2 {
		c, err := d.CheckYouTubeVideo(ctx, "mint", manualMintVideo)
		if err != nil {
			t.Fatalf("mint check %d: %v", i, err)
		}
		if c.MembersOnly || c.MembersOnlyChecked || c.State != "upcoming" {
			t.Errorf("mint check %d = %+v, want public, unchecked, upcoming", i, c)
		}
	}
	if n := api.count(MembersOnlyPlaylistID(membersMint)); n != 1 {
		t.Errorf("mint's members-only playlist read %d times, want once and then the backoff", n)
	}
	// Safe to read here: the loop is parked between requests, and the
	// request's reply ordered its write before this read.
	if a, ok := d.ytMembersAbsent[membersMint]; !ok || a.status != http.StatusNotFound {
		t.Errorf("absence = %+v (%v), want mint backed off on its 404", a, ok)
	}
}

func TestManualDiscoveryIsSpacedAMinuteApart(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(UploadsPlaylistID(membersDoki), "a", "b")
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "m")
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	ctx := context.Background()

	s, err := d.RunDiscoveryNow(ctx, "doki")
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// doki's uploads and members-only, mint's uploads; mint's members-only
	// playlist is a 404, which costs a unit but is not a read or a failure.
	if s.Channels != 2 || s.PlaylistsRead != 3 || s.UnitsSpent != 4 || s.Failures != 0 || s.BudgetRefused {
		t.Errorf("summary = %+v", s)
	}
	if !slices.Equal(s.Found["doki"], []string{"a", "b"}) || !slices.Equal(s.FoundMembersOnly["doki"], []string{"m"}) {
		t.Errorf("found = %v / %v", s.Found, s.FoundMembersOnly)
	}

	_, err = d.RunDiscoveryNow(ctx, "doki")
	var tooSoon *TooSoonError
	if !errors.As(err, &tooSoon) {
		t.Fatalf("second pass err = %v, want *TooSoonError", err)
	}
	if tooSoon.RetryAfter <= 0 || tooSoon.RetryAfter > manualDiscoveryMinInterval {
		t.Errorf("retryAfter = %s, want within the minute", tooSoon.RetryAfter)
	}
	if n := api.count(UploadsPlaylistID(membersDoki)); n != 1 {
		t.Errorf("uploads read %d times, want the refused pass to read nothing", n)
	}

	d.manualMu.Lock()
	d.lastManualDiscovery = time.Now().Add(-manualDiscoveryMinInterval - time.Second)
	d.manualMu.Unlock()
	s, err = d.RunDiscoveryNow(ctx, "doki")
	if err != nil {
		t.Fatalf("pass after the minute: %v", err)
	}
	if len(s.Found) != 0 {
		t.Errorf("found = %v, want nothing new on a second pass", s.Found)
	}
}

// The cap is per channel: one channel's script, or a stuck key, uses up its
// own checks and nobody else's - least of all during the incident another
// channel's operator needs them for.
func TestManualChecksAreCappedPerChannelPerQuotaDay(t *testing.T) {
	_, baseURL := newMembersFakeAPI(t)
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	now := time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)

	for i := range manualChecksPerDay {
		left, err := d.reserveManualCheck("doki", now)
		if err != nil {
			t.Fatalf("check %d: %v", i+1, err)
		}
		if left != manualChecksPerDay-i-1 {
			t.Fatalf("check %d: %d left, want %d", i+1, left, manualChecksPerDay-i-1)
		}
	}
	_, err := d.reserveManualCheck("doki", now)
	var tooSoon *TooSoonError
	if !errors.As(err, &tooSoon) {
		t.Fatalf("check %d err = %v, want *TooSoonError", manualChecksPerDay+1, err)
	}
	if want := 12 * time.Hour; tooSoon.RetryAfter != want {
		t.Errorf("retryAfter = %s, want %s (to 08:00 UTC)", tooSoon.RetryAfter, want)
	}
	if left, err := d.reserveManualCheck("mint", now); err != nil || left != manualChecksPerDay-1 {
		t.Errorf("another channel's first check: %d left, %v; doki's cap must not touch mint", left, err)
	}
	if left, err := d.reserveManualCheck("doki", nextQuotaDayStart(now)); err != nil || left != manualChecksPerDay-1 {
		t.Errorf("first check of the next quota day: %d left, %v", left, err)
	}

	// Through the public call: a capped check spends nothing.
	d.manualMu.Lock()
	d.manualDay = quotaDay(time.Now())
	d.manualChecks = map[string]int{"doki": manualChecksPerDay}
	d.manualPasses = map[string]int{}
	d.manualMu.Unlock()
	if _, err := d.CheckYouTubeVideo(context.Background(), "doki", manualDokiVideo); !errors.As(err, &tooSoon) {
		t.Fatalf("err = %v, want *TooSoonError", err)
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 0 {
		t.Errorf("unitsSpent = %d, want a capped check to spend nothing", spent)
	}
}

// An exhausted unit budget is a wait until the reset, not an API failure.
func TestCheckVideoRespectsTheUnitBudget(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setVideo(dokiVideo(manualDokiVideo, "live", time.Now()))
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	d.gov.BlockUnits(time.Now())

	_, err := d.CheckYouTubeVideo(context.Background(), "doki", manualDokiVideo)
	var tooSoon *TooSoonError
	if !errors.As(err, &tooSoon) || tooSoon.RetryAfter <= 0 {
		t.Fatalf("err = %v, want *TooSoonError until the quota day ends", err)
	}
	if d.watch.Known(manualDokiVideo) {
		t.Error("nothing may be seeded without the lookup")
	}
}

// One bad request must not take discovery down with it: spawn's recover would
// end the loop for the life of the process.
func TestPanicInARequestLeavesTheLoopRunning(t *testing.T) {
	_, baseURL := newMembersFakeAPI(t)
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	ctx := context.Background()

	err := d.onDiscoveryGoroutine(ctx, discoveryRequest{run: func(time.Time) error { panic("boom") }})
	if !errors.Is(err, errManualPanicked) {
		t.Fatalf("err = %v, want errManualPanicked", err)
	}
	if !d.ytDiscoveryRunning.Load() {
		t.Fatal("the discovery loop stopped after a request panicked")
	}
	if _, err := d.RunDiscoveryNow(ctx, "doki"); err != nil {
		t.Errorf("a request after the panic: %v", err)
	}
}

func TestNextQuotaDayStart(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ now, want string }{
		{"2026-10-11T20:03:00Z", "2026-10-12T08:00:00Z"},
		{"2026-10-12T07:59:59Z", "2026-10-12T08:00:00Z"},
		{"2026-10-12T08:00:00Z", "2026-10-13T08:00:00Z"},
		{"2026-10-31T09:00:00Z", "2026-11-01T08:00:00Z"},
		{"2026-12-31T23:00:00Z", "2027-01-01T08:00:00Z"},
		{"2026-10-11T22:00:00+09:00", "2026-10-12T08:00:00Z"},
	}
	for _, tc := range cases {
		got := nextQuotaDayStart(at(tc.now))
		if !got.Equal(at(tc.want)) {
			t.Errorf("nextQuotaDayStart(%s) = %s, want %s", tc.now, got.UTC().Format(time.RFC3339), tc.want)
		}
		// It must be exactly where quotaDay rolls over.
		if quotaDay(got) == quotaDay(got.Add(-time.Nanosecond)) {
			t.Errorf("quotaDay does not change at %s", got.Format(time.RFC3339))
		}
	}
}

func TestTooSoonErrorReadsLikeAWait(t *testing.T) {
	cases := map[time.Duration]string{
		100 * time.Millisecond:                "1s",
		42*time.Second + 300*time.Millisecond: "42s",
		59*time.Minute + 50*time.Second:       "1h",
		3*time.Minute + 10*time.Second:        "3m",
		5 * time.Hour:                         "5h",
		5*time.Hour + 12*time.Minute:          "5h 12m",
	}
	for wait, want := range cases {
		err := &TooSoonError{Reason: "limit", RetryAfter: wait}
		if got := err.Error(); got != "limit; try again in "+want {
			t.Errorf("RetryAfter %s: %q, want %q", wait, got, "limit; try again in "+want)
		}
	}
}

func TestUnretireReportsOnlyWhatWasHiding(t *testing.T) {
	w := newWatchlist()
	now := time.Now()
	w.retired["fresh"] = now.Add(-time.Hour)
	w.retiredMembersOnly["members"] = now.Add(-time.Hour)
	w.retired["expired"] = now.Add(-ytRetiredMemory - time.Minute)

	if !w.Unretire("fresh", now) || !w.Unretire("members", now) {
		t.Error("an id inside the negative-cache window was hiding from discovery")
	}
	if w.Unretire("expired", now) {
		t.Error("an expired entry was hiding nothing")
	}
	if w.Unretire("never", now) {
		t.Error("an id in neither cache was hiding nothing")
	}
	if len(w.retired) != 0 || len(w.retiredMembersOnly) != 0 {
		t.Errorf("caches = %v / %v, want every unretired id gone", w.retired, w.retiredMembersOnly)
	}
}

// nextDueIn reports how long until a tracked entry's next state poll, or a
// negative duration when it is due already. It fails the test for an id that
// is not tracked.
func nextDueIn(t *testing.T, d *Detector, videoID string) time.Duration {
	t.Helper()
	d.watch.mu.Lock()
	defer d.watch.mu.Unlock()
	e, ok := d.watch.entries[videoID]
	if !ok {
		t.Fatalf("%s is not on the watchlist", videoID)
	}
	return time.Until(e.NextDue)
}

// A check must not arm the ten-minute hot boost. It has just read the video's
// state, so one poll settles the entry and the ladder does the rest; a boost
// polled a waiting room every three seconds for ten minutes - about 200 units
// a check, re-armed by every re-check - and fifty checks a day of it were more
// than the whole budget.
func TestCheckVideoDoesNotArmTheHotBoost(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	now := time.Now()
	tomorrow := now.Add(24 * time.Hour)
	api.setVideo(membersBroadcast(manualDokiVideo, "upcoming", now.Add(-time.Hour), tomorrow, time.Time{}))
	d := manualTestDetector(t, sink, baseURL)
	ctx := context.Background()

	// A re-check must not arm what the first check did not arm either.
	for i := range 2 {
		c, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo)
		if err != nil {
			t.Fatalf("check %d: %v", i+1, err)
		}
		if c.State != "upcoming" {
			t.Fatalf("check %d: state = %q, want upcoming", i+1, c.State)
		}
		if boosted(d, manualDokiVideo) {
			t.Fatalf("check %d armed the seed boost: ten minutes of 3-second polls for a frame a day away", i+1)
		}
		if wait := nextDueIn(t, d, manualDokiVideo); wait > 0 {
			t.Errorf("check %d: next poll in %s, want it due at once", i+1, wait)
		}
		d.youtubeStateOnce()
		// One poll settles it, and a frame a day out belongs on the cold rung.
		if wait := nextDueIn(t, d, manualDokiVideo); wait < ytIntervalCold-time.Minute {
			t.Errorf("check %d: next poll in %s after the first, want the cold rung (%s)", i+1, wait, ytIntervalCold)
		}
	}
	if got := d.watch.RescuedBy(manualDokiVideo); got != "" {
		t.Errorf("rescuedBy = %q; checking a frame that has not started rescues nothing", got)
	}

	// When it does go live, that is the state poll's own detection.
	api.setVideo(membersBroadcast(manualDokiVideo, "live", now.Add(-time.Hour), tomorrow, time.Now()))
	membersMakeDue(d, manualDokiVideo)
	d.youtubeStateOnce()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.live) != 1 || sink.mechs[0] != MechanismYouTubeState {
		t.Errorf("live = %+v via %v, want one go-live booked to %s", sink.live, sink.mechs, MechanismYouTubeState)
	}
}

// "Was hidden from discovery" is only news for a video that can still go live.
// Only ended videos are ever retired, so finding an ended one in the cache is
// the cache doing its job; reporting it would send the operator hunting a bug
// that is not there.
func TestCheckVideoReportsUnretiredOnlyForAVideoThatHasNotEnded(t *testing.T) {
	const liveVideo = "liveVideo01"
	api, baseURL := newMembersFakeAPI(t)
	now := time.Now()
	api.setVideo(dokiVideo(manualDokiVideo, "none", now.Add(-3*time.Hour)))
	api.setVideo(dokiVideo(liveVideo, "live", now.Add(-time.Hour)))
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	d.watch.mu.Lock()
	d.watch.retired[manualDokiVideo] = now
	d.watch.retired[liveVideo] = now
	d.watch.mu.Unlock()
	ctx := context.Background()

	c, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo)
	if err != nil {
		t.Fatalf("ended video: %v", err)
	}
	if c.State != "ended" || c.Unretired {
		t.Errorf("ended video check = %+v, want ended and not reported as hidden", c)
	}
	c, err = d.CheckYouTubeVideo(ctx, "doki", liveVideo)
	if err != nil {
		t.Fatalf("live video: %v", err)
	}
	if c.State != "live" || !c.Unretired {
		t.Errorf("live video check = %+v, want live and reported as hidden", c)
	}
}

// The minute's spacing alone let one channel's script run a pass a minute all
// day - twice the default schedule, and with six channels more than the whole
// unit budget - so each channel also has a daily cap of its own, which one
// channel using up leaves every other channel's untouched.
func TestManualDiscoveryIsCappedPerChannelPerQuotaDay(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	d := manualTestDetector(t, &recordingSink{}, baseURL)
	now := time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)

	at := now
	for i := range manualPassesPerDay {
		if err := d.reserveManualDiscovery("doki", at); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		at = at.Add(manualDiscoveryMinInterval)
	}
	err := d.reserveManualDiscovery("doki", at)
	var tooSoon *TooSoonError
	if !errors.As(err, &tooSoon) {
		t.Fatalf("pass %d err = %v, want *TooSoonError", manualPassesPerDay+1, err)
	}
	if want := nextQuotaDayStart(at).Sub(at); tooSoon.RetryAfter != want {
		t.Errorf("retryAfter = %s, want %s (to the quota day's end)", tooSoon.RetryAfter, want)
	}
	if err := d.reserveManualDiscovery("mint", at); err != nil {
		t.Errorf("another channel's pass: %v; doki's cap must not touch mint", err)
	}
	if err := d.reserveManualDiscovery("doki", nextQuotaDayStart(at).Add(manualDiscoveryMinInterval)); err != nil {
		t.Errorf("first pass of the next quota day: %v", err)
	}

	// Through the public call: a capped pass reads nothing.
	d.manualMu.Lock()
	d.manualDay = quotaDay(time.Now())
	d.manualChecks = map[string]int{}
	d.manualPasses = map[string]int{"doki": manualPassesPerDay}
	d.lastManualDiscovery = time.Time{}
	d.manualMu.Unlock()
	if _, err := d.RunDiscoveryNow(context.Background(), "doki"); !errors.As(err, &tooSoon) {
		t.Fatalf("err = %v, want *TooSoonError", err)
	}
	if n := api.count(UploadsPlaylistID(membersDoki)); n != 0 {
		t.Errorf("uploads read %d times, want a capped pass to read nothing", n)
	}
}

// Shutdown stops the HTTP server before it closes the detector, and the HTTP
// server waits for every handler without cancelling its request. So the first
// step of shutdown must release callers parked on the discovery loop, or one
// check stuck behind a slow pass holds the whole shutdown until Docker kills
// the process before the database is closed.
func TestStopManualRequestsReleasesWaitingCallers(t *testing.T) {
	api, _ := newMembersFakeAPI(t)
	g := newGatedAPI(t, api)
	d := manualTestDetector(t, &recordingSink{}, g.URL)
	ctx := context.Background()

	passErr := make(chan error, 1)
	go func() {
		_, err := d.RunDiscoveryNow(ctx, "doki")
		passErr <- err
	}()
	// The pass is now held inside its first playlist read.
	g.waitArrivals(t, 1)
	checkErr := make(chan error, 1)
	go func() {
		_, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo)
		checkErr <- err
	}()
	time.Sleep(50 * time.Millisecond)

	d.StopManualRequests()
	for _, w := range []struct {
		name string
		ch   chan error
	}{{"the pass held by the API", passErr}, {"the check queued behind it", checkErr}} {
		select {
		case err := <-w.ch:
			if !errors.Is(err, ErrYouTubeNotRunning) {
				t.Errorf("%s: err = %v, want ErrYouTubeNotRunning", w.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was still waiting after StopManualRequests", w.name)
		}
	}

	if _, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo); !errors.Is(err, ErrYouTubeNotRunning) {
		t.Errorf("a new request after StopManualRequests: err = %v, want ErrYouTubeNotRunning", err)
	}
	d.StopManualRequests() // idempotent
	var none *Detector
	none.StopManualRequests() // nil-safe
}
