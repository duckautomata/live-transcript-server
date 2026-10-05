package livedetect

import (
	"sort"
	"sync"
	"time"
)

// Poll intervals for the YouTube state ladder. The whole point of the ladder
// is that videos.list costs one unit per CALL regardless of how many ids it
// carries, so polling faster costs the same whether we watch one video or
// fifty. Frequency is therefore spent where it buys latency and withdrawn
// where it does not.
const (
	ytIntervalHot      = 3 * time.Second  // a start is imminent or just seeded
	ytIntervalWarm     = 5 * time.Second  // shortly past a scheduled start
	ytIntervalNear     = 10 * time.Second // an overrunning scheduled start
	ytIntervalApproach = 15 * time.Second
	ytIntervalSoon     = 30 * time.Second
	ytIntervalCool     = 60 * time.Second
	ytIntervalCold     = 300 * time.Second
	// ytIntervalLive is how often an already-live broadcast is re-checked.
	// Only end detection depends on it, which is not latency critical.
	ytIntervalLive = 60 * time.Second
)

// Watchlist entry lifetimes.
const (
	// ytSeedBoost keeps a freshly seeded id at the hot interval, covering the
	// window where a WebSub push or a Discord announcement said something is
	// about to happen.
	ytSeedBoost = 10 * time.Minute
	// ytRestartBoost keeps a channel hot after one of its broadcasts ends,
	// because a restart is exactly when a new id appears.
	ytRestartBoost = 30 * time.Minute
	// ytUpcomingMaxAge retires a scheduled frame that never started. Creators
	// abandon waiting rooms, and watching one forever slowly leaks quota.
	ytUpcomingMaxAge = 24 * time.Hour
	// ytEndedLinger keeps an ended entry around briefly so a stale-cache flap
	// back to "live" does not look like a brand-new broadcast.
	ytEndedLinger = 10 * time.Minute
	// ytRetiredMemory is how long a swept id is remembered so discovery does
	// not re-seed it. It must comfortably exceed the discovery interval, or
	// the sweep and the next discovery pass simply resume churning the same
	// ids off and back onto the watchlist.
	ytRetiredMemory = 24 * time.Hour
	// ytMaxConsecutiveMisses is how many polls the id was due for may omit it
	// before it is dropped. Five at the 60-second miss cadence is about five
	// minutes - far longer than any transient API hiccup, and short enough
	// that a deleted video costs a handful of units rather than a day of them.
	ytMaxConsecutiveMisses = 5
	// ytAbandonedAfterSchedule drops a frame still sitting "upcoming" this long
	// past its own announced start. Creators leave dead waiting rooms up, and
	// one costs a poll every five minutes until the 24-hour sweep otherwise.
	ytAbandonedAfterSchedule = 12 * time.Hour
	// ytRetiredMax bounds the negative cache. Every finished id on a page
	// discovery reads ends up in it, and discovery reads two 50-id pages per
	// channel (uploads and members-only), so this holds fifty channels' worth
	// before evicting anything. An evicted id still on its playlist is
	// re-seeded, which is the churn the cache exists to stop; the cap is only
	// there so an unexpected flood cannot grow the map without limit.
	ytRetiredMax = 5000
)

// watchEntry is one YouTube video being tracked.
type watchEntry struct {
	VideoID    string
	ChannelKey string
	// Scheduled is the announced start time, used ONLY to decide how often to
	// poll. It never contributes to a reported detection delay.
	Scheduled time.Time
	FirstSeen time.Time
	// BoostUntil holds the entry at the hot interval regardless of schedule.
	BoostUntil time.Time
	State      State
	// EndedAt is when the entry was first observed ended, so the linger window
	// is measured from the end rather than from first sighting.
	EndedAt time.Time
	// SawLive records that this entry was observed live at least once. Only
	// such entries are worth reporting an end for; an ordinary upload from the
	// back catalogue reads as "ended" on its very first poll and never was a
	// broadcast we detected.
	SawLive bool
	// EndReported latches the single end report per entry.
	EndReported bool
	// SawUpcoming records that we watched this entry as a scheduled frame
	// before it went live. It is what separates "the ladder did its job" from
	// "discovery only found this after it started" in the reported delay -
	// two very different numbers that otherwise look identical.
	SawUpcoming bool
	// NextDue is when this entry should next be polled. It is stamped on EVERY
	// exit path, including errors and quota refusals - an entry left permanently
	// overdue would make the scheduler compute a zero wait and spin.
	NextDue time.Time
	// Misses counts consecutive polls this entry was due for in which
	// videos.list did not return it at all. A deleted or privated video is gone
	// for good, and retrying it every minute until the 24-hour sweep wastes up
	// to 1,440 quota units.
	Misses int
	// MembersOnly records that the id was last listed on the channel's
	// members-only playlist rather than its public uploads. Discovery keeps it
	// in step with whichever playlist the id currently sits on, because a
	// creator can open a members-only waiting room to everyone before it
	// starts, and that public stream must then be treated as one.
	MembersOnly bool
	// Retired entries are dropped on the next sweep.
	Retired bool
	// Announced latches which non-live kinds (scheduled, upload) have been
	// handed to the sink for this entry, so a level-triggered poll reports
	// each once per process rather than once per cycle. The durable
	// once-ever guarantee is the sink's ledger, not this.
	Announced map[string]bool
}

// interval returns how often this entry should be polled right now.
//
// The shape is a decaying plateau rather than a spike around the scheduled
// time, because "scheduled 19:00, actually starts 21:30" is the common case,
// not the exception. An overrunning stream stays at ten seconds or better for
// its first three hours past schedule instead of falling back to minutes.
func (e *watchEntry) interval(now time.Time) time.Duration {
	// A TERMINAL STATE BEATS THE BOOST. A boost exists to catch a go-live, and
	// a broadcast the API has already called ended has no go-live left to
	// catch. Checking the boost first was a quota catastrophe: discovery seeds
	// every id on the uploads playlist, an ordinary upload classifies as
	// StateEnded, and each one would then be polled every 3 seconds for the
	// whole boost window. One channel with one past upload is enough to spend
	// 28,800 units a day against an 8,000-unit budget.
	if e.State == StateLive {
		return ytIntervalLive
	}
	if e.State == StateEnded {
		return ytIntervalCold
	}
	if now.Before(e.BoostUntil) {
		return ytIntervalHot
	}

	// An upcoming frame with no announced time: stay hot briefly after first
	// sighting, then decay. This covers a surprise go-live whose id we learned
	// from discovery moments ago.
	if e.Scheduled.IsZero() {
		switch age := now.Sub(e.FirstSeen); {
		case age < 15*time.Minute:
			return ytIntervalHot
		case age < time.Hour:
			return ytIntervalApproach
		default:
			return ytIntervalCool
		}
	}

	d := now.Sub(e.Scheduled)
	switch {
	case d < -6*time.Hour:
		return ytIntervalCold
	case d < -time.Hour:
		return ytIntervalCool
	case d < -10*time.Minute:
		return ytIntervalApproach
	case d < 15*time.Minute:
		return ytIntervalHot
	case d < time.Hour:
		return ytIntervalApproach
	case d < 3*time.Hour:
		return ytIntervalCool
	default:
		// Hours past its scheduled time and still not started. The operator's
		// "seconds, not minutes" requirement is about streams that start when
		// they said they would; this tail exists only so an abandoned or
		// long-delayed frame is still noticed, not so it is noticed fast.
		return ytIntervalCold
	}
}

// watchlist is the set of YouTube video ids under observation.
//
// It is in-memory on purpose: it is a cache of what to look at, not a record
// of what happened. The durable record is the detection ledger, and the
// watchlist is re-seeded from it plus one discovery pass on startup.
type watchlist struct {
	mu      sync.Mutex
	entries map[string]*watchEntry
	// channelBoost holds a channel hot after one of its broadcasts ends, so a
	// restart's new id is picked up quickly by the next discovery pass.
	channelBoost map[string]time.Time
	// retired remembers ids swept off the watchlist so discovery does not
	// re-seed them from the uploads playlist they still sit on. It must
	// outlive the discovery interval by a wide margin or the churn resumes.
	retired map[string]time.Time
	// retiredMembersOnly is the same for members-only ids dropped because
	// videos.list would not answer for them. It is kept apart because it only
	// speaks for the members-only playlist: if such an id turns up on the
	// uploads playlist it has been made public, and must be seeded at once.
	retiredMembersOnly map[string]time.Time
	// membersServed records that videos.list has answered for at least one
	// members-only id. Until it does, a members-only id that is never returned
	// is evidence the API does not serve members-only content at all; after,
	// it is just a video that was deleted or hidden, like any other.
	membersServed bool

	// wake lets a Seed that pulls work forward interrupt a parked poll loop.
	// Without it the seconds of latency a push path buys are thrown away
	// waiting out a sleep the seed just invalidated. Buffered to one so a
	// burst of seeds collapses into a single wake-up.
	wake chan struct{}
}

func newWatchlist() *watchlist {
	return &watchlist{
		entries:      make(map[string]*watchEntry),
		channelBoost: make(map[string]time.Time),
		retired:      make(map[string]time.Time),
		wake:         make(chan struct{}, 1),

		retiredMembersOnly: make(map[string]time.Time),
	}
}

// Seed adds a video id, or refreshes an existing one's boost. Called by
// discovery, by WebSub pushes, and on startup from the ledger.
func (w *watchlist) Seed(videoID, channelKey string, now time.Time, boost bool) {
	w.seed(videoID, channelKey, now, boost, false)
}

// SeedMembersOnly is a boosted Seed for an id found on a channel's
// members-only playlist.
//
// The flag is set under the seed's own lock rather than by a second call. The
// seed wakes the state loop, and a poll that ran between the two could claim
// the go-live as an ordinary broadcast and queue it for the worker.
func (w *watchlist) SeedMembersOnly(videoID, channelKey string, now time.Time) {
	w.seed(videoID, channelKey, now, true, true)
}

func (w *watchlist) seed(videoID, channelKey string, now time.Time, boost, membersOnly bool) {
	if videoID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	e, ok := w.entries[videoID]
	isNew := !ok
	if !ok {
		e = &watchEntry{
			VideoID:    videoID,
			ChannelKey: channelKey,
			FirstSeen:  now,
			State:      StateUnknown,
			NextDue:    now,
		}
		w.entries[videoID] = e
	}
	if e.Retired {
		// A previously retired id reappearing in discovery is worth another
		// look - creators un-hide and reschedule frames.
		e.Retired = false
		e.NextDue = now
		isNew = true
	}
	if boost {
		e.BoostUntil = now.Add(ytSeedBoost)
		e.NextDue = now
	}
	// Only ever raised here. A WebSub push or a reseed knows nothing about
	// membership, so an ordinary Seed must not clear what discovery learned;
	// SetMembersOnly is the one place that does.
	if membersOnly {
		e.MembersOnly = true
	}
	// Only a seed that actually pulled work forward is worth interrupting a
	// parked loop for; re-seeding an existing entry without a boost changes
	// nothing it needs to wake for.
	if isNew || boost {
		w.poke()
	}
}

// poke wakes a parked poll loop without blocking if one is already pending.
func (w *watchlist) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Wake is the channel a poll loop selects on so a seed can cut its sleep short.
func (w *watchlist) Wake() <-chan struct{} { return w.wake }

// SeedClaimed restores an entry for a broadcast the ledger already claimed and
// has not yet marked ended.
//
// It differs from Seed in setting SawLive: such a broadcast was live by
// definition, so if it finished while the process was down, the first poll
// after a restart must still report the end. A plain Seed would leave SawLive
// false, Observe would never call the end worthy, ended_at would stay zero
// forever, and the row would be handed back on every restart and never pruned.
// No boost - the go-live has already happened, so there is nothing to race.
func (w *watchlist) SeedClaimed(videoID, channelKey string, now time.Time) {
	if videoID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	e, ok := w.entries[videoID]
	if !ok {
		e = &watchEntry{
			VideoID:    videoID,
			ChannelKey: channelKey,
			FirstSeen:  now,
			State:      StateUnknown,
			NextDue:    now,
		}
		w.entries[videoID] = e
	}
	e.SawLive = true
	e.Retired = false
}

// BoostChannel marks a channel as recently ended so discovery watches it
// closely for a restart.
func (w *watchlist) BoostChannel(channelKey string, now time.Time) {
	w.mu.Lock()
	w.channelBoost[channelKey] = now.Add(ytRestartBoost)
	w.mu.Unlock()
}

// ChannelBoosted reports whether a channel is in its post-end restart window.
func (w *watchlist) ChannelBoosted(channelKey string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Before(w.channelBoost[channelKey])
}

// Due returns the ids to poll now, capped at videosListBatchSize.
//
// It returns nothing until at least one entry is genuinely due - but once one
// is, it FILLS THE CALL with every other entry too, most-overdue first.
//
// That is the whole cost model. videos.list charges one unit per CALL for up
// to fifty ids, so an entry that rides along on a call we were making anyway is
// free. Returning only the strictly-due entries silently threw that away: with
// three entries on staggered schedules we paid three units per cycle instead of
// one, and the waste grew linearly with the number of scheduled frames - the
// exact thing batching was supposed to prevent.
//
// Polling an entry earlier than its ladder interval asks is harmless: the
// interval is a bound on staleness, not a quota. Everything simply stays
// fresher, and entries polled together drift back into a single batch, which
// keeps the saving.
func (w *watchlist) Due(now time.Time) []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	type candidate struct {
		id   string
		over time.Duration
	}
	var list []candidate
	anyDue := false
	for id, e := range w.entries {
		if e.Retired {
			continue
		}
		over := now.Sub(e.NextDue)
		if over >= 0 {
			anyDue = true
		}
		list = append(list, candidate{id: id, over: over})
	}
	if !anyDue {
		return nil
	}

	// Most overdue first, so a backlog past the fifty-id cap drains fairly
	// rather than starving whatever sorts last.
	sort.Slice(list, func(i, j int) bool { return list[i].over > list[j].over })

	out := make([]string, 0, min(len(list), videosListBatchSize))
	for i := 0; i < len(list) && i < videosListBatchSize; i++ {
		out = append(out, list[i].id)
	}
	return out
}

// MarkMissing records that videos.list did not return these ids, backing them
// off and eventually dropping them.
//
// Only an omission on a poll the entry was DUE for counts as a miss. Due fills
// every call with every entry, so while anything else on the watchlist is hot
// a missing id rides along every three seconds, and counting each of those
// would spend the whole miss allowance in fifteen seconds instead of five
// minutes. A rider that was not due learns nothing its own next poll will not.
//
// They are NOT added to the retired cache: a video that reappears (an
// unlisted-then-public flip, a lifted region block) should be rediscoverable on
// the next discovery pass, and the negative cache would blind us to it for a
// day. A genuinely deleted video is off the uploads playlist too, so it simply
// never comes back.
//
// A members-only id is retired when videos.list has never answered for a
// members-only id at all. Nothing guarantees that it serves members-only
// content to a plain API key, and if it does not, the id stays on the
// members-only playlist regardless: without the negative cache, discovery
// would re-seed it on every pass, poll it hot for nothing and drop it again,
// forever. The second return lists those ids so the caller can say so loudly -
// it means members-only detection is not working. Once any members-only id has
// been answered for, a missing one is simply deleted or hidden, and is treated
// like every other missing video so a restored waiting room is rediscovered.
func (w *watchlist) MarkMissing(ids []string, now time.Time) (dropped, droppedMembersOnly []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, id := range ids {
		e, ok := w.entries[id]
		if !ok || now.Before(e.NextDue) {
			continue
		}
		e.Misses++
		if e.Misses >= ytMaxConsecutiveMisses {
			delete(w.entries, id)
			dropped = append(dropped, id)
			if e.MembersOnly && !w.membersServed {
				w.retiredMembersOnly[id] = now
				droppedMembersOnly = append(droppedMembersOnly, id)
			}
			continue
		}
		e.NextDue = now.Add(ytIntervalCool)
	}
	if len(droppedMembersOnly) > 0 {
		w.pruneRetiredLocked(now)
	}
	return dropped, droppedMembersOnly
}

// Defer pushes back the next poll for a set of ids. Used on every non-success
// exit path - a transport error, a quota refusal, an id the API did not
// return - so no entry can stay permanently overdue and spin the scheduler.
func (w *watchlist) Defer(ids []string, until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range ids {
		if e, ok := w.entries[id]; ok && e.NextDue.Before(until) {
			e.NextDue = until
		}
	}
}

// Observe records a poll result for one id and schedules its next poll.
//
// It returns whether this observation is an end worth acting on: the entry was
// seen live at some point and has now finished, reported at most once. Without
// that guard the poller would issue a MarkDetectionEnded write for every
// ordinary upload on the watchlist on every single cycle, against the live
// product database, for broadcasts it never detected in the first place.
func (w *watchlist) Observe(videoID string, state State, scheduled time.Time, now time.Time) (endWorthy bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	e, ok := w.entries[videoID]
	if !ok {
		return false
	}
	e.Misses = 0
	if e.MembersOnly {
		w.membersServed = true
	}
	if state != StateUnknown {
		if state == StateLive {
			e.SawLive = true
		}
		if state == StateUpcoming {
			e.SawUpcoming = true
		}
		if state == StateEnded && e.State != StateEnded {
			e.EndedAt = now
		}
		e.State = state
	}
	if !scheduled.IsZero() {
		e.Scheduled = scheduled
	}
	e.NextDue = now.Add(e.interval(now))

	if state == StateEnded && e.SawLive && !e.EndReported {
		e.EndReported = true
		return true
	}
	return false
}

// NextWait returns how long to sleep before the next poll cycle, clamped so a
// wholly idle watchlist still wakes periodically and an overdue one never
// returns zero repeatedly.
func (w *watchlist) NextWait(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()

	best := ytIntervalCold
	found := false
	for _, e := range w.entries {
		if e.Retired {
			continue
		}
		found = true
		if d := e.NextDue.Sub(now); d < best {
			best = d
		}
	}
	if !found {
		return ytIntervalCold
	}
	// A floor of one second is the backstop against a busy loop: even if an
	// entry is somehow left overdue, the cycle cannot run hotter than 1/s.
	return max(best, time.Second)
}

// Sweep retires entries that are no longer worth watching and drops retired
// ones. Returns the ids dropped so callers can log the churn.
func (w *watchlist) Sweep(now time.Time) []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var dropped, retire []string
	for id, e := range w.entries {
		switch {
		// Measured from the END, not from first sighting: a broadcast whose
		// frame was seeded days before it started would otherwise be dropped on
		// the first sweep after it finished, losing the anti-flap window
		// entirely.
		case e.State == StateEnded && !e.EndedAt.IsZero() && now.Sub(e.EndedAt) > ytEndedLinger:
			dropped = append(dropped, id)
			// Only an ENDED entry is worth remembering. It is a finished
			// broadcast or an ordinary upload that will sit on the playlist
			// forever, and re-seeding it is pure churn.
			retire = append(retire, id)
		// Still "upcoming" long past the time it announced: the creator moved
		// on and left the waiting room up. Judged against the schedule rather
		// than first sighting, so a frame created days early is not dropped
		// while it is still legitimately pending.
		case e.State == StateUpcoming && !e.Scheduled.IsZero() &&
			now.Sub(e.Scheduled) > ytAbandonedAfterSchedule:
			dropped = append(dropped, id)
			retire = append(retire, id)
		case e.State == StateUpcoming && now.Sub(e.FirstSeen) > ytUpcomingMaxAge:
			dropped = append(dropped, id)
		case e.State == StateUnknown && now.Sub(e.FirstSeen) > ytUpcomingMaxAge:
			dropped = append(dropped, id)
		}
	}
	for _, id := range dropped {
		delete(w.entries, id)
	}
	// An ABANDONED UPCOMING frame is deliberately NOT retired. Retiring it
	// would blind discovery to that id for a full day, and a stream scheduled
	// more than 24 hours ahead is swept while still upcoming - so retiring it
	// could mean missing the go-live entirely. Letting it be rediscovered
	// costs one re-seed per day per abandoned frame, which is nothing.
	for _, id := range retire {
		// Remember it so discovery does not immediately re-seed the same id
		// from the uploads playlist, which the id is still sitting on. Without
		// this the sweep and the next discovery pass form a permanent churn
		// loop that re-arms the fast poll every cycle.
		w.retired[id] = now
	}
	w.pruneRetiredLocked(now)
	for key, until := range w.channelBoost {
		if now.After(until) {
			delete(w.channelBoost, key)
		}
	}
	return dropped
}

// Retired reports whether an id was swept recently, so discovery can skip
// re-seeding something already judged uninteresting.
func (w *watchlist) Retired(videoID string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.retired[videoID]
	return ok && now.Sub(t) < ytRetiredMemory
}

// RetiredMembersOnly reports whether a members-only id was dropped recently
// because videos.list would not answer for it. Only the members-only half of
// discovery consults it.
func (w *watchlist) RetiredMembersOnly(videoID string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.retiredMembersOnly[videoID]
	return ok && now.Sub(t) < ytRetiredMemory
}

// pruneRetiredLocked expires the negative caches and enforces their cap.
// Callers must hold w.mu.
func (w *watchlist) pruneRetiredLocked(now time.Time) {
	pruneNegativeCache(w.retired, now)
	pruneNegativeCache(w.retiredMembersOnly, now)
}

// pruneNegativeCache expires one negative cache and enforces ytRetiredMax.
func pruneNegativeCache(cache map[string]time.Time, now time.Time) {
	for id, t := range cache {
		if now.Sub(t) >= ytRetiredMemory {
			delete(cache, id)
		}
	}
	// If the cap is still exceeded, drop the oldest entries. Losing one only
	// costs a redundant re-seed, so an approximate policy is fine.
	for len(cache) > ytRetiredMax {
		var oldestID string
		var oldest time.Time
		for id, t := range cache {
			if oldestID == "" || t.Before(oldest) {
				oldestID, oldest = id, t
			}
		}
		delete(cache, oldestID)
	}
}

// Size reports how many live entries are tracked, for the admin page.
func (w *watchlist) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, e := range w.entries {
		if !e.Retired {
			n++
		}
	}
	return n
}

// SawUpcoming reports whether an id was observed as a scheduled frame before
// going live. False means discovery first saw it already live - the case where
// the reported delay measures the discovery gap rather than the poll ladder.
func (w *watchlist) SawUpcoming(videoID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.entries[videoID]; ok {
		return e.SawUpcoming
	}
	return false
}

// ClaimAnnounce latches a (video, kind) announcement and reports whether this
// call took the latch. False means it was already taken, or the id is not
// tracked at all.
func (w *watchlist) ClaimAnnounce(videoID, kind string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[videoID]
	if !ok {
		return false
	}
	if e.Announced == nil {
		e.Announced = make(map[string]bool)
	}
	if e.Announced[kind] {
		return false
	}
	e.Announced[kind] = true
	return true
}

// ReleaseAnnounce gives a latch back after the sink failed to record the
// observation, so the next poll tries again instead of losing it for the
// life of the process.
func (w *watchlist) ReleaseAnnounce(videoID, kind string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.entries[videoID]; ok && e.Announced != nil {
		delete(e.Announced, kind)
	}
}

// Known reports whether an id is already tracked, so discovery can tell a new
// video from one it has seen before without re-seeding it.
func (w *watchlist) Known(videoID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.entries[videoID]
	return ok
}

// SetMembersOnly records which playlist discovery last found a tracked id on.
// A no-op for an id that is not tracked.
func (w *watchlist) SetMembersOnly(videoID string, membersOnly bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.entries[videoID]; ok {
		e.MembersOnly = membersOnly
	}
}

// MembersOnly reports whether a tracked id was last listed on its channel's
// members-only playlist. False for an id that is not tracked.
func (w *watchlist) MembersOnly(videoID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.entries[videoID]; ok {
		return e.MembersOnly
	}
	return false
}

// ChannelOf returns the channel key an id was seeded under.
func (w *watchlist) ChannelOf(videoID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.entries[videoID]; ok {
		return e.ChannelKey
	}
	return ""
}
