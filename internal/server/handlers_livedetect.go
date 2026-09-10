package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/metrics"
)

// maxWebhookBody bounds an inbound push. Real payloads are a few kilobytes;
// this only stops a hostile caller from making us buffer something large,
// since the body must be read whole before it can be authenticated.
const maxWebhookBody = 1 << 20 // 1 MiB

// webhookWork runs post-acknowledgement work for an inbound push.
//
// The HTTP response is always sent BEFORE this runs. Twitch and the WebSub hub
// both count a slow or failed response as a delivery failure, and enough of
// them revoke the subscription , so no database write, Discord post, or
// outbound call may sit between receiving a push and acknowledging it.
//
// Work is tracked so shutdown waits for it rather than closing the database
// underneath a write. app.goBackground takes the Add under the same lock Close
// uses to stop accepting work, which is what makes that safe: a sync.WaitGroup
// must not take a positive delta from zero once Wait has begun. A bare context
// check would not close that window, because the context can be cancelled
// between the check and the Add.
func (app *App) webhookWork(fn func(ctx context.Context)) {
	started := app.goBackground(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered from panic in webhook worker", "func", "App.webhookWork", "panic", r)
			}
		}()
		// Detached from app.ctx: the push has already been acknowledged, so
		// this work must finish rather than be cancelled mid-write. Close
		// waits for it, bounded by the timeout here.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fn(ctx)
	})
	if !started {
		slog.Warn("dropped webhook work during shutdown", "func", "App.webhookWork")
	}
}

// twitchEventSubHandler receives Twitch EventSub callbacks.
//
// This is a PUBLIC route with no API key: the caller is Twitch, which cannot
// send one. It authenticates every request by HMAC instead.
func (app *App) twitchEventSubHandler(w http.ResponseWriter, r *http.Request) {
	// Marker for an external reachability probe. If a probe response lacks
	// this header, something in front of the server (Cloudflare, Caddy)
	// answered instead of Go , which is the silent failure mode for EventSub.
	w.Header().Set("X-Eventsub-Handler", "1")

	if !app.LiveDetect.EventSubEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	// RAW BYTES FIRST. The HMAC covers the exact bytes on the wire: decoding
	// and re-encoding JSON changes key order and whitespace, and every
	// signature then fails. Nothing above this line may touch r.Body.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "malformed").Inc()
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	msgID := r.Header.Get(livedetect.HeaderTwitchMessageID)
	msgTS := r.Header.Get(livedetect.HeaderTwitchMessageTimestamp)
	msgSig := r.Header.Get(livedetect.HeaderTwitchMessageSignature)
	msgType := r.Header.Get(livedetect.HeaderTwitchMessageType)
	if msgID == "" || msgTS == "" || msgSig == "" {
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "missing-headers").Inc()
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Signature before freshness. A 403 counts as a delivery failure in
	// Twitch's accounting, so it is reserved for genuinely unauthenticated
	// requests , never for a correctly signed message we merely dislike.
	if !livedetect.VerifyTwitchSignature(app.LiveDetect.EventSubSecret(), msgID, msgTS, raw, msgSig) {
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "bad-signature").Inc()
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// A stale but correctly signed message is acknowledged, not rejected. If
	// the VPS clock drifted, rejecting would fail every delivery and Twitch
	// would revoke the subscription , losing detection entirely to defend
	// against a replay that is already a no-op, since the ledger claim is
	// insert-or-ignore and the end stamp is guarded.
	if !livedetect.TwitchTimestampFresh(msgTS, time.Now()) {
		age, _ := livedetect.TwitchMessageAge(msgTS, time.Now())
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "stale").Inc()
		slog.Warn("twitch eventsub message outside the replay window; acknowledging without acting",
			"func", "twitchEventSubHandler", "msgId", msgID, "age", age.String())
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var env livedetect.TwitchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// The signature was valid, so this is our bug or a schema change.
		// Acknowledge it: making Twitch retry would not help and would count
		// against the subscription.
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "malformed").Inc()
		slog.Error("twitch eventsub body did not decode", "func", "twitchEventSubHandler",
			"msgId", msgID, "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch msgType {
	case livedetect.TwitchMsgVerification:
		// Deliberately NOT deduped. A redelivered challenge answered with an
		// empty body puts the subscription into
		// webhook_callback_verification_failed, which Twitch does not retry
		// out of and which needs manual repair. Verification must always echo.
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "verification").Inc()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(env.Challenge)))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, env.Challenge)
		slog.Info("twitch eventsub callback verified", "func", "twitchEventSubHandler",
			"subId", env.Subscription.ID, "type", env.Subscription.Type)

	case livedetect.TwitchMsgNotification:
		// Dedupe applies only here: delivery is at-least-once.
		if app.LiveDetect.SeenEventSubMessage(msgID, time.Now()) {
			metrics.LiveDetectWebhooks.WithLabelValues("twitch", "duplicate").Inc()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "accepted").Inc()
		w.WriteHeader(http.StatusNoContent)
		sentAt, _ := time.Parse(time.RFC3339Nano, msgTS)
		app.webhookWork(func(ctx context.Context) {
			app.LiveDetect.HandleEventSubNotification(ctx, env, sentAt)
		})

	case livedetect.TwitchMsgRevocation:
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "revocation").Inc()
		w.WriteHeader(http.StatusNoContent)
		sub := env.Subscription
		app.webhookWork(func(context.Context) {
			app.LiveDetect.HandleEventSubRevocation(sub)
		})

	default:
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "unknown-type").Inc()
		slog.Warn("unknown twitch eventsub message type", "func", "twitchEventSubHandler", "type", msgType)
		w.WriteHeader(http.StatusNoContent)
	}
}

// youtubeWebSubHandler receives Google PubSubHubbub callbacks.
//
// GET is the subscription verification handshake; POST is a feed push. Like
// the Twitch callback this is public and authenticates by HMAC.
func (app *App) youtubeWebSubHandler(w http.ResponseWriter, r *http.Request) {
	if !app.LiveDetect.WebSubEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		app.webSubVerify(w, r)
		return
	}
	app.webSubNotify(w, r)
}

// webSubVerify answers the hub's subscription challenge.
//
// The challenge must be echoed verbatim as text/plain with a 2xx. Only topics
// we actually subscribed to are confirmed: confirming an arbitrary topic would
// let anyone point the hub's traffic at this endpoint.
func (app *App) webSubVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	topic := q.Get("hub.topic")
	challenge := q.Get("hub.challenge")

	// ONLY subscribe is confirmed. Verification is unauthenticated by design ,
	// the hub has no shared secret at this point , so confirming an
	// unsubscribe would let anyone who knows the callback URL ask the hub to
	// drop the subscription and have us cheerfully agree, silently killing the
	// push leg. This server never unsubscribes, so an unsubscribe challenge is
	// always someone else's.
	if mode != livedetect.WebSubModeSubscribe {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "bad-mode").Inc()
		slog.Warn("refusing a websub verification we did not request",
			"func", "webSubVerify", "mode", mode, "topic", topic)
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if challenge == "" || len(challenge) > 2048 {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "bad-challenge").Inc()
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if !app.LiveDetect.KnownWebSubTopic(topic) {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "unknown-topic").Inc()
		slog.Warn("websub verification for an unknown topic; refusing",
			"func", "webSubVerify", "topic", topic, "mode", mode)
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	metrics.LiveDetectWebhooks.WithLabelValues("websub", "verification").Inc()
	slog.Info("websub subscription verified", "func", "webSubVerify",
		"topic", topic, "mode", mode, "lease", q.Get("hub.lease_seconds"))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(challenge)))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, challenge)
}

// webSubNotify handles a feed push.
func (app *App) webSubNotify(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "malformed").Inc()
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	feed, err := livedetect.ParseWebSubFeed(raw)
	if err != nil {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "malformed").Inc()
		slog.Warn("websub push did not parse", "func", "webSubNotify", "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Resolve the topic from the payload so the right per-topic secret is
	// used. A push whose entries name no configured channel is dropped.
	topic := ""
	for _, e := range feed.Entries {
		if t := livedetect.YouTubeTopicURL(e.ChannelID); app.LiveDetect.KnownWebSubTopic(t) {
			topic = t
			break
		}
	}
	if topic == "" {
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "unknown-topic").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !livedetect.VerifyWebSubSignature(app.LiveDetect.WebSubTopicSecret(topic), raw, r.Header.Get("X-Hub-Signature")) {
		// WebSub says an unverifiable message must be ignored locally, and the
		// hub keeps retrying a non-2xx for the whole lease. Answering 2xx and
		// dropping it avoids an infinite retry storm; the alert is what stops
		// a rotated secret from becoming a silent permanent outage.
		metrics.LiveDetectWebhooks.WithLabelValues("websub", "bad-signature").Inc()
		slog.Error("websub push failed signature verification; dropping",
			"func", "webSubNotify", "topic", topic)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	metrics.LiveDetectWebhooks.WithLabelValues("websub", "accepted").Inc()
	w.WriteHeader(http.StatusNoContent)
	app.webhookWork(func(context.Context) {
		app.LiveDetect.HandleWebSubPush(feed)
	})
}
