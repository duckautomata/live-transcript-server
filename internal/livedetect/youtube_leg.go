package livedetect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"live-transcript-server/internal/metrics"
)

// State-loop pacing bounds.
const (
	// ytMaxIdleWait caps how long the state loop parks, so a missed wake costs
	// seconds rather than minutes.
	ytMaxIdleWait = 2 * time.Second
	// ytMinCycle is the floor on how often a cycle may actually issue a call,
	// enforced after a wake because a wake bypasses NextWait's own floor.
	ytMinCycle = time.Second
	// ytRestartDiscovery is the discovery cadence while a channel is inside
	// its post-broadcast restart window.
	ytRestartDiscovery = 30 * time.Second
	// ytDiscoveryStartDelay is how long after Start the first discovery pass
	// runs. Operator requests are served from the moment the loop starts,
	// this wait included.
	ytDiscoveryStartDelay = 2 * time.Second
	// ytDiscoveryCallsPerChannel is the most one discovery pass spends on a
	// channel: its uploads playlist and its members-only playlist.
	ytDiscoveryCallsPerChannel = 2
	// ytMembersAbsentRetry is how long discovery waits before asking again
	// for a members-only playlist that did not exist. YouTube creates one only
	// once a channel posts members-only content, so for most channels it never
	// exists, and asking every pass would double their discovery bill for
	// nothing. The price is that a channel's very first members-only item can
	// wait this long to be found; every one after it is found on the normal
	// cadence.
	ytMembersAbsentRetry = time.Hour
	// webSubAbortAfterConsecutiveFailures stops a renewal pass once the hub
	// has clearly refused everything, rather than working through the rest at
	// twenty seconds apiece.
	webSubAbortAfterConsecutiveFailures = 3
)

// ytDiscoveryInterval is the effective uploads-playlist scan cadence.
func (d *Detector) ytDiscoveryInterval() time.Duration {
	s := d.cfg.YouTube.DiscoverySeconds
	if s <= 0 {
		s = defaultYTDiscoverySeconds
	}
	if s < minYouTubeDiscoverySeconds {
		s = minYouTubeDiscoverySeconds
	}
	return time.Duration(s) * time.Second
}

// runYouTubeState is the detection leg: it polls the live state of every video
// id on the watchlist.
//
// This is where seconds-scale latency comes from, and it is affordable only
// because videos.list costs one quota unit per CALL regardless of how many ids
// it carries. Watching four videos costs exactly what watching one costs, so
// the poll rate is decoupled from the channel count and can be spent entirely
// on latency for whatever is about to start.
func (d *Detector) runYouTubeState() {
	if !d.sleep(3 * time.Second) {
		return
	}
	for {
		cycleStart := time.Now()
		d.youtubeStateOnce()

		// The sleep is capped as well as wakeable. The wake channel covers the
		// seeds we know about; the cap covers a seed that raced the timer arm,
		// and any future seeder added without a poke. It is quota-neutral
		// because youtubeStateOnce returns before reserving anything when
		// nothing is due, so a spurious wake costs one guarded map scan.
		wait := min(d.watch.NextWait(time.Now()), ytMaxIdleWait)
		if !d.sleepOrWake(wait, d.watch.Wake()) {
			return
		}
		// A wake bypasses NextWait's floor, so enforce the minimum cycle time
		// here: a burst of hub pushes would otherwise bill one videos.list
		// unit each.
		if elapsed := time.Since(cycleStart); elapsed < ytMinCycle {
			if !d.sleep(ytMinCycle - elapsed) {
				return
			}
		}
	}
}

func (d *Detector) youtubeStateOnce() {
	now := time.Now()
	ids := d.watch.Due(now)
	if len(ids) == 0 {
		return
	}

	// Reserve before the call, and treat a refusal as a scheduling decision
	// rather than an error: the leg backs off instead of failing, so an
	// exhausted budget degrades latency without producing an alert storm.
	if !d.gov.ReserveUnits(now, 1) {
		metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeState, "skipped").Inc()
		d.watch.Defer(ids, now.Add(ytIntervalCool))
		return
	}
	metrics.LiveDetectQuotaUnits.Inc()

	ctx, cancel := d.pollCtx(15 * time.Second)
	defer cancel()

	videos, err := d.youtube.VideosList(ctx, ids)
	if err != nil {
		// Defer before returning: an entry left overdue would make NextWait
		// return its floor forever and spin this loop.
		d.watch.Defer(ids, now.Add(ytIntervalCool))
		if errors.Is(err, errYouTubeQuota) {
			// Only the shared-unit bucket is tripped. search.list has its own
			// allocation and must keep working.
			d.gov.BlockUnits(now)
			slog.Error("youtube quota exhausted; pausing unit spend until the daily reset",
				"func", "Detector.youtubeStateOnce")
		}
		d.recordFailure(MechanismYouTubeState, err)
		return
	}
	d.recordSuccess(MechanismYouTubeState)

	returned := make(map[string]bool, len(videos))
	for _, v := range videos {
		returned[v.ID] = true
		d.applyYouTubeVideo(ctx, v, now)
	}

	// An id the API did not return (deleted, private, or made unavailable)
	// must still be rescheduled or it stays permanently overdue.
	var missing []string
	for _, id := range ids {
		if !returned[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		// An id the API declines to return (deleted, made private,
		// region-blocked) is a SILENT miss: the call succeeded, so the leg
		// still reports healthy. Counting it is the only way this becomes
		// visible short of the search audit.
		metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeState, "unreturned").Add(float64(len(missing)))
		dropped, droppedMembersOnly := d.watch.markMissing(missing, now)
		// One line per id rather than one for the batch: drops are rare, and
		// the state each id was last seen in is the story. "upcoming" is a
		// waiting room its creator hid or deleted - one that may come back and
		// go live, and is worth remembering when it does - while "ended" is a
		// finished video being pulled.
		for _, dr := range dropped {
			slog.Info("dropping videos the API has stopped returning",
				append(dr.logAttrs("Detector.youtubeStateOnce", now),
					"after_consecutive_misses", ytMaxConsecutiveMisses)...)
		}
		// Members-only content is expected to be returned like anything else.
		// If it is not, members-only detection cannot work at all, and the
		// only symptom would otherwise be notifications that never arrive.
		if len(droppedMembersOnly) > 0 {
			slog.Warn("videos.list is not returning members-only videos; their streams cannot be detected",
				"func", "Detector.youtubeStateOnce", "ids", droppedMembersOnly)
		}
		slog.Debug("youtube did not return watched ids", "func", "Detector.youtubeStateOnce", "ids", missing)
	}
}

// announceMaxAge bounds how long after its publish time a video still counts
// as new. Discovery and WebSub notice a new video within minutes, so anything
// older than this that has never been announced either predates the feature
// or was published during an outage - and on the first run after enabling,
// the entire uploads playlist arrives at once. Without this bound that first
// discovery pass would announce every video on it.
const announceMaxAge = 6 * time.Hour

// recentlyPublished reports whether a publish time is inside announceMaxAge.
// A zero time (not reported) is never recent.
func recentlyPublished(published, now time.Time) bool {
	return !published.IsZero() && now.Sub(published) <= announceMaxAge
}

// applyYouTubeVideo turns one videos.list item into an observation.
func (d *Detector) applyYouTubeVideo(ctx context.Context, v YTVideo, now time.Time) {
	channelKey := d.watch.ChannelOf(v.ID)
	if channelKey == "" {
		// Fall back to the video's own channel id; an id can arrive from a
		// push before we know which configured channel it belongs to.
		channelKey = d.ytTargets[v.ChannelID()]
	}

	state := v.State()
	// Read before Observe: this poll is the one that flips upcoming -> live,
	// and asking afterwards would always report the new state.
	sawScheduled := d.watch.SawUpcoming(v.ID)
	endWorthy := d.watch.Observe(v.ID, state, v.ScheduledStartTime(), now)

	if channelKey == "" {
		return
	}

	switch state {
	case StateUpcoming:
		d.announceScheduled(ctx, v, channelKey, now)

	case StateLive:
		// The state poll makes the claim either way, but one the search audit
		// or an operator rescued after a missed go-live is booked under the
		// rescuer: see watchEntry.RescuedBy.
		mechanism := MechanismYouTubeState
		if rescuer := d.watch.RescuedBy(v.ID); rescuer != "" {
			mechanism = rescuer
		}
		// Livestreams and premieres are indistinguishable here and both are in
		// scope, so there is deliberately no discriminator between them.
		d.observe(ctx, Broadcast{
			Platform:     PlatformYouTube,
			ChannelKey:   channelKey,
			ID:           v.ID,
			URL:          YouTubeWatchURL(v.ID),
			Title:        v.Title(),
			Description:  v.Description(),
			StartedAt:    v.StartedAt(),
			SawScheduled: sawScheduled,
			MembersOnly:  d.watch.MembersOnly(v.ID),
		}, mechanism)

	case StateEnded:
		// "Ended" with no broadcast details is not a stream that finished; it
		// is an ordinary upload, which reads as "none" from its first poll.
		if !v.IsBroadcast() {
			d.announceUpload(v, channelKey, now)
		}
		// Only an entry we actually saw live, ending for the first time, is
		// worth a database write and a restart boost. Every ordinary upload on
		// the watchlist reads as "ended" on every cycle, and acting on those
		// would mean a write transaction per video per poll against the live
		// product database, plus a permanent restart boost on every channel.
		if !endWorthy {
			return
		}
		if err := d.sink.ObserveEnded(ctx, PlatformYouTube, v.ID); err != nil {
			slog.Error("failed to mark youtube broadcast ended", "func", "Detector.applyYouTubeVideo",
				"videoId", v.ID, "err", err)
		}
		// A restart mints a brand-new video id, so the window right after an
		// end is when discovery most needs to be looking.
		d.watch.BoostChannel(channelKey, now)
	}
}

// scheduledLead is how far ahead a scheduled start must be for the frame to
// be announced as "scheduled". A creator who makes the waiting room and goes
// live a few minutes later would otherwise ping the audience twice inside a
// minute - once for the frame, once for the go-live that supersedes it - and
// the second of those is the one that matters.
const scheduledLead = 15 * time.Minute

// announceScheduled reports a newly appeared waiting room or premiere page.
//
// The in-memory latch stops the poll repeating itself every cycle; the sink's
// ledger is what makes it once-ever. The recency check is what stops the
// first discovery pass after enabling from announcing every frame a channel
// has scheduled for the coming week - those are not news.
func (d *Detector) announceScheduled(ctx context.Context, v YTVideo, channelKey string, now time.Time) {
	published := v.PublishedAt()
	if !recentlyPublished(published, now) {
		return
	}
	// A frame with no announced time, or one starting imminently (or already
	// overdue), is not scheduling news: the go-live announcement covers it.
	scheduled := v.ScheduledStartTime()
	if scheduled.IsZero() || scheduled.Before(now.Add(scheduledLead)) {
		return
	}
	if !d.watch.ClaimAnnounce(v.ID, VideoScheduled) {
		return
	}
	err := d.sink.ObserveVideo(ctx, VideoEvent{
		Kind:        VideoScheduled,
		Platform:    PlatformYouTube,
		ChannelKey:  channelKey,
		ID:          v.ID,
		URL:         YouTubeWatchURL(v.ID),
		Title:       v.Title(),
		Description: v.Description(),
		PublishedAt: published,
		ScheduledAt: scheduled,
	})
	if err != nil {
		// Give the latch back so the next poll retries; the ledger makes a
		// retry harmless.
		d.watch.ReleaseAnnounce(v.ID, VideoScheduled)
		slog.Error("failed to record a scheduled broadcast", "func", "Detector.announceScheduled",
			"key", channelKey, "videoId", v.ID, "err", err)
	}
}

// announceUpload reports a newly published ordinary video or short.
//
// Telling the two apart needs one request to youtube.com, so it runs off the
// poll goroutine: the state poll is the latency path for going-live and must
// not wait on a classification that is nice to have. An unknown answer is
// announced as a video - the harmless direction.
func (d *Detector) announceUpload(v YTVideo, channelKey string, now time.Time) {
	published := v.PublishedAt()
	if !recentlyPublished(published, now) {
		return
	}
	// One latch covers both kinds: the classification decides which one this
	// video is, and it is only ever one of them.
	if !d.watch.ClaimAnnounce(v.ID, VideoUpload) {
		return
	}
	videoID, title, desc := v.ID, v.Title(), v.Description()
	d.spawn(func() {
		ctx, cancel := d.pollCtx(shortsProbeTimeout + 5*time.Second)
		defer cancel()

		kind, url := VideoUpload, YouTubeWatchURL(videoID)
		if isShort, err := d.shorts.IsShort(ctx, videoID); err != nil {
			if d.shuttingDown() {
				d.watch.ReleaseAnnounce(videoID, VideoUpload)
				return
			}
			slog.Warn("could not tell a short from a video; announcing it as a video",
				"func", "Detector.announceUpload", "key", channelKey, "videoId", videoID, "err", err)
		} else if isShort {
			kind, url = VideoShort, YouTubeShortsURL(videoID)
		}

		err := d.sink.ObserveVideo(ctx, VideoEvent{
			Kind:        kind,
			Platform:    PlatformYouTube,
			ChannelKey:  channelKey,
			ID:          videoID,
			URL:         url,
			Title:       title,
			Description: desc,
			PublishedAt: published,
		})
		if err != nil {
			d.watch.ReleaseAnnounce(videoID, VideoUpload)
			slog.Error("failed to record an upload", "func", "Detector.announceUpload",
				"key", channelKey, "videoId", videoID, "kind", kind, "err", err)
		}
	})
}

// runYouTubeDiscovery scans each channel's uploads and members-only playlists
// for video ids we have not seen.
//
// This is the latency floor for an UNSCHEDULED surprise go-live. Anything
// scheduled - every premiere, and any stream with a waiting room - is already
// on the watchlist long before it starts and is caught by the state poller in
// seconds, so this leg only has to cover the case where a channel goes live
// with no prior frame at all.
//
// For MEMBERS-ONLY content it is the only source of ids at all. The uploads
// playlist and the channel feed WebSub watches both leave members-only items
// out, so a members-only waiting room or video reaches the watchlist only when
// this leg reads the members-only playlist.
//
// Between passes it also serves the operator's manual requests (manual.go),
// which must run on this goroutine because it alone owns ytMembersAbsent.
func (d *Detector) runYouTubeDiscovery() {
	d.ytDiscoveryRunning.Store(true)
	defer d.ytDiscoveryRunning.Store(false)

	next := time.Now().Add(d.discoveryStartDelay)
	for {
		var ok bool
		if next, ok = d.awaitDiscovery(next); !ok {
			return
		}
		d.youtubeDiscoverOnce()
		now := time.Now()
		next = now.Add(d.discoveryWait(now))
	}
}

// discoveryWait shortens the discovery cadence while any channel is inside its
// post-broadcast restart window.
//
// A restart mints a brand-new video id, so it can only be found by discovery ,
// the state poller has nothing to look at. Without this the restart boost
// recorded on the channel was never read by anything, and a talent restarting
// a botched stream waited out the full fixed cadence.
func (d *Detector) discoveryWait(now time.Time) time.Duration {
	base := d.ytDiscoveryInterval()
	for _, key := range d.ytTargets {
		if d.watch.ChannelBoosted(key, now) {
			return min(base, ytRestartDiscovery)
		}
	}
	return base
}

// youtubeDiscoverOnce runs one discovery pass over every channel and reports
// what it did. The scheduled loop ignores the report; a manual pass
// (RunDiscoveryNow) hands it to the operator.
func (d *Detector) youtubeDiscoverOnce() DiscoverySummary {
	now := time.Now()

	channels := make([]string, 0, len(d.ytTargets))
	for id := range d.ytTargets {
		channels = append(channels, id)
	}
	slices.Sort(channels)
	summary := DiscoverySummary{Channels: len(channels)}

	ctx, cancel := d.pollCtx(20 * time.Second)
	defer cancel()

	anyOK := false
	// The members-only half reaches one verdict per pass, under its own
	// mechanism; see MechanismYouTubeMembersDiscover for why.
	membersOK, membersFailures := 0, 0
	var membersErr error
	for _, channelID := range channels {
		playlist := UploadsPlaylistID(channelID)
		if playlist == "" {
			continue
		}
		if !d.gov.ReserveUnits(now, 1) {
			metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeDiscover, "skipped").Inc()
			summary.BudgetRefused = true
			break
		}
		metrics.LiveDetectQuotaUnits.Inc()
		summary.UnitsSpent++

		ids, err := d.youtube.PlaylistItems(ctx, playlist, 50)
		if err != nil {
			if errors.Is(err, errYouTubeQuota) {
				d.gov.BlockUnits(now)
			}
			d.recordFailure(MechanismYouTubeDiscover, err)
			summary.Failures++
			continue
		}
		anyOK = true
		summary.PlaylistsRead++

		key := d.ytTargets[channelID]
		if fresh := d.seedDiscovered(ids, key, false, now); len(fresh) > 0 {
			slog.Info("youtube discovery found new videos", "func", "Detector.youtubeDiscoverOnce",
				"key", key, "count", len(fresh), freshIDsAttr(fresh), "watchlist", d.watch.Size())
			summary.noteFound(key, fresh, false)
		}

		outcome, err := d.discoverMembersOnly(ctx, channelID, key, ids, now, &summary)
		if outcome == membersRefused {
			summary.BudgetRefused = true
			break
		}
		if outcome == membersFailed {
			if membersFailures == 0 {
				membersErr = err
			}
			membersFailures++
			summary.Failures++
			continue
		}
		membersOK++
	}

	if anyOK {
		d.recordSuccess(MechanismYouTubeDiscover)
	}
	switch {
	case membersFailures == 1:
		d.recordFailure(MechanismYouTubeMembersDiscover, membersErr)
	case membersFailures > 1:
		d.recordFailure(MechanismYouTubeMembersDiscover,
			fmt.Errorf("%d members-only playlists could not be read; the first: %w", membersFailures, membersErr))
	case membersOK > 0:
		d.recordSuccess(MechanismYouTubeMembersDiscover)
	}
	if drops := d.watch.sweep(now); len(drops) > 0 {
		retired := 0
		for _, dr := range drops {
			if dr.Retired {
				retired++
			}
			// Ended drops stay in the count below: the sweeps after a start
			// let go of hundreds of back-catalogue uploads at once, all
			// expected. Anything else is a video leaving the watchlist without
			// having been seen to finish - rare, about one per pending frame
			// per day - and exactly the line wanted when a go-live turns out to
			// have been missed.
			if dr.Reason == dropEnded {
				continue
			}
			slog.Info("watchlist dropped a pending video without retiring it; discovery re-seeds it while it is still listed",
				dr.logAttrs("Detector.youtubeDiscoverOnce", now)...)
		}
		slog.Debug("watchlist swept", "func", "Detector.youtubeDiscoverOnce",
			"dropped", len(drops), "retired", retired)
	}
	return summary
}

// membersAbsence is a channel whose members-only playlist could not be read.
type membersAbsence struct {
	// retry is when discovery next asks for the playlist.
	retry time.Time
	// status is the answer that put the channel here, so a change - above all
	// a 404 turning into a 403 - is logged rather than swallowed by the backoff.
	status int
}

// membersOutcome is what the members-only half of discovery did for a channel.
type membersOutcome int

const (
	// membersDone covers a read, and a legitimate skip: no members-only
	// playlist, or still backing off from not having one.
	membersDone membersOutcome = iota
	// membersFailed is a read that failed; the error says why.
	membersFailed
	// membersRefused is the unit budget refusing the call.
	membersRefused
)

// discoverMembersOnly scans one channel's members-only playlist. publicIDs is
// the page just read from the same channel's uploads playlist.
//
// It runs only after that uploads read succeeded, which is what lets a 403 or
// 404 here be read as "this playlist", never "this key": the same key worked a
// moment ago, and had it not, that failure was recorded.
//
// summary is the pass's report, which it adds its unit, its read and its finds
// to; the caller counts the outcome.
func (d *Detector) discoverMembersOnly(ctx context.Context, channelID, key string, publicIDs []string, now time.Time, summary *DiscoverySummary) (membersOutcome, error) {
	ids, read, err := d.readMembersOnlyPlaylist(ctx, channelID, key, now)
	if read.spentUnit() {
		summary.UnitsSpent++
	}
	switch read {
	case membersReadSkipped, membersReadAbsent:
		return membersDone, nil
	case membersReadRefused:
		metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeMembersDiscover, "skipped").Inc()
		return membersRefused, nil
	case membersReadFailed:
		return membersFailed, fmt.Errorf("members-only playlist for %s: %w", key, err)
	}
	summary.PlaylistsRead++

	// An id on both pages is caught mid-move between them, and public wins
	// the tie. Treating a public stream as members-only keeps it off the
	// worker's queue and loses its transcript; the reverse costs the worker
	// some futile probes.
	ids = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(publicIDs, id) })

	if fresh := d.seedDiscovered(ids, key, true, now); len(fresh) > 0 {
		slog.Info("youtube discovery found new members-only videos", "func", "Detector.discoverMembersOnly",
			"key", key, "count", len(fresh), freshIDsAttr(fresh), "watchlist", d.watch.Size())
		summary.noteFound(key, fresh, true)
	}
	return membersDone, nil
}

// membersRead is what readMembersOnlyPlaylist did.
type membersRead int

const (
	// membersReadOK is a read; the ids are the page.
	membersReadOK membersRead = iota
	// membersReadSkipped asked nothing: the channel has no members-only
	// playlist id, or is still backing off from not having a playlist.
	membersReadSkipped
	// membersReadAbsent is a 404 or a 403 that is not a rate limit: the
	// channel has no playlist this key can read, and is now backed off.
	membersReadAbsent
	// membersReadRefused is the unit budget refusing the call.
	membersReadRefused
	// membersReadFailed is any other failure; the error says why.
	membersReadFailed
)

// spentUnit reports whether the read booked a unit: every outcome that got as
// far as asking YouTube.
func (r membersRead) spentUnit() bool {
	return r != membersReadSkipped && r != membersReadRefused
}

// readMembersOnlyPlaylist reads one channel's members-only playlist, keeping
// the backoff bookkeeping in ytMembersAbsent, and says what happened. It runs
// on the discovery goroutine, which alone may touch that map.
//
// It is the one place the members-only playlist is read and its answers are
// classified, for discovery and for the operator's check alike: they share
// ytMembersAbsent, and two copies of the rules drifting apart would have one
// backing a channel off that the other had just cleared.
//
// Callers must only ask after a read with the same key has succeeded - an
// uploads read, or a videos.list call - which is what lets a 403 or 404 here
// be read as "this playlist", never "this key".
func (d *Detector) readMembersOnlyPlaylist(ctx context.Context, channelID, key string, now time.Time) ([]string, membersRead, error) {
	playlist := MembersOnlyPlaylistID(channelID)
	if playlist == "" {
		return nil, membersReadSkipped, nil
	}
	if a, absent := d.ytMembersAbsent[channelID]; absent && now.Before(a.retry) {
		return nil, membersReadSkipped, nil
	}
	if !d.gov.ReserveUnits(now, 1) {
		return nil, membersReadRefused, nil
	}
	metrics.LiveDetectQuotaUnits.Inc()

	ids, err := d.youtube.PlaylistItems(ctx, playlist, 50)
	if err != nil {
		// A 404 is the normal answer for a channel that has never posted
		// members-only content, and a 403 is YouTube refusing to list one to
		// the key. Neither is the API failing, and neither goes away by asking
		// again a minute later, so both back off rather than counting against
		// the leg. A 403 that is only a rate limit is the exception: it passes
		// on its own, so it is retried like any other failure.
		status := youtubeStatus(err)
		if status == http.StatusNotFound || (status == http.StatusForbidden && !youtubeRateLimited(err)) {
			d.markMembersAbsent(channelID, key, status, now)
			return nil, membersReadAbsent, nil
		}
		if errors.Is(err, errYouTubeQuota) {
			d.gov.BlockUnits(now)
		}
		return nil, membersReadFailed, err
	}
	if _, wasAbsent := d.ytMembersAbsent[channelID]; wasAbsent {
		delete(d.ytMembersAbsent, channelID)
		slog.Info("channel now has a members-only playlist", "func", "Detector.readMembersOnlyPlaylist", "key", key)
	}
	return ids, membersReadOK, nil
}

// markMembersAbsent backs off a channel's members-only reads. It logs only
// when the answer changes, so the hourly recheck of a channel without
// memberships stays quiet while a 404 that turns into a 403 is still heard.
func (d *Detector) markMembersAbsent(channelID, key string, status int, now time.Time) {
	if prev, known := d.ytMembersAbsent[channelID]; !known || prev.status != status {
		if status == http.StatusForbidden {
			slog.Warn("youtube refuses to list this channel's members-only playlist; discovery cannot find its members-only content",
				"func", "Detector.markMembersAbsent", "key", key, "status", status,
				"retry_in", ytMembersAbsentRetry.String())
		} else {
			slog.Info("channel has no members-only playlist; checking again later",
				"func", "Detector.markMembersAbsent", "key", key, "status", status,
				"retry_in", ytMembersAbsentRetry.String())
		}
	}
	d.ytMembersAbsent[channelID] = membersAbsence{retry: now.Add(ytMembersAbsentRetry), status: status}
	metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeMembersDiscover, "no-members-playlist").Inc()
}

// seedDiscovered diffs one playlist page against the watchlist, seeds every id
// it has not seen, and returns those ids in page order.
//
// The WHOLE page is diffed, never just the newest entry: a stream scheduled in
// advance sorts by publish date and can sit well down the list by the time it
// actually starts.
//
// The ids themselves are returned, not a count, so the caller's log line can
// name them. "Was this id ever seeded, and by which pass?" is the first
// question about a missed go-live, and a count cannot answer it.
//
// membersOnly says which playlist the page came from, and it is applied to ids
// already tracked as well. The uploads and members-only playlists never share
// an id, so whichever one lists it now is the truth - a members-only waiting
// room opened to everyone moves to the uploads playlist, and from then on it is
// an ordinary stream.
func (d *Detector) seedDiscovered(ids []string, key string, membersOnly bool, now time.Time) (fresh []string) {
	for _, id := range ids {
		if d.watch.Known(id) {
			d.watch.SetMembersOnly(id, membersOnly)
			continue
		}
		// An id we already swept is still sitting on the playlist; re-seeding
		// it would restart its fast-poll window on every discovery pass
		// forever.
		if d.watch.Retired(id, now) {
			continue
		}
		// A members-only id videos.list would not answer for is skipped on the
		// members-only page only. On the uploads page it has been made public.
		if membersOnly && d.watch.RetiredMembersOnly(id, now) {
			continue
		}
		// Boost a newly discovered id so it is polled hot from the start - the
		// id may already be live. One the sweep only just aged out is re-seeded
		// without the boost: it was being watched until moments ago, and
		// boosting every such frame once a day is a standing cost for nothing.
		// Its first poll is immediate either way.
		d.watch.SeedDiscovered(id, key, now, membersOnly)
		fresh = append(fresh, id)
	}
	return fresh
}

// discoveryLogIDsMax is the most fresh ids a discovery log line names. A
// steady-state pass finds one or two, and naming them is what lets a missed
// go-live be traced to the pass that seeded it, or show that none did. The
// first pass after a start finds every id on every page - fifty a playlist -
// and listing those would bury the lines that matter; the count says enough.
const discoveryLogIDsMax = 10

// freshIDsAttr is the "ids" attribute of a discovery log line, or an empty
// attribute - which slog handlers leave out - when there are too many ids for
// the list to be useful.
func freshIDsAttr(fresh []string) slog.Attr {
	if len(fresh) > discoveryLogIDsMax {
		return slog.Attr{}
	}
	return slog.Any("ids", fresh)
}

// HandleWebSubPush processes a verified hub notification.
//
// The Atom payload carries no live-state field of any kind, so this cannot
// itself detect anything. What it does is hand the state poller a video id
// seconds after the frame appears, which is what turns a scheduled stream or
// premiere going live into a seconds-latency detection instead of one gated on
// the next discovery pass.
func (d *Detector) HandleWebSubPush(feed *WebSubFeed) {
	if d == nil || feed == nil {
		return
	}
	now := time.Now()
	d.recordSuccess(MechanismYouTubeWebSub)

	for _, e := range feed.Entries {
		if e.VideoID == "" {
			continue
		}
		key, ok := d.ytTargets[e.ChannelID]
		if !ok {
			slog.Warn("websub push for an unconfigured channel", "func", "Detector.HandleWebSubPush",
				"channelId", e.ChannelID, "videoId", e.VideoID)
			continue
		}
		// The hub is documented to resend and to fire on title and description
		// edits, so dedupe on (video, updated) or every edit restarts the
		// fast-poll window.
		if d.webSubSeen.SeenOrRecord(e.VideoID+"|"+e.Updated, now) {
			continue
		}

		slog.Info("websub push seeded a video", "func", "Detector.HandleWebSubPush",
			"key", key, "videoId", e.VideoID, "title", e.Title)
		d.watch.Seed(e.VideoID, key, now, true)
	}
}

// runWebSubRenew keeps hub subscriptions alive.
//
// The hub caps leases below what we ask for and reports the granted value on
// the verification callback rather than in the subscribe response, so renewal
// runs on a fixed cadence far shorter than any plausible lease instead of
// tracking an expiry we might never have observed. Re-subscribing is
// idempotent, so over-renewing costs nothing.
func (d *Detector) runWebSubRenew() {
	backoff := time.Duration(0)
	// pending is the set still to subscribe. Empty means "all of them", which
	// is what a scheduled renewal does; after a partial failure it narrows to
	// just the channels that failed.
	var pending []string
	for {
		// A 503 from the hub means "try again later", but the normal cadence
		// is twelve hours - so without a retry schedule one bad afternoon
		// costs the entire push path for half a day. Back off from minutes,
		// not from the renewal interval.
		stillFailing, hubRetryAfter := d.webSubRenewOnce(pending)
		if len(stillFailing) > 0 {
			// Retry ONLY what failed. Re-subscribing a channel that already
			// succeeded achieves nothing and adds load to a hub that has just
			// asked us to back off.
			pending = stillFailing
			backoff = nextWebSubBackoff(backoff, hubRetryAfter)
			slog.Warn("websub renewal failed; retrying only the failed channels",
				"func", "Detector.runWebSubRenew",
				"remaining", len(pending), "retry_in", backoff.String())
			if !d.sleep(backoff) {
				return
			}
			continue
		}

		pending = nil
		backoff = 0
		if !d.sleep(webSubRenewInterval) {
			return
		}
	}
}

// clampHubRetry bounds a hub-supplied Retry-After into a sane window. A floor
// stops a tiny value from turning a struggling hub into a hot loop; the same
// one-hour ceiling as the backoff stops an absurd one from costing a day of
// push coverage. Zero means the hub said nothing.
func clampHubRetry(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	const floor = 30 * time.Second
	if d < floor {
		return floor
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}

// nextWebSubBackoff picks the next retry delay.
//
// The hub's Retry-After is a FLOOR, not the whole answer. Google's hub repeats
// the same static "2 minutes" however long it has been unwell, so obeying it
// literally means retrying every two minutes forever against a service that is
// explicitly overloaded - compliant, but not useful to either side. So the
// first delay starts from the hub's suggestion, and then doubles on each
// consecutive failure, never dropping below whatever the hub last asked for.
func nextWebSubBackoff(current, hubRetryAfter time.Duration) time.Duration {
	const (
		firstWithoutHint = 5 * time.Minute
		ceiling          = time.Hour
	)
	hint := clampHubRetry(hubRetryAfter)

	var next time.Duration
	switch {
	case current <= 0 && hint > 0:
		next = hint
	case current <= 0:
		next = firstWithoutHint
	default:
		next = current * 2
	}

	if next < hint {
		next = hint
	}
	if next > ceiling {
		next = ceiling
	}
	return next
}

// webSubRenewOnce subscribes (or renews) the given channels, or every channel
// when the list is empty. It returns the channels that failed and the longest
// Retry-After the hub supplied.
func (d *Detector) webSubRenewOnce(only []string) (failedChannels []string, hubRetryAfter time.Duration) {
	callback := d.webSubCallbackURL()

	channels := only
	if len(channels) == 0 {
		channels = make([]string, 0, len(d.ytTargets))
		for id := range d.ytTargets {
			channels = append(channels, id)
		}
	}
	slices.Sort(channels)
	consecutive := 0
	for i, channelID := range channels {
		// Each channel gets its own deadline: one slow hub response must not
		// consume a shared budget and cascade-fail every channel after it.
		// This deliberately bypasses pollCtx's 20s cap - that cap exists to
		// keep latency-critical polls from delaying shutdown, and renewal is
		// neither latency-critical nor frequent. It still derives from d.ctx,
		// so Close cancels it immediately.
		ctx, cancel := context.WithTimeout(d.ctx, webSubRequestTimeout)
		err := d.websub.Subscribe(ctx, channelID, callback)
		cancel()

		if err != nil {
			// Shutdown cancels the in-flight request; that is not a failure
			// worth reporting, and the remaining channels are moot.
			if d.shuttingDown() {
				slog.Debug("abandoning websub renewal during shutdown",
					"func", "Detector.webSubRenewOnce", "channelId", channelID)
				return nil, 0
			}
			failedChannels = append(failedChannels, channelID)
			// The hub tells us when to come back on an overload; take the
			// longest suggestion across the pass so one channel's shorter
			// window cannot make us hammer a hub that is still unwell.
			var httpErr *WebSubHTTPError
			if errors.As(err, &httpErr) && httpErr.RetryAfter > hubRetryAfter {
				hubRetryAfter = httpErr.RetryAfter
			}
			slog.Error("failed to renew websub subscription", "func", "Detector.webSubRenewOnce",
				"channelId", channelID, "err", err)

			// The hub is overloaded as a whole, not per channel: once several
			// in a row have refused, working through the rest just adds load
			// to a service that has already asked us to stop, and delays our
			// own backoff by twenty seconds a channel.
			consecutive++
			if consecutive >= webSubAbortAfterConsecutiveFailures && i+1 < len(channels) {
				slog.Warn("hub is refusing every request; abandoning the rest of this pass",
					"func", "Detector.webSubRenewOnce",
					"failed_in_a_row", consecutive, "skipped", len(channels)-(i+1))
				failedChannels = append(failedChannels, channels[i+1:]...)
				break
			}
			continue
		}
		consecutive = 0
		slog.Info("websub subscription requested", "func", "Detector.webSubRenewOnce",
			"channelId", channelID, "key", d.ytTargets[channelID])
	}

	// One verdict per pass, under the RENEWAL mechanism rather than the
	// delivery one. Recording per-channel failures would make
	// failuresBeforeAlert count channels instead of consecutive cycles, and
	// recording success against the delivery leg would let a healthy hub
	// subscription mask the fact that no push has ever arrived.
	if len(failedChannels) > 0 {
		d.recordFailure(MechanismYouTubeWebSubRenew,
			fmt.Errorf("%d of %d websub subscriptions could not be renewed", len(failedChannels), len(channels)))
		return failedChannels, hubRetryAfter
	}
	d.recordSuccess(MechanismYouTubeWebSubRenew)
	return nil, 0
}

// WebSubTopicSecret returns the HMAC secret for a topic so the HTTP handler
// can verify a push.
func (d *Detector) WebSubTopicSecret(topic string) string {
	if d == nil || d.websub == nil {
		return ""
	}
	return d.websub.TopicSecret(topic)
}

// KnownWebSubTopic reports whether a topic belongs to a configured channel, so
// the callback can reject a subscription we never asked for.
func (d *Detector) KnownWebSubTopic(topic string) bool {
	if d == nil {
		return false
	}
	for channelID := range d.ytTargets {
		if YouTubeTopicURL(channelID) == topic {
			return true
		}
	}
	return false
}
