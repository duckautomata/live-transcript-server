package livedetect

import (
	"errors"
	"log/slog"
	"slices"
	"time"

	"live-transcript-server/internal/metrics"
)

// searchAuditInterval is the spacing of the audit's fixed UTC slots.
//
// search.list's billing sets the cadence rather than latency: every slot costs
// one call per channel that is not already live on YouTube, and there are
// eight slots a day, so six channels need up to 48 calls. That is over the
// 40-call daily budget (defaultSearchAuditCalls), so on a day nobody is live
// during an audit the last slots before the governor's quota-day rollover
// (08:00 UTC) run short and log the budget as exhausted. Every channel still
// gets several audits a day; raising the budget instead needs the billing
// check described on defaultSearchAuditCalls.
//
// It must divide 24 hours, so the slots fall at the same UTC times every day.
const searchAuditInterval = 3 * time.Hour

// searchAuditPhase offsets the slots from the hour: 00:20, 03:20, 06:20 ...
// UTC.
//
// The audit used to run two minutes after startup and then every three hours,
// so its timing was an accident of when the process last restarted. On
// 2026-10-11 that put it 47 s after beri's habitual 20:00Z go-live, before
// search had indexed the broadcast, and it saw nothing while the stream went
// undetected for two hours. Streams cluster on the hour and the half hour;
// twenty past gives an on-the-hour go-live twenty minutes to reach the search
// index, and a stream that started at any other time of the slot's hour has
// been live long enough by the next slot.
const searchAuditPhase = 20 * time.Minute

// searchAuditGrace is how long a broadcast search reports live, and that the
// watchlist already tracks but the ledger has not claimed, is given to be
// claimed before the audit calls it a miss.
//
// The audit seeds such an id with a boost, which pulls its next state poll
// forward to now; a broadcast that is genuinely live is then claimed within a
// few seconds. Without the grace, a broadcast that went live moments before
// the slot - tracked, but on a slow rung of the ladder - would page the
// operator for a detection that was merely seconds away.
const searchAuditGrace = 30 * time.Second

// MechanismYouTubeAudit labels the cross-check leg.
const MechanismYouTubeAudit = "youtube-search-audit"

// nextSearchAuditAt returns the first audit slot strictly after now, in UTC.
//
// Strictly after, so a pass that finishes in the same instant its slot began
// (a pass that skipped every channel can) schedules the next slot rather than
// running its own again.
func nextSearchAuditAt(now time.Time) time.Time {
	now = now.UTC()
	y, m, d := now.Date()
	next := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(searchAuditPhase)
	// At most a day's worth of steps; because the interval divides 24 hours,
	// stepping past midnight lands on the next day's first slot.
	for !next.After(now) {
		next = next.Add(searchAuditInterval)
	}
	return next
}

// runSearchAudit periodically asks YouTube directly which video each channel
// is live on, and alerts when the detection ledger has not claimed it.
//
// This is the backstop against SILENT failure, which is the worst outcome for
// a detector: if discovery never surfaces a channel's in-progress broadcast -
// the specific risk for a channel whose uploads playlist lags, or for an id
// the watchlist has wrongly decided to ignore - every other signal looks
// healthy while nothing is ever detected. Polls succeed, quota is spent, no
// leg errors, and the only symptom is an absence of notifications, which is
// indistinguishable from a channel that simply did not stream.
//
// It both alerts AND seeds. A miss is always reported - it is the record that
// detection failed, and the measurement this leg exists for - but the id is
// also seeded onto the watchlist so the state poller confirms it and the
// stream is announced late rather than not at all.
//
// It runs at fixed UTC slots (see searchAuditPhase) and deliberately not at
// startup. The server restarts often - 35 times in one month - and each
// startup pass spent a search call per channel, so restarts alone could
// exhaust the day's budget before a single scheduled slot ran.
func (d *Detector) runSearchAudit() {
	slot := nextSearchAuditAt(time.Now())
	slog.Info("search audit scheduled", "func", "Detector.runSearchAudit",
		rfc3339Attr("next_run", slot))
	for {
		// The timer runs on the monotonic clock and the slot is a wall-clock
		// time, so after a clock step the timer can fire just before the
		// slot. Sleeping out the remainder keeps the pass from starting early,
		// where the next slot computed after it would be this same slot and
		// the audit would run twice back to back.
		for wait := time.Until(slot); wait > 0; wait = time.Until(slot) {
			if !d.sleep(wait) {
				return
			}
		}
		d.searchAuditOnce()
		slot = nextSearchAuditAt(time.Now())
	}
}

// searchAuditReport is what one audit pass saw, for its log line and for
// tests. Broadcasts are rendered "key=videoId" so a log reads at a glance.
type searchAuditReport struct {
	// checked counts the channels search answered for.
	checked int
	// live is every live broadcast search returned.
	live []string
	// skipped is the ledger broadcast that let each skipped channel go
	// unsearched.
	skipped []string
	// missed is every broadcast alerted on as a detection miss.
	missed []string
	// claimedInGrace is every tracked broadcast that was claimed during the
	// grace re-check, and so was not a miss.
	claimedInGrace []string
}

// auditRecheck is a live broadcast the watchlist tracks but the ledger had
// not claimed when the audit looked.
type auditRecheck struct {
	key, channelID, videoID string
}

// searchAuditOnce runs one audit pass over every YouTube channel, alerting on
// and seeding each live broadcast the ledger has not claimed, and reports what
// it saw.
func (d *Detector) searchAuditOnce() searchAuditReport {
	now := time.Now()
	var report searchAuditReport

	channels := make([]string, 0, len(d.ytTargets))
	for id := range d.ytTargets {
		channels = append(channels, id)
	}
	slices.Sort(channels)

	liveOnYouTube := d.auditSkippableChannels()

	ctx, cancel := d.pollCtx(20 * time.Second)
	defer cancel()

	var recheck []auditRecheck
	for _, channelID := range channels {
		key := d.ytTargets[channelID]

		// A channel the ledger already holds a live YouTube broadcast for is
		// detection working, and the search call it would cost is better kept
		// for a channel that is not. Twitch does not count: a channel watched
		// on both platforms can be live on Twitch while a YouTube broadcast
		// goes unseen.
		if videoID, ok := liveOnYouTube[key]; ok {
			report.skipped = append(report.skipped, key+"="+videoID)
			continue
		}

		if !d.gov.ReserveSearch(now) {
			metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeAudit, "skipped").Inc()
			slog.Warn("search audit budget exhausted for today", "func", "Detector.searchAuditOnce",
				"key", key, "channelId", channelID)
			// Break rather than return: what earlier channels found still
			// needs its grace re-check, and the pass still needs its log line.
			break
		}

		ids, err := d.youtube.SearchLive(ctx, channelID)
		if err != nil {
			if errors.Is(err, errYouTubeQuota) {
				// Only the search bucket is tripped: the shared unit budget is
				// untouched and detection must keep running on it.
				d.gov.BlockSearch(now)
				slog.Error("search.list quota exhausted; audit paused until the daily reset",
					"func", "Detector.searchAuditOnce")
			}
			d.recordFailure(MechanismYouTubeAudit, err)
			continue
		}
		d.recordSuccess(MechanismYouTubeAudit)
		report.checked++

		for _, videoID := range ids {
			report.live = append(report.live, key+"="+videoID)

			// Read before seeding, which makes every id known.
			known := d.watch.Known(videoID)

			// The ledger, not the watchlist, is the measure of success: an id
			// can sit on the watchlist for hours without ever being claimed,
			// and only a claim means the broadcast was announced (and queued).
			claimed, err := d.detectionClaimed(videoID)
			if err != nil {
				// Cannot tell a miss from a detection, so no alert: paging on
				// a database hiccup would teach the operator to ignore the one
				// alert that matters. An id the watchlist does not know is
				// still seeded, as the audit always did, because that is the
				// half that rescues a stream discovery missed - and it costs
				// nothing if the broadcast was claimed after all, since the
				// ledger takes a claim once. A tracked id is already in the
				// state poll's hands.
				slog.Error("search audit could not read the detection ledger; not alerting on this broadcast",
					"func", "Detector.searchAuditOnce", "key", key, "videoId", videoID,
					"seeded", !known, "err", err)
				if !known {
					d.watch.Seed(videoID, key, now, true)
				}
				continue
			}
			if claimed {
				continue
			}

			// Seed with a boost either way, so the state poller confirms the
			// broadcast within seconds and it is announced late rather than
			// never. Seed deliberately ignores the retired (negative) cache
			// that discovery honours, so an id wrongly retired - the
			// 2026-10-11 failure - is still picked up here.
			if !known {
				// Discovery never surfaced it. That is a miss even though the
				// seed means it will be claimed seconds from now, so it is
				// reported without waiting to see - and the claim is booked
				// as the audit's rescue, which also keeps the late-detection
				// alert from paging a second time about the same stream.
				d.watch.SeedRescue(videoID, key, now, MechanismYouTubeAudit)
				slog.Error("search audit found a live broadcast detection had not seen",
					"func", "Detector.searchAuditOnce", "key", key, "videoId", videoID, "channelId", channelID)
				d.reportAuditMiss(&report, key, videoID)
				d.alerts.NotifyLiveDetectAuditMiss(key, videoID)
				continue
			}
			d.watch.Seed(videoID, key, now, true)

			slog.Warn("search audit found a tracked live broadcast the ledger has not claimed; re-checking after a grace period",
				"func", "Detector.searchAuditOnce", "key", key, "videoId", videoID, "channelId", channelID,
				"grace", d.auditGrace.String())
			recheck = append(recheck, auditRecheck{key: key, channelID: channelID, videoID: videoID})
		}
	}

	if len(recheck) > 0 && !d.recheckAuditClaims(recheck, &report) {
		return report // shutting down
	}

	// A pass that skipped every channel because each was already live ran on
	// schedule and had nothing to ask. Counting it as healthy keeps a
	// single-channel deployment from being reported quiet after three audits
	// in a row land inside one long stream.
	if len(channels) > 0 && len(report.skipped) == len(channels) {
		d.recordSuccess(MechanismYouTubeAudit)
	}

	// Logged even when it finds nothing. A silent safety net is one you cannot
	// tell apart from a safety net that never ran, and this leg only speaks up
	// every three hours - so the quiet case is the one worth confirming. The
	// live ids are listed so a later miss can be checked against what search
	// actually saw at the time.
	snap := d.gov.Snapshot(now)
	slog.Info("search audit complete", "func", "Detector.searchAuditOnce",
		"channels_checked", report.checked, "missed_broadcasts", len(report.missed),
		"live", report.live, "skipped_live", report.skipped,
		"search_calls_remaining_today", snap.SearchBudget-snap.SearchSpent,
		rfc3339Attr("next_run", nextSearchAuditAt(time.Now())))
	return report
}

// recheckAuditClaims waits out the grace period and then asks the ledger
// again about each tracked-but-unclaimed broadcast, alerting on any still
// unclaimed. It is one wait for the whole pass, not one per broadcast.
// Returns false if the detector shut down during the wait.
func (d *Detector) recheckAuditClaims(recheck []auditRecheck, report *searchAuditReport) bool {
	if !d.sleep(d.auditGrace) {
		return false
	}
	for _, rc := range recheck {
		claimed, err := d.detectionClaimed(rc.videoID)
		if err != nil {
			slog.Error("search audit could not re-read the detection ledger; skipping this broadcast",
				"func", "Detector.recheckAuditClaims", "key", rc.key, "videoId", rc.videoID, "err", err)
			continue
		}
		if claimed {
			slog.Info("tracked live broadcast was claimed during the search audit's grace period",
				"func", "Detector.recheckAuditClaims", "key", rc.key, "videoId", rc.videoID)
			report.claimedInGrace = append(report.claimedInGrace, rc.key+"="+rc.videoID)
			continue
		}
		// Discovery found it and the state poll had a boosted chance to
		// confirm it, and still nothing claimed it. Whatever is wrong is
		// between the watchlist and the ledger - the state poll, the unit
		// budget, or the sink - not discovery.
		slog.Error("search audit found a tracked live broadcast that was never detected",
			"func", "Detector.recheckAuditClaims", "key", rc.key, "videoId", rc.videoID,
			"channelId", rc.channelID, "grace", d.auditGrace.String())
		// Paged now, so a claim that does come later is the audit's rescue,
		// booked under it and not paged again as late.
		d.watch.MarkRescued(rc.videoID, MechanismYouTubeAudit)
		d.reportAuditMiss(report, rc.key, rc.videoID)
		d.alerts.NotifyLiveDetectAuditUnclaimed(rc.key, rc.videoID)
	}
	return true
}

// detectionClaimed asks the ledger whether a YouTube broadcast was claimed,
// under a short context of its own. The pass's context is shared with the
// search calls, and a lookup that inherited whatever the last search left of
// it could fail for no reason of its own.
func (d *Detector) detectionClaimed(videoID string) (bool, error) {
	ctx, cancel := d.pollCtx(5 * time.Second)
	defer cancel()
	return d.sink.DetectionClaimed(ctx, PlatformYouTube, videoID)
}

// reportAuditMiss counts one detection miss.
func (d *Detector) reportAuditMiss(report *searchAuditReport, key, videoID string) {
	metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeAudit, "missed").Inc()
	report.missed = append(report.missed, key+"="+videoID)
}

// auditSkippableChannels maps each channel key the ledger holds a live YouTube
// broadcast for to that broadcast's id.
//
// Only a broadcast the watchlist still tracks counts. The state poller is what
// ends a ledger row, and it can only do that for an id it is watching; a row
// whose id was dropped (a stream deleted while live, say) can keep ended_at =
// 0 indefinitely, and trusting it would exempt that channel from the audit for
// as long as it stands.
//
// A ledger that cannot be read skips nothing: the audit is exactly what should
// run when detection's own record is in doubt, and the price is a few search
// calls.
func (d *Detector) auditSkippableChannels() map[string]string {
	ctx, cancel := d.pollCtx(10 * time.Second)
	defer cancel()

	active, err := d.sink.ActiveBroadcasts(ctx)
	if err != nil {
		slog.Warn("search audit could not read the ledger's live broadcasts; auditing every channel",
			"func", "Detector.auditSkippableChannels", "err", err)
		return nil
	}
	out := map[string]string{}
	for _, b := range active {
		if b.Platform != PlatformYouTube || b.ID == "" || !d.watch.Known(b.ID) {
			continue
		}
		out[b.ChannelKey] = b.ID
	}
	return out
}
