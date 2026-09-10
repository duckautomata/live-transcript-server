package livedetect

import (
	"errors"
	"log/slog"
	"slices"
	"time"

	"live-transcript-server/internal/metrics"
)

// searchAuditInterval is how often the cross-check runs.
//
// search.list draws on its own allocation of 100 calls per day for the whole
// project, so the cadence is set by that budget rather than by latency: at two
// channels, every 3 hours costs 16 calls a day.
const searchAuditInterval = 3 * time.Hour

// MechanismYouTubeAudit labels the cross-check leg.
const MechanismYouTubeAudit = "youtube-search-audit"

// runSearchAudit periodically asks YouTube directly which video each channel
// is live on, and alerts when that disagrees with what detection believes.
//
// This is the backstop against SILENT failure, which is the worst outcome for
// a detector: if discovery never surfaces a channel's in-progress broadcast ,
// the specific risk for a channel whose uploads playlist lags - every other
// signal looks healthy while nothing is ever detected. Polls succeed, quota is
// spent, no leg errors, and the only symptom is an absence of notifications,
// which is indistinguishable from a channel that simply did not stream.
//
// It deliberately does NOT feed detections. Its job is to say "you are missing
// something", not to paper over the gap: a detector that quietly self-heals
// through an expensive fallback would hide the very measurement this
// shadow-mode build exists to collect.
func (d *Detector) runSearchAudit() {
	// Offset from the other legs so the first cycle does not land in the
	// startup burst.
	if !d.sleep(2 * time.Minute) {
		return
	}
	for {
		d.searchAuditOnce()
		if !d.sleep(searchAuditInterval) {
			return
		}
	}
}

func (d *Detector) searchAuditOnce() {
	now := time.Now()

	channels := make([]string, 0, len(d.ytTargets))
	for id := range d.ytTargets {
		channels = append(channels, id)
	}
	slices.Sort(channels)

	ctx, cancel := d.pollCtx(20 * time.Second)
	defer cancel()

	for _, channelID := range channels {
		if !d.gov.ReserveSearch(now) {
			metrics.LiveDetectPolls.WithLabelValues(MechanismYouTubeAudit, "skipped").Inc()
			slog.Warn("search audit budget exhausted for today", "func", "Detector.searchAuditOnce")
			return
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

		key := d.ytTargets[channelID]
		for _, videoID := range ids {
			// The watchlist is what detection actually looks at, so an id
			// missing from it is exactly the blind spot worth alerting on.
			if d.watch.Known(videoID) {
				continue
			}
			slog.Error("search audit found a live broadcast detection had not seen",
				"func", "Detector.searchAuditOnce", "key", key, "videoId", videoID, "channelId", channelID)
			d.alerts.NotifyLiveDetectAuditMiss(key, videoID)

			// Seed it so the state poller confirms and reports it, then keep
			// the alert as the record that discovery missed it.
			d.watch.Seed(videoID, key, now, true)
		}
	}
}
