// Package announce turns live-detection observations into public Discord
// announcements, driven by admin-configured notification events.
//
// It is the audience-facing half of live detection. The operator-facing half
// (leg health alerts, callback probes, the diagnostic feed) stays in
// internal/discord; this package deliberately knows nothing about errors or
// diagnostics, because the webhooks it posts to ping thousands of people. The
// only payload that can ever leave here is a rendered announcement of a real
// observation - a stream going live, a stream or premiere being scheduled, a
// video or short being published - or an explicit, admin-initiated test send
// with every mention suppressed.
//
// Three ideas hold it together:
//
//   - A Payload is one observation. It carries the facts (channel, title,
//     url, when) and nothing about how they were found.
//
//   - A notification event (model.NotificationEvent) is a rule: which
//     triggers, which webhooks, what template, how long between sends. The
//     dispatcher matches payloads to rules; the rule's cooldown is claimed
//     atomically in the store so two racing detections cannot both pass.
//
//   - Templates are plain strings with {placeholders}. Rendering is pure and
//     total: any template renders to something, unknown placeholders are left
//     as written, and Discord's length limits are enforced by truncation
//     rather than by failing at send time.
package announce

import (
	"fmt"
	"strings"
	"time"
)

// Trigger names an observable event kind an announcement rule can react to.
// These are stored in notification_events.triggers and in the log, so they
// are part of the on-disk format.
type Trigger string

const (
	// TriggerLive is a stream going live: a Twitch broadcast starting, a
	// YouTube livestream starting, or a YouTube premiere beginning. All three
	// are "going live" from the audience's point of view.
	TriggerLive Trigger = "live"
	// TriggerScheduled is a YouTube stream waiting room or premiere page
	// appearing with a scheduled start. The Data API does not reliably tell a
	// scheduled premiere from a scheduled livestream, so they share a trigger.
	TriggerScheduled Trigger = "scheduled"
	// TriggerUpload is an ordinary YouTube video being published.
	TriggerUpload Trigger = "upload"
	// TriggerShort is a YouTube short being published.
	TriggerShort Trigger = "short"
)

// TriggerInfo describes a trigger for the admin UI.
type TriggerInfo struct {
	ID          Trigger `json:"id"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	// Headline is what {headline} expands to for this trigger.
	Headline string `json:"headline"`
	// Platforms lists where the trigger can fire, for the UI's hint text.
	Platforms []string `json:"platforms"`
}

// Triggers is every trigger in display order.
var Triggers = []TriggerInfo{
	{
		ID:          TriggerLive,
		Label:       "Going live",
		Description: "A Twitch stream starts, a YouTube stream starts, or a YouTube premiere begins.",
		Headline:    "Stream Started",
		Platforms:   []string{"twitch", "youtube"},
	},
	{
		ID:          TriggerScheduled,
		Label:       "Stream or premiere scheduled",
		Description: "A YouTube waiting room or premiere page appears with a start time at least 15 minutes away.",
		Headline:    "Stream Scheduled",
		Platforms:   []string{"youtube"},
	},
	{
		ID:          TriggerUpload,
		Label:       "Video uploaded",
		Description: "A regular YouTube video is published (not a stream, premiere, or short).",
		Headline:    "New Video",
		Platforms:   []string{"youtube"},
	},
	{
		ID:          TriggerShort,
		Label:       "Short uploaded",
		Description: "A YouTube short is published.",
		Headline:    "New Short",
		Platforms:   []string{"youtube"},
	},
}

// KnownTrigger reports whether s names a trigger.
func KnownTrigger(s string) bool {
	_, ok := triggerInfo(Trigger(s))
	return ok
}

func triggerInfo(t Trigger) (TriggerInfo, bool) {
	for _, info := range Triggers {
		if info.ID == t {
			return info, true
		}
	}
	return TriggerInfo{}, false
}

// Headline is the short phrase {headline} expands to.
func (t Trigger) Headline() string {
	if info, ok := triggerInfo(t); ok {
		return info.Headline
	}
	return "Update"
}

// Label is the trigger's human name.
func (t Trigger) Label() string {
	if info, ok := triggerInfo(t); ok {
		return info.Label
	}
	return string(t)
}

// Platform identifiers, matching internal/livedetect.
const (
	PlatformYouTube = "youtube"
	PlatformTwitch  = "twitch"
)

// PlatformLabel is what {platform} expands to.
func PlatformLabel(platform string) string {
	switch platform {
	case PlatformYouTube:
		return "YouTube"
	case PlatformTwitch:
		return "Twitch"
	default:
		return platform
	}
}

// Payload is one observation to announce. It is built by the server from a
// ledger row the detection sink has just claimed, so by construction it
// describes something that really happened, exactly once.
type Payload struct {
	Trigger    Trigger
	Platform   string
	ChannelKey string
	// ID is the platform's identifier: a YouTube video id or a Twitch stream
	// id. It only feeds {id}, the thumbnail URL and the log.
	ID    string
	URL   string
	Title string
	// EventTime is the moment the announcement is about: the stream's start,
	// the scheduled start, or the publish time. Zero when the platform did not
	// report one, in which case the time placeholders render empty and the
	// embed carries no timestamp.
	EventTime time.Time
	// DetectedAt is when this server saw it. Used for the operator feed's
	// diagnostics and as a cache-buster on Twitch preview images.
	DetectedAt time.Time

	// Operator diagnostics. Only the operator feed renders these; a public
	// announcement never sees them.
	Mechanism    string
	SawScheduled bool
}

// Channel is how one configured channel is presented in announcements.
type Channel struct {
	Key         string
	DisplayName string
	TwitchLogin string
}

// ThumbnailURL is the platform's preview image for the payload. Twitch
// previews share one URL for every broadcast a login will ever do, so a
// cache-buster is appended: Discord proxies images by URL and would otherwise
// happily show a week-old frame.
func (p Payload) ThumbnailURL(ch Channel) string {
	switch p.Platform {
	case PlatformTwitch:
		login := ch.TwitchLogin
		if login == "" {
			login = strings.ToLower(ch.DisplayName)
		}
		u := fmt.Sprintf("https://static-cdn.jtvnw.net/previews-ttv/live_user_%s-1280x720.jpg", login)
		if !p.DetectedAt.IsZero() {
			u += fmt.Sprintf("?t=%d", p.DetectedAt.Unix())
		}
		return u
	case PlatformYouTube:
		if p.ID == "" {
			return ""
		}
		return fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", p.ID)
	default:
		return ""
	}
}
