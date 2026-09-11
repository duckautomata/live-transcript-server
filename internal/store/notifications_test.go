package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"live-transcript-server/internal/model"
)

// notifEvent builds a fully populated announcement rule so the JSON-valued
// columns (webhook list, trigger list, embed template) all carry data on the
// round trip.
func notifEvent(channel, name string) model.NotificationEvent {
	return model.NotificationEvent{
		ChannelKey: channel,
		Name:       name,
		Enabled:    true,
		Webhooks: model.WebhooksFromURLs(
			"https://discord.com/api/webhooks/1111/token-one",
			"https://discord.com/api/webhooks/2222/token-two",
		),
		Triggers:     []string{"live", "scheduled"},
		Content:      "<@&4242> {title} is live: {url}",
		EmbedEnabled: true,
		Embed: model.EmbedTemplate{
			Title:       "{title}",
			Description: "{channel} just went live",
			URL:         "{url}",
			Color:       "#5865F2",
			Image:       "{thumbnail}",
			Thumbnail:   "{avatar}",
			Footer:      "live-transcript",
			Timestamp:   true,
		},
		CooldownSeconds: 60,
	}
}

// notifLogEntry builds a log row whose SentAt doubles as an insertion
// sequence number, so ordering assertions can read it back.
func notifLogEntry(channel string, seq int64) model.NotificationLogEntry {
	return model.NotificationLogEntry{
		ChannelKey:  channel,
		EventID:     7,
		EventName:   "rule",
		Trigger:     "live",
		Platform:    "youtube",
		BroadcastID: fmt.Sprintf("vid-%d", seq),
		Title:       "a stream",
		URL:         "https://example.test/watch",
		Status:      model.NotificationStatusSent,
		Detail:      "",
		Webhooks:    2,
		Delivered:   2,
		SentAt:      seq,
	}
}

func notifVideo(platform, id, kind, channel string, detectedAt int64) model.DetectedVideo {
	return model.DetectedVideo{
		Platform:    platform,
		VideoID:     id,
		Kind:        kind,
		ChannelKey:  channel,
		URL:         "https://example.test/" + id,
		Title:       "video " + id,
		PublishedAt: detectedAt - 10,
		ScheduledAt: 0,
		DetectedAt:  detectedAt,
	}
}

// notifMustCreate inserts a rule and fails the test on error.
func notifMustCreate(t *testing.T, st *Store, ev model.NotificationEvent, now int64) int64 {
	t.Helper()
	id, err := st.CreateNotificationEvent(context.Background(), ev, now)
	if err != nil {
		t.Fatalf("CreateNotificationEvent(%s/%s): %v", ev.ChannelKey, ev.Name, err)
	}
	if id <= 0 {
		t.Fatalf("CreateNotificationEvent returned id %d, want a positive id", id)
	}
	return id
}

// notifMustGet reads a rule back and fails the test if it is missing.
func notifMustGet(t *testing.T, st *Store, channel string, id int64) *model.NotificationEvent {
	t.Helper()
	got, err := st.GetNotificationEvent(context.Background(), channel, id)
	if err != nil {
		t.Fatalf("GetNotificationEvent(%s, %d): %v", channel, id, err)
	}
	if got == nil {
		t.Fatalf("GetNotificationEvent(%s, %d) returned nil", channel, id)
	}
	return got
}

// notifLogCount counts a channel's log rows directly, bypassing the list
// cap, so the trim assertions measure the table and not the query.
func notifLogCount(t *testing.T, st *Store, channel string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM notification_log WHERE channel_key = ?", channel).Scan(&n); err != nil {
		t.Fatalf("count notification_log for %s: %v", channel, err)
	}
	return n
}

// Every admin-editable field, including the three JSON-valued columns, must
// survive Create -> Get unchanged, and the delivery trail must start at zero
// no matter what the caller passed.
func TestNotificationEventCreateGetRoundTrip(t *testing.T) {
	st := newTestStore(t)

	in := notifEvent("doki", "go-live ping")
	// A caller (say, a replayed PUT body) may carry trail values; Create
	// must not persist them.
	in.LastSentAt = 999
	in.SentCount = 5
	in.LastError = "stale"
	in.LastErrorAt = 998

	id := notifMustCreate(t, st, in, 1000)
	got := notifMustGet(t, st, "doki", id)

	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.ChannelKey != "doki" || got.Name != in.Name || got.Enabled != in.Enabled {
		t.Errorf("scalar fields mismatch: got %+v", got)
	}
	if !reflect.DeepEqual(got.WebhookURLs(), in.WebhookURLs()) {
		t.Errorf("WebhookURLs = %v, want %v", got.WebhookURLs(), in.WebhookURLs())
	}
	if !reflect.DeepEqual(got.Triggers, in.Triggers) {
		t.Errorf("Triggers = %v, want %v", got.Triggers, in.Triggers)
	}
	if got.Content != in.Content {
		t.Errorf("Content = %q, want %q", got.Content, in.Content)
	}
	if got.EmbedEnabled != in.EmbedEnabled {
		t.Errorf("EmbedEnabled = %v, want %v", got.EmbedEnabled, in.EmbedEnabled)
	}
	if got.Embed != in.Embed {
		t.Errorf("Embed = %+v, want %+v", got.Embed, in.Embed)
	}
	if got.CooldownSeconds != in.CooldownSeconds {
		t.Errorf("CooldownSeconds = %d, want %d", got.CooldownSeconds, in.CooldownSeconds)
	}
	if got.LastSentAt != 0 || got.SentCount != 0 || got.LastError != "" || got.LastErrorAt != 0 {
		t.Errorf("delivery trail must start at zero, got lastSent=%d count=%d err=%q errAt=%d",
			got.LastSentAt, got.SentCount, got.LastError, got.LastErrorAt)
	}
	if got.CreatedAt != 1000 || got.UpdatedAt != 1000 {
		t.Errorf("timestamps = (%d, %d), want (1000, 1000)", got.CreatedAt, got.UpdatedAt)
	}
}

// A rule saved with nil lists must be stored as JSON [] and read back as
// empty, non-nil slices, so the admin API never serialises null.
func TestNotificationEventNilListsStoredAsEmptyJSON(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, model.NotificationEvent{ChannelKey: "doki", Name: "bare"}, 1)

	var webhooks, triggers, embed string
	if err := st.db.QueryRowContext(ctx,
		"SELECT webhook_urls, triggers, embed FROM notification_events WHERE id = ?", id).
		Scan(&webhooks, &triggers, &embed); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if webhooks != "[]" || triggers != "[]" {
		t.Errorf("raw JSON columns = (%q, %q), want ([] , [])", webhooks, triggers)
	}
	if embed == "" || embed == "null" {
		t.Errorf("embed column = %q, want a JSON object", embed)
	}

	got := notifMustGet(t, st, "doki", id)
	if got.Webhooks == nil || len(got.WebhookURLs()) != 0 {
		t.Errorf("WebhookURLs = %#v, want an empty non-nil slice", got.WebhookURLs())
	}
	if got.Triggers == nil || len(got.Triggers) != 0 {
		t.Errorf("Triggers = %#v, want an empty non-nil slice", got.Triggers)
	}
	if got.Embed != (model.EmbedTemplate{}) {
		t.Errorf("Embed = %+v, want zero value", got.Embed)
	}
}

func TestNotificationEventListIsPerChannelInCreationOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	empty, err := st.ListNotificationEvents(ctx, "doki")
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no rules initially, got %d", len(empty))
	}

	var ids []int64
	for i, name := range []string{"first", "second", "third"} {
		ids = append(ids, notifMustCreate(t, st, notifEvent("doki", name), int64(100+i)))
	}
	notifMustCreate(t, st, notifEvent("mint", "elsewhere"), 200)

	got, err := st.ListNotificationEvents(ctx, "doki")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rules, want 3 scoped to doki", len(got))
	}
	for i, ev := range got {
		if ev.ID != ids[i] {
			t.Errorf("row %d has id %d, want %d (creation order)", i, ev.ID, ids[i])
		}
		if ev.ChannelKey != "doki" {
			t.Errorf("row %d belongs to %q, want doki", i, ev.ChannelKey)
		}
		if len(ev.WebhookURLs()) != 2 || len(ev.Triggers) != 2 {
			t.Errorf("row %d lost its JSON lists: %+v", i, ev)
		}
	}
	if got[0].Name != "first" || got[2].Name != "third" {
		t.Errorf("order = [%s %s %s], want creation order", got[0].Name, got[1].Name, got[2].Name)
	}
}

// The channel key is part of every lookup so one channel's admin can never
// read, edit or delete another channel's rule by guessing its id.
func TestNotificationEventChannelIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, notifEvent("doki", "private"), 1000)

	got, err := st.GetNotificationEvent(ctx, "mint", id)
	if err != nil {
		t.Fatalf("get with wrong channel: %v", err)
	}
	if got != nil {
		t.Fatalf("get with wrong channel returned %+v, want nil", got)
	}

	stolen := notifEvent("mint", "hijacked")
	stolen.ID = id
	err = st.UpdateNotificationEvent(ctx, stolen, 2000)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update with wrong channel: err = %v, want ErrNotFound", err)
	}

	rows, err := st.DeleteNotificationEvent(ctx, "mint", id)
	if err != nil {
		t.Fatalf("delete with wrong channel: %v", err)
	}
	if rows != 0 {
		t.Fatalf("delete with wrong channel removed %d rows, want 0", rows)
	}

	// The real owner still sees the untouched rule.
	still := notifMustGet(t, st, "doki", id)
	if still.Name != "private" || still.UpdatedAt != 1000 {
		t.Errorf("rule was modified through the wrong channel: %+v", still)
	}

	// Cross-channel lists never leak either.
	other, err := st.ListNotificationEvents(ctx, "mint")
	if err != nil {
		t.Fatalf("list mint: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("mint sees %d of doki's rules", len(other))
	}
}

// An admin edit replaces the editable fields (including every JSON column)
// but must not erase the delivery trail or reset a running cooldown.
func TestNotificationEventUpdatePreservesDeliveryTrail(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, notifEvent("doki", "before"), 1000)

	// Put a send on the record: claim starts the cooldown, the result
	// records a partial failure.
	allowed, _, err := st.ClaimNotificationSend(ctx, id, "live", 1500)
	if err != nil || !allowed {
		t.Fatalf("claim: allowed=%v err=%v", allowed, err)
	}
	if err := st.RecordNotificationResult(ctx, id, true, "1 of 2 webhooks failed", 1501); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The edit body an admin would PUT carries zero trail values.
	edit := model.NotificationEvent{
		ID:           id,
		ChannelKey:   "doki",
		Name:         "after",
		Enabled:      false,
		Webhooks:     model.WebhooksFromURLs("https://discord.com/api/webhooks/3333/token-three"),
		Triggers:     []string{"upload", "short", "scheduled"},
		Content:      "new upload: {url}",
		EmbedEnabled: false,
		Embed: model.EmbedTemplate{
			Title: "{title}", Color: "#FF0000", Footer: "changed", Timestamp: false,
		},
		CooldownSeconds: 300,
	}
	if err := st.UpdateNotificationEvent(ctx, edit, 2000); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := notifMustGet(t, st, "doki", id)
	if got.Name != "after" || got.Enabled || got.Content != edit.Content || got.EmbedEnabled ||
		got.CooldownSeconds != 300 {
		t.Errorf("editable fields not applied: %+v", got)
	}
	if !reflect.DeepEqual(got.WebhookURLs(), edit.WebhookURLs()) {
		t.Errorf("WebhookURLs = %v, want %v", got.WebhookURLs(), edit.WebhookURLs())
	}
	if !reflect.DeepEqual(got.Triggers, edit.Triggers) {
		t.Errorf("Triggers = %v, want %v", got.Triggers, edit.Triggers)
	}
	if got.Embed != edit.Embed {
		t.Errorf("Embed = %+v, want %+v", got.Embed, edit.Embed)
	}
	if got.LastSentAt != 1500 {
		t.Errorf("LastSentAt = %d, want 1500 (preserved)", got.LastSentAt)
	}
	if got.SentCount != 1 {
		t.Errorf("SentCount = %d, want 1 (preserved)", got.SentCount)
	}
	if got.LastError != "1 of 2 webhooks failed" || got.LastErrorAt != 1501 {
		t.Errorf("LastError = (%q, %d), want preserved (\"1 of 2 webhooks failed\", 1501)", got.LastError, got.LastErrorAt)
	}
	if got.CreatedAt != 1000 {
		t.Errorf("CreatedAt = %d, want 1000 (unchanged)", got.CreatedAt)
	}
	if got.UpdatedAt != 2000 {
		t.Errorf("UpdatedAt = %d, want 2000", got.UpdatedAt)
	}

	// Updating an id that does not exist at all is also ErrNotFound.
	missing := edit
	missing.ID = id + 1000
	if err := st.UpdateNotificationEvent(ctx, missing, 2001); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of missing id: err = %v, want ErrNotFound", err)
	}
}

// A no-change edit (same values) must still count as found: SQLite reports
// matched rows, not changed rows, and the handler answers 404 on zero.
func TestNotificationEventUpdateIdenticalValuesIsNotNotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := notifEvent("doki", "same")
	ev.ID = notifMustCreate(t, st, ev, 1000)
	if err := st.UpdateNotificationEvent(ctx, ev, 1000); err != nil {
		t.Fatalf("identical update: %v", err)
	}
}

func TestNotificationEventDelete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	keep := notifMustCreate(t, st, notifEvent("doki", "keep"), 1)
	gone := notifMustCreate(t, st, notifEvent("doki", "gone"), 2)

	rows, err := st.DeleteNotificationEvent(ctx, "doki", gone)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows != 1 {
		t.Fatalf("delete removed %d rows, want 1", rows)
	}

	got, err := st.GetNotificationEvent(ctx, "doki", gone)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got != nil {
		t.Errorf("deleted rule still readable: %+v", got)
	}

	// Deleting again is a clean zero, not an error.
	rows, err = st.DeleteNotificationEvent(ctx, "doki", gone)
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if rows != 0 {
		t.Errorf("second delete removed %d rows, want 0", rows)
	}

	// The sibling rule is untouched.
	notifMustGet(t, st, "doki", keep)
	list, err := st.ListNotificationEvents(ctx, "doki")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != keep {
		t.Errorf("list after delete = %+v, want only %d", list, keep)
	}
}

// The cooldown is measured from the claim: the first claim wins and starts
// the window, claims inside the window are refused with the time left, and
// the boundary (last_sent_at + cooldown == now) is open again.
func TestClaimNotificationSendCooldownWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, notifEvent("doki", "cooldown"), 1)

	allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", 1000)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !allowed || remaining != 0 {
		t.Fatalf("first claim = (%v, %d), want (true, 0)", allowed, remaining)
	}
	if got := notifMustGet(t, st, "doki", id); got.LastSentAt != 1000 {
		t.Fatalf("LastSentAt after claim = %d, want 1000", got.LastSentAt)
	}

	// Inside the 60s window: refused, with the exact seconds left.
	for _, tc := range []struct{ now, want int64 }{
		{1000, 60},
		{1010, 50},
		{1059, 1},
	} {
		allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", tc.now)
		if err != nil {
			t.Fatalf("claim at %d: %v", tc.now, err)
		}
		if allowed {
			t.Errorf("claim at %d was allowed inside the cooldown", tc.now)
		}
		if remaining != tc.want {
			t.Errorf("claim at %d: remaining = %d, want %d", tc.now, remaining, tc.want)
		}
	}
	// A refused claim must not have moved the cooldown.
	if got := notifMustGet(t, st, "doki", id); got.LastSentAt != 1000 {
		t.Fatalf("refused claims moved LastSentAt to %d", got.LastSentAt)
	}

	// Exactly at the boundary the window has elapsed.
	allowed, remaining, err = st.ClaimNotificationSend(ctx, id, "live", 1060)
	if err != nil {
		t.Fatalf("boundary claim: %v", err)
	}
	if !allowed || remaining != 0 {
		t.Fatalf("boundary claim = (%v, %d), want (true, 0)", allowed, remaining)
	}
	if got := notifMustGet(t, st, "doki", id); got.LastSentAt != 1060 {
		t.Fatalf("LastSentAt after second claim = %d, want 1060", got.LastSentAt)
	}

	// And the new window is in force immediately.
	allowed, remaining, err = st.ClaimNotificationSend(ctx, id, "live", 1061)
	if err != nil {
		t.Fatalf("claim in new window: %v", err)
	}
	if allowed || remaining != 59 {
		t.Fatalf("claim in new window = (%v, %d), want (false, 59)", allowed, remaining)
	}
}

// A disabled rule is never allowed to send, whether it was created disabled
// or switched off after a send; a rule that no longer exists is likewise a
// quiet no.
func TestClaimNotificationSendDisabledOrMissingNeverAllowed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	off := notifEvent("doki", "off")
	off.Enabled = false
	off.CooldownSeconds = 0
	offID := notifMustCreate(t, st, off, 1)

	for _, now := range []int64{1000, 5000, 1 << 40} {
		allowed, _, err := st.ClaimNotificationSend(ctx, offID, "live", now)
		if err != nil {
			t.Fatalf("claim disabled at %d: %v", now, err)
		}
		if allowed {
			t.Errorf("disabled rule was allowed to send at %d", now)
		}
	}
	if got := notifMustGet(t, st, "doki", offID); got.LastSentAt != 0 {
		t.Errorf("disabled rule's LastSentAt moved to %d", got.LastSentAt)
	}

	// Enabled, sends once, then gets switched off by an admin edit.
	on := notifEvent("doki", "on-then-off")
	onID := notifMustCreate(t, st, on, 1)
	if allowed, _, err := st.ClaimNotificationSend(ctx, onID, "live", 1000); err != nil || !allowed {
		t.Fatalf("claim enabled: allowed=%v err=%v", allowed, err)
	}
	on.ID = onID
	on.Enabled = false
	if err := st.UpdateNotificationEvent(ctx, on, 1001); err != nil {
		t.Fatalf("disable: %v", err)
	}
	allowed, _, err := st.ClaimNotificationSend(ctx, onID, "live", 10_000)
	if err != nil {
		t.Fatalf("claim after disable: %v", err)
	}
	if allowed {
		t.Error("a rule disabled after its last send was allowed to send again")
	}

	// A rule that has been deleted since it was listed.
	allowed, remaining, err := st.ClaimNotificationSend(ctx, 987654, "live", 1000)
	if err != nil {
		t.Fatalf("claim missing: %v", err)
	}
	if allowed || remaining != 0 {
		t.Errorf("claim on missing rule = (%v, %d), want (false, 0)", allowed, remaining)
	}
}

// Cooldown zero disables the limit: every claim is allowed, even at the same
// instant as the previous one.
func TestClaimNotificationSendZeroCooldownAlwaysAllowed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := notifEvent("doki", "no-limit")
	ev.CooldownSeconds = 0
	id := notifMustCreate(t, st, ev, 1)

	for i, now := range []int64{1000, 1000, 1000, 1001, 1001, 5000} {
		allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", now)
		if err != nil {
			t.Fatalf("claim %d at %d: %v", i, now, err)
		}
		if !allowed || remaining != 0 {
			t.Errorf("claim %d at %d = (%v, %d), want (true, 0)", i, now, allowed, remaining)
		}
	}
}

// With cooldown zero there is no window to respect, so a claim stamped a
// little earlier than the previous one (the dispatcher uses the wall clock,
// which an NTP correction can step backwards) should still be allowed.
func TestClaimNotificationSendZeroCooldownSurvivesClockStepBack(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := notifEvent("doki", "no-limit")
	ev.CooldownSeconds = 0
	id := notifMustCreate(t, st, ev, 1)

	if allowed, _, err := st.ClaimNotificationSend(ctx, id, "live", 1001); err != nil || !allowed {
		t.Fatalf("claim at 1001: allowed=%v err=%v", allowed, err)
	}
	allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", 999)
	if err != nil {
		t.Fatalf("claim at 999: %v", err)
	}
	if !allowed {
		t.Fatalf("claim at 999 refused with remaining=%d; a zero cooldown must never be a window", remaining)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
}

// The same clock step-back must not EXTEND a non-zero cooldown either: a
// last_sent_at in the future is treated as "already waited out", never as a
// gap that grows by however far the clock moved.
func TestClaimNotificationSendClockStepBackDoesNotExtendCooldown(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := notifEvent("doki", "limited")
	ev.CooldownSeconds = 60
	id := notifMustCreate(t, st, ev, 1)

	if allowed, _, err := st.ClaimNotificationSend(ctx, id, "live", 1000); err != nil || !allowed {
		t.Fatalf("first claim: allowed=%v err=%v", allowed, err)
	}
	// Inside the window, clock moving forward: refused with the remainder.
	if allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", 1030); err != nil || allowed || remaining != 30 {
		t.Fatalf("claim at 1030: allowed=%v remaining=%d err=%v, want refused with 30s left", allowed, remaining, err)
	}
	// Clock stepped back past the last send: allowed rather than refused for
	// an extra 60s on top of the step.
	if allowed, remaining, err := st.ClaimNotificationSend(ctx, id, "live", 990); err != nil || !allowed || remaining != 0 {
		t.Fatalf("claim at 990: allowed=%v remaining=%d err=%v, want allowed", allowed, remaining, err)
	}
}

// Two detections racing for the same rule (a restart, two mechanisms seeing
// one stream) must let exactly one through. The guard lives in the UPDATE's
// WHERE clause, so this holds without any locking in Go.
func TestClaimNotificationSendConcurrentRaceLetsOneThrough(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, notifEvent("doki", "raced"), 1)

	const racers = 32
	var wg sync.WaitGroup
	allowed := make([]bool, racers)
	remaining := make([]int64, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, rem, err := st.ClaimNotificationSend(ctx, id, "live", 1000)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			allowed[i] = ok
			remaining[i] = rem
		}()
	}
	wg.Wait()

	winners := 0
	for i := range racers {
		if allowed[i] {
			winners++
			continue
		}
		// Every loser ran after the winner's UPDATE, so each sees the full
		// window left.
		if remaining[i] != 60 {
			t.Errorf("loser %d saw remaining = %d, want 60", i, remaining[i])
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
}

// The delivery trail after an attempt: a delivered send bumps the counter and
// clears (or records, for a partial) the error; a failed send records the
// error without counting a send.
func TestRecordNotificationResult(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id := notifMustCreate(t, st, notifEvent("doki", "trail"), 1)

	// Total failure: nothing sent, error recorded with its time.
	if err := st.RecordNotificationResult(ctx, id, false, "all webhooks failed: 404", 1000); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	got := notifMustGet(t, st, "doki", id)
	if got.SentCount != 0 {
		t.Errorf("SentCount after failure = %d, want 0", got.SentCount)
	}
	if got.LastError != "all webhooks failed: 404" || got.LastErrorAt != 1000 {
		t.Errorf("failure trail = (%q, %d), want (\"all webhooks failed: 404\", 1000)", got.LastError, got.LastErrorAt)
	}

	// Full success: counted and the earlier error is cleared.
	if err := st.RecordNotificationResult(ctx, id, true, "", 2000); err != nil {
		t.Fatalf("record success: %v", err)
	}
	got = notifMustGet(t, st, "doki", id)
	if got.SentCount != 1 {
		t.Errorf("SentCount after success = %d, want 1", got.SentCount)
	}
	if got.LastError != "" || got.LastErrorAt != 0 {
		t.Errorf("success must clear the error, got (%q, %d)", got.LastError, got.LastErrorAt)
	}

	// Partial: delivered somewhere, so it counts, but the failure is kept.
	if err := st.RecordNotificationResult(ctx, id, true, "1 of 2 webhooks failed", 3000); err != nil {
		t.Fatalf("record partial: %v", err)
	}
	got = notifMustGet(t, st, "doki", id)
	if got.SentCount != 2 {
		t.Errorf("SentCount after partial = %d, want 2", got.SentCount)
	}
	if got.LastError != "1 of 2 webhooks failed" || got.LastErrorAt != 3000 {
		t.Errorf("partial trail = (%q, %d), want (\"1 of 2 webhooks failed\", 3000)", got.LastError, got.LastErrorAt)
	}

	// Another full success clears the partial's error too.
	if err := st.RecordNotificationResult(ctx, id, true, "", 4000); err != nil {
		t.Fatalf("record success 2: %v", err)
	}
	got = notifMustGet(t, st, "doki", id)
	if got.SentCount != 3 || got.LastError != "" || got.LastErrorAt != 0 {
		t.Errorf("after second success: count=%d err=%q errAt=%d, want 3, \"\", 0", got.SentCount, got.LastError, got.LastErrorAt)
	}

	// Recording against a rule that does not exist is a no-op, not an error.
	if err := st.RecordNotificationResult(ctx, id+1000, true, "", 5000); err != nil {
		t.Errorf("record on missing rule: %v", err)
	}
}

// The log is a bounded recent-activity view: each insert trims the channel
// to notificationLogKeep rows, keeping the newest, and never touches another
// channel's rows.
func TestInsertNotificationLogTrimsPerChannel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const extra = 50
	total := int64(notificationLogKeep + extra)

	// A handful of rows on another channel, inserted first so they have the
	// lowest ids: a channel-blind trim would remove exactly these.
	for i := int64(1); i <= 5; i++ {
		if err := st.InsertNotificationLog(ctx, notifLogEntry("mint", i)); err != nil {
			t.Fatalf("insert mint %d: %v", i, err)
		}
	}
	for i := int64(1); i <= total; i++ {
		if err := st.InsertNotificationLog(ctx, notifLogEntry("doki", i)); err != nil {
			t.Fatalf("insert doki %d: %v", i, err)
		}
	}

	if n := notifLogCount(t, st, "doki"); n != notificationLogKeep {
		t.Fatalf("doki has %d log rows, want %d", n, notificationLogKeep)
	}
	if n := notifLogCount(t, st, "mint"); n != 5 {
		t.Fatalf("mint has %d log rows, want 5 (untouched by doki's trim)", n)
	}

	// The survivors are the newest: seq extra+1 .. total.
	got, err := st.ListNotificationLog(ctx, "doki", notificationLogKeep)
	if err != nil {
		t.Fatalf("list doki: %v", err)
	}
	if len(got) != notificationLogKeep {
		t.Fatalf("list returned %d rows, want %d", len(got), notificationLogKeep)
	}
	if got[0].SentAt != total {
		t.Errorf("newest surviving seq = %d, want %d", got[0].SentAt, total)
	}
	if got[len(got)-1].SentAt != extra+1 {
		t.Errorf("oldest surviving seq = %d, want %d (the first %d were trimmed)", got[len(got)-1].SentAt, extra+1, extra)
	}

	mint, err := st.ListNotificationLog(ctx, "mint", 10)
	if err != nil {
		t.Fatalf("list mint: %v", err)
	}
	if len(mint) != 5 || mint[0].SentAt != 5 || mint[4].SentAt != 1 {
		t.Errorf("mint rows = %+v, want the original 5 newest-first", mint)
	}
}

// ListNotificationLog returns newest first, round-trips every column, honours
// the limit, and falls back to the default page size for a bad limit.
func TestListNotificationLogNewestFirstAndLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	empty, err := st.ListNotificationLog(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no rows initially, got %d", len(empty))
	}

	const rows = 60
	for i := int64(1); i <= rows; i++ {
		e := notifLogEntry("doki", i)
		if i%2 == 0 {
			e.Status = model.NotificationStatusSuppressed
			e.Detail = "cooldown: 42s left"
			e.Delivered = 0
		}
		if err := st.InsertNotificationLog(ctx, e); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := st.InsertNotificationLog(ctx, notifLogEntry("mint", 1)); err != nil {
		t.Fatalf("insert mint: %v", err)
	}

	got, err := st.ListNotificationLog(ctx, "doki", 3)
	if err != nil {
		t.Fatalf("list limit 3: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit 3 returned %d rows", len(got))
	}
	for i, want := range []int64{rows, rows - 1, rows - 2} {
		if got[i].SentAt != want {
			t.Errorf("row %d seq = %d, want %d (newest first)", i, got[i].SentAt, want)
		}
		if i > 0 && got[i].ID >= got[i-1].ID {
			t.Errorf("row %d id %d is not below row %d id %d", i, got[i].ID, i-1, got[i-1].ID)
		}
	}

	// Full column round trip on the newest (an even seq: suppressed).
	newest := got[0]
	want := notifLogEntry("doki", rows)
	want.Status = model.NotificationStatusSuppressed
	want.Detail = "cooldown: 42s left"
	want.Delivered = 0
	want.ID = newest.ID
	if newest != want {
		t.Errorf("newest row = %+v, want %+v", newest, want)
	}

	// Exact limit.
	got, err = st.ListNotificationLog(ctx, "doki", rows)
	if err != nil {
		t.Fatalf("list limit %d: %v", rows, err)
	}
	if len(got) != rows {
		t.Errorf("limit %d returned %d rows", rows, len(got))
	}

	// A non-positive limit falls back to the default page of 50.
	got, err = st.ListNotificationLog(ctx, "doki", 0)
	if err != nil {
		t.Fatalf("list limit 0: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("limit 0 returned %d rows, want the default 50", len(got))
	}
	if got[0].SentAt != rows {
		t.Errorf("default page starts at seq %d, want %d", got[0].SentAt, rows)
	}
	got, err = st.ListNotificationLog(ctx, "doki", -5)
	if err != nil {
		t.Fatalf("list limit -5: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("limit -5 returned %d rows, want the default 50", len(got))
	}

	// A limit above the retention cap is also replaced by the default.
	got, err = st.ListNotificationLog(ctx, "doki", notificationLogKeep+1)
	if err != nil {
		t.Fatalf("list limit over cap: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("limit %d returned %d rows, want the default 50", notificationLogKeep+1, len(got))
	}

	// Scoped to the channel.
	for _, e := range got {
		if e.ChannelKey != "doki" {
			t.Fatalf("list leaked a %q row", e.ChannelKey)
		}
	}
}

// The non-live ledger is keyed on (platform, video_id, kind): re-observing
// the same event is a silent no-op that keeps the first row's data, but the
// same video id under a different kind (scheduled, then uploaded) or on a
// different platform is a fresh claim.
func TestClaimVideoDetectionDedupesOnPlatformIDKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := notifVideo("youtube", "abc", "scheduled", "doki", 1000)
	first.ScheduledAt = 5000
	won, err := st.ClaimVideoDetection(ctx, first)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !won {
		t.Fatal("expected the first claim to win")
	}

	// Re-observations, including with changed metadata, never win and never
	// overwrite.
	for i := range 20 {
		again := first
		again.Title = fmt.Sprintf("renamed %d", i)
		again.DetectedAt = int64(2000 + i)
		won, err := st.ClaimVideoDetection(ctx, again)
		if err != nil {
			t.Fatalf("re-claim %d: %v", i, err)
		}
		if won {
			t.Fatalf("re-claim %d won; an event must only ever be claimed once", i)
		}
	}

	// Same id, different kind: allowed.
	upload := notifVideo("youtube", "abc", "upload", "doki", 3000)
	won, err = st.ClaimVideoDetection(ctx, upload)
	if err != nil {
		t.Fatalf("upload claim: %v", err)
	}
	if !won {
		t.Fatal("the same video id under a different kind must be a new claim")
	}

	// Same id and kind, different platform: allowed.
	won, err = st.ClaimVideoDetection(ctx, notifVideo("twitch", "abc", "scheduled", "doki", 3500))
	if err != nil {
		t.Fatalf("twitch claim: %v", err)
	}
	if !won {
		t.Fatal("the same id on a different platform is a different event")
	}

	// The original row kept its first-seen data.
	got, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	var sched *model.DetectedVideo
	for i := range got {
		if got[i].Platform == "youtube" && got[i].Kind == "scheduled" {
			sched = &got[i]
		}
	}
	if sched == nil {
		t.Fatal("scheduled row missing")
	}
	if *sched != first {
		t.Errorf("scheduled row = %+v, want the first claim's data %+v", *sched, first)
	}
}

// The poller and a WebSub push can see the same upload at once; exactly one
// caller may announce it.
func TestClaimVideoDetectionConcurrentRace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const racers = 16
	var wg sync.WaitGroup
	wins := make([]bool, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := st.ClaimVideoDetection(ctx, notifVideo("youtube", "raced", "upload", "doki", 1000))
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			wins[i] = won
		}()
	}
	wg.Wait()

	total := 0
	for _, w := range wins {
		if w {
			total++
		}
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", total)
	}
}

// Newest detection first; ties on detected_at break toward the later insert;
// scoped to the channel; a non-positive limit means the default page.
func TestGetRecentVideoDetectionsOrdering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	empty, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("recent empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no rows initially, got %d", len(empty))
	}

	// Inserted out of time order on purpose.
	inserts := []model.DetectedVideo{
		notifVideo("youtube", "mid", "upload", "doki", 1001),
		notifVideo("youtube", "old", "short", "doki", 1000),
		notifVideo("youtube", "new", "scheduled", "doki", 1002),
		notifVideo("youtube", "tie-first", "upload", "doki", 1002),
		notifVideo("youtube", "tie-second", "upload", "doki", 1002),
		notifVideo("youtube", "other", "upload", "mint", 9999),
	}
	for _, v := range inserts {
		if won, err := st.ClaimVideoDetection(ctx, v); err != nil || !won {
			t.Fatalf("claim %s: won=%v err=%v", v.VideoID, won, err)
		}
	}

	got, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	wantOrder := []string{"tie-second", "tie-first", "new", "mid", "old"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d rows, want %d scoped to doki", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].VideoID != id {
			t.Errorf("row %d = %q, want %q", i, got[i].VideoID, id)
		}
		if got[i].ChannelKey != "doki" {
			t.Errorf("row %d belongs to %q", i, got[i].ChannelKey)
		}
	}

	// Every column comes back.
	if got[4] != inserts[1] {
		t.Errorf("oldest row = %+v, want %+v", got[4], inserts[1])
	}

	// Limit is honoured and keeps the newest.
	got, err = st.GetRecentVideoDetections(ctx, "doki", 2)
	if err != nil {
		t.Fatalf("recent limit 2: %v", err)
	}
	if len(got) != 2 || got[0].VideoID != "tie-second" || got[1].VideoID != "tie-first" {
		t.Errorf("limit 2 = %+v, want the two newest", got)
	}

	// A non-positive limit means the default page of 20: fill past it.
	for i := range 30 {
		v := notifVideo("youtube", fmt.Sprintf("bulk-%d", i), "upload", "doki", int64(2000+i))
		if _, err := st.ClaimVideoDetection(ctx, v); err != nil {
			t.Fatalf("claim bulk %d: %v", i, err)
		}
	}
	got, err = st.GetRecentVideoDetections(ctx, "doki", 0)
	if err != nil {
		t.Fatalf("recent limit 0: %v", err)
	}
	if len(got) != 20 {
		t.Errorf("limit 0 returned %d rows, want the default 20", len(got))
	}
	if got[0].VideoID != "bulk-29" {
		t.Errorf("default page starts at %q, want bulk-29", got[0].VideoID)
	}
}

// Cleanup drops rows detected strictly before the cutoff and reports how
// many; a row at exactly the cutoff survives. Once a row is gone the same
// event can be claimed again, which is why retention must exceed the
// announcement window.
func TestCleanupOldVideoDetectionsCutoff(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for _, v := range []model.DetectedVideo{
		notifVideo("youtube", "ancient", "upload", "doki", 100),
		notifVideo("youtube", "old", "short", "doki", 500),
		notifVideo("youtube", "edge", "upload", "doki", 1000),
		notifVideo("youtube", "fresh", "scheduled", "doki", 1500),
		notifVideo("youtube", "other-old", "upload", "mint", 200),
	} {
		if won, err := st.ClaimVideoDetection(ctx, v); err != nil || !won {
			t.Fatalf("claim %s: won=%v err=%v", v.VideoID, won, err)
		}
	}

	removed, err := st.CleanupOldVideoDetections(ctx, 1000)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d rows, want 3 (ancient, old, other-old)", removed)
	}

	got, err := st.GetRecentVideoDetections(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("recent doki: %v", err)
	}
	if len(got) != 2 || got[0].VideoID != "fresh" || got[1].VideoID != "edge" {
		t.Errorf("doki after cleanup = %+v, want [fresh edge]", got)
	}
	mint, err := st.GetRecentVideoDetections(ctx, "mint", 10)
	if err != nil {
		t.Fatalf("recent mint: %v", err)
	}
	if len(mint) != 0 {
		t.Errorf("mint after cleanup = %+v, want none (cleanup is not channel-scoped)", mint)
	}

	// Nothing left below the cutoff: a second pass removes nothing.
	removed, err = st.CleanupOldVideoDetections(ctx, 1000)
	if err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if removed != 0 {
		t.Errorf("second cleanup removed %d rows, want 0", removed)
	}

	// A pruned event is claimable again; a surviving one is not.
	won, err := st.ClaimVideoDetection(ctx, notifVideo("youtube", "ancient", "upload", "doki", 2000))
	if err != nil {
		t.Fatalf("re-claim pruned: %v", err)
	}
	if !won {
		t.Error("a pruned row must be claimable again")
	}
	won, err = st.ClaimVideoDetection(ctx, notifVideo("youtube", "edge", "upload", "doki", 2000))
	if err != nil {
		t.Fatalf("re-claim survivor: %v", err)
	}
	if won {
		t.Error("a row at exactly the cutoff must survive and still dedupe")
	}
}
