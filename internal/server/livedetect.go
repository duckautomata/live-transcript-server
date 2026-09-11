package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
)

// detectionRetention is how long ended detection rows are kept. It must
// comfortably exceed the longest plausible single broadcast: pruning a row
// whose stream is still live would let the next observation claim it again and
// re-notify. Only ended rows are eligible, so this is belt-and-braces.
//
// The same retention covers the non-live ledger, where it must exceed the
// detector's announcement recency window by a wide margin for the same
// reason: a pruned row is a row that can be claimed again.
const detectionRetention = 30 * 24 * time.Hour

// ObserveLive implements livedetect.Sink.
//
// It is called by every detection mechanism on every cycle it sees a broadcast
// live - hundreds of times over one stream, from several goroutines at once.
// Store.ClaimDetection is what collapses that into a single action: the
// INSERT OR IGNORE means exactly one caller wins, and the winner is the one
// that queues the stream and announces it. Everyone else returns silently.
//
// This is also why redundant mechanisms are safe. Twitch EventSub and Twitch
// polling both report the same broadcast; whichever arrives first claims it,
// and the mechanism recorded on the row is a real measurement of which path is
// faster.
//
// What the winner does depends on liveDetect.queueIncoming. With it set, the
// stream URL is queued for the worker exactly as a Pingcord announcement would
// be - this is the path that replaces the Discord listener bot. Without it,
// detection is an observer: it announces and records, and never puts work in
// front of the worker or touches the streams table.
func (app *App) ObserveLive(ctx context.Context, b livedetect.Broadcast, mechanism string) error {
	if _, ok := app.Channels[b.ChannelKey]; !ok {
		return fmt.Errorf("unknown channel %q", b.ChannelKey)
	}
	if b.ID == "" {
		return fmt.Errorf("broadcast for channel %q has no platform id", b.ChannelKey)
	}

	det := model.DetectedBroadcast{
		Platform:    b.Platform,
		BroadcastID: b.ID,
		ChannelKey:  b.ChannelKey,
		URL:         b.URL,
		Title:       b.Title,
		DetectedAt:  time.Now().Unix(),
		Mechanism:   mechanism,
	}
	// A zero StartedAt means the platform reported no start time. Keep it
	// zero rather than stamping now(): the operator feed renders the delay as
	// unknown, which is honest, where a substituted timestamp would render a
	// delay of zero and read as a perfect result.
	if !b.StartedAt.IsZero() {
		det.StartedAt = b.StartedAt.Unix()
	}

	won, err := app.Store.ClaimDetection(ctx, det)
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	metrics.LiveDetectDetections.WithLabelValues(b.ChannelKey, b.Platform, mechanism).Inc()
	if det.StartedAt > 0 {
		// Split by whether we watched it before it started. A scheduled stream
		// and a surprise go-live have completely different expected delays,
		// and averaging them together hides both.
		metrics.LiveDetectDelaySeconds.
			WithLabelValues(b.Platform, mechanism, scheduledLabel(b)).
			Observe(float64(det.DetectedAt - det.StartedAt))
	}

	slog.Info("live broadcast detected",
		"func", "App.ObserveLive",
		"key", b.ChannelKey,
		"platform", b.Platform,
		"broadcastId", b.ID,
		"mechanism", mechanism,
		"startedAt", det.StartedAt,
		"detectedAt", det.DetectedAt,
		"delaySeconds", det.DetectedAt-det.StartedAt,
		"sawScheduled", b.SawScheduled,
		"queueIncoming", app.QueueIncoming,
		"url", b.URL,
	)

	// The worker first: it is the product, and an announcement that goes out
	// before the transcript starts is the normal order of events anyway.
	//
	// Detached from the poll's context: the claim above is already durable,
	// so a cancellation landing between the two writes would lose the queue
	// entry with nothing left to retry it. The write itself is a few
	// milliseconds against the local database.
	if app.QueueIncoming {
		if err := app.QueueIncomingStream(context.WithoutCancel(ctx), b.ChannelKey, b.URL); err != nil {
			// The claim is already taken, so this broadcast will not be
			// retried by the next poll. Say so loudly; the operator can queue
			// it by hand from the admin page.
			slog.Error("failed to queue a detected stream for the worker",
				"func", "App.ObserveLive", "key", b.ChannelKey, "url", b.URL, "err", err)
		} else {
			metrics.LiveDetectQueued.WithLabelValues(b.ChannelKey, b.Platform).Inc()
			slog.Info("detected stream queued for the worker",
				"func", "App.ObserveLive", "key", b.ChannelKey, "url", b.URL)
		}
	}

	// Twitch EventSub wins nearly every Twitch race and carries no title. One
	// Helix round trip here - after the claim and the queue write, so it sits
	// between detection and announcement rather than on the latency path -
	// is what keeps the audience embed from reading "(untitled)".
	title := b.Title
	if title == "" && b.Platform == livedetect.PlatformTwitch && app.TwitchTitleLookup != nil {
		if looked := app.TwitchTitleLookup(context.WithoutCancel(ctx), b.ChannelKey); looked != "" {
			title = looked
			det.Title = looked
			if err := app.Store.UpdateDetectionTitle(context.WithoutCancel(ctx), b.Platform, b.ID, looked); err != nil {
				slog.Warn("failed to record a looked-up stream title", "func", "App.ObserveLive",
					"key", b.ChannelKey, "broadcastId", b.ID, "err", err)
			}
		}
	}

	payload := announce.Payload{
		Trigger:      announce.TriggerLive,
		Platform:     b.Platform,
		ChannelKey:   b.ChannelKey,
		ID:           b.ID,
		URL:          b.URL,
		Title:        title,
		EventTime:    b.StartedAt,
		DetectedAt:   time.Unix(det.DetectedAt, 0),
		Mechanism:    mechanism,
		SawScheduled: b.SawScheduled,
	}
	app.Announcer.Dispatch(payload)

	// Wake the admin page so a detection shows up while the operator is
	// watching, rather than on the next slow refresh. Detections are
	// once-per-broadcast, so this costs one extra long-poll recheck per
	// stream - unlike the per-poll churn bumpAdminChange deliberately avoids.
	app.bumpAdminChange(b.ChannelKey)
	return nil
}

// scheduledLabel renders the scheduled/surprise split for metrics. Only
// YouTube broadcasts can be scheduled in advance, so Twitch reports neither
// rather than a misleading "surprise".
func scheduledLabel(b livedetect.Broadcast) string {
	if b.Platform != livedetect.PlatformYouTube {
		return "n/a"
	}
	if b.SawScheduled {
		return "scheduled"
	}
	return "surprise"
}

// ObserveVideo implements livedetect.Sink for the non-live observations: a
// stream or premiere being scheduled, a video or a short being published.
// Same shape as ObserveLive - claim the ledger row, and only the winner
// announces - but nothing is ever queued for the worker: none of these is a
// stream to transcribe.
func (app *App) ObserveVideo(ctx context.Context, v livedetect.VideoEvent) error {
	if _, ok := app.Channels[v.ChannelKey]; !ok {
		return fmt.Errorf("unknown channel %q", v.ChannelKey)
	}
	if v.ID == "" {
		return fmt.Errorf("video for channel %q has no platform id", v.ChannelKey)
	}
	if !announce.KnownTrigger(v.Kind) || v.Kind == string(announce.TriggerLive) {
		return fmt.Errorf("video %q for channel %q has unknown kind %q", v.ID, v.ChannelKey, v.Kind)
	}

	now := time.Now()
	det := model.DetectedVideo{
		Platform:   v.Platform,
		VideoID:    v.ID,
		Kind:       v.Kind,
		ChannelKey: v.ChannelKey,
		URL:        v.URL,
		Title:      v.Title,
		DetectedAt: now.Unix(),
	}
	if !v.PublishedAt.IsZero() {
		det.PublishedAt = v.PublishedAt.Unix()
	}
	if !v.ScheduledAt.IsZero() {
		det.ScheduledAt = v.ScheduledAt.Unix()
	}

	won, err := app.Store.ClaimVideoDetection(ctx, det)
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	metrics.LiveDetectVideoEvents.WithLabelValues(v.ChannelKey, v.Kind).Inc()
	slog.Info("video event detected",
		"func", "App.ObserveVideo",
		"key", v.ChannelKey,
		"kind", v.Kind,
		"videoId", v.ID,
		"publishedAt", det.PublishedAt,
		"scheduledAt", det.ScheduledAt,
		"url", v.URL,
	)

	// A scheduled frame is about its start time; an upload is about when it
	// appeared.
	eventTime := v.PublishedAt
	if v.Kind == livedetect.VideoScheduled && !v.ScheduledAt.IsZero() {
		eventTime = v.ScheduledAt
	}
	app.Announcer.Dispatch(announce.Payload{
		Trigger:    announce.Trigger(v.Kind),
		Platform:   v.Platform,
		ChannelKey: v.ChannelKey,
		ID:         v.ID,
		URL:        v.URL,
		Title:      v.Title,
		EventTime:  eventTime,
		DetectedAt: now,
	})
	app.bumpAdminChange(v.ChannelKey)
	return nil
}

// ObserveEnded implements livedetect.Sink. It stamps a broadcast as finished
// so the detector can stop watching it and can accelerate its next discovery
// pass - a restart is a brand-new broadcast id, so the window right after an
// end is exactly when a new one is most likely to appear.
//
// Best-effort by design: a missed end costs only that acceleration. Nothing
// downstream depends on an end ever being observed, which matters because the
// end of a stream is the observation most likely to be lost to an outage.
func (app *App) ObserveEnded(ctx context.Context, platform, broadcastID string) error {
	ended, err := app.Store.MarkDetectionEnded(ctx, platform, broadcastID, time.Now().Unix())
	if err != nil {
		return err
	}
	if ended {
		slog.Info("detected broadcast ended", "func", "App.ObserveEnded",
			"platform", platform, "broadcastId", broadcastID)
	}
	return nil
}

// ActiveBroadcasts implements livedetect.Sink, handing back what the ledger
// still believes is live so a restart can resume watching in-progress
// broadcasts rather than waiting to rediscover them.
func (app *App) ActiveBroadcasts(ctx context.Context) ([]livedetect.Broadcast, error) {
	rows, err := app.Store.GetLiveDetections(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]livedetect.Broadcast, 0, len(rows))
	for _, r := range rows {
		b := livedetect.Broadcast{
			Platform:   r.Platform,
			ChannelKey: r.ChannelKey,
			ID:         r.BroadcastID,
			URL:        r.URL,
			Title:      r.Title,
		}
		if r.StartedAt > 0 {
			b.StartedAt = time.Unix(r.StartedAt, 0)
		}
		out = append(out, b)
	}
	return out, nil
}

// cleanupDetections prunes ended detection rows past detectionRetention, and
// the non-live ledger on the same schedule. It runs on the maintenance loop
// beside the other periodic sweeps.
func (app *App) cleanupDetections() {
	cutoff := time.Now().Add(-detectionRetention).Unix()
	removed, err := app.Store.CleanupOldDetections(context.Background(), cutoff)
	if err != nil {
		slog.Error("failed to cleanup old detections", "func", "cleanupDetections", "err", err)
	} else if removed > 0 {
		slog.Info("removed old detection rows", "func", "cleanupDetections", "count", removed)
	}

	removed, err = app.Store.CleanupOldVideoDetections(context.Background(), cutoff)
	if err != nil {
		slog.Error("failed to cleanup old video detections", "func", "cleanupDetections", "err", err)
	} else if removed > 0 {
		slog.Info("removed old video detection rows", "func", "cleanupDetections", "count", removed)
	}
}
