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
