package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
)

// detectionRetention is how long ended detection rows are kept. It must
// comfortably exceed the longest plausible single broadcast: pruning a row
// whose stream is still live would let the next observation claim it again and
// re-notify. Only ended rows are eligible, so this is belt-and-braces.
const detectionRetention = 30 * 24 * time.Hour

// ObserveLive implements livedetect.Sink.
//
// It is called by every detection mechanism on every cycle it sees a broadcast
// live , hundreds of times over one stream, from several goroutines at once.
// Store.ClaimDetection is what collapses that into a single notification: the
// INSERT OR IGNORE means exactly one caller wins, and the winner is the one
// that sends the Discord message. Everyone else returns silently.
//
// This is also why redundant mechanisms are safe. Twitch EventSub and Twitch
// polling both report the same broadcast; whichever arrives first claims it,
// and the mechanism recorded on the row is a real measurement of which path is
// faster. That measurement is the entire output of shadow mode.
//
// SHADOW MODE: this deliberately does NOT call QueueIncomingStream, does not
// write to the streams table, and does not activate anything. Detection only
// observes and reports until the operator is satisfied it is correct.
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
	// zero rather than stamping now(): the notification renders the delay as
	// "unknown", which is honest, where a substituted timestamp would render a
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
		metrics.LiveDetectDelaySeconds.WithLabelValues(b.Platform, mechanism).
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
		"url", b.URL,
	)

	app.Discord.NotifyStreamDetected(det)
	// Wake the admin page so a detection shows up while the operator is
	// watching a soak, rather than on the next slow refresh. Detections are
	// once-per-broadcast, so this costs one extra long-poll recheck per
	// stream , unlike the per-poll churn bumpAdminChange deliberately avoids.
	app.bumpAdminChange(b.ChannelKey)
	return nil
}

// ObserveEnded implements livedetect.Sink. It stamps a broadcast as finished
// so the detector can stop watching it and can accelerate its next discovery
// pass , a restart is a brand-new broadcast id, so the window right after an
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

// cleanupDetections prunes ended detection rows past detectionRetention. It
// runs on the maintenance loop beside the other periodic sweeps.
func (app *App) cleanupDetections() {
	cutoff := time.Now().Add(-detectionRetention).Unix()
	removed, err := app.Store.CleanupOldDetections(context.Background(), cutoff)
	if err != nil {
		slog.Error("failed to cleanup old detections", "func", "cleanupDetections", "err", err)
		return
	}
	if removed > 0 {
		slog.Info("removed old detection rows", "func", "cleanupDetections", "count", removed)
	}
}
