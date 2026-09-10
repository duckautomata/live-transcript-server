package livedetect

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// The ladder is a decaying plateau, not a spike around the scheduled time,
// because "scheduled 19:00, actually starts 21:30" is the common case. A late
// stream must never fall back to minutes inside its first few hours.
func TestWatchEntryIntervalLadder(t *testing.T) {
	sched := base
	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"a day out", sched.Add(-24 * time.Hour), ytIntervalCold},
		{"three hours out", sched.Add(-3 * time.Hour), ytIntervalCool},
		{"thirty minutes out", sched.Add(-30 * time.Minute), ytIntervalApproach},
		{"five minutes out", sched.Add(-5 * time.Minute), ytIntervalHot},
		{"right on time", sched, ytIntervalHot},
		{"ten minutes late", sched.Add(10 * time.Minute), ytIntervalHot},
		{"half an hour late", sched.Add(30 * time.Minute), ytIntervalWarm},
		{"two hours late", sched.Add(2 * time.Hour), ytIntervalNear},
		{"four hours late", sched.Add(4 * time.Hour), ytIntervalSoon},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &watchEntry{Scheduled: sched, FirstSeen: sched.Add(-48 * time.Hour), State: StateUpcoming}
			if got := e.interval(tc.now); got != tc.want {
				t.Errorf("interval = %v, want %v", got, tc.want)
			}
		})
	}
}

// The 150-minute overrun is the scenario the plateau exists for: detection
// must still be within ten seconds when the stream finally starts.
func TestLateStartStaysFast(t *testing.T) {
	e := &watchEntry{Scheduled: base, FirstSeen: base.Add(-6 * time.Hour), State: StateUpcoming}
	if got := e.interval(base.Add(150 * time.Minute)); got > 10*time.Second {
		t.Fatalf("interval %v at 2.5h past schedule; a late start must still be caught within seconds", got)
	}
}

func TestUnscheduledEntryStartsHotThenDecays(t *testing.T) {
	e := &watchEntry{FirstSeen: base, State: StateUpcoming}
	if got := e.interval(base.Add(time.Minute)); got != ytIntervalHot {
		t.Errorf("a just-discovered id must be polled hot, got %v", got)
	}
	if got := e.interval(base.Add(30 * time.Minute)); got != ytIntervalApproach {
		t.Errorf("interval at 30m = %v", got)
	}
	if got := e.interval(base.Add(3 * time.Hour)); got != ytIntervalCool {
		t.Errorf("interval at 3h = %v", got)
	}
}

func TestBoostOverridesTheLadder(t *testing.T) {
	// A push arriving for a stream scheduled days out must still be checked
	// immediately: the push is evidence the schedule is stale.
	e := &watchEntry{Scheduled: base.Add(48 * time.Hour), FirstSeen: base, State: StateUpcoming}
	if got := e.interval(base); got != ytIntervalCold {
		t.Fatalf("precondition: expected a cold interval, got %v", got)
	}
	e.BoostUntil = base.Add(10 * time.Minute)
	if got := e.interval(base); got != ytIntervalHot {
		t.Fatalf("a boosted entry must poll hot, got %v", got)
	}
}

func TestWatchlistDueAndObserve(t *testing.T) {
	w := newWatchlist()
	w.Seed("v1", "doki", base, false)

	if due := w.Due(base); len(due) != 1 || due[0] != "v1" {
		t.Fatalf("a freshly seeded id must be immediately due, got %v", due)
	}
	w.Observe("v1", StateLive, time.Time{}, base)
	if due := w.Due(base); len(due) != 0 {
		t.Fatalf("an id just observed must not be due again, got %v", due)
	}
	if due := w.Due(base.Add(ytIntervalLive + time.Second)); len(due) != 1 {
		t.Fatal("a live entry must come due again after its interval")
	}
}

func TestWatchlistDueRespectsBatchCap(t *testing.T) {
	w := newWatchlist()
	for i := range videosListBatchSize + 20 {
		w.Seed(string(rune('a'+i%26))+string(rune('a'+i/26)), "doki", base, false)
	}
	if due := w.Due(base); len(due) > videosListBatchSize {
		t.Fatalf("Due returned %d ids, more than videos.list accepts in one call", len(due))
	}
}

// Every non-success exit path defers, because an entry left permanently
// overdue makes NextWait return its floor forever and spins the poll loop.
func TestWatchlistDeferPreventsBusyLoop(t *testing.T) {
	w := newWatchlist()
	w.Seed("v1", "doki", base, false)

	if got := w.NextWait(base); got > time.Second {
		t.Fatalf("precondition: an overdue entry should want an immediate poll, got %v", got)
	}
	w.Defer([]string{"v1"}, base.Add(time.Minute))
	if got := w.NextWait(base); got < 30*time.Second {
		t.Fatalf("after Defer the loop must sleep, got %v", got)
	}
}

// Even with an entry somehow left overdue, the loop must never spin.
func TestNextWaitHasAFloor(t *testing.T) {
	w := newWatchlist()
	w.Seed("v1", "doki", base.Add(-time.Hour), false)
	if got := w.NextWait(base); got < time.Second {
		t.Fatalf("NextWait = %v, must never be below one second", got)
	}
}

func TestNextWaitWithEmptyWatchlist(t *testing.T) {
	if got := newWatchlist().NextWait(base); got != ytIntervalCold {
		t.Fatalf("an empty watchlist should idle, got %v", got)
	}
}

func TestWatchlistSweepRetiresStaleEntries(t *testing.T) {
	w := newWatchlist()
	w.Seed("ended", "doki", base, false)
	w.Seed("abandoned", "doki", base, false)
	w.Seed("live", "doki", base, false)

	w.Observe("ended", StateEnded, time.Time{}, base)
	w.Observe("abandoned", StateUpcoming, time.Time{}, base)
	w.Observe("live", StateLive, time.Time{}, base)

	// Nothing is old enough yet.
	if dropped := w.Sweep(base.Add(time.Minute)); len(dropped) != 0 {
		t.Fatalf("swept too early: %v", dropped)
	}
	// The ended entry lingers briefly so a stale-cache flap back to "live"
	// does not read as a brand new broadcast, then goes.
	if dropped := w.Sweep(base.Add(ytEndedLinger + time.Minute)); len(dropped) != 1 || dropped[0] != "ended" {
		t.Fatalf("expected only the ended entry, got %v", dropped)
	}
	// A waiting room that never started is eventually abandoned.
	if dropped := w.Sweep(base.Add(ytUpcomingMaxAge + time.Hour)); len(dropped) != 1 || dropped[0] != "abandoned" {
		t.Fatalf("expected only the abandoned entry, got %v", dropped)
	}
	if !w.Known("live") {
		t.Error("a live entry must never be swept")
	}
}

func TestChannelBoostExpires(t *testing.T) {
	w := newWatchlist()
	w.BoostChannel("doki", base)
	if !w.ChannelBoosted("doki", base.Add(time.Minute)) {
		t.Error("a channel should be boosted right after one of its broadcasts ends")
	}
	if w.ChannelBoosted("doki", base.Add(ytRestartBoost+time.Minute)) {
		t.Error("the restart boost must expire")
	}
	if w.ChannelBoosted("never-ended", base) {
		t.Error("an untouched channel must not be boosted")
	}
}

func TestSeedIsIdempotentAndTracksChannel(t *testing.T) {
	w := newWatchlist()
	w.Seed("v1", "doki", base, false)
	w.Seed("v1", "doki", base.Add(time.Minute), false)

	if got := w.Size(); got != 1 {
		t.Fatalf("re-seeding must not duplicate an entry, size = %d", got)
	}
	if got := w.ChannelOf("v1"); got != "doki" {
		t.Fatalf("ChannelOf = %q", got)
	}
	w.Seed("", "doki", base, false)
	if got := w.Size(); got != 1 {
		t.Fatalf("an empty id must be ignored, size = %d", got)
	}
}

func TestQuotaGovernorBucketsAreIndependent(t *testing.T) {
	g := newQuotaGovernor(3, 2)

	for i := range 3 {
		if !g.ReserveUnits(base, 1) {
			t.Fatalf("unit reservation %d should have been allowed", i)
		}
	}
	if g.ReserveUnits(base, 1) {
		t.Fatal("the unit budget must be enforced")
	}
	// search.list draws on a separate allocation: exhausting units must not
	// stop it, or a single exhausted bucket takes down the cross-check too.
	if !g.ReserveSearch(base) {
		t.Fatal("the search bucket must be unaffected by unit exhaustion")
	}
}

func TestQuotaGovernorBreakersAreScoped(t *testing.T) {
	g := newQuotaGovernor(100, 100)

	g.BlockUnits(base)
	if g.ReserveUnits(base, 1) {
		t.Error("blocking units must stop unit spend")
	}
	if !g.ReserveSearch(base) {
		t.Error("blocking units must NOT stop search spend")
	}

	g2 := newQuotaGovernor(100, 100)
	g2.BlockSearch(base)
	if g2.ReserveSearch(base) {
		t.Error("blocking search must stop search spend")
	}
	if !g2.ReserveUnits(base, 1) {
		t.Error("blocking search must NOT stop unit spend")
	}
}

func TestQuotaGovernorRollsOverAtThePacificDay(t *testing.T) {
	g := newQuotaGovernor(1, 1)
	if !g.ReserveUnits(base, 1) {
		t.Fatal("first reservation should be allowed")
	}
	if g.ReserveUnits(base, 1) {
		t.Fatal("budget should be exhausted")
	}
	g.BlockUnits(base)

	// A new quota day resets both the counters and the breaker.
	next := base.Add(24 * time.Hour)
	if !g.ReserveUnits(next, 1) {
		t.Fatal("a new quota day must reset the budget and clear the breaker")
	}
}

// Discovery seeds every id on the uploads playlist with a boost, and an
// ordinary past upload classifies as ended. If the boost outranked that state,
// each one would be polled every 3 seconds - one channel with one old upload
// is enough to spend 28,800 quota units a day against an 8,000 budget.
func TestEndedStateBeatsTheBoost(t *testing.T) {
	e := &watchEntry{FirstSeen: base, State: StateEnded, BoostUntil: base.Add(10 * time.Minute)}
	if got := e.interval(base); got != ytIntervalCold {
		t.Fatalf("interval = %v, want %v; a boost must never revive an ended entry", got, ytIntervalCold)
	}

	live := &watchEntry{FirstSeen: base, State: StateLive, BoostUntil: base.Add(10 * time.Minute)}
	if got := live.interval(base); got != ytIntervalLive {
		t.Fatalf("interval = %v, want %v for a live entry", got, ytIntervalLive)
	}
}

// The sweep/re-seed churn: a swept id is still on the uploads playlist, so
// without a negative cache the next discovery pass re-seeds it and re-arms the
// fast poll, forever.
func TestSweptIDsAreRememberedSoDiscoveryDoesNotRecycleThem(t *testing.T) {
	w := newWatchlist()
	w.Seed("upload", "doki", base, true)
	w.Observe("upload", StateEnded, time.Time{}, base)

	after := base.Add(ytEndedLinger + time.Minute)
	if dropped := w.Sweep(after); len(dropped) != 1 {
		t.Fatalf("expected the ended entry to be swept, got %v", dropped)
	}
	if !w.Retired("upload", after) {
		t.Fatal("a swept id must be remembered so discovery skips it")
	}
	if w.Retired("upload", after.Add(ytRetiredMemory+time.Hour)) {
		t.Error("the negative cache must eventually expire")
	}
	if w.Retired("never-seen", after) {
		t.Error("an unknown id must not read as retired")
	}
}

// The linger window must be measured from the end, not from first sighting, or
// a frame seeded days before its stream is dropped on the first sweep after it
// finishes.
func TestEndedLingerIsMeasuredFromTheEnd(t *testing.T) {
	w := newWatchlist()
	w.Seed("scheduled-long-ago", "doki", base, false)

	endedAt := base.Add(48 * time.Hour)
	w.Observe("scheduled-long-ago", StateEnded, time.Time{}, endedAt)

	if dropped := w.Sweep(endedAt.Add(time.Minute)); len(dropped) != 0 {
		t.Fatalf("swept immediately after the end: %v", dropped)
	}
	if dropped := w.Sweep(endedAt.Add(ytEndedLinger + time.Minute)); len(dropped) != 1 {
		t.Fatalf("expected a sweep once the linger elapsed, got %v", dropped)
	}
}

// Only a broadcast we actually saw live is worth an end report. Every ordinary
// upload on the watchlist reads as ended on every cycle, and acting on those
// would mean a write transaction per video per poll.
func TestObserveReportsEndOnlyForBroadcastsSeenLive(t *testing.T) {
	w := newWatchlist()

	w.Seed("never-live", "doki", base, false)
	for i := range 5 {
		if w.Observe("never-live", StateEnded, time.Time{}, base.Add(time.Duration(i)*time.Minute)) {
			t.Fatal("an entry never seen live must never report an end")
		}
	}

	w.Seed("real-stream", "doki", base, false)
	w.Observe("real-stream", StateLive, time.Time{}, base)
	if !w.Observe("real-stream", StateEnded, time.Time{}, base.Add(time.Hour)) {
		t.Fatal("a broadcast seen live must report its end")
	}
	// Latched: the end is reported exactly once however often it is re-observed.
	for range 5 {
		if w.Observe("real-stream", StateEnded, time.Time{}, base.Add(2*time.Hour)) {
			t.Fatal("the end must be reported only once")
		}
	}
}

// A broadcast restored from the ledger was live by definition, so an end that
// happened while the process was down must still be reported - otherwise the
// row stays live forever and is never pruned.
func TestSeedClaimedReportsAnEndThatHappenedWhileDown(t *testing.T) {
	w := newWatchlist()
	w.SeedClaimed("was-live", "doki", base)

	if !w.Observe("was-live", StateEnded, time.Time{}, base.Add(time.Minute)) {
		t.Fatal("a re-seeded claimed broadcast must still report its end")
	}
}

// A seed that pulls work forward must interrupt a parked loop, or the seconds
// of latency the push paths buy are spent waiting out a stale sleep.
func TestSeedWakesThePolLoop(t *testing.T) {
	w := newWatchlist()

	// Drain any wake left by construction.
	select {
	case <-w.Wake():
	default:
	}

	w.Seed("new-id", "doki", base, true)
	select {
	case <-w.Wake():
	default:
		t.Fatal("a boosted seed must wake the poll loop")
	}

	// A no-op re-seed changes nothing and must not wake anything.
	w.Seed("new-id", "doki", base, false)
	select {
	case <-w.Wake():
		t.Fatal("re-seeding an existing entry without a boost must not wake the loop")
	default:
	}
}

// A stream scheduled more than 24 hours ahead is swept while still upcoming.
// Retiring it would blind discovery to that id for a full day — potentially
// straight through the go-live — so only ended entries are remembered.
func TestAbandonedUpcomingFramesAreNotRetired(t *testing.T) {
	w := newWatchlist()
	w.Seed("far-future-stream", "doki", base, false)
	w.Observe("far-future-stream", StateUpcoming, base.Add(72*time.Hour), base)

	after := base.Add(ytUpcomingMaxAge + time.Hour)
	if dropped := w.Sweep(after); len(dropped) != 1 {
		t.Fatalf("expected the stale upcoming frame to be swept, got %v", dropped)
	}
	if w.Retired("far-future-stream", after) {
		t.Fatal("an upcoming frame must stay rediscoverable; retiring it could miss the go-live")
	}

	// An ended entry is still remembered, so the churn protection survives.
	w.Seed("ordinary-upload", "doki", base, true)
	w.Observe("ordinary-upload", StateEnded, time.Time{}, base)
	endAfter := base.Add(ytEndedLinger + time.Minute)
	w.Sweep(endAfter)
	if !w.Retired("ordinary-upload", endAfter) {
		t.Fatal("an ended entry must still be retired, or discovery churns it forever")
	}
}
