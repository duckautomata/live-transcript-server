package store

import (
	"context"
	"errors"
	"testing"

	"live-transcript-server/internal/model"
)

func TestUsersCreateGetAndUniqueness(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateUser(ctx, "Doki", "doki", "$hash$", 1000)
	if err != nil || id == 0 {
		t.Fatalf("CreateUser: %d %v", id, err)
	}
	if _, err := st.CreateUser(ctx, "DOKI", "doki", "$other$", 1001); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("duplicate key: err=%v, want ErrUsernameTaken", err)
	}
	u, err := st.GetUserByUsernameKey(ctx, "doki")
	if err != nil || u == nil || u.ID != id || u.Username != "Doki" || u.PasswordHash != "$hash$" || u.CreatedAt != 1000 {
		t.Fatalf("GetUserByUsernameKey = %+v, %v", u, err)
	}
	if u, err := st.GetUserByUsernameKey(ctx, "nobody"); err != nil || u != nil {
		t.Errorf("unknown key = %+v, %v; want nil, nil", u, err)
	}
	if u, err := st.GetUserByID(ctx, id); err != nil || u == nil || u.Username != "Doki" {
		t.Errorf("GetUserByID = %+v, %v", u, err)
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Errorf("CountUsers = %d", n)
	}
	names, err := st.Usernames(ctx, []int64{id, 0, 999, id})
	if err != nil || len(names) != 1 || names[id] != (UserLabel{Username: "Doki"}) {
		t.Errorf("Usernames = %v, %v", names, err)
	}
}

func TestUsersLoginAndPasswordChange(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateUser(ctx, "doki", "doki", "$hash$", 1000)

	if err := st.RecordLoginSuccess(ctx, id, 6000, "$rehashed$"); err != nil {
		t.Fatalf("RecordLoginSuccess: %v", err)
	}
	u, _ := st.GetUserByID(ctx, id)
	if u.LastLoginAt != 6000 || u.PasswordHash != "$rehashed$" {
		t.Errorf("after success: %+v", u)
	}
	if err := st.RecordLoginSuccess(ctx, id, 6500, ""); err != nil {
		t.Fatalf("RecordLoginSuccess without rehash: %v", err)
	}
	if u, _ = st.GetUserByID(ctx, id); u.LastLoginAt != 6500 || u.PasswordHash != "$rehashed$" {
		t.Errorf("a login without a rehash changed the hash: %+v", u)
	}

	// A password change ends every other session in the same transaction.
	keep, _ := st.CreateSession(ctx, id, "keep", 1000, 9000, "", "")
	st.CreateSession(ctx, id, "other-1", 1000, 9000, "", "")
	st.CreateSession(ctx, id, "other-2", 1000, 9000, "", "")
	ended, err := st.ChangePassword(ctx, id, "$new$", 7000, keep)
	if err != nil || ended != 2 {
		t.Fatalf("ChangePassword = %d, %v; want 2 sessions ended", ended, err)
	}
	if u, _ = st.GetUserByID(ctx, id); u.PasswordHash != "$new$" || u.PasswordChangedAt != 7000 {
		t.Errorf("after password change: %+v", u)
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "keep", 2000); s == nil {
		t.Error("the changing session was ended")
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "other-1", 2000); s != nil {
		t.Error("another session survived the password change")
	}
}

func TestSessionsLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateUser(ctx, "doki", "doki", "$hash$", 1000)

	if _, err := st.CreateSession(ctx, id, "hash-a", 1000, 2000, "browser a", "203.0.113.1"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sidB, err := st.CreateSession(ctx, id, "hash-b", 1000, 2000, "browser b", "203.0.113.2")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := st.CreateSession(ctx, id, "hash-a", 1000, 2000, "", ""); err == nil {
		t.Error("a duplicate token hash was accepted")
	}

	sess, u, err := st.GetSessionByTokenHash(ctx, "hash-a", 1500)
	if err != nil || sess == nil || u == nil || u.ID != id || sess.Client != "browser a" || sess.ExpiresAt != 2000 {
		t.Fatalf("GetSessionByTokenHash = %+v, %+v, %v", sess, u, err)
	}
	if sess, _, _ := st.GetSessionByTokenHash(ctx, "hash-a", 2000); sess != nil {
		t.Error("an expired session was returned")
	}
	if sess, _, _ := st.GetSessionByTokenHash(ctx, "hash-zzz", 1500); sess != nil {
		t.Error("an unknown hash found a session")
	}
	if n, _ := st.CountUserSessions(ctx, id, 1500); n != 2 {
		t.Errorf("CountUserSessions = %d", n)
	}

	if err := st.TouchSession(ctx, sess.ID, 1800, 3000); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "hash-a", 2500); s == nil || s.LastSeenAt != 1800 {
		t.Errorf("after touch: %+v", s)
	}

	if ok, err := st.DeleteSession(ctx, "hash-b"); err != nil || !ok {
		t.Errorf("DeleteSession = %v, %v", ok, err)
	}
	if ok, _ := st.DeleteSession(ctx, "hash-b"); ok {
		t.Error("deleting twice reported a row")
	}
	if _, err := st.CreateSession(ctx, id, "hash-c", 1000, 2000, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.DeleteUserSessions(ctx, id, sess.ID); n != 1 {
		t.Errorf("DeleteUserSessions kept the wrong ones: %d ended", n)
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "hash-a", 2500); s == nil {
		t.Error("the kept session is gone")
	}
	_ = sidB

	if n, _ := st.CleanupExpiredSessions(ctx, 3000); n != 1 {
		t.Errorf("CleanupExpiredSessions = %d, want the one now expired", n)
	}
}

// Deleting a user takes sessions, rules, cooldowns and log rows with it, and
// leaves other accounts alone.
func TestDeleteUserCascades(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	me, _ := st.CreateUser(ctx, "me", "me", "$h$", 1000)
	you, _ := st.CreateUser(ctx, "you", "you", "$h$", 1000)
	st.CreateSession(ctx, me, "mine", 1000, 9000, "", "")
	st.CreateSession(ctx, you, "yours", 1000, 9000, "", "")

	mk := func(user int64, name string) int64 {
		id, err := st.CreateNotificationEvent(ctx, model.NotificationEvent{
			ChannelKey: "doki", UserID: user, Name: name, Enabled: true, Triggers: []string{"live"},
			Webhooks: model.WebhooksFromURLs("https://discord.com/api/webhooks/1/x"),
		}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mineID := mk(me, "mine")
	yoursID := mk(you, "yours")
	if _, _, err := st.ClaimNotificationSend(ctx, mineID, "live", 1000); err != nil {
		t.Fatal(err)
	}
	for _, e := range []model.NotificationLogEntry{
		{ChannelKey: "doki", EventID: mineID, UserID: me, Status: "sent", SentAt: 1000},
		{ChannelKey: "doki", EventID: yoursID, UserID: you, Status: "sent", SentAt: 1000},
	} {
		if err := st.InsertNotificationLog(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DeleteUser(ctx, me); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if u, _ := st.GetUserByID(ctx, me); u != nil {
		t.Error("user still exists")
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "mine", 1000); s != nil {
		t.Error("session survived")
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "yours", 1000); s == nil {
		t.Error("the other account's session was removed")
	}
	events, _ := st.ListNotificationEvents(ctx, "doki")
	if len(events) != 1 || events[0].ID != yoursID {
		t.Errorf("events after delete = %+v", events)
	}
	log, _ := st.ListNotificationLog(ctx, "doki", 10)
	if len(log) != 1 || log[0].UserID != you {
		t.Errorf("log after delete = %+v", log)
	}
	mine, _ := st.ListNotificationEventsForUser(ctx, me, "doki")
	if len(mine) != 0 {
		t.Errorf("deleted user still lists %+v", mine)
	}
}

// Every account-facing query is scoped by owner.
func TestNotificationEventsAreScopedByUser(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a, _ := st.CreateUser(ctx, "a", "a", "$h$", 1000)
	b, _ := st.CreateUser(ctx, "b", "b", "$h$", 1000)
	ev := model.NotificationEvent{
		ChannelKey: "doki", UserID: a, Name: "A's", Enabled: true, Triggers: []string{"live"},
		Webhooks: model.WebhooksFromURLs("https://discord.com/api/webhooks/1/x"),
	}
	id, err := st.CreateNotificationEvent(ctx, ev, 1000)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := st.GetNotificationEventForUser(ctx, b, "doki", id); got != nil {
		t.Error("b can read a's rule")
	}
	if got, _ := st.GetNotificationEventForUser(ctx, a, "mint", id); got != nil {
		t.Error("a's rule is visible on another channel")
	}
	if got, _ := st.GetNotificationEventForUser(ctx, a, "doki", id); got == nil || got.UserID != a {
		t.Errorf("a cannot read its own rule: %+v", got)
	}
	if list, _ := st.ListNotificationEventsForUser(ctx, b, "doki"); len(list) != 0 {
		t.Errorf("b lists %+v", list)
	}
	if n, _ := st.CountNotificationEventsForUser(ctx, a, "doki"); n != 1 {
		t.Errorf("count for a = %d", n)
	}

	hijack := ev
	hijack.ID, hijack.UserID, hijack.Name = id, b, "B's now"
	if err := st.UpdateNotificationEvent(ctx, hijack, 2000); !errors.Is(err, ErrNotFound) {
		t.Errorf("b updating a's rule: err=%v, want ErrNotFound", err)
	}
	if n, _ := st.DeleteNotificationEventForUser(ctx, b, "doki", id); n != 0 {
		t.Error("b deleted a's rule")
	}
	if got, _ := st.GetNotificationEvent(ctx, "doki", id); got == nil || got.Name != "A's" {
		t.Errorf("rule changed: %+v", got)
	}
	// A rule from before accounts existed (user 0) is not editable by anyone.
	legacy := ev
	legacy.UserID = 0
	lid, _ := st.CreateNotificationEvent(ctx, legacy, 1000)
	legacy.ID, legacy.UserID = lid, a
	if err := st.UpdateNotificationEvent(ctx, legacy, 2000); !errors.Is(err, ErrNotFound) {
		t.Errorf("editing a legacy rule: err=%v, want ErrNotFound", err)
	}
	if n, _ := st.DeleteNotificationEvent(ctx, "doki", lid); n != 1 {
		t.Error("the operator could not delete the legacy rule")
	}
}

// A shared account lists everyone signed in to it and can end any one of
// its own sessions - but never another account's.
func TestSessionsListAndRevoke(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	me, _ := st.CreateUser(ctx, "team", "team", "$h$", 1000)
	you, _ := st.CreateUser(ctx, "other", "other", "$h$", 1000)
	a, _ := st.CreateSession(ctx, me, "hash-a", 1000, 9000, "Chrome", "203.0.113.1")
	b, _ := st.CreateSession(ctx, me, "hash-b", 1500, 9000, "Firefox", "203.0.113.2")
	old, _ := st.CreateSession(ctx, me, "hash-old", 100, 200, "expired", "")
	theirs, _ := st.CreateSession(ctx, you, "hash-theirs", 1000, 9000, "", "")
	if err := st.TouchSession(ctx, a, 2000, 9500); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListUserSessions(ctx, me, 1800)
	if err != nil {
		t.Fatalf("ListUserSessions: %v", err)
	}
	if len(list) != 2 || list[0].ID != a || list[1].ID != b {
		t.Fatalf("sessions = %+v, want a (most recently active) then b, without the expired one", list)
	}
	if list[0].Client != "Chrome" || list[0].Address != "203.0.113.1" || list[0].LastSeenAt != 2000 {
		t.Errorf("session a = %+v", list[0])
	}
	_ = old

	if ok, err := st.DeleteUserSession(ctx, me, theirs); err != nil || ok {
		t.Errorf("deleting another account's session = %v, %v; want not found", ok, err)
	}
	if ok, err := st.DeleteUserSession(ctx, me, b); err != nil || !ok {
		t.Errorf("deleting own session = %v, %v", ok, err)
	}
	if ok, _ := st.DeleteUserSession(ctx, me, b); ok {
		t.Error("deleting twice reported a row")
	}
	if list, _ := st.ListUserSessions(ctx, me, 1800); len(list) != 1 || list[0].ID != a {
		t.Errorf("after revoke = %+v", list)
	}
	if s, _, _ := st.GetSessionByTokenHash(ctx, "hash-theirs", 1800); s == nil {
		t.Error("the other account's session was touched")
	}
}
