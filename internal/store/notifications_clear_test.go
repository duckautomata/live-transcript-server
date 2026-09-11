package store

import (
	"context"
	"testing"

	"live-transcript-server/internal/model"
)

// Webhook names travel through the JSON column unchanged, and a row written
// before webhooks had names (a plain list of URL strings) is still readable
// as unnamed webhooks.
func TestNotificationEventWebhookNamesRoundTripAndLegacyShape(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := notifEvent("doki", "named")
	ev.Webhooks = []model.Webhook{
		{Name: "#announcements", URL: "https://discord.com/api/webhooks/1111/token-one"},
		{Name: "", URL: "https://discord.com/api/webhooks/2222/token-two"},
	}
	id := notifMustCreate(t, st, ev, 1)
	got := notifMustGet(t, st, "doki", id)
	if len(got.Webhooks) != 2 || got.Webhooks[0].Name != "#announcements" || got.Webhooks[1].Name != "" ||
		got.Webhooks[0].URL != ev.Webhooks[0].URL || got.Webhooks[1].URL != ev.Webhooks[1].URL {
		t.Errorf("webhooks = %+v, want names and URLs preserved in order", got.Webhooks)
	}

	// Simulate a row from before names existed.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE notification_events SET webhook_urls = '["https://discord.com/api/webhooks/3333/token-three"]' WHERE id = ?`, id); err != nil {
		t.Fatalf("rewrite column: %v", err)
	}
	got = notifMustGet(t, st, "doki", id)
	if len(got.Webhooks) != 1 || got.Webhooks[0].URL != "https://discord.com/api/webhooks/3333/token-three" || got.Webhooks[0].Name != "" {
		t.Errorf("legacy shape read back as %+v, want one unnamed webhook", got.Webhooks)
	}

	// Garbage is still an error rather than a silently empty rule.
	if _, err := st.db.ExecContext(ctx, `UPDATE notification_events SET webhook_urls = 'nope' WHERE id = ?`, id); err != nil {
		t.Fatalf("rewrite column: %v", err)
	}
	if _, err := st.GetNotificationEvent(ctx, "doki", id); err == nil {
		t.Error("an unreadable webhook column must surface as an error")
	}
}

func TestClearNotificationLogIsPerChannel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for i := int64(1); i <= 3; i++ {
		if err := st.InsertNotificationLog(ctx, notifLogEntry("doki", i)); err != nil {
			t.Fatalf("insert doki %d: %v", i, err)
		}
	}
	if err := st.InsertNotificationLog(ctx, notifLogEntry("mint", 1)); err != nil {
		t.Fatalf("insert mint: %v", err)
	}

	removed, err := st.ClearNotificationLog(ctx, "doki")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	if n := notifLogCount(t, st, "doki"); n != 0 {
		t.Errorf("doki still has %d rows", n)
	}
	if n := notifLogCount(t, st, "mint"); n != 1 {
		t.Errorf("mint has %d rows, want its 1 row untouched", n)
	}
	if removed, err := st.ClearNotificationLog(ctx, "doki"); err != nil || removed != 0 {
		t.Errorf("second clear: removed=%d err=%v, want 0 and nil", removed, err)
	}
}

// Clearing history keeps everything that still guards against a repeat
// announcement: live broadcasts, broadcasts that ended after the cutoff, and
// videos detected after theirs.
func TestClearDetectionHistoryKeepsWhatStillGuards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	claim := func(id string, endedAt int64) {
		t.Helper()
		if _, err := st.ClaimDetection(ctx, model.DetectedBroadcast{
			Platform: "youtube", BroadcastID: id, ChannelKey: "doki", URL: "u", DetectedAt: 1000, Mechanism: "m",
		}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if endedAt > 0 {
			if _, err := st.MarkDetectionEnded(ctx, "youtube", id, endedAt); err != nil {
				t.Fatalf("end %s: %v", id, err)
			}
		}
	}
	claim("still-live", 0)
	claim("ended-recently", 5000)
	claim("ended-long-ago", 2000)
	if _, err := st.ClaimDetection(ctx, model.DetectedBroadcast{
		Platform: "youtube", BroadcastID: "other-channel", ChannelKey: "mint", URL: "u", DetectedAt: 1000, Mechanism: "m",
	}); err != nil {
		t.Fatalf("claim other: %v", err)
	}
	if _, err := st.MarkDetectionEnded(ctx, "youtube", "other-channel", 2000); err != nil {
		t.Fatalf("end other: %v", err)
	}

	for _, v := range []model.DetectedVideo{
		notifVideo("youtube", "old-video", "upload", "doki", 1000),
		notifVideo("youtube", "new-video", "short", "doki", 9000),
		notifVideo("youtube", "other-video", "upload", "mint", 1000),
	} {
		if _, err := st.ClaimVideoDetection(ctx, v); err != nil {
			t.Fatalf("claim video %s: %v", v.VideoID, err)
		}
	}

	broadcasts, videos, err := st.ClearDetectionHistory(ctx, "doki", 4000, 5000)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if broadcasts != 1 || videos != 1 {
		t.Errorf("removed (%d broadcasts, %d videos), want (1, 1)", broadcasts, videos)
	}

	remaining := map[string]bool{}
	dets, err := st.GetRecentDetections(ctx, "doki", 50)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	for _, d := range dets {
		remaining[d.BroadcastID] = true
	}
	if !remaining["still-live"] || !remaining["ended-recently"] || remaining["ended-long-ago"] {
		t.Errorf("broadcasts remaining = %v, want still-live and ended-recently only", remaining)
	}
	vids, err := st.GetRecentVideoDetections(ctx, "doki", 50)
	if err != nil {
		t.Fatalf("recent videos: %v", err)
	}
	if len(vids) != 1 || vids[0].VideoID != "new-video" {
		t.Errorf("videos remaining = %+v, want only new-video", vids)
	}

	// The other channel is untouched.
	if dets, _ := st.GetRecentDetections(ctx, "mint", 50); len(dets) != 1 {
		t.Errorf("mint broadcasts = %d, want 1", len(dets))
	}
	if vids, _ := st.GetRecentVideoDetections(ctx, "mint", 50); len(vids) != 1 {
		t.Errorf("mint videos = %d, want 1", len(vids))
	}

	// A cleared, long-ended broadcast is claimable again - which is exactly why
	// recent ones are kept.
	if won, err := st.ClaimDetection(ctx, model.DetectedBroadcast{
		Platform: "youtube", BroadcastID: "ended-long-ago", ChannelKey: "doki", URL: "u", DetectedAt: 1, Mechanism: "m",
	}); err != nil || !won {
		t.Errorf("re-claim after clear: won=%v err=%v", won, err)
	}
}
