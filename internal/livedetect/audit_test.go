package livedetect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/discord"
)

// Two channels: doki is watched on Twitch and YouTube, mint on YouTube only.
const (
	auditDoki = "UCaaaaaaaaaaaaaaaaaaaaaa"
	auditMint = "UCbbbbbbbbbbbbbbbbbbbbbb"
)

// auditFakeSearch fakes search.list: which video ids each channel is live on,
// and how many times each channel was asked.
type auditFakeSearch struct {
	mu    sync.Mutex
	live  map[string][]string
	calls map[string]int
}

func newAuditFakeSearch(t *testing.T) (*auditFakeSearch, string) {
	t.Helper()
	f := &auditFakeSearch{live: map[string][]string{}, calls: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		channelID := r.URL.Query().Get("channelId")
		f.calls[channelID]++
		type item struct {
			ID struct {
				VideoID string `json:"videoId"`
			} `json:"id"`
		}
		items := []item{}
		for _, id := range f.live[channelID] {
			var it item
			it.ID.VideoID = id
			items = append(items, it)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *auditFakeSearch) setLive(channelID string, videoIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[channelID] = videoIDs
}

func (f *auditFakeSearch) count(channelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[channelID]
}

// auditSink is a recordingSink whose ledger lookups can be scripted: an id in
// claimAfter reads as unclaimed for that many lookups and claimed after, which
// is the state poll winning the race during the audit's grace period.
type auditSink struct {
	recordingSink

	lookupMu   sync.Mutex
	claimAfter map[string]int
	lookups    map[string]int
	err        error
}

func newAuditSink() *auditSink {
	return &auditSink{claimAfter: map[string]int{}, lookups: map[string]int{}}
}

func (s *auditSink) DetectionClaimed(ctx context.Context, platform, broadcastID string) (bool, error) {
	s.lookupMu.Lock()
	s.lookups[broadcastID]++
	n := s.lookups[broadcastID]
	after, scripted := s.claimAfter[broadcastID]
	err := s.err
	s.lookupMu.Unlock()

	if err != nil {
		return false, err
	}
	if scripted && n > after {
		return true, nil
	}
	return s.recordingSink.DetectionClaimed(ctx, platform, broadcastID)
}

func (s *auditSink) lookupCount(id string) int {
	s.lookupMu.Lock()
	defer s.lookupMu.Unlock()
	return s.lookups[id]
}

// auditDetector builds a YouTube detector with the search audit on, against
// the fake. Nothing is started: tests drive the audit directly, and the grace
// period is cut to a millisecond.
func auditDetector(t *testing.T, sink Sink, baseURL string) *Detector {
	t.Helper()
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k", SearchAudit: true},
	}, []config.ChannelConfig{
		{Name: "doki", TwitchLogin: "dokibird", YouTubeChannelId: auditDoki},
		{Name: "mint", YouTubeChannelId: auditMint},
	}, sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	d.youtube.BaseURL = baseURL
	d.auditGrace = time.Millisecond
	return d
}

// boosted reports whether a watchlist entry is inside a seed boost, i.e. the
// audit pulled its next state poll forward.
func boosted(d *Detector, videoID string) bool {
	d.watch.mu.Lock()
	defer d.watch.mu.Unlock()
	e, ok := d.watch.entries[videoID]
	return ok && time.Now().Before(e.BoostUntil)
}

// The audit runs at fixed UTC slots - twenty past every third hour - and
// always strictly after the time it is asked about.
func TestNextSearchAuditAtFallsOnFixedUTCSlots(t *testing.T) {
	if (24*time.Hour)%searchAuditInterval != 0 {
		t.Fatalf("searchAuditInterval %v must divide a day, or the slots drift from one day to the next", searchAuditInterval)
	}
	if searchAuditPhase >= searchAuditInterval {
		t.Fatalf("searchAuditPhase %v must be inside one interval", searchAuditPhase)
	}

	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return v
	}
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// The incident: beri went live at 20:00Z and the old schedule audited
		// at 20:03Z. The next slot gives search an hour and twenty minutes.
		{"incident", utc("2026-10-11T20:03:00Z"), "2026-10-11T21:20:00Z"},
		{"midnight", utc("2026-10-11T00:00:00Z"), "2026-10-11T00:20:00Z"},
		{"just before a slot", utc("2026-10-11T03:19:59.999999999Z"), "2026-10-11T03:20:00Z"},
		{"exactly on a slot", utc("2026-10-11T03:20:00Z"), "2026-10-11T06:20:00Z"},
		{"just after a slot", utc("2026-10-11T03:20:00.000000001Z"), "2026-10-11T06:20:00Z"},
		{"last slot of the day", utc("2026-10-11T21:20:00Z"), "2026-10-12T00:20:00Z"},
		{"day wrap", utc("2026-10-11T23:59:59Z"), "2026-10-12T00:20:00Z"},
		{"year wrap", utc("2026-12-31T22:00:00Z"), "2027-01-01T00:20:00Z"},
		// A local time is the same instant; the slots are UTC regardless.
		{"non-UTC input", time.Date(2026, 10, 11, 13, 3, 0, 0, time.FixedZone("PDT", -7*3600)), "2026-10-11T21:20:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextSearchAuditAt(tc.now)
			if want := utc(tc.want); !got.Equal(want) {
				t.Errorf("nextSearchAuditAt(%s) = %s, want %s", tc.now.Format(time.RFC3339Nano),
					got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
			if got.Location() != time.UTC {
				t.Errorf("location = %v, want UTC", got.Location())
			}
			if gap := got.Sub(tc.now); gap <= 0 || gap > searchAuditInterval {
				t.Errorf("gap to next slot = %v, want within (0, %v]", gap, searchAuditInterval)
			}
		})
	}
}

// Restarts are frequent, and a startup pass spent a search call per channel
// each time. The leg must wait for its slot - and must still stop promptly on
// shutdown while it waits.
func TestSearchAuditDoesNotRunAtStartup(t *testing.T) {
	if time.Until(nextSearchAuditAt(time.Now())) < 5*time.Second {
		t.Skip("a real audit slot is due within the test window")
	}
	fake, url := newAuditFakeSearch(t)
	d := auditDetector(t, newAuditSink(), url)

	d.spawn(d.runSearchAudit)
	time.Sleep(200 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		d.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while the audit was waiting for its slot")
	}
	if n := fake.count(auditDoki) + fake.count(auditMint); n != 0 {
		t.Errorf("made %d search calls at startup, want none before the first slot", n)
	}
}

// With the first slot up to three hours away, the leg must be visible as
// waiting from boot, never as missing or degraded.
func TestSearchAuditLegIsIdleBeforeItsFirstSlot(t *testing.T) {
	_, url := newAuditFakeSearch(t)
	d := auditDetector(t, newAuditSink(), url)

	var found bool
	for _, leg := range d.Status().Legs {
		if leg.Mechanism != MechanismYouTubeAudit {
			continue
		}
		found = true
		if leg.State != "idle" {
			t.Errorf("audit leg state = %q before its first slot, want idle", leg.State)
		}
	}
	if !found {
		t.Error("the audit leg must be listed from boot")
	}

	// And a detector without the audit does not list it at all.
	off, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, []config.ChannelConfig{{Name: "doki", YouTubeChannelId: auditDoki}}, newAuditSink(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { off.Close() })
	for _, leg := range off.Status().Legs {
		if leg.Mechanism == MechanismYouTubeAudit {
			t.Error("the audit leg is listed although the audit is off")
		}
	}
}

// What the ledger claimed is a detection, whatever the watchlist says. It is
// neither alerted nor seeded.
func TestSearchAuditIgnoresABroadcastTheLedgerClaimed(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	d := auditDetector(t, sink, url)

	sink.ObserveLive(context.Background(), Broadcast{Platform: PlatformYouTube, ChannelKey: "doki", ID: "done"}, MechanismYouTubeState)
	fake.setLive(auditDoki, "done")

	report := d.searchAuditOnce()

	if len(report.missed) != 0 {
		t.Errorf("missed = %v, want nothing: the ledger claimed it", report.missed)
	}
	if !slices.Equal(report.live, []string{"doki=done"}) {
		t.Errorf("live = %v, want what search returned", report.live)
	}
	if report.checked != 2 {
		t.Errorf("checked = %d, want both channels", report.checked)
	}
	if d.watch.Known("done") {
		t.Error("a claimed broadcast must not be re-seeded")
	}
}

// An id discovery never surfaced is a miss the moment search shows it, even
// though seeding it means the state poll will claim it seconds later. Waiting
// for that claim would erase the evidence that discovery failed.
func TestSearchAuditAlertsOnAnUntrackedBroadcastWithoutWaiting(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	// The ledger would answer "claimed" to a second lookup.
	sink.claimAfter["surprise"] = 1
	d := auditDetector(t, sink, url)

	// Even an id in the retired cache - the 2026-10-11 failure - is seeded.
	d.watch.mu.Lock()
	d.watch.retired["surprise"] = time.Now()
	d.watch.mu.Unlock()
	fake.setLive(auditDoki, "surprise")

	report := d.searchAuditOnce()

	if !slices.Equal(report.missed, []string{"doki=surprise"}) {
		t.Errorf("missed = %v, want [doki=surprise]", report.missed)
	}
	if n := sink.lookupCount("surprise"); n != 1 {
		t.Errorf("ledger looked up %d times, want 1: an untracked broadcast gets no grace re-check", n)
	}
	if !d.watch.Known("surprise") || !boosted(d, "surprise") {
		t.Error("the missed broadcast must be seeded with a boost so the state poll claims it")
	}
	if got := d.watch.RescuedBy("surprise"); got != MechanismYouTubeAudit {
		t.Errorf("rescuedBy = %q, want the claim booked as the audit's rescue", got)
	}
}

// A tracked broadcast the ledger has not claimed yet gets a boosted poll and a
// grace period. Claimed in that window, it is no miss; still unclaimed after
// it, it is - and one wait covers every such broadcast in the pass.
func TestSearchAuditGivesATrackedBroadcastAGracePeriod(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.claimAfter["racing"] = 1 // the state poll claims it during the grace
	d := auditDetector(t, sink, url)

	now := time.Now()
	d.watch.Seed("racing", "doki", now, false)
	d.watch.Seed("stuck", "mint", now, false)
	if boosted(d, "racing") || boosted(d, "stuck") {
		t.Fatal("test setup: entries must start unboosted")
	}
	fake.setLive(auditDoki, "racing")
	fake.setLive(auditMint, "stuck")

	report := d.searchAuditOnce()

	if !slices.Equal(report.missed, []string{"mint=stuck"}) {
		t.Errorf("missed = %v, want only [mint=stuck]", report.missed)
	}
	if !slices.Equal(report.claimedInGrace, []string{"doki=racing"}) {
		t.Errorf("claimedInGrace = %v, want [doki=racing]", report.claimedInGrace)
	}
	for _, id := range []string{"racing", "stuck"} {
		if n := sink.lookupCount(id); n != 2 {
			t.Errorf("%s: ledger looked up %d times, want 2 (before and after the grace)", id, n)
		}
		if !boosted(d, id) {
			t.Errorf("%s: a tracked, unclaimed broadcast must be re-seeded with a boost", id)
		}
	}
	// Only the one the audit paged about is its rescue; the one claimed in
	// the grace was the state poll's own detection.
	if got := d.watch.RescuedBy("stuck"); got != MechanismYouTubeAudit {
		t.Errorf("stuck: rescuedBy = %q, want %q", got, MechanismYouTubeAudit)
	}
	if got := d.watch.RescuedBy("racing"); got != "" {
		t.Errorf("racing: rescuedBy = %q, want none", got)
	}
}

// Shutdown must not wait out the grace period, and must not alert on what it
// did not get to re-check.
func TestSearchAuditStopsDuringTheGraceOnShutdown(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	d := auditDetector(t, sink, url)
	d.auditGrace = time.Hour

	d.watch.Seed("stuck", "doki", time.Now(), false)
	fake.setLive(auditDoki, "stuck")

	done := make(chan searchAuditReport, 1)
	go func() { done <- d.searchAuditOnce() }()

	// The second channel's search follows the first lookup, so wait for both
	// searches before shutting down.
	deadline := time.Now().Add(5 * time.Second)
	for fake.count(auditMint) == 0 || sink.lookupCount("stuck") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the audit never reached its grace period")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.Close()

	select {
	case report := <-done:
		if len(report.missed) != 0 {
			t.Errorf("missed = %v, want nothing alerted during shutdown", report.missed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the audit kept waiting out its grace period after shutdown")
	}
}

// A channel the ledger already holds a live YouTube broadcast for is skipped
// to save its search call. A live Twitch broadcast does not count: doki is
// watched on both, and a YouTube stream alongside a Twitch one must still be
// audited.
func TestSearchAuditSkipsAChannelLiveOnYouTubeButNotOnTwitch(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.active = []Broadcast{
		{Platform: PlatformTwitch, ChannelKey: "doki", ID: "314159"},
		{Platform: PlatformYouTube, ChannelKey: "mint", ID: "mintlive"},
	}
	d := auditDetector(t, sink, url)
	// As Start does: the ledger's live YouTube broadcasts go back on the
	// watchlist.
	d.reseedFromLedger()

	report := d.searchAuditOnce()

	if n := fake.count(auditDoki); n != 1 {
		t.Errorf("doki searched %d times, want 1: a Twitch live must not skip YouTube", n)
	}
	if n := fake.count(auditMint); n != 0 {
		t.Errorf("mint searched %d times, want 0: it is already live on YouTube", n)
	}
	if !slices.Equal(report.skipped, []string{"mint=mintlive"}) {
		t.Errorf("skipped = %v, want [mint=mintlive]", report.skipped)
	}
	if spent := d.gov.Snapshot(time.Now()).SearchSpent; spent != 1 {
		t.Errorf("searchSpent = %d, want 1", spent)
	}
}

// A ledger row says live but the watchlist has let the id go - deleted while
// live, say - so nothing will ever end the row. Trusting it would exempt the
// channel from the audit for as long as it stands.
func TestSearchAuditDoesNotTrustALedgerRowTheWatchlistDropped(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.active = []Broadcast{{Platform: PlatformYouTube, ChannelKey: "doki", ID: "gone"}}
	d := auditDetector(t, sink, url)
	// No reseed: the id is not on the watchlist.

	report := d.searchAuditOnce()

	if n := fake.count(auditDoki); n != 1 {
		t.Errorf("doki searched %d times, want 1", n)
	}
	if len(report.skipped) != 0 {
		t.Errorf("skipped = %v, want nothing", report.skipped)
	}
}

// A pass in which every channel is already live on YouTube asks search
// nothing, but it ran: the leg must not later read as gone quiet.
func TestSearchAuditCountsAnAllLivePassAsHealthy(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.active = []Broadcast{
		{Platform: PlatformYouTube, ChannelKey: "doki", ID: "dokilive"},
		{Platform: PlatformYouTube, ChannelKey: "mint", ID: "mintlive"},
	}
	d := auditDetector(t, sink, url)
	d.reseedFromLedger()

	report := d.searchAuditOnce()

	if n := fake.count(auditDoki) + fake.count(auditMint); n != 0 {
		t.Errorf("made %d search calls, want none", n)
	}
	if report.checked != 0 || len(report.skipped) != 2 {
		t.Errorf("checked = %d, skipped = %v; want 0 and both channels", report.checked, report.skipped)
	}
	d.mu.Lock()
	lastSuccess := d.health[MechanismYouTubeAudit].lastSuccess
	d.mu.Unlock()
	if lastSuccess.IsZero() {
		t.Error("a pass that skipped every channel as live must still count as a successful cycle")
	}
}

// A ledger that cannot be read cannot tell a miss from a detection. Paging on
// a database hiccup would teach the operator to ignore the real alert, so
// nothing is alerted - but an id the watchlist does not know is still seeded.
// That seed is the half of the audit that rescues a stream discovery missed;
// dropping it with the alert left such a stream undetected until the next slot
// three hours later. A tracked id is left to the state poll as it is.
func TestSearchAuditStillSeedsWhenTheLedgerCannotBeRead(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.err = errors.New("database is locked")
	d := auditDetector(t, sink, url)
	d.watch.Seed("tracked", "mint", time.Now(), false)
	fake.setLive(auditDoki, "unknowable")
	fake.setLive(auditMint, "tracked")

	report := d.searchAuditOnce()

	if len(report.missed) != 0 {
		t.Errorf("missed = %v, want nothing alerted on a ledger error", report.missed)
	}
	if !d.watch.Known("unknowable") || !boosted(d, "unknowable") {
		t.Error("an untracked live id must still be seeded with a boost when the ledger cannot be read")
	}
	if got := d.watch.RescuedBy("unknowable"); got != "" {
		t.Errorf("rescuedBy = %q; nothing was paged, so the claim is not the audit's to book", got)
	}
	if boosted(d, "tracked") {
		t.Error("a tracked id must be left as it was: nothing is known about it that the state poll does not know")
	}
	if !slices.Equal(report.live, []string{"doki=unknowable", "mint=tracked"}) {
		t.Errorf("live = %v; what search saw must still be logged", report.live)
	}
}

// The state poll makes every YouTube claim, but one an audit or an operator
// rescued is booked under the rescuer. Its delay measures how long the miss
// lasted, not how fast the ladder is, and booked as youtube-state-poll it
// would bury that leg's real numbers under hours-long samples.
func TestStatePollBooksARescuedClaimUnderItsRescuer(t *testing.T) {
	sink := &recordingSink{}
	_, url := newAuditFakeSearch(t)
	d := auditDetector(t, sink, url)
	now := time.Now()
	started := now.Add(-2 * time.Hour)

	d.watch.SeedRescue("rescued", "doki", now, MechanismYouTubeAudit)
	d.watch.SeedDueNow("checked", "doki", now, false, MechanismYouTubeManual)
	d.watch.Seed("ordinary", "doki", now, true)
	for _, id := range []string{"rescued", "checked", "ordinary"} {
		v := membersBroadcast(id, "live", started, time.Time{}, started)
		v.Snippet.ChannelID = auditDoki
		d.applyYouTubeVideo(context.Background(), v, now)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	got := map[string]string{}
	for i, b := range sink.live {
		got[b.ID] = sink.mechs[i]
	}
	want := map[string]string{
		"rescued":  MechanismYouTubeAudit,
		"checked":  MechanismYouTubeManual,
		"ordinary": MechanismYouTubeState,
	}
	for id, mech := range want {
		if got[id] != mech {
			t.Errorf("%s booked as %q, want %q", id, got[id], mech)
		}
	}
}

// captureAuditAlerts points the detector's operator alerts at a test webhook
// and returns a channel of the decoded payloads. Posts are sent from a
// goroutine, so tests must receive rather than assert immediately.
func captureAuditAlerts(t *testing.T, d *Detector) chan map[string]any {
	t.Helper()
	payloads := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode alert payload: %v", err)
		}
		payloads <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	d.alerts = discord.NewClient(config.DiscordConfig{DetectWebhookURL: srv.URL}, "test", []config.ChannelConfig{
		{Name: "doki", DisplayName: "Dokibird"}, {Name: "mint", DisplayName: "Mint"},
	})
	return payloads
}

// alertDescription returns the single embed's description of an alert.
func alertDescription(t *testing.T, payload map[string]any) string {
	t.Helper()
	embeds, ok := payload["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds = %#v, want exactly one", payload["embeds"])
	}
	embed, _ := embeds[0].(map[string]any)
	desc, _ := embed["description"].(string)
	return desc
}

// Which alert goes out is the diagnosis, so it is pinned. An untracked live
// broadcast is a discovery miss, paged at once. A tracked one still unclaimed
// after the grace is a failure between the watchlist and the ledger, paged
// after it. One claimed during the grace was detected, and pages nothing.
func TestSearchAuditSendsTheAlertThatNamesTheFailure(t *testing.T) {
	fake, url := newAuditFakeSearch(t)
	sink := newAuditSink()
	sink.claimAfter["racing"] = 1
	d := auditDetector(t, sink, url)
	alerts := captureAuditAlerts(t, d)

	now := time.Now()
	d.watch.Seed("racing", "doki", now, false)
	d.watch.Seed("stuck", "mint", now, false)
	fake.setLive(auditDoki, "surprise", "racing")
	fake.setLive(auditMint, "stuck")

	d.searchAuditOnce()

	byVideo := map[string]string{}
	for range 2 {
		select {
		case p := <-alerts:
			desc := alertDescription(t, p)
			for _, id := range []string{"surprise", "racing", "stuck"} {
				if strings.Contains(desc, "watch?v="+id+")") {
					byVideo[id] = desc
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for the audit's alerts; got %v", byVideo)
		}
	}
	select {
	case p := <-alerts:
		t.Errorf("unexpected third alert: %s", alertDescription(t, p))
	case <-time.After(300 * time.Millisecond):
	}

	if desc := byVideo["surprise"]; !strings.Contains(desc, "**Dokibird**") || !strings.Contains(desc, "Discovery is not surfacing") {
		t.Errorf("untracked broadcast alert = %q, want the discovery-miss alert", desc)
	}
	if desc := byVideo["stuck"]; !strings.Contains(desc, "**Mint**") || !strings.Contains(desc, "the state poll did not confirm it") {
		t.Errorf("tracked, unclaimed broadcast alert = %q, want the never-claimed alert", desc)
	}
	if desc, ok := byVideo["racing"]; ok {
		t.Errorf("a broadcast claimed during the grace was alerted: %q", desc)
	}
}
