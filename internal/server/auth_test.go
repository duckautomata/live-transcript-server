package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/auth"
	"live-transcript-server/internal/config"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// authDecode reads a JSON body into v, failing the test on any decode error.
func authDecode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	notifDecode(t, rec, v)
}

func authError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	authDecode(t, rec, &body)
	return body.Error
}

// Registration signs the account in; the token it returns is a session; the
// same credentials sign in again later; and every kind of failed sign-in is
// the same answer, so the form cannot be used to enumerate usernames.
func TestAuthRegisterLoginAndMe(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": "Doki_Fan", "password": "swordfish-tacos"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response carrying a token", cc)
	}
	var sess SessionResponse
	authDecode(t, rec, &sess)
	if sess.Token == "" || sess.Account.Username != "Doki_Fan" || sess.Account.Sessions != 1 {
		t.Fatalf("session = %+v", sess)
	}
	if got := time.Unix(sess.ExpiresAt, 0); got.Before(time.Now().Add(29*24*time.Hour)) || got.After(time.Now().Add(31*24*time.Hour)) {
		t.Errorf("expiresAt = %v, want about 30 days out", got)
	}

	// The token is a session, and the account keeps its capitalisation.
	rec = userReq(t, mux, http.MethodGet, "/auth/me", sess.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var me AccountResponse
	authDecode(t, rec, &me)
	if me.Username != "Doki_Fan" || me.Sessions != 1 {
		t.Errorf("me = %+v", me)
	}

	// Only the hash of the token is stored: the token itself is not a key.
	now := time.Now().Unix()
	if s, _, err := app.Store.GetSessionByTokenHash(context.Background(), sess.Token, now); err != nil || s != nil {
		t.Errorf("the raw token found a session (%v, %v); only its hash should", s, err)
	}
	if s, u, err := app.Store.GetSessionByTokenHash(context.Background(), auth.HashToken(sess.Token), now); err != nil || s == nil || u == nil || u.Username != "Doki_Fan" {
		t.Errorf("the token hash did not find the session: %v %v %v", s, u, err)
	}

	// Signing in again (case-insensitively) is a second session.
	rec = userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "doki_fan", "password": "swordfish-tacos"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var second SessionResponse
	authDecode(t, rec, &second)
	if second.Token == "" || second.Token == sess.Token || second.Account.Sessions != 2 {
		t.Errorf("second session = %+v", second)
	}

	// Wrong password, unknown user and a malformed username all get the same
	// 401 with the same text.
	var answers []string
	for _, creds := range []map[string]string{
		{"username": "Doki_Fan", "password": "wrong"},
		{"username": "nobody-here", "password": "swordfish-tacos"},
		{"username": "!!", "password": "swordfish-tacos"},
	} {
		rec := userReq(t, mux, http.MethodPost, "/auth/login", "", creds)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("login %v: status=%d want 401", creds, rec.Code)
		}
		answers = append(answers, rec.Body.String())
	}
	for _, a := range answers[1:] {
		if a != answers[0] {
			t.Errorf("failed sign-ins differ: %q vs %q", answers[0], a)
		}
	}
	if !strings.Contains(answers[0], "Wrong username or password") {
		t.Errorf("failed sign-in body = %q", answers[0])
	}
}

func TestAuthRegisterValidation(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	cases := []struct {
		name, username, password, want string
	}{
		{"short username", "ab", "swordfish-tacos", "3 to 32"},
		{"bad characters", "doki fan", "swordfish-tacos", "letters, digits"},
		{"leading dot", ".doki", "swordfish-tacos", "start with a letter"},
		{"short password", "doki", "short", "8 to 128"},
		{"common password", "doki", "password123", "too common"},
		{"password is the username", "dokidoki", "DokiDoki", "must not be the username"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": tc.username, "password": tc.password})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if msg := authError(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/register", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("empty body: status=%d want 400", rec.Code)
	}

	// A username is taken whatever its case.
	notifSignUp(t, mux, "Taken", "swordfish-tacos")
	rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": "taken", "password": "another-passphrase"})
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if n, _ := app.Store.CountUsers(context.Background()); n != 1 {
		t.Errorf("accounts = %d, want the one that succeeded", n)
	}

	// The operator can close registration; existing accounts still sign in.
	app.Accounts.DisableRegistration = true
	if rec := userReq(t, mux, http.MethodGet, "/auth/info", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"registrationOpen":false`) {
		t.Errorf("auth info = %d %s, want registrationOpen false", rec.Code, rec.Body.String())
	}
	rec = userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": "late", "password": "swordfish-tacos"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("closed registration: status=%d want 403", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "taken", "password": "swordfish-tacos"}); rec.Code != http.StatusOK {
		t.Errorf("existing account with registration closed: status=%d want 200", rec.Code)
	}
}

func TestAuthLogoutAndLogoutEverywhere(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	first := notifSignUp(t, mux, "doki", "swordfish-tacos")
	login := func() string {
		rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "doki", "password": "swordfish-tacos"})
		if rec.Code != http.StatusOK {
			t.Fatalf("login: status=%d", rec.Code)
		}
		var s SessionResponse
		authDecode(t, rec, &s)
		return s.Token
	}
	second, third := login(), login()

	if rec := userReq(t, mux, http.MethodPost, "/auth/logout", first, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", first, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed-out token still works: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/logout", first, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("second logout of the same token: status=%d want 401", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", second, nil); rec.Code != http.StatusOK {
		t.Errorf("another session was signed out too: status=%d", rec.Code)
	}

	// "Sign out other sessions" keeps the browser that asked.
	rec := userReq(t, mux, http.MethodPost, "/auth/logout-all", second, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessionsEnded":1`) {
		t.Fatalf("logout-all: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", second, nil); rec.Code != http.StatusOK {
		t.Errorf("the requesting session was ended by logout-all: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", third, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("another session survived logout-all: status=%d", rec.Code)
	}
}

// An account can be shared by a whole team: everyone signs in at once, from
// wherever they are, and the sessions list shows each of them. Any member
// can end any session of the account - and only of this account.
func TestAuthSessionsListAndRevoke(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	first := notifSignUp(t, mux, "modteam", "swordfish-tacos")

	signIn := func(ua, ip string) string {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"modteam","password":"swordfish-tacos"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login as %s: status=%d body=%s", ua, rec.Code, rec.Body.String())
		}
		var s SessionResponse
		authDecode(t, rec, &s)
		return s.Token
	}
	phone := signIn("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "198.51.100.7")
	laptop := signIn("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36", "203.0.113.9")

	rec := userReq(t, mux, http.MethodGet, "/auth/sessions", laptop, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Sessions []SessionView `json:"sessions"`
	}
	authDecode(t, rec, &body)
	if len(body.Sessions) != 3 {
		t.Fatalf("sessions = %+v, want the three sign-ins", body.Sessions)
	}
	byLabel := map[string]SessionView{}
	for _, s := range body.Sessions {
		byLabel[s.Label] = s
	}
	if s, ok := byLabel["Chrome on Windows"]; !ok || !s.Current {
		t.Errorf("laptop session = %+v, want current and labelled", byLabel)
	}
	if s, ok := byLabel["Safari on iOS"]; !ok || s.Current {
		t.Errorf("phone session = %+v", byLabel)
	}
	// The listing names devices, never where they signed in from: a shared
	// mod-team account must not leak one member's address to another.
	if strings.Contains(rec.Body.String(), "198.51.100.7") || strings.Contains(rec.Body.String(), "203.0.113.9") {
		t.Errorf("sessions listing exposes addresses: %s", rec.Body.String())
	}
	// Every session works at the same time.
	for _, tok := range []string{first, phone, laptop} {
		if rec := userReq(t, mux, http.MethodGet, "/auth/me", tok, nil); rec.Code != http.StatusOK {
			t.Errorf("a concurrent session was refused: status=%d", rec.Code)
		}
	}

	// One member ends the phone's session; the phone is out, the rest stay.
	phoneID := byLabel["Safari on iOS"].ID
	if rec := userReq(t, mux, http.MethodDelete, fmt.Sprintf("/auth/sessions/%d", phoneID), laptop, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", phone, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked session still works: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", laptop, nil); rec.Code != http.StatusOK {
		t.Errorf("the revoking session was ended too: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodDelete, fmt.Sprintf("/auth/sessions/%d", phoneID), laptop, nil); rec.Code != http.StatusNotFound {
		t.Errorf("revoking twice: status=%d want 404", rec.Code)
	}

	// Another account cannot see or end this account's sessions.
	other := notifSignUp(t, mux, "someone", "another-passphrase")
	laptopID := byLabel["Chrome on Windows"].ID
	if rec := userReq(t, mux, http.MethodDelete, fmt.Sprintf("/auth/sessions/%d", laptopID), other, nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-account revoke: status=%d want 404", rec.Code)
	}
	rec = userReq(t, mux, http.MethodGet, "/auth/sessions", other, nil)
	authDecode(t, rec, &body)
	if len(body.Sessions) != 1 || body.Sessions[0].ID == laptopID {
		t.Errorf("another account lists %+v", body.Sessions)
	}
}

func TestDescribeClient(t *testing.T) {
	cases := map[string]string{
		"": "Unknown browser",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36":               "Chrome on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.0.0": "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15":            "Safari on macOS",
		"Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0":                                                        "Firefox on Linux",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Mobile Safari/537.36":         "Chrome on Android",
		"curl/8.4.0": "Browser",
	}
	for ua, want := range cases {
		if got := describeClient(ua); got != want {
			t.Errorf("describeClient(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestAuthPasswordChangeEndsOtherSessions(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	mine := notifSignUp(t, mux, "doki", "swordfish-tacos")
	rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "doki", "password": "swordfish-tacos"})
	var other SessionResponse
	authDecode(t, rec, &other)

	change := func(current, next string) *httptest.ResponseRecorder {
		return userReq(t, mux, http.MethodPost, "/auth/password", mine, map[string]string{"currentPassword": current, "newPassword": next})
	}
	if rec := change("nope", "brand-new-passphrase"); rec.Code != http.StatusForbidden {
		t.Errorf("wrong current password: status=%d want 403", rec.Code)
	}
	if rec := change("swordfish-tacos", "short"); rec.Code != http.StatusBadRequest {
		t.Errorf("weak new password: status=%d want 400", rec.Code)
	}
	rec = change("swordfish-tacos", "brand-new-passphrase")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessionsEnded":1`) {
		t.Fatalf("change: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// This browser stays signed in; the other one does not.
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", mine, nil); rec.Code != http.StatusOK {
		t.Errorf("own session ended by the change: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", other.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("other session survived the change: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "doki", "password": "swordfish-tacos"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still signs in: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "doki", "password": "brand-new-passphrase"}); rec.Code != http.StatusOK {
		t.Errorf("new password refused: status=%d", rec.Code)
	}
}

// Deleting an account takes its rules, its delivery trail and its sessions
// with it: nothing that could name a webhook is left behind.
func TestAuthDeleteAccountRemovesEverythingItOwns(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	ctx := context.Background()
	tok := notifToken(t, mux)

	rule := notifCreate(t, mux, "doki", notifRule("Mine", []string{"live"}))
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "vid1",
		URL: livedetect.YouTubeWatchURL("vid1"), Title: "Live", StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	ws.next(t)
	if entries := notifWaitLog(t, app, 1); entries[0].UserID == 0 || entries[0].EventID != rule.ID {
		t.Fatalf("log entry = %+v, want it stamped with the owner", entries[0])
	}

	if rec := userReq(t, mux, http.MethodDelete, "/auth/account", tok, map[string]string{"password": "wrong"}); rec.Code != http.StatusForbidden {
		t.Errorf("wrong password: status=%d want 403", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodDelete, "/auth/account", tok, map[string]string{"password": notifPassword}); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	if rec := userReq(t, mux, http.MethodGet, "/auth/me", tok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("session survived account deletion: status=%d", rec.Code)
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": notifUsername, "password": notifPassword}); rec.Code != http.StatusUnauthorized {
		t.Errorf("deleted account signs in: status=%d", rec.Code)
	}
	if events, _ := app.Store.ListNotificationEvents(ctx, "doki"); len(events) != 0 {
		t.Errorf("rules survived: %+v", events)
	}
	if log, _ := app.Store.ListNotificationLog(ctx, "doki", 10); len(log) != 0 {
		t.Errorf("log survived: %+v", log)
	}
	if n, _ := app.Store.CountUsers(ctx); n != 0 {
		t.Errorf("accounts = %d, want 0", n)
	}
	// The username is free again.
	notifSignUp(t, mux, notifUsername, notifPassword)
}

// loginFrom signs in as if from a forwarded address ("" for the socket peer).
func loginFrom(t *testing.T, mux *http.ServeMux, username, password, addr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	req.Header.Set("Content-Type", "application/json")
	if addr != "" {
		req.Header.Set("X-Forwarded-For", addr)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Repeated wrong passwords lock further attempts from that address, and the
// right password does not open the lock early. The lock is per address and
// per username typed: it never blocks the account's other members, and an
// unknown username locks exactly the same way, so it says nothing about
// which usernames exist.
func TestAuthLockoutAfterRepeatedFailures(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	notifSignUp(t, mux, "doki", "swordfish-tacos")

	for i := 0; i < auth.LockoutFreeFailures; i++ {
		if rec := loginFrom(t, mux, "doki", "guess", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d want 401", i+1, rec.Code)
		}
	}
	rec := loginFrom(t, mux, "doki", "swordfish-tacos", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the right password got %d, want 429", auth.LockoutFreeFailures, rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("no Retry-After on the lockout")
	}
	if !strings.Contains(authError(t, rec), "Too many failed sign-in attempts") {
		t.Errorf("lockout body = %s", rec.Body.String())
	}

	// A teammate somewhere else is not locked out by a stranger's guessing.
	if rec := loginFrom(t, mux, "doki", "swordfish-tacos", "203.0.113.50"); rec.Code != http.StatusOK {
		t.Errorf("the right password from another address: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A username that does not exist locks in exactly the same way.
	for i := 0; i < auth.LockoutFreeFailures; i++ {
		if rec := loginFrom(t, mux, "nobody-here", "guess", "198.51.100.3"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown user attempt %d: status=%d want 401", i+1, rec.Code)
		}
	}
	unknown := loginFrom(t, mux, "nobody-here", "guess", "198.51.100.3")
	if unknown.Code != http.StatusTooManyRequests || unknown.Body.String() != rec.Body.String() {
		t.Errorf("unknown username lockout = %d %s, want the same 429 as a real one (%s)", unknown.Code, unknown.Body.String(), rec.Body.String())
	}
}

// Sign-ins and sign-ups are limited per address, and the address is what
// the proxy says it is.
func TestAuthLimitsPerAddress(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})

	for i := 0; i < registerPerHour; i++ {
		rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": fmt.Sprintf("user%d", i), "password": "swordfish-tacos"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": "one-too-many", "password": "swordfish-tacos"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("register past the limit: status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// A different forwarded address is a different bucket.
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"elsewhere","password":"swordfish-tacos"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	fwd := httptest.NewRecorder()
	mux.ServeHTTP(fwd, req)
	if fwd.Code != http.StatusCreated {
		t.Errorf("register from another address: status=%d body=%s", fwd.Code, fwd.Body.String())
	}

	for i := 0; i < loginPerMinute; i++ {
		userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "user0", "password": "nope"})
	}
	rec = userReq(t, mux, http.MethodPost, "/auth/login", "", map[string]string{"username": "user0", "password": "swordfish-tacos"})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("login past the per-address limit: status=%d want 429", rec.Code)
	}
}

// The operator's view names owners and masks every webhook; the moderation
// delete removes any account's rule.
func TestAdminNotificationsViewMasksWebhooksAndNamesOwners(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})

	ev := notifRule("Fan pings", []string{"live"})
	ev.Webhooks = []model.Webhook{{Name: "#announcements", URL: notifWebhookURL}}
	created := notifCreate(t, mux, "doki", ev)

	admin := notifAdminGet(t, mux, "doki")
	if len(admin.Events) != 1 || admin.Events[0].Owner != notifUsername || admin.Events[0].ID != created.ID {
		t.Fatalf("admin events = %+v", admin.Events)
	}
	hook := admin.Events[0].Webhooks[0]
	if hook.Name != "#announcements" || strings.Contains(hook.URL, notifWebhookToken) || !strings.Contains(hook.URL, "••••") {
		t.Errorf("admin sees webhook %+v, want the name and a masked url", hook)
	}
	if admin.Accounts != 1 {
		t.Errorf("accounts = %d", admin.Accounts)
	}

	rec := adminReq(t, mux, http.MethodDelete, fmt.Sprintf("%s/%d", notifAdminBase, created.ID), notifAdminKey, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp := notifGet(t, mux, "doki"); len(resp.Events) != 0 {
		t.Errorf("the account still sees %+v after the admin deleted it", resp.Events)
	}
}

func TestNotificationsEventLimitPerChannel(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})
	// Filling a channel takes more writes than the hourly bound allows.
	app.authLimits.writes = auth.NewLimiter(10*maxEventsPerUserPerChannel, time.Hour)
	for i := 0; i < maxEventsPerUserPerChannel; i++ {
		notifCreate(t, mux, "doki", notifRule(fmt.Sprintf("Rule %d", i), []string{"live"}))
	}
	rec := notifReq(t, mux, http.MethodPost, notifBase, notifRule("Too many", []string{"live"}))
	if rec.Code != http.StatusConflict {
		t.Errorf("past the limit: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	// The limit is per channel, and per account.
	notifCreate(t, mux, "mint", notifRule("Mint", []string{"live"}))
	other := notifSignUp(t, mux, "another", "swordfish-tacos")
	if rec := userReq(t, mux, http.MethodPost, notifBase, other, notifRule("Theirs", []string{"live"})); rec.Code != http.StatusCreated {
		t.Errorf("another account past someone else's limit: status=%d", rec.Code)
	}
}

// The public detection feed needs no account and carries no leg diagnostics.
func TestLiveDetectPublicEndpoint(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	ctx := context.Background()
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "vid1",
		URL: livedetect.YouTubeWatchURL("vid1"), Title: "Live now", StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	rec := userReq(t, mux, http.MethodGet, "/doki/livedetect", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q, want a short public cache", cc)
	}
	var resp LiveDetectResponse
	authDecode(t, rec, &resp)
	if resp.Enabled || resp.State != "disabled" || len(resp.Watching) != 0 || len(resp.Legs) != 0 {
		t.Errorf("status = %+v, want disabled with no legs", resp)
	}
	if len(resp.Detections) != 1 || resp.Detections[0].Title != "Live now" {
		t.Errorf("detections = %+v", resp.Detections)
	}
	if resp.Videos == nil {
		t.Error("videos must be an empty list, not null")
	}
	if rec := userReq(t, mux, http.MethodGet, "/nobody/livedetect", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status=%d", rec.Code)
	}
}

// A password check behind a session - change password, delete account - is
// throttled like sign-in: a stolen session is not a free password oracle.
func TestAuthPasswordChecksAreThrottled(t *testing.T) {
	_, mux := setupTestApp(t, []string{"doki"})
	tok := notifSignUp(t, mux, "doki", "swordfish-tacos")
	change := map[string]string{"currentPassword": "guess", "newPassword": "brand-new-passphrase"}
	for i := 0; i < auth.LockoutFreeFailures; i++ {
		if rec := userReq(t, mux, http.MethodPost, "/auth/password", tok, change); rec.Code != http.StatusForbidden {
			t.Fatalf("wrong current password %d: status=%d want 403", i+1, rec.Code)
		}
	}
	right := map[string]string{"currentPassword": "swordfish-tacos", "newPassword": "brand-new-passphrase"}
	if rec := userReq(t, mux, http.MethodPost, "/auth/password", tok, right); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after the free failures the right password got %d, want 429", rec.Code)
	}
	// The same lock covers the other password check, since it is the same password.
	if rec := userReq(t, mux, http.MethodDelete, "/auth/account", tok, map[string]string{"password": "swordfish-tacos"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("delete during the lock: status=%d want 429", rec.Code)
	}
	// It is per address: from elsewhere the change goes through.
	body, _ := json.Marshal(right)
	req := httptest.NewRequest(http.MethodPost, "/auth/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("password change from another address: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// A session past the hard ceiling is refused and removed, however recently
// it was used.
func TestAuthSessionHardCeiling(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	notifSignUp(t, mux, "doki", "swordfish-tacos")
	ctx := context.Background()
	u, _ := app.Store.GetUserByUsernameKey(ctx, "doki")
	tok, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-auth.SessionMax - time.Hour).Unix()
	if _, err := app.Store.CreateSession(ctx, u.ID, hash, created, time.Now().Add(time.Hour).Unix(), "", ""); err != nil {
		t.Fatal(err)
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", tok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session older than the ceiling was accepted: status=%d", rec.Code)
	}
	if s, _, _ := app.Store.GetSessionByTokenHash(ctx, hash, time.Now().Unix()); s != nil {
		t.Error("the aged-out session is still stored")
	}
}

// With trusted proxies configured, a forwarded address is believed only from
// them; from anyone else the socket peer is what counts. An IPv6 client is
// keyed by its /64.
func TestClientIPTrust(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.authLimits = newAuthLimits(config.AccountsConfig{TrustedProxies: []string{"10.0.0.0/8", "203.0.113.200"}})

	// httptest's peer is 192.0.2.1: not a trusted proxy, so sign-ups with a
	// rotating X-Forwarded-For all land in the peer's bucket.
	for i := 0; i < registerPerHour; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(fmt.Sprintf(`{"username":"user%d","password":"swordfish-tacos"}`, i)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"one-more","password":"swordfish-tacos"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("a forwarded address from an untrusted peer was believed: status=%d", rec.Code)
	}

	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	probe.RemoteAddr = "10.1.2.3:555"
	probe.Header.Set("CF-Connecting-IP", "2001:db8:1234:5678:aaaa:bbbb:cccc:dddd")
	if got := app.clientIP(probe); got != "2001:db8:1234:5678::/64" {
		t.Errorf("IPv6 client from a trusted proxy keyed as %q, want its /64", got)
	}
	probe.Header.Set("CF-Connecting-IP", "not an address")
	if got := app.clientIP(probe); got != "10.1.2.3" {
		t.Errorf("an unparseable forwarded address keyed as %q, want the peer", got)
	}
	probe.Header.Set("CF-Connecting-IP", "198.51.100.8")
	probe.RemoteAddr = "192.0.2.9:1"
	if got := app.clientIP(probe); got != "192.0.2.9" {
		t.Errorf("a forwarded address from an untrusted peer keyed as %q, want the peer", got)
	}
	probe.RemoteAddr = "203.0.113.200:1"
	if got := app.clientIP(probe); got != "198.51.100.8" {
		t.Errorf("a forwarded address from a trusted single-address proxy keyed as %q", got)
	}
}
