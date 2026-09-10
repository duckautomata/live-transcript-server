package livedetect

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"live-transcript-server/internal/metrics"
)

// twitchAbsencesBeforeEnd is how many consecutive polls must omit a
// previously-live login before we believe the stream ended.
//
// Helix responses are edge-cached per server, so a successful 200 can simply
// omit a channel that is genuinely still live. That looks identical to
// "offline" and is not caught by the "never treat an error as offline" rule,
// because it is not an error. Requiring three consecutive absences across more
// than a poll interval each is what stops a cache miss from ending a broadcast
// mid-stream and corrupting the ledger.
const twitchAbsencesBeforeEnd = 3

// eventSubReconcileInterval is how often subscriptions are reconciled against
// Twitch's view of them.
const eventSubReconcileInterval = 15 * time.Minute

// twitchPollInterval is the effective Helix cadence, clamped at the floor.
func (d *Detector) twitchPollInterval() time.Duration {
	s := d.cfg.Twitch.PollSeconds
	if s < minTwitchPollSeconds {
		if s > 0 {
			slog.Warn("twitch pollSeconds below the floor; clamping",
				"func", "Detector.twitchPollInterval", "configured", s, "using", minTwitchPollSeconds)
		}
		s = defaultTwitchPollSeconds
	}
	return time.Duration(s) * time.Second
}

// runTwitchPoll is the level-triggered Twitch leg.
//
// It is the SAFETY NET, not the latency path. Helix caches its responses, so
// this detects on the order of a minute however fast it runs - EventSub is the
// only seconds-scale Twitch mechanism. What polling provides is the guarantee:
// it re-derives ground truth every cycle, so it recovers on its own from a
// missed webhook, a revoked subscription, a Cloudflare block, or a restart
// during a live stream, none of which an edge-triggered path can recover from.
func (d *Detector) runTwitchPoll() {
	// A short initial delay lets the process finish booting before the first
	// outbound call, and staggers the legs.
	if !d.sleep(5 * time.Second) {
		return
	}
	for {
		d.twitchPollOnce()
		if !d.sleep(d.twitchPollInterval()) {
			return
		}
	}
}

func (d *Detector) twitchPollOnce() {
	logins := make([]string, 0, len(d.twitchTargets))
	for login := range d.twitchTargets {
		logins = append(logins, login)
	}
	slices.Sort(logins) // deterministic request shape, easier to read in logs
	if len(logins) == 0 {
		return
	}

	ctx, cancel := d.pollCtx(d.twitchPollInterval())
	defer cancel()

	streams, err := d.twitch.GetStreams(ctx, logins)
	if err != nil {
		// An error means "I don't know", never "offline": absence counters are
		// deliberately left untouched so a transient outage cannot accumulate
		// into a false stream end.
		d.recordFailure(MechanismTwitchPoll, err)
		return
	}
	d.recordSuccess(MechanismTwitchPoll)

	present := make(map[string]TwitchStream, len(streams))
	for _, s := range streams {
		login := strings.ToLower(s.UserLogin)
		key, ok := d.twitchTargets[login]
		if !ok {
			continue
		}
		// Anything that is not a first-class live broadcast (a rerun or a
		// playlist) is dropped BEFORE the sink. There is no claim-without-
		// notify path, so filtering has to happen here or not at all.
		if s.Type != "" && s.Type != "live" {
			slog.Debug("skipping non-live twitch stream type", "func", "Detector.twitchPollOnce",
				"key", key, "login", login, "type", s.Type)
			continue
		}
		present[login] = s

		startedAt, _ := time.Parse(time.RFC3339, s.StartedAt)
		d.observe(ctx, Broadcast{
			Platform:   PlatformTwitch,
			ChannelKey: key,
			ID:         s.ID,
			URL:        TwitchChannelURL(login),
			Title:      s.Title,
			StartedAt:  startedAt,
		}, MechanismTwitchPoll)
	}

	d.reconcileTwitchAbsences(ctx, logins, present)
}

// reconcileTwitchAbsences turns repeated absence from a successful Helix
// response into an end, once the absence is credible.
func (d *Detector) reconcileTwitchAbsences(ctx context.Context, logins []string, present map[string]TwitchStream) {
	for _, e := range d.applyTwitchPresence(logins, present, time.Now()) {
		slog.Info("twitch broadcast ended", "func", "Detector.reconcileTwitchAbsences",
			"login", e.login, "broadcastId", e.id)
		if err := d.sink.ObserveEnded(ctx, PlatformTwitch, e.id); err != nil {
			slog.Error("failed to mark twitch broadcast ended", "func", "Detector.reconcileTwitchAbsences",
				"broadcastId", e.id, "err", err)
		}
	}
}

// twitchEnding is one broadcast the poll leg concluded has finished.
type twitchEnding struct{ login, id string }

// applyTwitchPresence folds one Helix response into the tracked live state and
// returns the broadcasts that ended.
//
// The lock covers only the state update, never the sink calls: reporting an end
// takes a database write, and holding this mutex across it would block the
// EventSub delivery path behind it. The unlock is deferred so no panic in here
// can leave the mutex held - it is shared with the webhook handlers, and
// leaking it once would wedge both legs permanently and hang shutdown.
func (d *Detector) applyTwitchPresence(logins []string, present map[string]TwitchStream, now time.Time) []twitchEnding {
	var ended []twitchEnding

	d.twitchAbsentMu.Lock()
	defer d.twitchAbsentMu.Unlock()

	for _, login := range logins {
		if s, ok := present[login]; ok {
			d.twitchAbsent[login] = 0
			prev := d.twitchLiveID[login]
			// A CHANGED stream id on a present channel is a restart: the
			// broadcaster's ingest dropped past the reconnect grace window and
			// Twitch minted a new id. Overwriting silently would leave the old
			// broadcast live in the ledger forever. This only fires for an id
			// we are currently tracking, so a stale Helix page carrying an id
			// we already ended cannot resurrect it.
			if prev != "" && prev != s.ID {
				ended = append(ended, twitchEnding{login: login, id: prev})
			}
			d.twitchLiveID[login] = s.ID
			d.twitchLiveSince[login] = now
			continue
		}

		liveID := d.twitchLiveID[login]
		if liveID == "" {
			continue // nothing was live; absence is unremarkable
		}
		d.twitchAbsent[login]++
		if d.twitchAbsent[login] >= twitchAbsencesBeforeEnd {
			ended = append(ended, twitchEnding{login: login, id: liveID})
			delete(d.twitchLiveID, login)
			delete(d.twitchLiveSince, login)
			d.twitchAbsent[login] = 0
		}
	}
	return ended
}

// TwitchEnvelope is the JSON body of every EventSub callback.
type TwitchEnvelope struct {
	Challenge    string               `json:"challenge"`
	Subscription EventSubSubscription `json:"subscription"`
	Event        json.RawMessage      `json:"event"`
}

// TwitchStreamOnlineEvent is the stream.online payload.
type TwitchStreamOnlineEvent struct {
	ID                   string `json:"id"`
	BroadcasterUserID    string `json:"broadcaster_user_id"`
	BroadcasterUserLogin string `json:"broadcaster_user_login"`
	BroadcasterUserName  string `json:"broadcaster_user_name"`
	Type                 string `json:"type"`
	StartedAt            string `json:"started_at"`
}

// TwitchStreamOfflineEvent is the stream.offline payload.
type TwitchStreamOfflineEvent struct {
	BroadcasterUserID    string `json:"broadcaster_user_id"`
	BroadcasterUserLogin string `json:"broadcaster_user_login"`
	BroadcasterUserName  string `json:"broadcaster_user_name"`
}

// SeenEventSubMessage reports whether an EventSub message id was already
// handled, recording it if not.
//
// Callers must apply this ONLY to notifications. A redelivered verification
// handshake answered with an empty body puts the subscription into
// webhook_callback_verification_failed, which Twitch does not retry out of.
func (d *Detector) SeenEventSubMessage(msgID string, now time.Time) bool {
	if d == nil {
		return false
	}
	return d.seen.SeenOrRecord(msgID, now)
}

// HandleEventSubNotification processes one verified stream.online or
// stream.offline. It runs AFTER the HTTP handler has already answered, so
// nothing here affects the response Twitch sees.
//
// Work for a single broadcaster is serialised: an offline and the online that
// follows it (a restart) would otherwise be free to execute in either order,
// and an offline applied after the online would end the NEW broadcast instead
// of the old one - precisely the case restart detection exists to handle.
func (d *Detector) HandleEventSubNotification(ctx context.Context, env TwitchEnvelope, sentAt time.Time) {
	if d == nil {
		return
	}

	broadcasterID := env.Subscription.Condition["broadcaster_user_id"]
	lk, _ := d.eventSubOrder.LoadOrStore(broadcasterID, &sync.Mutex{})
	mu := lk.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	switch env.Subscription.Type {
	case EventSubTypeStreamOnline:
		var ev TwitchStreamOnlineEvent
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			slog.Error("undecodable stream.online event", "func", "Detector.HandleEventSubNotification", "err", err)
			return
		}
		d.handleStreamOnline(ctx, ev)

	case EventSubTypeStreamOffline:
		var ev TwitchStreamOfflineEvent
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			slog.Error("undecodable stream.offline event", "func", "Detector.HandleEventSubNotification", "err", err)
			return
		}
		d.handleStreamOffline(ctx, ev, sentAt)

	default:
		slog.Warn("unexpected eventsub subscription type", "func", "Detector.HandleEventSubNotification",
			"type", env.Subscription.Type)
	}
}

func (d *Detector) handleStreamOnline(ctx context.Context, ev TwitchStreamOnlineEvent) {
	login := strings.ToLower(ev.BroadcasterUserLogin)
	key, ok := d.twitchTargets[login]
	if !ok {
		// A leftover subscription, a config change, or another deployment
		// sharing the client id. Drop it before the sink, which would reject
		// an unknown channel as an error.
		slog.Warn("eventsub notification for an unconfigured channel",
			"func", "Detector.handleStreamOnline", "login", login, "broadcasterId", ev.BroadcasterUserID)
		return
	}
	// Reruns and playlists are not live broadcasts; drop before the sink.
	if ev.Type != "" && ev.Type != "live" {
		slog.Info("ignoring non-live stream.online", "func", "Detector.handleStreamOnline",
			"key", key, "type", ev.Type)
		return
	}

	d.recordSuccess(MechanismTwitchEventSub)

	startedAt, _ := time.Parse(time.RFC3339, ev.StartedAt)
	// The event carries no title. Enriching it here would mean a Helix call on
	// the latency path, which is exactly what EventSub exists to avoid - the
	// notification renders "(no title reported)" instead, and that is also a
	// visible marker that EventSub won the race.
	d.observe(ctx, Broadcast{
		Platform:   PlatformTwitch,
		ChannelKey: key,
		ID:         ev.ID,
		URL:        TwitchChannelURL(login),
		StartedAt:  startedAt,
	}, MechanismTwitchEventSub)

	d.trackTwitchLive(login, ev.ID, time.Now())
}

// trackTwitchLive records the broadcast currently believed live for a login.
//
// The unlock is deferred rather than written out: this mutex is taken on the
// EventSub delivery path AND the poll path, so leaking it once - to a panic
// between Lock and Unlock - would wedge the poll leg permanently and hang
// shutdown, since the blocked goroutine holds the app WaitGroup.
func (d *Detector) trackTwitchLive(login, broadcastID string, now time.Time) {
	d.twitchAbsentMu.Lock()
	defer d.twitchAbsentMu.Unlock()
	d.twitchLiveID[login] = broadcastID
	d.twitchLiveSince[login] = now
	d.twitchAbsent[login] = 0
}

// clearTwitchLive forgets the tracked broadcast for a login and returns what it
// was, or "" when the offline should be ignored as out-of-order.
func (d *Detector) clearTwitchLive(login string, sentAt time.Time) (id string, ignored bool) {
	d.twitchAbsentMu.Lock()
	defer d.twitchAbsentMu.Unlock()

	id = d.twitchLiveID[login]
	trackedSince := d.twitchLiveSince[login]
	// The per-broadcaster mutex gives mutual exclusion, not arrival ordering:
	// an offline and the online that followed it can still be handled in
	// either order. Comparing Twitch's own send timestamp against when we
	// started tracking the current broadcast is what stops a late-arriving
	// offline from ending the restart that superseded it.
	if id != "" && !trackedSince.IsZero() && sentAt.Before(trackedSince) {
		return "", true
	}
	delete(d.twitchLiveID, login)
	delete(d.twitchLiveSince, login)
	d.twitchAbsent[login] = 0
	return id, false
}

func (d *Detector) handleStreamOffline(ctx context.Context, ev TwitchStreamOfflineEvent, sentAt time.Time) {
	login := strings.ToLower(ev.BroadcasterUserLogin)
	if _, ok := d.twitchTargets[login]; !ok {
		return
	}

	d.recordSuccess(MechanismTwitchEventSub)

	id, ignored := d.clearTwitchLive(login, sentAt)
	if ignored {
		slog.Info("ignoring a stream.offline that predates the broadcast we are tracking",
			"func", "Detector.handleStreamOffline", "login", login)
		return
	}

	if id == "" {
		// We never saw this broadcast start - a restart during downtime, or a
		// stream that began before the process did. Nothing to end.
		slog.Info("stream.offline for a broadcast we never saw start",
			"func", "Detector.handleStreamOffline", "login", login)
		return
	}
	if err := d.sink.ObserveEnded(ctx, PlatformTwitch, id); err != nil {
		slog.Error("failed to mark twitch broadcast ended", "func", "Detector.handleStreamOffline",
			"broadcastId", id, "err", err)
	}
}

// HandleEventSubRevocation reacts to Twitch dropping a subscription. The
// reconciler recreates it on its next pass; this exists so the operator hears
// about it rather than discovering it through silence.
func (d *Detector) HandleEventSubRevocation(sub EventSubSubscription) {
	if d == nil {
		return
	}
	slog.Error("twitch revoked an eventsub subscription", "func", "Detector.HandleEventSubRevocation",
		"id", sub.ID, "type", sub.Type, "status", sub.Status,
		"broadcasterId", sub.Condition["broadcaster_user_id"])
	d.alerts.NotifyLiveDetectRevoked(sub.Type, sub.Status, sub.Condition["broadcaster_user_id"])
}

// runEventSubReconcile keeps Twitch's subscriptions matching what we want.
func (d *Detector) runEventSubReconcile() {
	if !d.sleep(10 * time.Second) {
		return
	}
	for {
		d.reconcileEventSub()
		if !d.sleep(eventSubReconcileInterval) {
			return
		}
	}
}

// reconcileEventSub resolves broadcaster ids, then creates missing
// subscriptions and removes stale ones.
//
// Deletion is scoped by CALLBACK HOST, not merely by "we did not want this".
// The subscription list is per client id, so a dev box or a twitch-cli session
// sharing the credentials appears here too - deleting those would have the two
// deployments tear down each other's subscriptions every reconcile pass, and
// the resulting silence is exactly the failure this design exists to prevent.
func (d *Detector) reconcileEventSub() {
	ctx, cancel := d.pollCtx(30 * time.Second)
	defer cancel()

	if len(d.twitchUserIDs) == 0 {
		if err := d.resolveTwitchUserIDs(ctx); err != nil {
			d.recordFailure(MechanismTwitchEventSub, err)
			return
		}
	}

	subs, err := d.twitch.ListEventSubSubscriptions(ctx)
	if err != nil {
		d.recordFailure(MechanismTwitchEventSub, err)
		return
	}

	callback := d.twitchCallbackURL()
	ourHost := callbackHost(callback)

	// What we want: online+offline for every resolved broadcaster.
	want := map[string]bool{}
	for id := range d.twitchUserIDs {
		want[EventSubTypeStreamOnline+"|"+id] = true
		want[EventSubTypeStreamOffline+"|"+id] = true
	}

	have := map[string]bool{}
	for _, s := range subs {
		if callbackHost(s.Transport.Callback) != ourHost {
			continue // another deployment's subscription; never touch it
		}
		key := s.Type + "|" + s.Condition["broadcaster_user_id"]
		switch {
		// ALLOWLIST, not a denylist. Twitch has many non-delivering statuses
		// (verification failed, notification_failures_exceeded, user removed,
		// authorization revoked, version removed), and treating anything not
		// explicitly known-bad as healthy meant a webhook Cloudflare was
		// blocking would be adopted as working and never repaired - the exact
		// silent failure this whole design is supposed to make impossible.
		case !want[key], s.Status != EventSubStatusEnabled && !pendingAndYoung(s, time.Now()):
			// Stale or unusable and pointed at our host: safe to remove.
			if err := d.twitch.DeleteEventSubSubscription(ctx, s.ID); err != nil {
				slog.Warn("failed to delete stale eventsub subscription",
					"func", "Detector.reconcileEventSub", "id", s.ID, "err", err)
				continue
			}
			slog.Info("deleted stale eventsub subscription", "func", "Detector.reconcileEventSub",
				"id", s.ID, "type", s.Type, "status", s.Status)
		default:
			have[key] = true
		}
	}

	created, failed := 0, 0
	for key := range want {
		if have[key] {
			continue
		}
		typ, broadcasterID, _ := strings.Cut(key, "|")
		sub, err := d.twitch.CreateEventSubSubscription(ctx, typ, broadcasterID, callback, d.cfg.Twitch.EventSubSecret)
		if err != nil {
			failed++
			slog.Error("failed to create eventsub subscription", "func", "Detector.reconcileEventSub",
				"type", typ, "broadcasterId", broadcasterID, "err", err)
			continue
		}
		created++
		slog.Info("created eventsub subscription", "func", "Detector.reconcileEventSub",
			"id", sub.ID, "type", sub.Type, "status", sub.Status, "broadcasterId", broadcasterID)
	}

	// Health for the RECONCILER, not for delivery. Recording success against
	// MechanismTwitchEventSub here would make the leg report healthy purely
	// because Twitch answered an API call - while zero notifications were
	// arriving. Delivery health is driven only by real deliveries.
	if failed > 0 {
		d.recordFailure(MechanismTwitchEventSubReconcile,
			fmt.Errorf("%d of %d wanted subscriptions could not be created", failed, len(want)))
	} else {
		d.recordSuccess(MechanismTwitchEventSubReconcile)
	}

	// A subscription that stays un-enabled across consecutive passes means
	// Twitch cannot deliver to the callback - most often something in front of
	// the server challenging its Go-http-client user agent. Latched so it
	// alerts once per outage rather than every reconcile.
	missing := len(want) - len(have) - created
	metrics.LiveDetectEventSubSubs.WithLabelValues("wanted").Set(float64(len(want)))
	metrics.LiveDetectEventSubSubs.WithLabelValues("enabled").Set(float64(len(have) + created))
	d.noteEventSubGap(missing > 0 || failed > 0, missing, failed)

	if created > 0 {
		slog.Info("eventsub reconciliation complete", "func", "Detector.reconcileEventSub",
			"created", created, "failed", failed, "wanted", len(want))
	}
}

// pendingAndYoung reports whether a subscription is legitimately mid-handshake
// rather than stuck. A sub created seconds ago in this same pass has not had
// time to verify; one still pending a full reconcile interval later never will.
func pendingAndYoung(s EventSubSubscription, now time.Time) bool {
	if s.Status != EventSubStatusVerificationPending {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, s.CreatedAt)
	if err != nil {
		// An unreadable timestamp is treated as NOT young, so the
		// subscription is recreated rather than trusted indefinitely.
		return false
	}
	return now.Sub(created) < 10*time.Minute
}

// noteEventSubGap latches an alert for subscriptions that will not come up.
func (d *Detector) noteEventSubGap(bad bool, missing, failed int) {
	d.mu.Lock()
	h := d.legHealthLocked(MechanismTwitchEventSub)
	var alert bool
	if bad {
		h.failures++
		// Two consecutive bad passes: one can be a handshake still in flight.
		alert = h.failures >= 2 && !h.alerted
		if alert {
			h.alerted = true
		}
	} else {
		h.failures = 0
	}
	d.mu.Unlock()

	if alert {
		slog.Error("twitch eventsub subscriptions are not reaching the enabled state",
			"func", "Detector.noteEventSubGap", "missing", missing, "failed", failed)
		d.alerts.NotifyLiveDetectDown(MechanismTwitchEventSub,
			fmt.Errorf("%d subscription(s) are not enabled; Twitch cannot deliver to the callback (check for a bot challenge on Go-http-client)", missing+failed), 0)
	}
}

// resolveTwitchUserIDs maps configured logins to numeric broadcaster ids.
// Logins Twitch does not know are simply absent from the response, so each
// missing one is reported rather than silently skipped.
func (d *Detector) resolveTwitchUserIDs(ctx context.Context) error {
	logins := make([]string, 0, len(d.twitchTargets))
	for login := range d.twitchTargets {
		logins = append(logins, login)
	}
	slices.Sort(logins)

	users, err := d.twitch.GetUsers(ctx, logins)
	if err != nil {
		return err
	}
	for _, login := range logins {
		u, ok := users[login]
		if !ok {
			slog.Error("twitch does not know this login; no eventsub subscription will exist for it",
				"func", "Detector.resolveTwitchUserIDs", "login", login, "key", d.twitchTargets[login])
			continue
		}
		d.twitchUserIDs[u.ID] = d.twitchTargets[login]
	}
	if len(d.twitchUserIDs) == 0 {
		return errNoResolvableLogins
	}
	return nil
}

// callbackHost extracts the host of a callback URL for ownership comparison.
func callbackHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}
