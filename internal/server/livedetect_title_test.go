package server

import (
	"context"
	"testing"
	"time"

	"live-transcript-server/internal/livedetect"
)

// EventSub claims a Twitch broadcast without a title and the lookup can
// fail; when the poll leg then sees the same broadcast with its title, the
// claim is lost but the title lands in the ledger, so the admin page shows
// the stream by name. A title already recorded is never replaced.
func TestObserveLiveBackfillsTitleFromALaterObservation(t *testing.T) {
	app, _ := setupDetectApp(t) // no title lookup installed
	ctx := context.Background()
	started := time.Now().Add(-time.Minute)

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", StartedAt: started,
	}, livedetect.MechanismTwitchEventSub); err != nil {
		t.Fatalf("ObserveLive (eventsub): %v", err)
	}
	det, err := app.Store.GetDetection(ctx, "twitch", "s1")
	if err != nil || det == nil {
		t.Fatalf("detection missing: %v %v", det, err)
	}
	if det.Title != "" {
		t.Fatalf("eventsub detection has title %q, want none yet", det.Title)
	}

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", Title: "Playing games", StartedAt: started,
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (poll): %v", err)
	}
	det, _ = app.Store.GetDetection(ctx, "twitch", "s1")
	if det.Title != "Playing games" {
		t.Errorf("title after the poll leg = %q, want it backfilled", det.Title)
	}
	if det.Mechanism != livedetect.MechanismTwitchEventSub {
		t.Errorf("mechanism = %q; the backfill must not rewrite who won", det.Mechanism)
	}
	dets, _ := app.Store.GetRecentDetections(ctx, "doki", 10)
	if len(dets) != 1 {
		t.Errorf("%d detections, want the one claim", len(dets))
	}

	// The streamer renames mid-stream: the ledger keeps what was announced.
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", Title: "Renamed", StartedAt: started,
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (poll again): %v", err)
	}
	if det, _ = app.Store.GetDetection(ctx, "twitch", "s1"); det.Title != "Playing games" {
		t.Errorf("title after a rename = %q, want the first one kept", det.Title)
	}
}
