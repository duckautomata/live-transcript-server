// Package livedetect watches YouTube and Twitch for configured channels going
// live and reports what it sees to a Sink.
//
// It is an OBSERVER. It does not queue work for the worker, does not touch the
// streams table, and does not activate anything - the worker still owns
// activation. In its current form the Sink's only action is a Discord
// notification recording when a broadcast started, when we detected it, the
// delay between the two, and which detection mechanism won the race. That
// measurement is the point: it is what decides whether this is trustworthy
// enough to later drive the worker.
//
// Design notes that the rest of the package depends on:
//
//   - Every mechanism produces a LEVEL signal ("this channel is live right
//     now") and re-observes the same broadcast on every cycle. Converting that
//     to a single notification is NOT done here - it happens once, atomically,
//     in the Sink (Store.ClaimDetection). That is why this package keeps no
//     durable state and why a restart, a crash loop, or two mechanisms seeing
//     the same stream are all safe with no special handling.
//
//   - Mechanisms are redundant on purpose. A push path (Twitch EventSub,
//     YouTube WebSub) gives seconds of latency but is edge-triggered: one
//     dropped delivery and the stream is never seen. A polling path is slower
//     but level-triggered and self-healing. Running both means the push path
//     supplies the latency and the poll supplies the guarantee, and the ledger
//     makes the race between them harmless.
//
//   - "I don't know" must never look like "offline". Every poll returns
//     (nil, err) rather than an empty result when it could not determine
//     liveness, so a 5xx, a 429, an expired token or an exhausted quota can
//     never be read as a stream ending.
//
// Everything is nil-receiver-safe (New returns (nil, nil) when unconfigured),
// matching discord.Bot, so callers never special-case the disabled build.
package livedetect

import (
	"context"
	"time"
)

// Platform identifiers. These are persisted in the detection ledger's primary
// key, so they are part of the on-disk format and must not change casually.
const (
	PlatformYouTube = "youtube"
	PlatformTwitch  = "twitch"
)

// Mechanism identifiers, recorded on the ledger row that wins a claim. A soak
// reads these to learn which detection path actually delivers the latency, so
// they are deliberately specific about the path taken rather than the platform.
const (
	MechanismTwitchEventSub  = "twitch-eventsub"
	MechanismTwitchPoll      = "twitch-poll"
	MechanismYouTubeWebSub   = "youtube-websub"
	MechanismYouTubeState    = "youtube-state-poll"
	MechanismYouTubeDiscover = "youtube-discovery"
	// MechanismTwitchEventSubReconcile is the SUBSCRIPTION-MANAGEMENT half of
	// EventSub, tracked separately from delivery. Folding the two together
	// would let a healthy Helix API call mask the fact that no notification
	// has arrived in hours.
	MechanismTwitchEventSubReconcile = "twitch-eventsub-reconcile"
	// MechanismYouTubeWebSubRenew is the lease-renewal half of WebSub, tracked
	// separately from push delivery for the same reason.
	MechanismYouTubeWebSubRenew = "youtube-websub-renew"
)

// Target is one channel to watch on one platform. ID is the platform's own
// identifier for the channel: a Twitch login, or a "UC..." YouTube channel ID.
// Both are resolved from config at startup and never at runtime, so a typo is
// a boot failure rather than a silent permanent "never live".
type Target struct {
	ChannelKey string
	ID         string
}

// Broadcast is one live broadcast observed on a platform.
type Broadcast struct {
	Platform   string
	ChannelKey string
	// ID is the platform's per-broadcast identifier: a YouTube video ID, or a
	// Twitch numeric stream ID. It is the ledger key. Never the URL - a Twitch
	// channel reuses one URL for every broadcast it will ever do.
	ID    string
	URL   string
	Title string
	// StartedAt is the PLATFORM's start time (Helix started_at,
	// liveStreamingDetails.actualStartTime), not our first sighting. Zero when
	// the platform reported none, in which case the delay is unknowable and
	// the notification says so rather than inventing a number.
	StartedAt time.Time
	// SawScheduled reports that this broadcast was watched as a scheduled
	// frame before it started, so the measured delay reflects the poll ladder
	// rather than how long discovery took to notice it. Only YouTube can be
	// scheduled; Twitch broadcasts are always false.
	SawScheduled bool
}

// Sink receives observations. *server.App implements it.
//
// ObserveLive is called by EVERY mechanism on EVERY cycle it sees a broadcast
// live - typically hundreds of times for one stream. Deduplication is the
// Sink's responsibility, not the caller's, and it is what makes the redundant
// mechanisms safe.
//
// ObserveEnded is called when a mechanism sees a previously-live broadcast
// stop. It is best-effort: nothing depends on an end being observed, and a
// missed end costs only the accelerated restart re-poll.
type Sink interface {
	ObserveLive(ctx context.Context, b Broadcast, mechanism string) error
	ObserveEnded(ctx context.Context, platform, broadcastID string) error
	// ActiveBroadcasts returns what the ledger still believes is live, so a
	// restart can resume watching a broadcast already in progress instead of
	// waiting for the next discovery pass to rediscover it.
	ActiveBroadcasts(ctx context.Context) ([]Broadcast, error)
}

// State is the liveness verdict for one broadcast, kept separate from
// Broadcast so that "not live" can be expressed without inventing a zero
// Broadcast that downstream code might mistake for a real one.
type State int

const (
	// StateUnknown means the platform could not be asked. It must never be
	// treated as StateEnded - that is the bug that makes a transient outage
	// look like a stream ending and then restarting.
	StateUnknown State = iota
	// StateUpcoming is a scheduled broadcast that has not started: a YouTube
	// waiting room or a scheduled premiere. Never reported as live.
	StateUpcoming
	// StateLive is in progress. For YouTube this covers both livestreams and
	// premieres, which is intended - the API does not distinguish them while
	// running, and both are in scope.
	StateLive
	// StateEnded is finished.
	StateEnded
)

func (s State) String() string {
	switch s {
	case StateUpcoming:
		return "upcoming"
	case StateLive:
		return "live"
	case StateEnded:
		return "ended"
	default:
		return "unknown"
	}
}
