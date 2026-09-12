package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"live-transcript-server/internal/auth"
	"live-transcript-server/internal/model"
)

const siteAdminKey = "site-owner-key"

// siteApp is a test app with the site admin key set.
func siteApp(t *testing.T, channels ...string) (*App, *http.ServeMux) {
	t.Helper()
	app, mux := setupTestApp(t, channels)
	app.AdminKey = siteAdminKey
	return app, mux
}

// siteReq calls a site admin route with the site admin key.
func siteReq(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return adminReq(t, mux, method, path, siteAdminKey, body)
}

func siteAccounts(t *testing.T, mux *http.ServeMux) map[string]AccountSummary {
	t.Helper()
	rec := siteReq(t, mux, http.MethodGet, "/admin/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accounts []AccountSummary `json:"accounts"`
	}
	authDecode(t, rec, &body)
	out := map[string]AccountSummary{}
	for _, a := range body.Accounts {
		out[a.Username] = a
	}
	return out
}

func siteOverview(t *testing.T, mux *http.ServeMux) SiteOverview {
	t.Helper()
	rec := siteReq(t, mux, http.MethodGet, "/admin/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var o SiteOverview
	authDecode(t, rec, &o)
	return o
}

// dispatchable is the set of rule names live detection would fire on the
// channel right now.
func dispatchable(t *testing.T, app *App, channel string) map[string]bool {
	t.Helper()
	events, err := app.Store.ListNotificationEvents(context.Background(), channel)
	if err != nil {
		t.Fatalf("ListNotificationEvents: %v", err)
	}
	out := map[string]bool{}
	for _, ev := range events {
		out[ev.Name] = true
	}
	return out
}

func TestSiteAdminRequiresKey(t *testing.T) {
	app, mux := siteApp(t, "doki")

	// The channel admin key is not the site key.
	for _, key := range []string{"", "wrong", notifAdminKey} {
		rec := adminReq(t, mux, http.MethodGet, "/admin/overview", key, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("key %q: status=%d want 403", key, rec.Code)
		}
	}
	if rec := siteReq(t, mux, http.MethodGet, "/admin/overview", nil); rec.Code != http.StatusOK {
		t.Errorf("right key: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Guessing is throttled per address.
	var last int
	for i := 0; i < siteAdminFailuresPerMinute+1; i++ {
		last = adminReq(t, mux, http.MethodGet, "/admin/overview", "wrong", nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d wrong keys: status=%d want 429", siteAdminFailuresPerMinute+1, last)
	}
	// The throttle is on wrong keys only: the right one still works.
	if rec := siteReq(t, mux, http.MethodGet, "/admin/overview", nil); rec.Code != http.StatusOK {
		t.Errorf("right key after throttling: status=%d", rec.Code)
	}

	// With no key configured the page is off, whatever is sent.
	app.AdminKey = ""
	rec := adminReq(t, mux, http.MethodGet, "/admin/overview", "", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(authError(t, rec), "credentials.adminKey") {
		t.Errorf("unconfigured: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The page itself is served without the key; it asks for one.
	rec = adminReq(t, mux, http.MethodGet, "/admin/ui", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Site admin") {
		t.Errorf("ui: status=%d type=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestSiteAdminOverviewCountsEveryAccount(t *testing.T) {
	_, mux := siteApp(t, "doki", "mint")

	// doki-fan: three rules over two channels, one paused, five webhooks.
	notifCreate(t, mux, "doki", notifRule("Live", []string{"live"}))
	notifCreate(t, mux, "doki", notifRule("Both", []string{"live", "upload"}, notifWebhookURL, "https://discord.com/api/webhooks/222222222222222222/SecondTokenAbcdefghijklmnop"))
	paused := notifRule("Paused", []string{"live"}, notifWebhookURL, "https://discord.com/api/webhooks/333333333333333333/ThirdTokenAbcdefghijklmnopq")
	paused.Enabled = false
	notifCreate(t, mux, "mint", paused)
	// another: one rule on doki.
	other := notifSignUp(t, mux, "another", "swordfish-tacos")
	if rec := userReq(t, mux, http.MethodPost, notifBase, other, notifRule("Theirs", []string{"live"})); rec.Code != http.StatusCreated {
		t.Fatalf("another's rule: status=%d body=%s", rec.Code, rec.Body.String())
	}

	o := siteOverview(t, mux)
	if o.Totals.Accounts != 2 || o.Totals.DisabledAccounts != 0 || o.Totals.Sessions != 2 ||
		o.Totals.Events != 4 || o.Totals.EnabledEvents != 3 || o.Totals.Webhooks != 6 || o.Totals.LegacyEvents != 0 {
		t.Errorf("totals = %+v", o.Totals)
	}
	if !o.Registration.Open || o.Registration.ClosedByAdmin || o.Registration.ClosedByConfig {
		t.Errorf("registration = %+v, want open", o.Registration)
	}
	if o.Server.Version != "test-version" {
		t.Errorf("server = %+v", o.Server)
	}
	byKey := map[string]ChannelSummary{}
	for _, c := range o.Channels {
		byKey[c.Key] = c
	}
	if c := byKey["doki"]; c.Events != 3 || c.Accounts != 2 {
		t.Errorf("doki channel = %+v", c)
	}
	if c := byKey["mint"]; c.Events != 1 || c.Accounts != 1 {
		t.Errorf("mint channel = %+v", c)
	}
	if len(o.RecentSignups) != 2 || o.RecentSignups[0].Username != "another" {
		t.Errorf("recent signups = %+v, want newest first", o.RecentSignups)
	}

	accounts := siteAccounts(t, mux)
	fan := accounts[notifUsername]
	if fan.Events != 3 || fan.EnabledEvents != 2 || fan.Webhooks != 5 || fan.Sessions != 1 ||
		strings.Join(fan.Channels, ",") != "doki,mint" || fan.DisabledAt != 0 {
		t.Errorf("doki-fan = %+v", fan)
	}
	if a := accounts["another"]; a.Events != 1 || a.Webhooks != 1 || strings.Join(a.Channels, ",") != "doki" {
		t.Errorf("another = %+v", a)
	}

	// The operator sees each account's rules with every webhook masked.
	rec := siteReq(t, mux, http.MethodGet, fmt.Sprintf("/admin/accounts/%d/events", fan.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account events: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Events []model.NotificationEvent `json:"events"`
	}
	authDecode(t, rec, &body)
	if len(body.Events) != 3 {
		t.Errorf("account events = %d, want 3", len(body.Events))
	}
	for _, secret := range []string{notifWebhookToken, "SecondTokenAbcdefghijklmnop", "ThirdTokenAbcdefghijklmnopq"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("account events expose a webhook token %q", secret)
		}
	}
	for _, ev := range body.Events {
		if ev.Owner != notifUsername {
			t.Errorf("event %q owner = %q", ev.Name, ev.Owner)
		}
	}
	for _, path := range []string{"/admin/overview", "/admin/accounts"} {
		if b := siteReq(t, mux, http.MethodGet, path, nil).Body.String(); strings.Contains(b, notifWebhookToken) {
			t.Errorf("%s exposes a webhook token", path)
		}
	}
}

func TestSiteAdminDisableAndEnable(t *testing.T) {
	app, mux := siteApp(t, "doki")
	notifCreate(t, mux, "doki", notifRule("Live", []string{"live"}))
	token := notifToken(t, mux)
	id := siteAccounts(t, mux)[notifUsername].ID

	rec := siteReq(t, mux, http.MethodPost, fmt.Sprintf("/admin/accounts/%d/disable", id), map[string]string{"reason": "spamming test sends"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Disabling ends every session, and a fresh sign-in with the right
	// password is refused with the reason.
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", token, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("disabled session: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := loginFrom(t, mux, notifUsername, notifPassword, ""); rec.Code != http.StatusForbidden || !strings.Contains(authError(t, rec), "spamming test sends") {
		t.Errorf("disabled login: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// A wrong password does not reveal the account is disabled.
	if rec := loginFrom(t, mux, notifUsername, "guess", ""); rec.Code != http.StatusUnauthorized || strings.Contains(authError(t, rec), "disabled") {
		t.Errorf("disabled login, wrong password: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Its rules stop firing but stay visible to the operator, marked.
	if d := dispatchable(t, app, "doki"); d["Live"] {
		t.Errorf("a disabled account's rule is still dispatchable: %v", d)
	}
	view := notifAdminGet(t, mux, "doki")
	if len(view.Events) != 1 || view.Events[0].Owner != notifUsername+" (disabled)" {
		t.Errorf("channel admin view = %+v, want the parked rule labelled", view.Events)
	}
	o := siteOverview(t, mux)
	if o.Totals.DisabledAccounts != 1 || o.Totals.Sessions != 0 {
		t.Errorf("totals after disable = %+v", o.Totals)
	}
	if a := siteAccounts(t, mux)[notifUsername]; a.DisabledAt == 0 || a.DisabledReason != "spamming test sends" {
		t.Errorf("account after disable = %+v", a)
	}

	// Enable puts everything back.
	if rec := siteReq(t, mux, http.MethodPost, fmt.Sprintf("/admin/accounts/%d/enable", id), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("enable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := loginFrom(t, mux, notifUsername, notifPassword, ""); rec.Code != http.StatusOK {
		t.Errorf("login after enable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := dispatchable(t, app, "doki"); !d["Live"] {
		t.Errorf("rule not dispatchable after enable: %v", d)
	}
	if view := notifAdminGet(t, mux, "doki"); len(view.Events) != 1 || view.Events[0].Owner != notifUsername {
		t.Errorf("channel admin view after enable = %+v", view.Events)
	}

	// Unknown accounts and bad ids are refused cleanly.
	if rec := siteReq(t, mux, http.MethodPost, "/admin/accounts/999/disable", nil); rec.Code != http.StatusNotFound {
		t.Errorf("disable unknown: status=%d", rec.Code)
	}
	if rec := siteReq(t, mux, http.MethodPost, "/admin/accounts/x/disable", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("disable bad id: status=%d", rec.Code)
	}
}

func TestSiteAdminSessionsEventsAndDelete(t *testing.T) {
	app, mux := siteApp(t, "doki", "mint")
	first := notifToken(t, mux)
	notifCreate(t, mux, "doki", notifRule("Live", []string{"live"}))
	notifCreate(t, mux, "mint", notifRule("Mint", []string{"live"}))
	var tokens []string
	for _, addr := range []string{"203.0.113.9", "198.51.100.7"} {
		rec := loginFrom(t, mux, notifUsername, notifPassword, addr)
		var sess SessionResponse
		authDecode(t, rec, &sess)
		tokens = append(tokens, sess.Token)
	}
	id := siteAccounts(t, mux)[notifUsername].ID

	// Sign out everywhere: every token dies, the password still works.
	rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d/sessions", id), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessionsEnded":3`) {
		t.Fatalf("sign out all: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, tok := range append(tokens, first) {
		if rec := userReq(t, mux, http.MethodGet, "/auth/me", tok, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("session survived sign-out-all: status=%d", rec.Code)
		}
	}
	rec = loginFrom(t, mux, notifUsername, notifPassword, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login after sign-out-all: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sess SessionResponse
	authDecode(t, rec, &sess)

	// Delete the account's rules on every channel; the account stays.
	rec = siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d/events", id), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"eventsDeleted":2`) {
		t.Fatalf("delete events: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, ch := range []string{"doki", "mint"} {
		rec := userReq(t, mux, http.MethodGet, "/"+ch+"/notifications", sess.Token, nil)
		var resp NotificationsResponse
		authDecode(t, rec, &resp)
		if rec.Code != http.StatusOK || len(resp.Events) != 0 {
			t.Errorf("%s rules after delete = %d (status %d)", ch, len(resp.Events), rec.Code)
		}
	}
	if a := siteAccounts(t, mux)[notifUsername]; a.ID != id || a.Events != 0 {
		t.Errorf("account after deleting events = %+v", a)
	}

	// Delete the account: it is gone, its session with it, the name is free.
	if rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d", id), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete account: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := userReq(t, mux, http.MethodGet, "/auth/me", sess.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("session survived account deletion: status=%d", rec.Code)
	}
	if n, _ := app.Store.CountUsers(context.Background()); n != 0 {
		t.Errorf("accounts = %d, want 0", n)
	}
	if rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d", id), nil); rec.Code != http.StatusNotFound {
		t.Errorf("delete again: status=%d want 404", rec.Code)
	}
	notifSignUp(t, mux, notifUsername, notifPassword)
}

func TestSiteAdminUnlock(t *testing.T) {
	_, mux := siteApp(t, "doki")
	notifSignUp(t, mux, "Doki", "swordfish-tacos")
	for i := 0; i < auth.LockoutFreeFailures; i++ {
		loginFrom(t, mux, "doki", "guess", "203.0.113.9")
	}
	if rec := loginFrom(t, mux, "doki", "swordfish-tacos", "203.0.113.9"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("not locked: status=%d", rec.Code)
	}
	id := siteAccounts(t, mux)["Doki"].ID
	rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d/lockouts", id), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cleared":1`) {
		t.Fatalf("unlock: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := loginFrom(t, mux, "doki", "swordfish-tacos", "203.0.113.9"); rec.Code != http.StatusOK {
		t.Errorf("login after unlock: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Nothing left to clear.
	if rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/accounts/%d/lockouts", id), nil); !strings.Contains(rec.Body.String(), `"cleared":0`) {
		t.Errorf("second unlock: body=%s", rec.Body.String())
	}
}

func TestSiteAdminRegistrationOverride(t *testing.T) {
	app, mux := siteApp(t, "doki")
	rec := siteReq(t, mux, http.MethodPost, "/admin/registration", map[string]bool{"closed": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"open":false`) || !strings.Contains(rec.Body.String(), `"closedByAdmin":true`) {
		t.Fatalf("close: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := userReq(t, mux, http.MethodPost, "/auth/register", "", map[string]string{"username": "late", "password": "swordfish-tacos"}); rec.Code != http.StatusForbidden {
		t.Errorf("register while closed: status=%d", rec.Code)
	}
	rec = userReq(t, mux, http.MethodGet, "/auth/info", "", nil)
	var info AuthInfoResponse
	authDecode(t, rec, &info)
	if info.RegistrationOpen {
		t.Errorf("auth info still says open: %s", rec.Body.String())
	}
	// The override lives in the database, so it survives a restart.
	if closed, byConfig, byAdmin := app.registrationClosed(context.Background()); !closed || byConfig || !byAdmin {
		t.Errorf("registrationClosed = %v %v %v", closed, byConfig, byAdmin)
	}

	rec = siteReq(t, mux, http.MethodPost, "/admin/registration", map[string]bool{"closed": false})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"open":true`) {
		t.Fatalf("reopen: status=%d body=%s", rec.Code, rec.Body.String())
	}
	notifSignUp(t, mux, "late", "swordfish-tacos")

	// The config's switch cannot be overridden from the page.
	app.Accounts.DisableRegistration = true
	rec = siteReq(t, mux, http.MethodPost, "/admin/registration", map[string]bool{"closed": false})
	if !strings.Contains(rec.Body.String(), `"open":false`) || !strings.Contains(rec.Body.String(), `"closedByConfig":true`) {
		t.Errorf("reopen against the config: body=%s", rec.Body.String())
	}
}

func TestSiteAdminDeletesOneEvent(t *testing.T) {
	_, mux := siteApp(t, "doki")
	keep := notifCreate(t, mux, "doki", notifRule("Keep", []string{"live"}))
	drop := notifCreate(t, mux, "doki", notifRule("Drop", []string{"live"}))
	if rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/events/%d", drop.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete event: status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := notifGet(t, mux, "doki")
	if len(resp.Events) != 1 || resp.Events[0].ID != keep.ID {
		t.Errorf("rules after delete = %+v", resp.Events)
	}
	if rec := siteReq(t, mux, http.MethodDelete, fmt.Sprintf("/admin/events/%d", drop.ID), nil); rec.Code != http.StatusNotFound {
		t.Errorf("delete again: status=%d want 404", rec.Code)
	}
}
