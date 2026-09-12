package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

// The site admin page is the operator's view of the whole site: every
// account, how many rules and webhooks each has, who is signed in, what is
// failing, and the levers to stop abuse - disable an account, drop its rules,
// end its sessions, delete it, unlock it, close sign-ups. It is gated by
// credentials.adminKey, a key that belongs to the operator alone and is
// separate from the per-channel admin keys. Every action is written to the
// admin audit webhook and the log. Webhook URLs never appear here either:
// the operator sees each webhook's name and masked form, like the channel
// admin page does.

// siteAdminFailuresPerMinute bounds wrong keys per address.
const siteAdminFailuresPerMinute = 10

// withSiteAdmin requires the site admin key in X-Admin-Key. With no key
// configured the page is simply off.
func (app *App) withSiteAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if app.AdminKey == "" {
			writeJSONError(w, http.StatusForbidden, "The site admin page is disabled: set credentials.adminKey in the server config.")
			return
		}
		provided := r.Header.Get("X-Admin-Key")
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(app.AdminKey)) != 1 {
			ip := app.clientIP(r)
			if ok, wait := app.authLimits.siteAdmin.Allow(ip); !ok {
				tooMany(w, wait, "Too many attempts. Try again in "+humanDuration(wait)+".")
				return
			}
			slog.Warn("wrong site admin key", "func", "withSiteAdmin", "ip", ip, "path", r.URL.Path)
			writeJSONError(w, http.StatusForbidden, "Wrong admin key.")
			return
		}
		h(w, r)
	}
}

// notifySiteAdminAction records an operator action on the audit webhook.
func (app *App) notifySiteAdminAction(r *http.Request, action string, fields ...discord.AdminField) {
	fields = append(fields, discord.AdminField{Name: "Endpoint", Value: r.Method + " " + r.URL.Path})
	app.Discord.NotifyAdminAction("site admin", action, fields...)
}

// AccountSummary is one account as the operator sees it.
type AccountSummary struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	CreatedAt   int64  `json:"createdAt"`
	LastLoginAt int64  `json:"lastLoginAt"`
	// Sessions is how many browsers are signed in; LastSeenAt when any of
	// them was last active; LastAddress where the newest one signed in from.
	// The address is shown to the operator only, never to the account's
	// other members.
	Sessions      int      `json:"sessions"`
	LastSeenAt    int64    `json:"lastSeenAt"`
	LastAddress   string   `json:"lastAddress"`
	Events        int      `json:"events"`
	EnabledEvents int      `json:"enabledEvents"`
	Webhooks      int      `json:"webhooks"`
	Channels      []string `json:"channels"`
	SentCount     int64    `json:"sentCount"`
	LastError     string   `json:"lastError"`
	LastErrorAt   int64    `json:"lastErrorAt"`
	// DisabledAt is when the operator disabled the account, 0 if it is
	// active; DisabledReason is what whoever tries to sign in is told.
	DisabledAt     int64  `json:"disabledAt"`
	DisabledReason string `json:"disabledReason"`
}

// SiteOverview is the top of the page.
type SiteOverview struct {
	Server       model.ServerInfo `json:"server"`
	Registration struct {
		Open           bool `json:"open"`
		ClosedByConfig bool `json:"closedByConfig"`
		ClosedByAdmin  bool `json:"closedByAdmin"`
	} `json:"registration"`
	Totals struct {
		Accounts         int `json:"accounts"`
		DisabledAccounts int `json:"disabledAccounts"`
		Sessions         int `json:"sessions"`
		Events           int `json:"events"`
		EnabledEvents    int `json:"enabledEvents"`
		Webhooks         int `json:"webhooks"`
		LegacyEvents     int `json:"legacyEvents"`
	} `json:"totals"`
	Channels      []ChannelSummary             `json:"channels"`
	LiveDetect    livedetect.Status            `json:"liveDetect"`
	RecentSignups []AccountSummary             `json:"recentSignups"`
	Problems      []model.NotificationLogEntry `json:"problems"`
	// Hashing is how many password hashes are running right now.
	Hashing int `json:"hashing"`
}

// ChannelSummary is one channel's share of the rules.
type ChannelSummary struct {
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
	Events      int    `json:"events"`
	Accounts    int    `json:"accounts"`
}

// accountSummaries builds the operator's view of every account, joining
// users, their sessions and their rules in memory: the site has a handful of
// accounts, and one page load reading three tables is simpler than a query
// that decodes webhook JSON.
func (app *App) accountSummaries(r *http.Request) ([]AccountSummary, []model.NotificationEvent, error) {
	ctx := r.Context()
	users, err := app.Store.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	events, err := app.Store.ListAllNotificationEvents(ctx)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := app.Store.SessionSummaries(ctx, time.Now().Unix())
	if err != nil {
		return nil, nil, err
	}

	byUser := map[int64]*AccountSummary{}
	out := make([]AccountSummary, 0, len(users))
	for _, u := range users {
		s := sessions[u.ID]
		out = append(out, AccountSummary{
			ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
			Sessions: s.Count, LastSeenAt: s.LastSeenAt, LastAddress: s.LastAddress,
			Channels: []string{}, DisabledAt: u.DisabledAt, DisabledReason: u.DisabledReason,
		})
	}
	for i := range out {
		byUser[out[i].ID] = &out[i]
	}
	for _, ev := range events {
		a, ok := byUser[ev.UserID]
		if !ok {
			continue
		}
		a.Events++
		if ev.Enabled {
			a.EnabledEvents++
		}
		a.Webhooks += len(ev.Webhooks)
		if !containsString(a.Channels, ev.ChannelKey) {
			a.Channels = append(a.Channels, ev.ChannelKey)
		}
		a.SentCount += ev.SentCount
		if ev.LastErrorAt > a.LastErrorAt {
			a.LastErrorAt = ev.LastErrorAt
			a.LastError = ev.LastError
		}
	}
	for i := range out {
		sort.Strings(out[i].Channels)
	}
	return out, events, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// siteAdminUIHandler serves the page; the key is asked for by the page.
func (app *App) siteAdminUIHandler(w http.ResponseWriter, r *http.Request) {
	data, err := adminUIFS.ReadFile("admin_site_ui.html")
	if err != nil {
		http.Error(w, "Failed to load the site admin page", http.StatusInternalServerError)
		slog.Error("failed to read site admin UI", "func", "siteAdminUIHandler", "err", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(data); err != nil {
		slog.Error("failed to write site admin UI", "func", "siteAdminUIHandler", "err", err)
	}
}

func (app *App) getSiteOverviewHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, events, err := app.accountSummaries(r)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to build the site overview", "func", "getSiteOverviewHandler", "err", err)
		return
	}
	var o SiteOverview
	o.Server = model.ServerInfo{Version: app.Version, BuildTime: app.BuildTime}
	closed, byConfig, byAdmin := app.registrationClosed(ctx)
	o.Registration.Open = !closed
	o.Registration.ClosedByConfig = byConfig
	o.Registration.ClosedByAdmin = byAdmin
	o.Totals.Accounts = len(accounts)
	for _, a := range accounts {
		if a.DisabledAt > 0 {
			o.Totals.DisabledAccounts++
		}
		o.Totals.Sessions += a.Sessions
		o.Totals.Events += a.Events
		o.Totals.EnabledEvents += a.EnabledEvents
		o.Totals.Webhooks += a.Webhooks
	}
	perChannel := map[string]*ChannelSummary{}
	channelAccounts := map[string]map[int64]bool{}
	for _, ev := range events {
		if ev.UserID == 0 {
			o.Totals.LegacyEvents++
		}
		c, ok := perChannel[ev.ChannelKey]
		if !ok {
			c = &ChannelSummary{Key: ev.ChannelKey, DisplayName: app.Announcer.ChannelInfo(ev.ChannelKey).DisplayName}
			perChannel[ev.ChannelKey] = c
			channelAccounts[ev.ChannelKey] = map[int64]bool{}
		}
		c.Events++
		if ev.UserID != 0 {
			channelAccounts[ev.ChannelKey][ev.UserID] = true
		}
	}
	o.Channels = []ChannelSummary{}
	for key := range app.Channels {
		c, ok := perChannel[key]
		if !ok {
			c = &ChannelSummary{Key: key, DisplayName: app.Announcer.ChannelInfo(key).DisplayName}
		}
		c.Accounts = len(channelAccounts[key])
		o.Channels = append(o.Channels, *c)
	}
	sort.Slice(o.Channels, func(i, j int) bool { return o.Channels[i].Key < o.Channels[j].Key })
	o.LiveDetect = app.LiveDetect.Status()
	if o.LiveDetect.Legs == nil {
		o.LiveDetect.Legs = []livedetect.LegStatus{}
	}
	o.RecentSignups = accounts
	if len(o.RecentSignups) > 10 {
		o.RecentSignups = o.RecentSignups[:10]
	}
	problems, err := app.Store.ListNotificationProblems(ctx, 50)
	if err != nil {
		slog.Error("failed to list delivery problems", "func", "getSiteOverviewHandler", "err", err)
	}
	if problems == nil {
		problems = []model.NotificationLogEntry{}
	}
	names := map[int64]store.UserLabel{}
	for _, a := range accounts {
		names[a.ID] = store.UserLabel{Username: a.Username, Disabled: a.DisabledAt != 0}
	}
	for i := range problems {
		problems[i].Owner = ownerLabel(names, problems[i].UserID)
	}
	o.Problems = problems
	o.Hashing = app.authLimits.hashing.InFlight()
	writeJSON(w, o)
}

func (app *App) getSiteAccountsHandler(w http.ResponseWriter, r *http.Request) {
	accounts, _, err := app.accountSummaries(r)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to list accounts", "func", "getSiteAccountsHandler", "err", err)
		return
	}
	writeJSON(w, map[string]any{"accounts": accounts})
}

// siteAccount resolves the {id} path value to an account, answering the
// error itself.
func (app *App) siteAccount(w http.ResponseWriter, r *http.Request) (*model.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid account id")
		return nil, false
	}
	u, err := app.Store.GetUserByID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return nil, false
	}
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "No such account")
		return nil, false
	}
	return u, true
}

// maskEventWebhooks replaces every webhook URL with its masked form.
func maskEventWebhooks(events []model.NotificationEvent) {
	for i := range events {
		for j := range events[i].Webhooks {
			events[i].Webhooks[j].URL = announce.MaskWebhookURL(events[i].Webhooks[j].URL)
		}
	}
}

func (app *App) getSiteAccountEventsHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	all, err := app.Store.ListAllNotificationEvents(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	events := []model.NotificationEvent{}
	for _, ev := range all {
		if ev.UserID == u.ID {
			ev.Owner = u.Username
			events = append(events, ev)
		}
	}
	maskEventWebhooks(events)
	writeJSON(w, map[string]any{"events": events})
}

type disableRequest struct {
	Reason string `json:"reason"`
}

func (app *App) postSiteAccountDisableHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	var req disableRequest
	if r.ContentLength != 0 && !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if len([]rune(reason)) > 200 {
		reason = string([]rune(reason)[:200])
	}
	if _, err := app.Store.SetUserDisabled(r.Context(), u.ID, time.Now().Unix(), reason); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to disable account", "func", "postSiteAccountDisableHandler", "err", err)
		return
	}
	app.notifySiteAdminAction(r, "Account disabled",
		discord.AdminField{Name: "Account", Value: u.Username, Inline: true},
		discord.AdminField{Name: "Reason", Value: firstNonEmpty(reason, "(none given)")})
	slog.Warn("site admin disabled an account", "func", "postSiteAccountDisableHandler", "user", u.Username, "reason", reason)
	for _, cs := range app.Channels {
		app.bumpAdminChange(cs.Key)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (app *App) postSiteAccountEnableHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	if _, err := app.Store.SetUserDisabled(r.Context(), u.ID, 0, ""); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	app.notifySiteAdminAction(r, "Account enabled", discord.AdminField{Name: "Account", Value: u.Username, Inline: true})
	slog.Info("site admin enabled an account", "func", "postSiteAccountEnableHandler", "user", u.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (app *App) deleteSiteAccountSessionsHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	n, err := app.Store.DeleteUserSessions(r.Context(), u.ID, 0)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	app.notifySiteAdminAction(r, "Account signed out everywhere",
		discord.AdminField{Name: "Account", Value: u.Username, Inline: true},
		discord.AdminField{Name: "Sessions", Value: strconv.FormatInt(n, 10), Inline: true})
	slog.Info("site admin signed an account out everywhere", "func", "deleteSiteAccountSessionsHandler", "user", u.Username, "sessions", n)
	writeJSON(w, map[string]int64{"sessionsEnded": n})
}

func (app *App) deleteSiteAccountEventsHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	n, err := app.Store.DeleteNotificationEventsForUser(r.Context(), u.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	app.notifySiteAdminAction(r, "Account's notification events deleted",
		discord.AdminField{Name: "Account", Value: u.Username, Inline: true},
		discord.AdminField{Name: "Events", Value: strconv.FormatInt(n, 10), Inline: true})
	slog.Warn("site admin deleted an account's events", "func", "deleteSiteAccountEventsHandler", "user", u.Username, "events", n)
	for _, cs := range app.Channels {
		app.bumpAdminChange(cs.Key)
	}
	writeJSON(w, map[string]int64{"eventsDeleted": n})
}

func (app *App) deleteSiteAccountHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	if err := app.Store.DeleteUser(r.Context(), u.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to delete account", "func", "deleteSiteAccountHandler", "err", err)
		return
	}
	app.authLimits.lockout.ResetPrefix(strings.ToLower(u.Username) + "|")
	app.notifySiteAdminAction(r, "Account deleted", discord.AdminField{Name: "Account", Value: u.Username, Inline: true})
	slog.Warn("site admin deleted an account", "func", "deleteSiteAccountHandler", "user", u.Username)
	for _, cs := range app.Channels {
		app.bumpAdminChange(cs.Key)
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteSiteAccountLockoutsHandler forgets every address's failed attempts
// against the account's username: the operator's unlock.
func (app *App) deleteSiteAccountLockoutsHandler(w http.ResponseWriter, r *http.Request) {
	u, ok := app.siteAccount(w, r)
	if !ok {
		return
	}
	n := app.authLimits.lockout.ResetPrefix(strings.ToLower(u.Username) + "|")
	app.notifySiteAdminAction(r, "Account unlocked",
		discord.AdminField{Name: "Account", Value: u.Username, Inline: true},
		discord.AdminField{Name: "Locks cleared", Value: strconv.Itoa(n), Inline: true})
	slog.Info("site admin cleared lockouts", "func", "deleteSiteAccountLockoutsHandler", "user", u.Username, "entries", n)
	writeJSON(w, map[string]int{"cleared": n})
}

func (app *App) deleteSiteEventHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid event id")
		return
	}
	ev, err := app.Store.DeleteNotificationEventByID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if ev == nil {
		writeJSONError(w, http.StatusNotFound, "No such event")
		return
	}
	names, _ := app.Store.Usernames(r.Context(), []int64{ev.UserID})
	app.notifySiteAdminAction(r, "Notification event deleted (site admin)", notificationAuditFields(ev, ownerLabel(names, ev.UserID))...)
	slog.Warn("site admin deleted an event", "func", "deleteSiteEventHandler", "id", id, "name", ev.Name, "owner", ownerLabel(names, ev.UserID))
	app.bumpAdminChange(ev.ChannelKey)
	w.WriteHeader(http.StatusNoContent)
}

type registrationRequest struct {
	Closed bool `json:"closed"`
}

// postSiteRegistrationHandler closes or reopens sign-ups at runtime. Closing
// sets the override; reopening clears it, so the config's own switch, if
// set, still applies.
func (app *App) postSiteRegistrationHandler(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	var err error
	if req.Closed {
		err = app.Store.SetSetting(r.Context(), store.SettingRegistrationClosed, "1", time.Now().Unix())
	} else {
		err = app.Store.DeleteSetting(r.Context(), store.SettingRegistrationClosed)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	closed, byConfig, byAdmin := app.registrationClosed(r.Context())
	app.notifySiteAdminAction(r, "Registration "+map[bool]string{true: "closed", false: "reopened"}[req.Closed])
	slog.Warn("site admin changed registration", "func", "postSiteRegistrationHandler", "closed", closed, "byConfig", byConfig, "byAdmin", byAdmin)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]bool{"open": !closed, "closedByConfig": byConfig, "closedByAdmin": byAdmin}); err != nil {
		slog.Error("failed to write JSON response", "err", err)
	}
}
