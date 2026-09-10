package livedetect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/metrics"
)

// Callback paths for the inbound push endpoints. They are registered as public
// routes and authenticate themselves by HMAC, not by an API key: the caller is
// Twitch or Google's hub, neither of which can send one.
const (
	TwitchEventSubPath = "/livedetect/twitch/eventsub"
	YouTubeWebSubPath  = "/livedetect/youtube/websub"
)

// Defaults for the tunables a config may leave unset.
const (
	defaultTwitchPollSeconds  = 60
	defaultYTDiscoverySeconds = 120
	defaultStaleAlertMinutes  = 15
	defaultYouTubeUnitBudget  = 8000
	// defaultSearchAuditCalls leaves most of search.list's own 100-call daily
	// bucket unspent, so a manual query is still possible after a full day of
	// automated audits.
	defaultSearchAuditCalls    = 40
	minTwitchPollSeconds       = 60
	minYouTubeDiscoverySeconds = 30
)

// ytChannelIDPattern matches a YouTube channel id: "UC" plus 22 characters of
// base64url alphabet.
var ytChannelIDPattern = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)

// validTwitchLogin reports whether s looks like a Twitch login: 4-25
// characters of letters, digits and underscores.
func validTwitchLogin(s string) bool {
	if len(s) < 4 || len(s) > 25 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

// failuresBeforeAlert tolerates blips and far-end redeploys before alerting on
// a leg. Alerts fire once on the healthy->down transition and once on
// recovery, never per cycle: every discord.Client notifier is a bare goroutine
// with no bound, so per-tick alerting during a day-long outage would spawn
// thousands of them and bury the real signal.
const failuresBeforeAlert = 5

// legHealth is one detection leg's health trail.
type legHealth struct {
	lastSuccess time.Time
	lastErr     string
	lastErrAt   time.Time
	failures    int
	alerted     bool
}

// Detector runs the detection legs and reports observations to a Sink.
type Detector struct {
	cfg     config.LiveDetectConfig
	sink    Sink
	alerts  *discord.Client
	twitch  *TwitchClient
	youtube *YouTubeClient
	websub  *WebSubClient

	// twitchTargets maps a lowercase Twitch login to the server channel key,
	// and twitchUserIDs maps a numeric broadcaster id to the same. Both are
	// resolved once at startup: a runtime resolution failure would be silent
	// and permanent.
	twitchTargets map[string]string
	twitchUserIDs map[string]string
	// ytTargets maps a "UC..." channel id to the server channel key.
	ytTargets map[string]string
	watch     *watchlist
	gov       *quotaGovernor
	seen      *seenCache

	// webSubSeen dedupes hub pushes, which are documented to arrive more than
	// once and to fire on title and description edits.
	webSubSeen *seenCache

	// twitchAbsent counts consecutive polls in which a previously-live login
	// was missing from the Helix response. Helix is edge-cached and can omit a
	// genuinely live channel from a successful response, so a single absence
	// is never enough to call a stream ended.
	twitchAbsentMu sync.Mutex
	twitchAbsent   map[string]int
	twitchLiveID   map[string]string
	// twitchLiveSince records when we started tracking the current broadcast
	// for a login, so an out-of-order stream.offline can be recognised and
	// ignored rather than ending the restart that replaced it.
	twitchLiveSince map[string]time.Time

	// eventSubOrder serialises notification handling per broadcaster so an
	// offline and the online that follows it (a restart) cannot be applied out
	// of order and end the wrong broadcast.
	eventSubOrder sync.Map // broadcasterID -> *sync.Mutex

	mu     sync.Mutex
	health map[string]*legHealth

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

// New constructs a detector from config. Returns (nil, nil) when live
// detection is disabled so callers can no-op without special-casing.
//
// Configuration problems DISABLE a leg and return a descriptive error rather
// than being fatal. The caller logs it and carries on: this is an observer,
// and nothing about a misconfigured shadow-mode detector should be able to
// stop the transcript server , the actual product , from starting.
func New(cfg config.LiveDetectConfig, channels []config.ChannelConfig, sink Sink, alerts *discord.Client) (*Detector, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	d := &Detector{
		cfg:             cfg,
		sink:            sink,
		alerts:          alerts,
		twitchTargets:   map[string]string{},
		twitchUserIDs:   map[string]string{},
		ytTargets:       map[string]string{},
		watch:           newWatchlist(),
		gov:             newQuotaGovernor(cfg.YouTube.DailyUnitBudget, 0),
		seen:            newSeenCache(twitchReplayMaxAge + time.Minute),
		webSubSeen:      newSeenCache(webSubDedupeTTL),
		twitchAbsent:    map[string]int{},
		twitchLiveID:    map[string]string{},
		twitchLiveSince: map[string]time.Time{},
		health:          map[string]*legHealth{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())

	var problems []string

	// Which platforms a channel is watched on is decided PER CHANNEL by which
	// identifiers it carries: twitchLogin opts it into Twitch, youtubeChannelId
	// into YouTube, both into both, neither into nothing. A YouTube-only
	// channel simply leaves twitchLogin empty.
	//
	// Both identifiers are format-checked here rather than trusted, because
	// every way of getting one wrong fails SILENTLY: Helix answers an unknown
	// login with an empty data array, and the Data API answers an unknown
	// channel id with an empty item list. Neither is an error, so a typo reads
	// as "this channel never goes live" for as long as it stands.
	for _, cc := range channels {
		if login := strings.ToLower(strings.TrimSpace(cc.TwitchLogin)); login != "" {
			if !validTwitchLogin(login) {
				problems = append(problems, fmt.Sprintf(
					"channel %q: twitchLogin %q is not a valid Twitch login; twitch detection disabled for this channel", cc.Name, cc.TwitchLogin))
				continue
			}
			d.twitchTargets[login] = cc.Name
		}
	}
	for _, cc := range channels {
		if id := strings.TrimSpace(cc.YouTubeChannelId); id != "" {
			if !ytChannelIDPattern.MatchString(id) {
				problems = append(problems, fmt.Sprintf(
					"channel %q: youtubeChannelId %q is not a UC-prefixed 24-character channel id; youtube detection disabled for this channel", cc.Name, cc.YouTubeChannelId))
				continue
			}
			d.ytTargets[id] = cc.Name
		}
	}

	if cfg.Twitch.Enabled {
		switch {
		case cfg.Twitch.ClientId == "" || cfg.Twitch.ClientSecret == "":
			problems = append(problems, "twitch: clientId and clientSecret are both required; twitch detection disabled")
			d.cfg.Twitch.Enabled = false
		case len(d.twitchTargets) == 0:
			problems = append(problems, "twitch: no channel has an explicit twitchLogin; twitch detection disabled")
			d.cfg.Twitch.Enabled = false
		default:
			d.twitch = NewTwitchClient(cfg.Twitch.ClientId, cfg.Twitch.ClientSecret)
			d.health[MechanismTwitchPoll] = &legHealth{}
		}
	}

	if d.cfg.Twitch.Enabled && cfg.Twitch.EventSub {
		n := len(cfg.Twitch.EventSubSecret)
		switch {
		case cfg.PublicBaseURL == "":
			problems = append(problems, "twitch eventsub: publicBaseUrl is required; falling back to polling only")
			d.cfg.Twitch.EventSub = false
		case n < EventSubSecretMinLen || n > EventSubSecretMaxLen:
			problems = append(problems, fmt.Sprintf("twitch eventsub: eventSubSecret must be %d-%d characters, got %d; falling back to polling only",
				EventSubSecretMinLen, EventSubSecretMaxLen, n))
			d.cfg.Twitch.EventSub = false
		default:
			d.health[MechanismTwitchEventSub] = &legHealth{}
			d.health[MechanismTwitchEventSubReconcile] = &legHealth{}
		}
	}

	if cfg.YouTube.Enabled {
		switch {
		case cfg.YouTube.ApiKey == "":
			problems = append(problems, "youtube: apiKey is required; youtube detection disabled")
			d.cfg.YouTube.Enabled = false
		case len(d.ytTargets) == 0:
			problems = append(problems, "youtube: no channel has a youtubeChannelId; youtube detection disabled")
			d.cfg.YouTube.Enabled = false
		default:
			d.youtube = NewYouTubeClient(cfg.YouTube.ApiKey)
			d.health[MechanismYouTubeState] = &legHealth{}
			d.health[MechanismYouTubeDiscover] = &legHealth{}
		}
	}

	if d.cfg.YouTube.Enabled && cfg.YouTube.WebSub {
		switch {
		case cfg.PublicBaseURL == "":
			problems = append(problems, "youtube websub: publicBaseUrl is required; falling back to polling discovery only")
			d.cfg.YouTube.WebSub = false
		case cfg.YouTube.WebSubSecret == "":
			problems = append(problems, "youtube websub: webSubSecret is required; falling back to polling discovery only")
			d.cfg.YouTube.WebSub = false
		default:
			d.websub = NewWebSubClient(cfg.YouTube.WebSubSecret)
			d.health[MechanismYouTubeWebSub] = &legHealth{}
			d.health[MechanismYouTubeWebSubRenew] = &legHealth{}
		}
	}

	if !d.cfg.Twitch.Enabled && !d.cfg.YouTube.Enabled {
		problems = append(problems, "no detection leg is usable; live detection is inert")
	}

	if len(problems) > 0 {
		return d, fmt.Errorf("live detection config problems: %s", strings.Join(problems, "; "))
	}
	return d, nil
}

// Enabled reports whether any leg will actually run.
func (d *Detector) Enabled() bool {
	return d != nil && (d.cfg.Twitch.Enabled || d.cfg.YouTube.Enabled)
}

// EventSubSecret exposes the callback signing secret to the HTTP handler.
func (d *Detector) EventSubSecret() string {
	if d == nil {
		return ""
	}
	return d.cfg.Twitch.EventSubSecret
}

// EventSubEnabled reports whether the Twitch callback should be served.
func (d *Detector) EventSubEnabled() bool {
	return d != nil && d.cfg.Twitch.Enabled && d.cfg.Twitch.EventSub
}

// WebSubEnabled reports whether the YouTube callback should be served.
func (d *Detector) WebSubEnabled() bool {
	return d != nil && d.cfg.YouTube.Enabled && d.cfg.YouTube.WebSub
}

func (d *Detector) twitchCallbackURL() string {
	return strings.TrimRight(d.cfg.PublicBaseURL, "/") + TwitchEventSubPath
}

func (d *Detector) webSubCallbackURL() string {
	return strings.TrimRight(d.cfg.PublicBaseURL, "/") + YouTubeWebSubPath
}

// Start launches every enabled leg. Safe to call once; later calls are no-ops.
func (d *Detector) Start() error {
	if d == nil || !d.Enabled() {
		return nil
	}
	d.startOnce.Do(func() {
		d.reseedFromLedger()
		if d.cfg.Twitch.Enabled {
			d.spawn(d.runTwitchPoll)
			if d.cfg.Twitch.EventSub {
				d.spawn(d.runEventSubReconcile)
			}
		}
		if d.cfg.YouTube.Enabled {
			d.spawn(d.runYouTubeState)
			d.spawn(d.runYouTubeDiscovery)
			if d.cfg.YouTube.WebSub {
				d.spawn(d.runWebSubRenew)
			}
			if d.cfg.YouTube.SearchAudit {
				d.spawn(d.runSearchAudit)
			}
		}
		d.spawn(d.runHealthWatch)

		slog.Info("live detection started",
			"func", "Detector.Start",
			"twitch", d.cfg.Twitch.Enabled,
			"twitch_eventsub", d.cfg.Twitch.EventSub,
			"youtube", d.cfg.YouTube.Enabled,
			"youtube_websub", d.cfg.YouTube.WebSub,
			"watching", d.watchSummary(),
		)
	})
	return nil
}

// Close stops every leg and waits for in-flight work. Called from main before
// the store is closed, so no poll is mid-write when the database goes away.
func (d *Detector) Close() error {
	if d == nil {
		return nil
	}
	d.stopOnce.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
	})
	d.wg.Wait()
	return nil
}

// watchSummary renders which platforms each channel is watched on, e.g.
// "doki=twitch+youtube mint=youtube".
//
// Printed at startup because a channel silently watched on the wrong platform
// (or on none) is otherwise indistinguishable from one that simply never went
// live, and this is the only place that mapping is visible.
func (d *Detector) watchSummary() string {
	platforms := map[string][]string{}
	for _, key := range d.twitchTargets {
		platforms[key] = append(platforms[key], "twitch")
	}
	for _, key := range d.ytTargets {
		platforms[key] = append(platforms[key], "youtube")
	}

	keys := make([]string, 0, len(platforms))
	for key := range platforms {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		slices.Sort(platforms[key])
		parts = append(parts, key+"="+strings.Join(platforms[key], "+"))
	}
	if len(parts) == 0 {
		return "(nothing)"
	}
	return strings.Join(parts, " ")
}

// spawn runs fn as a tracked goroutine, recovering panics so one wedged leg
// cannot take the server down with it.
func (d *Detector) spawn(fn func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered from panic in live detection leg", "func", "Detector.spawn", "panic", r)
			}
		}()
		fn()
	}()
}

// sleep waits for d, returning false if the detector is shutting down.
func (d *Detector) sleep(dur time.Duration) bool {
	if dur <= 0 {
		dur = time.Second
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-d.ctx.Done():
		return false
	}
}

// sleepOrWake waits for dur, but returns early when something is seeded that
// wants polling sooner. Returns false if the detector is shutting down.
func (d *Detector) sleepOrWake(dur time.Duration, wake <-chan struct{}) bool {
	if dur <= 0 {
		dur = time.Second
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-wake:
		return true
	case <-d.ctx.Done():
		return false
	}
}

// pollCtx bounds one cycle so a hung upstream can neither stall the leg
// forever nor delay shutdown. Docker SIGKILLs ten seconds after SIGTERM, so
// nothing here may block longer than that.
func (d *Detector) pollCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	return context.WithTimeout(d.ctx, timeout)
}

// recordSuccess marks a leg healthy and announces recovery once.
func (d *Detector) recordSuccess(mechanism string) {
	metrics.LiveDetectPolls.WithLabelValues(mechanism, "ok").Inc()
	metrics.LiveDetectLastSuccess.WithLabelValues(mechanism).SetToCurrentTime()

	d.mu.Lock()
	h := d.legHealthLocked(mechanism)
	h.lastSuccess = time.Now()
	h.failures = 0
	recovered := h.alerted
	h.alerted = false
	d.mu.Unlock()

	if recovered {
		slog.Info("live detection leg recovered", "func", "Detector.recordSuccess", "mechanism", mechanism)
		d.alerts.NotifyLiveDetectRecovered(mechanism)
	}
}

// recordFailure marks a leg failed and alerts once per outage.
func (d *Detector) recordFailure(mechanism string, err error) {
	metrics.LiveDetectPolls.WithLabelValues(mechanism, "error").Inc()

	d.mu.Lock()
	h := d.legHealthLocked(mechanism)
	h.failures++
	h.lastErr = err.Error()
	h.lastErrAt = time.Now()
	firstAlert := h.failures >= failuresBeforeAlert && !h.alerted
	if firstAlert {
		h.alerted = true
	}
	failures := h.failures
	d.mu.Unlock()

	slog.Error("live detection leg failed", "func", "Detector.recordFailure",
		"mechanism", mechanism, "failures", failures, "err", err)
	if firstAlert {
		d.alerts.NotifyLiveDetectDown(mechanism, err, failures)
	}
}

// legHealthLocked returns (creating if needed) a leg's health record. Callers
// must hold d.mu.
func (d *Detector) legHealthLocked(mechanism string) *legHealth {
	h, ok := d.health[mechanism]
	if !ok {
		h = &legHealth{}
		d.health[mechanism] = h
	}
	return h
}

// LegStatus is one detection leg's health, for the admin page.
type LegStatus struct {
	Mechanism   string `json:"mechanism"`
	State       string `json:"state"` // ok | degraded | down | idle
	Detail      string `json:"detail,omitempty"`
	LastSuccess int64  `json:"lastSuccess,omitempty"`
	Failures    int    `json:"failures,omitempty"`
}

// Status is a point-in-time snapshot of live detection for the admin page.
type Status struct {
	Enabled       bool          `json:"enabled"`
	ShadowMode    bool          `json:"shadowMode"`
	Legs          []LegStatus   `json:"legs"`
	WatchlistSize int           `json:"watchlistSize"`
	Quota         QuotaSnapshot `json:"quota"`
}

// Status reports detection health. Safe on a nil receiver.
func (d *Detector) Status() Status {
	if d == nil || !d.Enabled() {
		return Status{Enabled: false, ShadowMode: true}
	}

	now := time.Now()

	d.mu.Lock()
	legs := make([]LegStatus, 0, len(d.health))
	for mech, h := range d.health {
		ls := LegStatus{Mechanism: mech, Failures: h.failures}
		if !h.lastSuccess.IsZero() {
			ls.LastSuccess = h.lastSuccess.Unix()
		}
		stale := d.staleAfter(mech)
		switch {
		case h.failures >= failuresBeforeAlert:
			ls.State = "down"
			ls.Detail = h.lastErr
		case h.lastSuccess.IsZero():
			// A push leg has no polls to succeed, so "no success yet" is its
			// normal resting state rather than a fault.
			ls.State = "idle"
			ls.Detail = "no successful cycle yet"
		case stale > 0 && now.Sub(h.lastSuccess) > stale:
			ls.State = "degraded"
			ls.Detail = fmt.Sprintf("no successful cycle for %s", now.Sub(h.lastSuccess).Round(time.Second))
		default:
			ls.State = "ok"
		}
		legs = append(legs, ls)
	}
	d.mu.Unlock()

	return Status{
		Enabled:       true,
		ShadowMode:    true,
		Legs:          legs,
		WatchlistSize: d.watch.Size(),
		Quota:         d.gov.Snapshot(now),
	}
}

// expectedCadence is how often a leg is supposed to produce a successful
// cycle. A single global staleness threshold is wrong: the search audit runs
// every 3 hours and WebSub renewal every 12, so a 15-minute rule would report
// both as permanently degraded and alert on every cycle. Push legs return zero
// , they are legitimately silent when nobody is streaming, and silence there is
// not evidence of a fault.
func (d *Detector) expectedCadence(mechanism string) time.Duration {
	switch mechanism {
	case MechanismTwitchPoll:
		return d.twitchPollInterval()
	case MechanismYouTubeState:
		return ytIntervalCold
	case MechanismYouTubeDiscover:
		return d.ytDiscoveryInterval()
	case MechanismYouTubeAudit:
		return searchAuditInterval
	case MechanismTwitchEventSubReconcile:
		return eventSubReconcileInterval
	case MechanismYouTubeWebSubRenew:
		return webSubRenewInterval
	default:
		// Push legs (delivery, not polling): never stale.
		return 0
	}
}

// staleAfter is how long a leg may go quiet before it counts as degraded.
// Zero means "never stale".
func (d *Detector) staleAfter(mechanism string) time.Duration {
	cadence := d.expectedCadence(mechanism)
	if cadence == 0 {
		return 0
	}
	floor := time.Duration(d.staleAlertMinutes()) * time.Minute
	// Three missed cycles, or the configured floor, whichever is longer.
	return max(3*cadence, floor)
}

func (d *Detector) staleAlertMinutes() int {
	if d.cfg.StaleAlertMinutes > 0 {
		return d.cfg.StaleAlertMinutes
	}
	return defaultStaleAlertMinutes
}

// runHealthWatch alerts when a polling leg goes quiet without erroring ,
// the failure mode where a loop is alive but producing nothing.
func (d *Detector) runHealthWatch() {
	for {
		if !d.sleep(5 * time.Minute) {
			return
		}
		now := time.Now()

		type quietLeg struct {
			mechanism string
			after     time.Duration
		}
		d.mu.Lock()
		var quiet []quietLeg
		for mech, h := range d.health {
			stale := d.staleAfter(mech)
			// Zero means a push leg, which is legitimately silent whenever
			// nobody is streaming.
			if stale == 0 {
				continue
			}
			if !h.lastSuccess.IsZero() && now.Sub(h.lastSuccess) > stale && !h.alerted {
				h.alerted = true
				quiet = append(quiet, quietLeg{mechanism: mech, after: stale})
			}
		}
		d.mu.Unlock()

		for _, q := range quiet {
			slog.Error("live detection leg has gone quiet", "func", "Detector.runHealthWatch",
				"mechanism", q.mechanism, "stale_for", q.after.String())
			d.alerts.NotifyLiveDetectDown(q.mechanism, fmt.Errorf("no successful cycle for %s", q.after), 0)
		}
	}
}

// observe hands a confirmed live broadcast to the sink, which dedupes it.
func (d *Detector) observe(ctx context.Context, b Broadcast, mechanism string) {
	if err := d.sink.ObserveLive(ctx, b, mechanism); err != nil {
		slog.Error("failed to record live detection", "func", "Detector.observe",
			"key", b.ChannelKey, "platform", b.Platform, "broadcastId", b.ID,
			"mechanism", mechanism, "err", err)
	}
}

// errNoResolvableLogins reports that none of the configured Twitch logins
// exist, so EventSub can never have a subscription to deliver through.
var errNoResolvableLogins = errors.New("no configured twitch login could be resolved to a broadcaster id")

// reseedFromLedger puts broadcasts the ledger still believes live back on the
// watchlist.
//
// Without this a restart mid-broadcast loses track of a stream it already
// detected: the watchlist is in-memory, so the id would only come back on the
// next discovery pass, and until then nothing would notice the broadcast
// ending , which in turn suppresses the accelerated re-poll that catches a
// restart. Re-seeding cannot cause a duplicate notification, because the
// ledger claim for those broadcasts is already taken.
func (d *Detector) reseedFromLedger() {
	ctx, cancel := d.pollCtx(10 * time.Second)
	defer cancel()

	active, err := d.sink.ActiveBroadcasts(ctx)
	if err != nil {
		slog.Error("failed to re-seed live detection from the ledger", "func", "Detector.reseedFromLedger", "err", err)
		return
	}
	now := time.Now()
	yt, tw := 0, 0
	for _, b := range active {
		if b.ID == "" {
			continue
		}
		switch b.Platform {
		case PlatformYouTube:
			// SeedClaimed, not Seed: these were live by definition, so if one
			// ended while the process was down the first poll must still
			// report the end.
			d.watch.SeedClaimed(b.ID, b.ChannelKey, now)
			yt++
		case PlatformTwitch:
			// Twitch has no watchlist — the poll leg re-derives state from
			// Helix every cycle — but it DOES need to know which broadcast it
			// was tracking. Without this the absence path bails on an empty
			// twitchLiveID, so a broadcast that ended while the process was
			// down keeps ended_at = 0 forever: it is handed back by
			// GetLiveDetections on every subsequent restart and can never be
			// pruned, since pruning only deletes ended rows.
			login := d.twitchLoginFor(b.ChannelKey)
			if login == "" {
				continue
			}
			d.trackTwitchLive(login, b.ID, now)
			tw++
		}
	}
	if yt+tw > 0 {
		slog.Info("re-seeded live detection from the ledger",
			"func", "Detector.reseedFromLedger", "youtube", yt, "twitch", tw)
	}
}

// twitchLoginFor reverses the login -> channel-key map. The map is small and
// this runs once at startup, so a scan beats keeping a second index in sync.
func (d *Detector) twitchLoginFor(channelKey string) string {
	for login, key := range d.twitchTargets {
		if key == channelKey {
			return login
		}
	}
	return ""
}
