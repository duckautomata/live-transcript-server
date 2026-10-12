package livedetect

import (
	"bytes"
	"fmt"
	"log"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer is a bytes.Buffer safe to write from the logger while a test
// reads it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns every captured line containing all of the given substrings.
func (b *logBuffer) lines(contains ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		match := line != ""
		for _, s := range contains {
			if !strings.Contains(line, s) {
				match = false
				break
			}
		}
		if match {
			out = append(out, line)
		}
	}
	return out
}

// captureLogs routes the default logger into a buffer, at debug level, for the
// rest of the test. Text format, as the console log is.
//
// The log package's output and flags are saved and restored as well, as
// notifCaptureLogs does in the server package. slog.SetDefault with a handler
// of its own points the log package at that handler, and setting the previous
// default back does not undo it when that default is slog's own, which writes
// through the log package: every later test's logging would land in this
// test's buffer instead of on stderr.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	prev := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return buf
}

// The sweep must say why each entry went, not just that it did. Only the
// ended one is retired; the pending ones are dropped unretired, and each
// record carries the state and schedule the entry was last seen with - the
// two facts that, logged, would have explained FdwrQid9ulg at a glance.
func TestSweepReportsWhyEachEntryWent(t *testing.T) {
	w := newWatchlist()
	stale := base.Add(-7 * 24 * time.Hour)

	w.Seed("ended", "doki", base, false)
	w.Observe("ended", StateEnded, time.Time{}, base)

	w.Seed("pending", "mint", base, false)
	w.Observe("pending", StateUpcoming, stale, base)

	w.SeedMembersOnly("members", "mint", base)
	w.Observe("members", StateUpcoming, time.Time{}, base)

	// Never classified: no poll answered for it with a verdict.
	w.Seed("silent", "doki", base, false)

	// Neither of these may go: one is live, one has not reached its age.
	w.Seed("live", "doki", base, false)
	w.Observe("live", StateLive, time.Time{}, base)
	w.Seed("young", "doki", base.Add(time.Hour), false)
	w.Observe("young", StateUpcoming, base.Add(30*time.Hour), base.Add(time.Hour))

	now := base.Add(ytUpcomingMaxAge + time.Minute)
	drops := w.sweep(now)

	want := []watchDrop{
		{VideoID: "ended", ChannelKey: "doki", Reason: dropEnded, State: StateEnded, FirstSeen: base, Retired: true},
		{VideoID: "silent", ChannelKey: "doki", Reason: dropAgedUnknown, State: StateUnknown, FirstSeen: base},
		{VideoID: "members", ChannelKey: "mint", Reason: dropAgedUpcoming, State: StateUpcoming, FirstSeen: base, MembersOnly: true},
		{VideoID: "pending", ChannelKey: "mint", Reason: dropAgedUpcoming, State: StateUpcoming, Scheduled: stale, FirstSeen: base},
	}
	if len(drops) != len(want) {
		t.Fatalf("drops = %+v, want %d (sorted by channel, then id)", drops, len(want))
	}
	for i, exp := range want {
		got := drops[i]
		if got.VideoID != exp.VideoID || got.ChannelKey != exp.ChannelKey || got.Reason != exp.Reason ||
			got.State != exp.State || !got.Scheduled.Equal(exp.Scheduled) || !got.FirstSeen.Equal(exp.FirstSeen) ||
			got.MembersOnly != exp.MembersOnly || got.Retired != exp.Retired {
			t.Errorf("drop %d = %+v, want %+v", i, got, exp)
		}
	}

	if !w.Retired("ended", now) {
		t.Error("the ended entry must be retired, or discovery churns it")
	}
	for _, id := range []string{"pending", "members", "silent"} {
		if w.Retired(id, now) {
			t.Errorf("%s was retired; a pending entry must stay rediscoverable", id)
		}
	}
	if !w.Known("live") || !w.Known("young") {
		t.Error("a live entry and one inside its age must both survive the sweep")
	}

	// The exported wrapper reports the same ids, for the callers that count.
	if got := dropIDs(drops); !slices.Equal(got, []string{"ended", "silent", "members", "pending"}) {
		t.Errorf("dropIDs = %v", got)
	}
	if again := w.Sweep(now); again != nil {
		t.Errorf("a second sweep dropped %v, want nothing", again)
	}
}

// The tripwire against the incident: whatever a future sweep case asks for,
// nothing but an ended entry may reach the negative cache. One that tries is
// refused - left rediscoverable - and reported loudly as a bug.
func TestRetireRefusesAnEntryThatHasNotEnded(t *testing.T) {
	logs := captureLogs(t)

	for _, state := range []State{StateUnknown, StateUpcoming, StateLive} {
		w := newWatchlist()
		id := "frame-" + state.String()
		e := &watchEntry{VideoID: id, ChannelKey: "beri", State: state, FirstSeen: base, Scheduled: base.Add(-7 * 24 * time.Hour)}

		w.mu.Lock()
		ok := w.retireLocked(e, base)
		w.mu.Unlock()

		if ok {
			t.Errorf("retireLocked accepted a %v entry", state)
		}
		if w.Retired(id, base) {
			t.Errorf("a %v entry reached the negative cache; discovery would skip it for a day", state)
		}
		if got := logs.lines("level=ERROR", "BUG", "videoId="+id, "state="+state.String(), "key=beri"); len(got) != 1 {
			t.Errorf("want one BUG error naming %s and its state, got %q", id, got)
		}
	}

	// An ended entry is retired, silently.
	w := newWatchlist()
	e := &watchEntry{VideoID: "done", ChannelKey: "beri", State: StateEnded, FirstSeen: base, EndedAt: base}
	w.mu.Lock()
	ok := w.retireLocked(e, base)
	w.mu.Unlock()
	if !ok || !w.Retired("done", base) {
		t.Error("an ended entry must be retired")
	}
	if got := logs.lines("videoId=done"); len(got) != 0 {
		t.Errorf("retiring an ended entry logged %q", got)
	}
}

// Dropping a video the API stopped returning must keep what it last looked
// like. A waiting room that goes private reads "upcoming" here - the line that,
// a week before FdwrQid9ulg went live, would have shown its frame being hidden.
func TestMarkMissingReportsWhatTheDroppedVideoLastLookedLike(t *testing.T) {
	w := newWatchlist()
	sched := base.Add(6 * time.Hour)
	w.Seed("hidden-frame", "beri", base, false)
	w.Observe("hidden-frame", StateUpcoming, sched, base)

	now := base
	var drops []watchDrop
	for range ytMaxConsecutiveMisses {
		now = now.Add(ytIntervalCold)
		drops, _ = w.markMissing([]string{"hidden-frame"}, now)
	}
	if len(drops) != 1 {
		t.Fatalf("drops = %+v, want the frame gone after %d misses", drops, ytMaxConsecutiveMisses)
	}
	dr := drops[0]
	if dr.VideoID != "hidden-frame" || dr.ChannelKey != "beri" || dr.Reason != dropUnreturned ||
		dr.State != StateUpcoming || !dr.Scheduled.Equal(sched) || !dr.FirstSeen.Equal(base) || dr.Retired {
		t.Errorf("drop = %+v, want the frame's key, last state and schedule, unretired", dr)
	}
}

// Discovery names what it seeded, so a missed go-live can be traced to the
// pass that found its id - or shown never to have been found. The ids come
// back in page order, without anything already tracked or negatively cached.
func TestSeedDiscoveredReturnsTheFreshIDsInPageOrder(t *testing.T) {
	w := newWatchlist()
	d := &Detector{watch: w}

	w.Seed("known", "doki", base, false)
	w.Seed("retired", "doki", base, false)
	w.Observe("retired", StateEnded, time.Time{}, base)
	now := base.Add(ytEndedLinger + time.Minute)
	w.Sweep(now)

	page := []string{"new-b", "known", "retired", "new-a"}
	if fresh := d.seedDiscovered(page, "doki", false, now); !slices.Equal(fresh, []string{"new-b", "new-a"}) {
		t.Fatalf("fresh = %v, want [new-b new-a]", fresh)
	}
	if again := d.seedDiscovered(page, "doki", false, now); len(again) != 0 {
		t.Errorf("re-reading the same page found %v, want nothing new", again)
	}

	// A members-only id videos.list would not answer for is skipped on the
	// members-only page only; on the uploads page it has been made public.
	w.SeedMembersOnly("unserved", "doki", now)
	for range ytMaxConsecutiveMisses {
		now = now.Add(ytIntervalCold)
		w.MarkMissing([]string{"unserved"}, now)
	}
	if fresh := d.seedDiscovered([]string{"unserved", "members-new"}, "doki", true, now); !slices.Equal(fresh, []string{"members-new"}) {
		t.Errorf("members-only fresh = %v, want [members-new]", fresh)
	}
	if fresh := d.seedDiscovered([]string{"unserved"}, "doki", false, now); !slices.Equal(fresh, []string{"unserved"}) {
		t.Errorf("uploads fresh = %v, want the now-public id", fresh)
	}
}

// Through a real discovery pass: the "found new videos" line names a handful
// of fresh ids but only counts a startup-sized page, and a pending frame
// leaving the watchlist gets an INFO line of its own with its reason, state,
// schedule and age, while an ended upload is only counted.
func TestDiscoveryLogsNameFreshIDsAndPendingDrops(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(UploadsPlaylistID(membersDoki), "new-1", "new-2")
	bulk := make([]string, discoveryLogIDsMax+1)
	for i := range bulk {
		bulk[i] = fmt.Sprintf("bulk-%02d", i)
	}
	api.setPlaylist(UploadsPlaylistID(membersMint), bulk...)

	d := membersDetector(t, sink, baseURL)
	t0 := time.Now()
	stale := t0.Add(-7 * 24 * time.Hour)
	seenAt := t0.Add(-ytUpcomingMaxAge - time.Hour)
	d.watch.Seed("stale-frame", "doki", seenAt, false)
	d.watch.Observe("stale-frame", StateUpcoming, stale, seenAt)
	d.watch.Seed("old-upload", "doki", t0.Add(-time.Hour), false)
	d.watch.Observe("old-upload", StateEnded, time.Time{}, t0.Add(-time.Hour))

	logs := captureLogs(t)
	d.youtubeDiscoverOnce()

	if got := logs.lines(`msg="youtube discovery found new videos"`, "key=doki", "count=2", `ids="[new-1 new-2]"`); len(got) != 1 {
		t.Errorf("want the doki discovery line naming both fresh ids, got %q", logs.lines("found new videos"))
	}
	mint := logs.lines(`msg="youtube discovery found new videos"`, "key=mint", fmt.Sprintf("count=%d", len(bulk)))
	if len(mint) != 1 || strings.Contains(mint[0], "ids=") {
		t.Errorf("want the mint discovery line counting %d ids without listing them, got %q", len(bulk), mint)
	}

	pending := logs.lines("level=INFO", "watchlist dropped a pending video", "videoId=stale-frame")
	if len(pending) != 1 {
		t.Fatalf("want one INFO line for the aged-out frame, got %q", logs.lines("stale-frame"))
	}
	for _, want := range []string{
		"key=doki", "reason=" + dropAgedUpcoming, "state=upcoming",
		"scheduled=" + stale.UTC().Format(time.RFC3339), "first_seen_ago=", "members_only=false",
	} {
		if !strings.Contains(pending[0], want) {
			t.Errorf("aged-out line %q is missing %q", pending[0], want)
		}
	}
	if got := logs.lines("old-upload"); len(got) != 0 {
		t.Errorf("an ended drop must only be counted, got %q", got)
	}
	if got := logs.lines("level=DEBUG", `msg="watchlist swept"`, "dropped=2", "retired=1"); len(got) != 1 {
		t.Errorf("want the swept count line, got %q", logs.lines("watchlist swept"))
	}
}

// The state poll's drop line names each video with its channel and the state
// and schedule it was last seen with, and leaves out a schedule it never had.
func TestUnreturnedVideoDropIsLoggedWithItsLastState(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	d := membersDetector(t, sink, baseURL)

	now := time.Now()
	sched := now.Add(48 * time.Hour)
	// Published long ago, so neither is announced: this is about the drop.
	published := now.Add(-7 * 24 * time.Hour)
	api.setVideo(membersBroadcast("hidden", "upcoming", published, sched, time.Time{}))
	api.setVideo(membersBroadcast("pulled", "none", published, time.Time{}, time.Time{}))
	d.watch.Seed("hidden", "doki", now, true)
	d.watch.Seed("pulled", "doki", now, true)
	d.youtubeStateOnce()

	// Both go private.
	api.mu.Lock()
	clear(api.videos)
	api.mu.Unlock()

	logs := captureLogs(t)
	for range ytMaxConsecutiveMisses {
		membersMakeDue(d, "hidden")
		membersMakeDue(d, "pulled")
		d.youtubeStateOnce()
	}

	hidden := logs.lines(`msg="dropping videos the API has stopped returning"`, "videoId=hidden")
	if len(hidden) != 1 {
		t.Fatalf("want one drop line for the hidden frame, got %q", logs.lines("stopped returning"))
	}
	for _, want := range []string{
		"key=doki", "state=upcoming", "scheduled=" + sched.UTC().Format(time.RFC3339),
		"reason=" + dropUnreturned, fmt.Sprintf("after_consecutive_misses=%d", ytMaxConsecutiveMisses),
	} {
		if !strings.Contains(hidden[0], want) {
			t.Errorf("drop line %q is missing %q", hidden[0], want)
		}
	}

	pulled := logs.lines(`msg="dropping videos the API has stopped returning"`, "videoId=pulled")
	if len(pulled) != 1 {
		t.Fatalf("want one drop line for the pulled upload, got %q", logs.lines("stopped returning"))
	}
	if !strings.Contains(pulled[0], "state=ended") || strings.Contains(pulled[0], "scheduled=") {
		t.Errorf("drop line %q: want state=ended and no schedule", pulled[0])
	}
}

// Discovery's seed boosts unless the sweep aged the id out: a frame the
// watchlist was watching until moments ago is not news. Every other way off
// the watchlist keeps the boost on rediscovery - above all a video videos.list
// stopped returning, which reappearing on its playlist means re-published,
// exactly the waiting room that may be about to go live.
func TestSeedDiscoveredBoostsUnlessTheSweepAgedTheIDOut(t *testing.T) {
	w := newWatchlist()
	w.Seed("aged", "doki", base, false)
	w.Observe("aged", StateUpcoming, base.Add(48*time.Hour), base)
	w.Seed("hidden", "doki", base, false)
	at := base
	for range ytMaxConsecutiveMisses {
		w.MarkMissing([]string{"hidden"}, at)
		at = at.Add(ytIntervalCool)
	}
	if w.Known("hidden") {
		t.Fatal("test setup: the unreturned video should have been dropped")
	}

	later := base.Add(ytUpcomingMaxAge + time.Minute)
	if drops := w.sweep(later); len(drops) != 1 || drops[0].VideoID != "aged" || drops[0].Reason != dropAgedUpcoming {
		t.Fatalf("drops = %+v, want the aged-out frame", drops)
	}

	if w.SeedDiscovered("aged", "doki", later, false) {
		t.Error("an aged-out frame was re-seeded with the boost")
	}
	if !w.SeedDiscovered("hidden", "doki", later, false) {
		t.Error("a video that stopped being returned and is listed again must be boosted")
	}
	if !w.SeedDiscovered("new", "doki", later, true) {
		t.Error("a new id must be boosted")
	}
	if !w.MembersOnly("new") {
		t.Error("SeedDiscovered must carry the members-only flag in with the seed")
	}

	// The record is spent by the re-seed, and expires on its own besides.
	w.mu.Lock()
	delete(w.entries, "aged")
	w.mu.Unlock()
	if !w.SeedDiscovered("aged", "doki", later.Add(time.Minute), false) {
		t.Error("a second rediscovery must not reuse a spent aged-out record")
	}
	w.mu.Lock()
	delete(w.entries, "aged")
	w.aged["aged"] = later
	w.mu.Unlock()
	if !w.SeedDiscovered("aged", "doki", later.Add(ytRetiredMemory), false) {
		t.Error("an aged-out record older than ytRetiredMemory must not suppress the boost")
	}
}
