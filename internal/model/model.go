// Package model holds the shared data types exchanged between the worker,
// the database, and web clients. It is a leaf package: it may not import any
// other package from this module.
package model

import "encoding/json"

// Line is a single transcript line.
type Line struct {
	ID             int             `json:"id"`
	FileID         string          `json:"fileId"`
	Timestamp      int             `json:"timestamp"`
	Segments       json.RawMessage `json:"segments"`
	MediaAvailable bool            `json:"mediaAvailable"`
	VodAccurate    bool            `json:"vodAccurate"`
}

// Stream represents the state of a stream for a channel in the database.
type Stream struct {
	ChannelID     string `json:"channelId"`
	StreamID      string `json:"streamId"`
	StreamTitle   string `json:"streamTitle"`
	StartTime     string `json:"startTime"`
	IsLive        bool   `json:"isLive"`
	MediaType     string `json:"mediaType"`
	ActivatedTime int64  `json:"activatedTime"`
}

// WorkerData represents the full state of the worker. Used to sync the server
// with the worker.
type WorkerData struct {
	StreamID    string `json:"streamId"`
	StreamTitle string `json:"streamTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
}

// WorkerStatus represents the status of a worker for a specific channel key.
type WorkerStatus struct {
	ChannelKey      string `json:"channelKey"`
	WorkerVersion   string `json:"workerVersion"`
	WorkerBuildTime string `json:"workerBuildTime"`
	LastSeen        int64  `json:"lastSeen"`
	IsActive        bool   `json:"isActive"` // Computed field
}

// WorkerStatusRequest is the body of the worker's POST /status heartbeat.
type WorkerStatusRequest struct {
	Version   string   `json:"version"`
	BuildTime string   `json:"build_time"`
	Keys      []string `json:"keys"`
	// CookieState is optional: an older worker omits it entirely, and this
	// server must keep accepting that body. Empty means "not reported", which
	// is never treated as a failure.
	CookieState  string `json:"cookie_state,omitempty"`
	CookieReason string `json:"cookie_reason,omitempty"`
}

// ServerInfo represents the version information of the server.
type ServerInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"buildTime"`
}

// FullInfoResponse is the response for the public GET /status endpoint.
type FullInfoResponse struct {
	Server  ServerInfo     `json:"server"`
	Workers []WorkerStatus `json:"workers"`
	// Cookies is nil until a worker reports cookie health, so existing
	// clients that do not know the field are unaffected.
	Cookies *CookieStatus `json:"cookies,omitempty"`
}

// DetectedBroadcast is one broadcast that live detection has observed going
// live. It is the ledger row that makes detection notify exactly once per
// broadcast no matter how many mechanisms see it or how often they poll.
//
// The primary key is (Platform, BroadcastID) - the platform's own per-broadcast
// identifier, never the URL. A Twitch channel reuses the same
// https://twitch.tv/{login} for every broadcast it will ever do, so a
// URL-keyed ledger would report a channel's first stream and then go silent
// forever.
type DetectedBroadcast struct {
	Platform    string `json:"platform"`    // "youtube" or "twitch"
	BroadcastID string `json:"broadcastId"` // YouTube video ID, or Twitch numeric stream ID
	ChannelKey  string `json:"channelKey"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	// StartedAt is the platform's own start time (Helix started_at,
	// liveStreamingDetails.actualStartTime) in unix seconds, NOT our first
	// sighting. Zero when the platform did not report one.
	StartedAt int64 `json:"startedAt"`
	// DetectedAt is when this server first observed the broadcast live, in
	// unix seconds. DetectedAt-StartedAt is the detection delay the shadow-mode
	// notification reports.
	DetectedAt int64 `json:"detectedAt"`
	// Mechanism names the detection path that won the race to claim this
	// broadcast, e.g. "twitch-eventsub" or "youtube-state-poll". It is the
	// measurement the shadow-mode soak exists to collect.
	Mechanism string `json:"mechanism"`
	// EndedAt is when the broadcast was first observed to be over, in unix
	// seconds; zero while it is still live or was never seen to end.
	EndedAt int64 `json:"endedAt"`
}

// DetectedVideo is the ledger row for a non-live YouTube observation: a
// stream or premiere being scheduled, or a plain video or short being
// published. Like DetectedBroadcast it exists so that an observation which the
// poller re-derives on every cycle - and which a restart re-derives from
// scratch - is announced exactly once. Keyed on the video id AND the kind: the
// same id is legitimately "scheduled" first and then goes live, and the live
// half lives in detected_broadcasts.
type DetectedVideo struct {
	Platform   string `json:"platform"`
	VideoID    string `json:"videoId"`
	Kind       string `json:"kind"` // "scheduled", "upload" or "short"
	ChannelKey string `json:"channelKey"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	// PublishedAt is the platform's publish time in unix seconds; zero when
	// not reported.
	PublishedAt int64 `json:"publishedAt"`
	// ScheduledAt is the announced start time of a scheduled stream or
	// premiere, in unix seconds; zero for uploads and shorts.
	ScheduledAt int64 `json:"scheduledAt"`
	DetectedAt  int64 `json:"detectedAt"`
}

// EmbedTemplate is the admin-editable shape of a notification's Discord
// embed. Every string field is a template: {placeholders} are expanded at send
// time. Color is "#RRGGBB"; empty means Discord's default. Timestamp adds the
// event's time (stream start, scheduled start, publish time) to the embed.
type EmbedTemplate struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Color       string `json:"color"`
	Image       string `json:"image"`
	Thumbnail   string `json:"thumbnail"`
	Footer      string `json:"footer"`
	Timestamp   bool   `json:"timestamp"`
}

// Webhook is one Discord webhook a notification event posts to. Name is the
// admin's label for it ("#announcements", "members server") so the list and
// the delivery log can say where a post went without showing the URL; it may
// be empty. URL is the sensitive part and is never logged.
type Webhook struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Label is how a webhook is referred to in logs and on the admin page: its
// name when it has one, otherwise whatever masked form the caller supplies.
func (w Webhook) Label(masked string) string {
	if w.Name != "" {
		return w.Name + " (" + masked + ")"
	}
	return masked
}

// WebhooksFromURLs builds unnamed webhooks from URLs. Mostly for tests and
// for reading the older list-of-strings form back from the database.
func WebhooksFromURLs(urls ...string) []Webhook {
	out := make([]Webhook, 0, len(urls))
	for _, u := range urls {
		out = append(out, Webhook{URL: u})
	}
	return out
}

// NotificationEvent is one admin-configured announcement rule: when any of its
// Triggers fires for its channel, the rendered Content and Embed are posted to
// every one of its Webhooks.
//
// The webhook URLs are the sensitive part. A public announcement webhook pings
// thousands of people, so nothing but a rendered announcement may ever be sent
// through one - never an error, never a diagnostic - and the URLs are masked
// in every log line and audit record.
type NotificationEvent struct {
	ID         int64  `json:"id"`
	ChannelKey string `json:"channelKey"`
	// UserID is the account that owns the rule. Every read and write of a
	// rule is scoped by it, so one account can never see or touch another's
	// webhooks. Zero marks a rule created on the admin page before accounts
	// existed; it keeps firing and can only be deleted, not edited.
	UserID int64 `json:"userId"`
	// Owner is the owning account's username, filled in only for the
	// operator's view on the admin page.
	Owner    string    `json:"owner,omitempty"`
	Name     string    `json:"name"`
	Enabled  bool      `json:"enabled"`
	Webhooks []Webhook `json:"webhooks"`
	// Triggers is the set of event kinds this rule announces: "live",
	// "scheduled", "upload", "short".
	Triggers []string `json:"triggers"`
	// Content is the message-body template. Role pings go here as <@&ROLE_ID>.
	Content      string        `json:"content"`
	EmbedEnabled bool          `json:"embedEnabled"`
	Embed        EmbedTemplate `json:"embed"`
	// CooldownSeconds is the minimum gap between two sends of this rule for
	// the SAME trigger. Zero disables the limit. It is what stops a stream
	// restart - a brand-new broadcast id minutes after the first - from
	// pinging everyone twice, while a "scheduled" ping followed by the
	// "live" ping it announced still goes out.
	CooldownSeconds int64 `json:"cooldownSeconds"`

	// Delivery trail, maintained by the dispatcher.
	LastSentAt  int64  `json:"lastSentAt"`
	SentCount   int64  `json:"sentCount"`
	LastError   string `json:"lastError"`
	LastErrorAt int64  `json:"lastErrorAt"`

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// WebhookURLs returns the rule's webhook URLs in order.
func (e NotificationEvent) WebhookURLs() []string {
	out := make([]string, 0, len(e.Webhooks))
	for _, w := range e.Webhooks {
		out = append(out, w.URL)
	}
	return out
}

// Notification log statuses.
const (
	NotificationStatusSent       = "sent"       // every webhook accepted it
	NotificationStatusPartial    = "partial"    // some webhooks failed
	NotificationStatusFailed     = "failed"     // no webhook accepted it
	NotificationStatusSuppressed = "suppressed" // inside the rule's cooldown
	NotificationStatusTest       = "test"       // an admin-initiated test send
)

// NotificationLogEntry records one dispatch decision for the admin page, so an
// operator can see what was announced, where, and why something was not.
type NotificationLogEntry struct {
	ID         int64  `json:"id"`
	ChannelKey string `json:"channelKey"`
	EventID    int64  `json:"eventId"`
	// UserID is the owner of the rule the entry is about, so an account sees
	// its own delivery trail and nobody else's. Owner is the username, for
	// the admin page only.
	UserID      int64  `json:"userId"`
	Owner       string `json:"owner,omitempty"`
	EventName   string `json:"eventName"`
	Trigger     string `json:"trigger"`
	Platform    string `json:"platform"`
	BroadcastID string `json:"broadcastId"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	// Detail is a human-readable note: which webhooks failed and why (masked),
	// or how long the cooldown had left.
	Detail    string `json:"detail"`
	Webhooks  int    `json:"webhooks"`
	Delivered int    `json:"delivered"`
	SentAt    int64  `json:"sentAt"`
}

// Worker cookie-auth states, mirroring the worker's cookieauth.CookieAuth.
const (
	CookieStateNA      = "na"
	CookieStateOK      = "ok"
	CookieStateAbsent  = "absent"
	CookieStateRotated = "rotated"
)

// CookieStatus is the worker's YouTube cookie health. Worker-global rather
// than per-channel: one cookie jar backs every YouTube channel.
type CookieStatus struct {
	WorkerID  string `json:"workerId"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Since     int64  `json:"since"`     // when the current state began
	Alerted   bool   `json:"alerted"`   // an operator alert has already gone out
	UpdatedAt int64  `json:"updatedAt"` // last heartbeat that carried cookie health
}

// CookieDegraded reports whether a state means yt-dlp is scraping YouTube
// anonymously. Unknown states are treated as healthy: a newer worker reporting
// a state this server does not know must not be able to page the operator.
func CookieDegraded(state string) bool {
	return state == CookieStateAbsent || state == CookieStateRotated
}

// User is an account on the live-transcript site. Accounts exist so that
// anyone can run their own notification events, isolated from everyone
// else's: a rule and its webhooks belong to exactly one user. The password
// hash and the lockout state never serialize.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	// PasswordHash is the argon2id PHC string; see internal/auth.
	PasswordHash      string `json:"-"`
	CreatedAt         int64  `json:"createdAt"`
	PasswordChangedAt int64  `json:"-"`
	LastLoginAt       int64  `json:"lastLoginAt"`
	// DisabledAt is set when the operator disables the account for abuse:
	// it cannot sign in, its sessions are refused and its rules stop firing,
	// but nothing is deleted, so it can be enabled again. DisabledReason is
	// the operator's note, shown to the account when it tries to sign in.
	DisabledAt     int64  `json:"disabledAt"`
	DisabledReason string `json:"disabledReason"`
}

// Session is one signed-in browser. Only the hash of its bearer token is
// stored; the token itself is shown once, at sign-in.
type Session struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"userId"`
	TokenHash  string `json:"-"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	// Client is a short description of the browser that signed in, and
	// Address where it signed in from, for the account's sessions list - an
	// account can be shared by a whole mod team, and the list is how they
	// see who is signed in. Neither is used for anything security-relevant.
	Client  string `json:"client"`
	Address string `json:"address"`
}
