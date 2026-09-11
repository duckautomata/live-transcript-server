package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"live-transcript-server/internal/model"
)

const clearTestWebhook = "https://discord.com/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"

// Webhook names round-trip through create and read, and an unnamed webhook
// is still fine.
func TestAdminNotificationWebhookNames(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/notifications", "admin-doki", map[string]any{
		"name": "Named", "enabled": true, "triggers": []string{"live"}, "content": "hi", "embedEnabled": false,
		"webhooks": []map[string]string{
			{"name": " #announcements ", "url": clearTestWebhook},
			{"name": "", "url": "https://discord.com/api/webhooks/223456789012345678/abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created model.NotificationEvent
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(created.Webhooks) != 2 || created.Webhooks[0].Name != "#announcements" || created.Webhooks[1].Name != "" {
		t.Errorf("webhooks = %+v, want the trimmed name kept and the empty one allowed", created.Webhooks)
	}

	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/notifications", "admin-doki", nil)
	var listed NotificationsResponse
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Events) != 1 || len(listed.Events[0].Webhooks) != 2 || listed.Events[0].Webhooks[0].Name != "#announcements" {
		t.Errorf("listed = %+v", listed.Events)
	}

	// The old field name is simply ignored: a body without "webhooks" has none.
	rec = adminReq(t, mux, http.MethodPost, "/doki/admin/notifications", "admin-doki", map[string]any{
		"name": "Old shape", "triggers": []string{"live"}, "content": "hi", "webhookUrls": []string{clearTestWebhook},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("old-shape body: status=%d, want 400 (no webhooks)", rec.Code)
	}
}

func TestAdminClearNotificationLog(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})
	ctx := context.Background()

	for _, key := range []string{"doki", "mint"} {
		if err := app.Store.InsertNotificationLog(ctx, model.NotificationLogEntry{
			ChannelKey: key, EventName: "rule", Trigger: "live", Status: model.NotificationStatusSent, Webhooks: 1, Delivered: 1, SentAt: 1,
		}); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}

	if rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/notifications/log", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("no key: status=%d want 403", rec.Code)
	}
	if rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/notifications/log", "admin-mint", nil); rec.Code != http.StatusForbidden {
		t.Errorf("other channel's key: status=%d want 403", rec.Code)
	}

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/notifications/log", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if log, _ := app.Store.ListNotificationLog(ctx, "doki", 50); len(log) != 0 {
		t.Errorf("doki log still has %d entries", len(log))
	}
	if log, _ := app.Store.ListNotificationLog(ctx, "mint", 50); len(log) != 1 {
		t.Errorf("mint log has %d entries, want 1 untouched", len(log))
	}
	// Idempotent.
	if rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/notifications/log", "admin-doki", nil); rec.Code != http.StatusNoContent {
		t.Errorf("second clear: status=%d want 204", rec.Code)
	}
}

// Clearing detections through the API applies the safety graces: a live
// broadcast, a just-ended one and a just-detected video survive.
func TestAdminClearDetectionsKeepsRecentOnes(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ctx := context.Background()
	now := time.Now().Unix()

	seed := func(id string, endedAt int64) {
		t.Helper()
		if _, err := app.Store.ClaimDetection(ctx, model.DetectedBroadcast{
			Platform: "twitch", BroadcastID: id, ChannelKey: "doki", URL: "https://twitch.tv/dokibird", DetectedAt: now - 7200, Mechanism: "twitch-poll",
		}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if endedAt > 0 {
			if _, err := app.Store.MarkDetectionEnded(ctx, "twitch", id, endedAt); err != nil {
				t.Fatalf("end %s: %v", id, err)
			}
		}
	}
	seed("live", 0)
	seed("just-ended", now-60)
	seed("old", now-2*int64(detectionClearEndedGrace.Seconds()))
	for _, v := range []model.DetectedVideo{
		{Platform: "youtube", VideoID: "fresh", Kind: "upload", ChannelKey: "doki", URL: "u", DetectedAt: now - 60},
		{Platform: "youtube", VideoID: "stale", Kind: "upload", ChannelKey: "doki", URL: "u", DetectedAt: now - 2*int64(detectionClearVideoGrace.Seconds())},
	} {
		if _, err := app.Store.ClaimVideoDetection(ctx, v); err != nil {
			t.Fatalf("claim video %s: %v", v.VideoID, err)
		}
	}

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/notifications/detections", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear: status=%d body=%s", rec.Code, rec.Body.String())
	}

	dets, _ := app.Store.GetRecentDetections(ctx, "doki", 50)
	ids := map[string]bool{}
	for _, d := range dets {
		ids[d.BroadcastID] = true
	}
	if !ids["live"] || !ids["just-ended"] || ids["old"] {
		t.Errorf("broadcasts after clear = %v, want live and just-ended kept, old removed", ids)
	}
	vids, _ := app.Store.GetRecentVideoDetections(ctx, "doki", 50)
	if len(vids) != 1 || vids[0].VideoID != "fresh" {
		t.Errorf("videos after clear = %+v, want only fresh", vids)
	}
}
