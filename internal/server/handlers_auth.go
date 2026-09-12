package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"live-transcript-server/internal/auth"
	"live-transcript-server/internal/config"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

// Accounts on the live-transcript site exist so that anyone can run their
// own notification events, and so that nobody can see anyone else's: a
// Discord webhook URL is a credential, and a rule's webhooks belong to the
// account that created it alone.
//
// Sessions are bearer tokens. The client sends "Authorization: Bearer
// <token>" on every account request. A session cookie was the alternative
// and was rejected deliberately: the API host serves the production and dev
// servers under one origin, so a cookie set by one would be sent to the
// other; the site is developed against the production API from localhost,
// where a cross-site cookie is blocked outright; and a bearer token is never
// attached by the browser on its own, so a forged cross-site request has no
// way to carry it and there is no CSRF surface to defend. The trade is that
// the token lives in the browser's storage, where a script injected into the
// page could read it. The site loads no third-party scripts, tokens expire
// and are revocable, and the worst an account can do is edit its own rules.
//
// What the server does with credentials:
//   - passwords are argon2id-hashed (internal/auth) and never logged;
//   - only the SHA-256 of a session token is stored, so a copy of the
//     database contains no usable session;
//   - sign-in attempts are limited per address, and after a few wrong
//     passwords the (username, address) pair waits a growing interval - per
//     address so that a stranger cannot lock a shared account's owners out,
//     and for unknown usernames too so the wait reveals nothing;
//   - a wrong username and a wrong password get the same answer after the
//     same work, so usernames cannot be enumerated through the sign-in form
//     (registration necessarily says when a name is taken; it is limited per
//     address for that reason among others);
//   - every password check - sign-in, change password, delete account - goes
//     through the same limits, and the number of password hashes in flight
//     is capped process-wide;
//   - a password change ends every other session, atomically.
const (
	maxAuthBody = 16 << 10
	// registerPerHour is per address: enough for a household, not for a
	// script creating accounts by the thousand.
	registerPerHour = 5
	// loginPerMinute is per address, on top of the per-account lockout.
	loginPerMinute = 10
	// maxClientLength bounds the user agent kept with a session. Real user
	// agents run to about 150 characters; the browser and system names the
	// sessions list is derived from sit near the end.
	maxClientLength = 256
)

// authLimits holds the request limiters behind the account endpoints.
type authLimits struct {
	register *auth.Limiter
	login    *auth.Limiter
	// test bounds notification test sends per account: each one posts to a
	// webhook of the account's choosing. writes bounds rule creates, edits
	// and deletes per account: each one posts an audit record.
	test   *auth.Limiter
	writes *auth.Limiter
	// lockout is the per-(username, address) wait after wrong passwords;
	// it applies to sign-in and to every other place a password is checked.
	lockout *auth.Lockout
	// hashing bounds concurrent argon2 work across the whole process.
	hashing *auth.Guard
	// siteAdmin bounds wrong site-admin keys per address.
	siteAdmin *auth.Limiter
	// trustedProxies, when set, are the only peers whose forwarded client
	// address is believed.
	trustedProxies []*net.IPNet
}

// hashSlots is how many password hashes may run at once. Each pins 19 MiB
// and a core for tens of milliseconds; one per CPU keeps a flood from
// taking memory the transcript pipeline on the same box needs.
func hashSlots() int {
	n := runtime.NumCPU()
	if n < 2 {
		n = 2
	}
	if n > 16 {
		n = 16
	}
	return n
}

// hashWait is how long a request waits for a hashing slot before it is
// turned away with a 503; the caller can simply try again.
const hashWait = 3 * time.Second

func newAuthLimits(cfg config.AccountsConfig) authLimits {
	l := authLimits{
		register:  auth.NewLimiter(registerPerHour, time.Hour),
		login:     auth.NewLimiter(loginPerMinute, time.Minute),
		test:      auth.NewLimiter(testSendsPerHour, time.Hour),
		writes:    auth.NewLimiter(ruleWritesPerHour, time.Hour),
		lockout:   auth.NewLockout(),
		hashing:   auth.NewGuard(hashSlots(), hashWait),
		siteAdmin: auth.NewLimiter(siteAdminFailuresPerMinute, time.Minute),
	}
	for _, raw := range cfg.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "/") {
			if strings.Contains(raw, ":") {
				raw += "/128"
			} else {
				raw += "/32"
			}
		}
		_, cidr, err := net.ParseCIDR(raw)
		if err != nil {
			slog.Error("ignoring an unparseable accounts.trustedProxies entry", "func", "newAuthLimits", "entry", raw, "err", err)
			continue
		}
		l.trustedProxies = append(l.trustedProxies, cidr)
	}
	return l
}

// hashed runs fn, which does one password hash or verification, under the
// process-wide bound. It answers 503 itself when no slot came free.
func (app *App) hashed(w http.ResponseWriter, r *http.Request, fn func()) bool {
	if app.authLimits.hashing.Do(r.Context(), fn) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	writeJSONError(w, http.StatusServiceUnavailable, "The server is busy checking passwords. Try again in a moment.")
	return false
}

// authedUser is the account behind an authenticated request.
type authedUser struct {
	User    *model.User
	Session *model.Session
}

type userHandler func(w http.ResponseWriter, r *http.Request, u *authedUser)
type userChannelHandler func(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser)

// withUser requires a valid session and passes the account to the handler.
// Anything less than a live session is a 401 with no detail about why: the
// client's only move is to sign in again.
func (app *App) withUser(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		u, ok := app.authenticate(w, r)
		if !ok {
			return
		}
		h(w, r, u)
	}
}

// withUserChannel resolves the channel and requires a session.
func (app *App) withUserChannel(h userChannelHandler) http.HandlerFunc {
	return app.withChannel(func(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
		w.Header().Set("Cache-Control", "no-store")
		u, ok := app.authenticate(w, r)
		if !ok {
			return
		}
		h(w, r, cs, u)
	})
}

// authenticate resolves the bearer token on a request. On failure it has
// already written the 401.
func (app *App) authenticate(w http.ResponseWriter, r *http.Request) (*authedUser, bool) {
	token := bearerToken(r)
	if token == "" {
		unauthorized(w, "Sign in to continue.")
		return nil, false
	}
	now := time.Now()
	sess, user, err := app.Store.GetSessionByTokenHash(r.Context(), auth.HashToken(token), now.Unix())
	if err != nil {
		slog.Error("session lookup failed", "func", "authenticate", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		metrics.Http500Errors.Inc()
		return nil, false
	}
	if sess == nil {
		unauthorized(w, "Your session has expired. Sign in again.")
		return nil, false
	}
	if user.DisabledAt > 0 {
		unauthorized(w, disabledMessage(user))
		return nil, false
	}
	// The idle expiry slides; the absolute one does not.
	if time.Unix(sess.CreatedAt, 0).Add(auth.SessionMax).Before(now) {
		if _, err := app.Store.DeleteSession(r.Context(), sess.TokenHash); err != nil {
			slog.Warn("failed to delete an aged-out session", "func", "authenticate", "err", err)
		}
		unauthorized(w, "Your session has expired. Sign in again.")
		return nil, false
	}
	if now.Sub(time.Unix(sess.LastSeenAt, 0)) > auth.SessionTouchInterval {
		expires := sessionExpiry(now, time.Unix(sess.CreatedAt, 0))
		if err := app.Store.TouchSession(r.Context(), sess.ID, now.Unix(), expires); err != nil {
			slog.Warn("failed to touch session", "func", "authenticate", "err", err)
		}
	}
	return &authedUser{User: user, Session: sess}, true
}

// sessionExpiry is the idle expiry from now, never past the absolute one.
func sessionExpiry(now, created time.Time) int64 {
	exp := now.Add(auth.SessionIdle)
	if hard := created.Add(auth.SessionMax); exp.After(hard) {
		exp = hard
	}
	return exp.Unix()
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="live-transcript"`)
	writeJSONError(w, http.StatusUnauthorized, msg)
}

// writeJSONError answers with {"error": msg}. The account and notification
// endpoints speak JSON both ways so the site can show the message as is.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		slog.Error("failed to write JSON error", "err", err)
	}
	if status >= 500 {
		metrics.Http500Errors.Inc()
	} else if status >= 400 {
		metrics.Http400Errors.Inc()
	}
}

// decodeJSONBody reads a small JSON body into v, answering 400 itself.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	if err := dec.Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	return true
}

// clientIP is the address the request limiters and the lockout key on.
//
// Behind Cloudflare or a reverse proxy the socket peer is the proxy, so the
// address the proxy reports (CF-Connecting-IP, then the first X-Forwarded-For
// hop) is what identifies the client. Those headers are believed only from a
// configured trusted proxy when accounts.trustedProxies is set, and from any
// peer otherwise - the usual deployment is only reachable through its proxy.
// Whatever is chosen must parse as an address, or the socket peer is used;
// an IPv6 client is keyed by its /64, since one machine typically has a
// whole /64 to pick fresh addresses from.
func (app *App) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = host
	}
	peerIP := net.ParseIP(peer)

	forwarded := ""
	if len(app.authLimits.trustedProxies) == 0 || (peerIP != nil && inAnyNet(peerIP, app.authLimits.trustedProxies)) {
		forwarded = strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))
		if forwarded == "" {
			first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
			forwarded = strings.TrimSpace(first)
		}
	}
	if len(forwarded) > 64 {
		forwarded = ""
	}
	if ip := net.ParseIP(forwarded); ip != nil {
		return limiterKey(ip)
	}
	if peerIP != nil {
		return limiterKey(peerIP)
	}
	return peer
}

// limiterKey is the string the limiters key an address by: the address
// itself for IPv4, the /64 prefix for IPv6.
func limiterKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func inAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func tooMany(w http.ResponseWriter, retryAfter time.Duration, msg string) {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSONError(w, http.StatusTooManyRequests, msg)
}

// humanDuration renders a wait for a person: "45 seconds", "3 minutes".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		s := int(d / time.Second)
		if s <= 1 {
			return "a second"
		}
		return fmt.Sprintf("%d seconds", s)
	case d < time.Hour:
		m := int((d + 30*time.Second) / time.Minute)
		if m <= 1 {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", m)
	default:
		return "an hour"
	}
}

// Request and response shapes.

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AccountResponse describes the signed-in account to the site.
type AccountResponse struct {
	Username  string `json:"username"`
	CreatedAt int64  `json:"createdAt"`
	// Sessions is how many browsers are currently signed in.
	Sessions int `json:"sessions"`
}

// SessionResponse is what sign-in and registration return: the token the
// browser must send from now on, and when it expires.
type SessionResponse struct {
	Token     string          `json:"token"`
	ExpiresAt int64           `json:"expiresAt"`
	Account   AccountResponse `json:"account"`
}

// AuthInfoResponse tells the site what the server allows before it shows a
// sign-up form.
type AuthInfoResponse struct {
	RegistrationOpen  bool `json:"registrationOpen"`
	MinPasswordLength int  `json:"minPasswordLength"`
	MaxPasswordLength int  `json:"maxPasswordLength"`
	MinUsernameLength int  `json:"minUsernameLength"`
	MaxUsernameLength int  `json:"maxUsernameLength"`
}

// disabledMessage is what a disabled account is told, with the operator's
// reason when there is one.
func disabledMessage(u *model.User) string {
	if strings.TrimSpace(u.DisabledReason) != "" {
		return "This account has been disabled: " + strings.TrimSpace(u.DisabledReason)
	}
	return "This account has been disabled."
}

// registrationClosed reports whether sign-ups are refused, and by which
// switch: the config's accounts.disableRegistration, or the site admin's
// runtime override.
func (app *App) registrationClosed(ctx context.Context) (closed, byConfig, byAdmin bool) {
	byConfig = app.Accounts.DisableRegistration
	if v, ok, err := app.Store.GetSetting(ctx, store.SettingRegistrationClosed); err != nil {
		slog.Error("failed to read the registration setting", "func", "registrationClosed", "err", err)
	} else if ok && v == "1" {
		byAdmin = true
	}
	return byConfig || byAdmin, byConfig, byAdmin
}

func (app *App) getAuthInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	closed, _, _ := app.registrationClosed(r.Context())
	writeJSON(w, AuthInfoResponse{
		RegistrationOpen:  !closed,
		MinPasswordLength: auth.MinPasswordLength,
		MaxPasswordLength: auth.MaxPasswordLength,
		MinUsernameLength: auth.MinUsernameLength,
		MaxUsernameLength: auth.MaxUsernameLength,
	})
}

// postRegisterHandler creates an account and signs it in.
func (app *App) postRegisterHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if closed, _, _ := app.registrationClosed(r.Context()); closed {
		writeJSONError(w, http.StatusForbidden, "New accounts are not being accepted right now.")
		return
	}
	var req credentialsRequest
	if !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	display, key, err := auth.NormalizeUsername(req.Username)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if err := auth.ValidatePassword(req.Password, display); err != nil {
		writeJSONError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	// The limit counts sign-ups that get this far, not every fumbled form:
	// a rejected body costs nothing, while an account costs a row forever.
	ip := app.clientIP(r)
	if ok, wait := app.authLimits.register.Allow(ip); !ok {
		tooMany(w, wait, "Too many accounts created from this address. Try again in "+humanDuration(wait)+".")
		return
	}
	var hash string
	if !app.hashed(w, r, func() { hash, err = auth.HashPassword(req.Password) }) {
		return
	}
	if err != nil {
		slog.Error("password hashing failed", "func", "postRegisterHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Could not create the account.")
		return
	}
	now := time.Now()
	id, err := app.Store.CreateUser(r.Context(), display, key, hash, now.Unix())
	if errors.Is(err, store.ErrUsernameTaken) {
		writeJSONError(w, http.StatusConflict, "That username is taken.")
		return
	}
	if err != nil {
		slog.Error("failed to create user", "func", "postRegisterHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	user := &model.User{ID: id, Username: display, CreatedAt: now.Unix()}
	if err := app.Store.RecordLoginSuccess(r.Context(), id, now.Unix(), ""); err != nil {
		slog.Warn("failed to stamp first login", "func", "postRegisterHandler", "err", err)
	}
	slog.Info("account created", "func", "postRegisterHandler", "user", display, "ip", ip)
	app.issueSession(w, r, user, now, http.StatusCreated)
}

// postLoginHandler signs an account in. Every failure - unknown username,
// wrong password, malformed body - is the same 401 in the same time.
func (app *App) postLoginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := app.clientIP(r)
	if ok, wait := app.authLimits.login.Allow(ip); !ok {
		tooMany(w, wait, "Too many sign-in attempts. Try again in "+humanDuration(wait)+".")
		return
	}
	var req credentialsRequest
	if !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	const failed = "Wrong username or password."
	// The lockout is keyed by what was typed and where from, whether or not
	// such an account exists: its answer therefore says nothing about which
	// usernames are real, and a stranger who knows a username cannot lock its
	// owners out of a shared account from somewhere else.
	_, key, nameErr := auth.NormalizeUsername(req.Username)
	if nameErr != nil {
		key = strings.ToLower(strings.TrimSpace(req.Username))
		if len(key) > auth.MaxUsernameLength {
			key = key[:auth.MaxUsernameLength]
		}
	}
	lockKey := key + "|" + ip
	if wait := app.authLimits.lockout.Locked(lockKey); wait > 0 {
		tooMany(w, wait, "Too many failed sign-in attempts. Try again in "+humanDuration(wait)+".")
		return
	}
	now := time.Now()
	var user *model.User
	if nameErr == nil {
		var err error
		user, err = app.Store.GetUserByUsernameKey(r.Context(), key)
		if err != nil {
			slog.Error("user lookup failed", "func", "postLoginHandler", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "Database error")
			return
		}
	}
	// A wrong username and a wrong password get the same answer after the
	// same amount of work.
	ok := false
	if !app.hashed(w, r, func() {
		if user == nil {
			auth.BurnVerify(req.Password)
			return
		}
		var err error
		if ok, err = auth.VerifyPassword(user.PasswordHash, req.Password); err != nil {
			slog.Error("stored password hash is unreadable", "func", "postLoginHandler", "user", user.Username, "err", err)
		}
	}) {
		return
	}
	if !ok {
		wait := app.authLimits.lockout.Fail(lockKey)
		slog.Info("sign-in failed", "func", "postLoginHandler", "username", key, "known", user != nil, "ip", ip, "lock", wait)
		unauthorized(w, failed)
		return
	}
	app.authLimits.lockout.Reset(lockKey)
	// Only the account's own password learns that it was disabled.
	if user.DisabledAt > 0 {
		slog.Info("disabled account tried to sign in", "func", "postLoginHandler", "user", user.Username, "ip", ip)
		writeJSONError(w, http.StatusForbidden, disabledMessage(user))
		return
	}
	rehash := ""
	if auth.NeedsRehash(user.PasswordHash) {
		// Best effort: a full guard means the old hash simply stays.
		app.authLimits.hashing.Do(r.Context(), func() {
			if h, err := auth.HashPassword(req.Password); err == nil {
				rehash = h
			}
		})
	}
	if err := app.Store.RecordLoginSuccess(r.Context(), user.ID, now.Unix(), rehash); err != nil {
		slog.Warn("failed to record login", "func", "postLoginHandler", "err", err)
	}
	slog.Info("signed in", "func", "postLoginHandler", "user", user.Username, "ip", ip)
	app.issueSession(w, r, user, now, http.StatusOK)
}

// checkPassword verifies the signed-in account's password before a
// sensitive change, with the same per-address limit, lockout and hashing
// bound as sign-in: a stolen session must not become a free online oracle
// for the password it never knew. On failure the response is written.
func (app *App) checkPassword(w http.ResponseWriter, r *http.Request, u *authedUser, password, wrongMsg string) bool {
	ip := app.clientIP(r)
	if ok, wait := app.authLimits.login.Allow(ip); !ok {
		tooMany(w, wait, "Too many attempts. Try again in "+humanDuration(wait)+".")
		return false
	}
	lockKey := strings.ToLower(u.User.Username) + "|" + ip
	if wait := app.authLimits.lockout.Locked(lockKey); wait > 0 {
		tooMany(w, wait, "Too many failed attempts. Try again in "+humanDuration(wait)+".")
		return false
	}
	ok := false
	if !app.hashed(w, r, func() { ok, _ = auth.VerifyPassword(u.User.PasswordHash, password) }) {
		return false
	}
	if !ok {
		wait := app.authLimits.lockout.Fail(lockKey)
		slog.Info("password check failed", "func", "checkPassword", "user", u.User.Username, "ip", ip, "path", r.URL.Path, "lock", wait)
		writeJSONError(w, http.StatusForbidden, wrongMsg)
		return false
	}
	app.authLimits.lockout.Reset(lockKey)
	return true
}

// issueSession creates a session for user and writes it out.
func (app *App) issueSession(w http.ResponseWriter, r *http.Request, user *model.User, now time.Time, status int) {
	token, hash, err := auth.NewSessionToken()
	if err != nil {
		slog.Error("failed to generate session token", "func", "issueSession", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Could not sign in.")
		return
	}
	expires := sessionExpiry(now, now)
	if _, err := app.Store.CreateSession(r.Context(), user.ID, hash, now.Unix(), expires, clientLabel(r), app.clientIP(r)); err != nil {
		slog.Error("failed to create session", "func", "issueSession", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	sessions, _ := app.Store.CountUserSessions(r.Context(), user.ID, now.Unix())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(SessionResponse{
		Token:     token,
		ExpiresAt: expires,
		Account:   AccountResponse{Username: user.Username, CreatedAt: user.CreatedAt, Sessions: sessions},
	}); err != nil {
		slog.Error("failed to write session response", "func", "issueSession", "err", err)
	}
}

func clientLabel(r *http.Request) string {
	ua := strings.TrimSpace(r.UserAgent())
	if len(ua) > maxClientLength {
		ua = ua[:maxClientLength]
	}
	return ua
}

// getMeHandler describes the signed-in account.
func (app *App) getMeHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	sessions, _ := app.Store.CountUserSessions(r.Context(), u.User.ID, time.Now().Unix())
	writeJSON(w, AccountResponse{Username: u.User.Username, CreatedAt: u.User.CreatedAt, Sessions: sessions})
}

// postLogoutHandler ends the current session.
func (app *App) postLogoutHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	if _, err := app.Store.DeleteSession(r.Context(), u.Session.TokenHash); err != nil {
		slog.Error("failed to delete session", "func", "postLogoutHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postLogoutAllHandler ends every OTHER session of the account. The browser
// asking stays signed in: an account can be shared by a whole mod team, and
// "everyone else out, but not me" is the move that makes sense when a device
// is lost or a member leaves.
func (app *App) postLogoutAllHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	n, err := app.Store.DeleteUserSessions(r.Context(), u.User.ID, u.Session.ID)
	if err != nil {
		slog.Error("failed to delete sessions", "func", "postLogoutAllHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	slog.Info("signed out other sessions", "func", "postLogoutAllHandler", "user", u.User.Username, "sessions", n)
	writeJSON(w, map[string]int64{"sessionsEnded": n})
}

// SessionView is one signed-in browser as the account sees it. The address
// a session signed in from is deliberately not part of it: an account can
// be shared by a mod team, and a member's address is their own business.
type SessionView struct {
	ID int64 `json:"id"`
	// Label is the browser and system in words ("Chrome on Windows").
	Label      string `json:"label"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	// Current marks the session making the request.
	Current bool `json:"current"`
}

// getSessionsHandler lists the account's sessions: everyone signed in to it,
// on every device, most recently active first.
func (app *App) getSessionsHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	sessions, err := app.Store.ListUserSessions(r.Context(), u.User.ID, time.Now().Unix())
	if err != nil {
		slog.Error("failed to list sessions", "func", "getSessionsHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, SessionView{
			ID:         s.ID,
			Label:      describeClient(s.Client),
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			ExpiresAt:  s.ExpiresAt,
			Current:    s.ID == u.Session.ID,
		})
	}
	writeJSON(w, map[string]any{"sessions": views})
}

// deleteSessionHandler ends one of the account's sessions, including the
// current one (which is just signing out).
func (app *App) deleteSessionHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid session id")
		return
	}
	ok, err := app.Store.DeleteUserSession(r.Context(), u.User.ID, id)
	if err != nil {
		slog.Error("failed to delete session", "func", "deleteSessionHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "No such session")
		return
	}
	slog.Info("session ended", "func", "deleteSessionHandler", "user", u.User.Username, "session", id, "own", id == u.Session.ID)
	w.WriteHeader(http.StatusNoContent)
}

// describeClient turns a user agent into "Chrome on Windows". Best effort
// and purely cosmetic: the sessions list needs something a person can tell
// apart, not an accurate fingerprint.
func describeClient(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "Unknown browser"
	}
	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "Edge/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	system := ""
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		system = "iOS"
	case strings.Contains(ua, "Android"):
		system = "Android"
	case strings.Contains(ua, "Windows"):
		system = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		system = "macOS"
	case strings.Contains(ua, "CrOS"):
		system = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		system = "Linux"
	}
	if system == "" {
		return browser
	}
	return browser + " on " + system
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// postPasswordHandler changes the password and signs every other browser
// out: whoever else had the old password does not keep a session.
func (app *App) postPasswordHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	var req passwordChangeRequest
	if !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	if err := auth.ValidatePassword(req.NewPassword, u.User.Username); err != nil {
		writeJSONError(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if !app.checkPassword(w, r, u, req.CurrentPassword, "The current password is wrong.") {
		return
	}
	var hash string
	var err error
	if !app.hashed(w, r, func() { hash, err = auth.HashPassword(req.NewPassword) }) {
		return
	}
	if err != nil {
		slog.Error("password hashing failed", "func", "postPasswordHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Could not change the password.")
		return
	}
	// The new hash and the end of every other session land together.
	ended, err := app.Store.ChangePassword(r.Context(), u.User.ID, hash, time.Now().Unix(), u.Session.ID)
	if err != nil {
		slog.Error("failed to change password", "func", "postPasswordHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	slog.Info("password changed", "func", "postPasswordHandler", "user", u.User.Username, "otherSessionsEnded", ended)
	writeJSON(w, map[string]int64{"sessionsEnded": ended})
}

type deleteAccountRequest struct {
	Password string `json:"password"`
}

// deleteAccountHandler removes the account and everything it owns, after the
// password is confirmed. It cannot be undone.
func (app *App) deleteAccountHandler(w http.ResponseWriter, r *http.Request, u *authedUser) {
	var req deleteAccountRequest
	if !decodeJSONBody(w, r, maxAuthBody, &req) {
		return
	}
	if !app.checkPassword(w, r, u, req.Password, "The password is wrong.") {
		return
	}
	if err := app.Store.DeleteUser(r.Context(), u.User.ID); err != nil {
		slog.Error("failed to delete account", "func", "deleteAccountHandler", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		return
	}
	slog.Info("account deleted", "func", "deleteAccountHandler", "user", u.User.Username)
	w.WriteHeader(http.StatusNoContent)
}

// cleanupSessions drops expired session rows. They are refused at lookup
// regardless; this keeps the table from growing forever.
func (app *App) cleanupSessions() {
	removed, err := app.Store.CleanupExpiredSessions(context.Background(), time.Now().Unix())
	if err != nil {
		slog.Error("failed to cleanup expired sessions", "func", "cleanupSessions", "err", err)
		return
	}
	if removed > 0 {
		slog.Info("removed expired sessions", "func", "cleanupSessions", "count", removed)
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
