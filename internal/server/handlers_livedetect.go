package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"live-transcript-server/internal/discord"
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
// them revoke the subscription - so no database write, Discord post, or
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
	// answered instead of Go - which is the silent failure mode for EventSub.
	w.Header().Set("X-Eventsub-Handler", "1")

	if !app.LiveDetect.EventSubEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	// The reachability probe only needs to learn who answered, which the
	// marker above already tells it.
	if r.Header.Get(livedetect.HeaderCallbackProbe) != "" {
		w.WriteHeader(http.StatusNoContent)
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
	// requests - never for a correctly signed message we merely dislike.
	if !livedetect.VerifyTwitchSignature(app.LiveDetect.EventSubSecret(), msgID, msgTS, raw, msgSig) {
		metrics.LiveDetectWebhooks.WithLabelValues("twitch", "bad-signature").Inc()
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// A stale but correctly signed message is acknowledged, not rejected. If
	// the VPS clock drifted, rejecting would fail every delivery and Twitch
	// would revoke the subscription - losing detection entirely to defend
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
	// Same reachability marker as the Twitch callback: if a probe response
	// lacks it, something in front of the server answered instead of Go.
	w.Header().Set(livedetect.HeaderCallbackMarker, "1")

	if !app.LiveDetect.WebSubEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Header.Get(livedetect.HeaderCallbackProbe) != "" {
		w.WriteHeader(http.StatusNoContent)
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
	// the hub has no shared secret at this point - so confirming an
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

// ---------------------------------------------------------------------------
// Manual YouTube detection (admin key, per channel)
//
// The two admin endpoints below are the operator's way to recover a missed
// go-live without restarting the server, which used to be the only remedy
// (FdwrQid9ulg, 2026-10-11): a restart cleared what hid the stream, but also
// erased the in-memory state that explained the miss, reset the quota counters
// and disconnected every viewer. The work runs on the detector's discovery
// goroutine (livedetect/manual.go); these handlers only parse, map errors and
// keep the audit trail.
// ---------------------------------------------------------------------------

// manualDetector is the slice of the detector the manual-control endpoints
// use. *livedetect.Detector implements it, nil included: every method is
// nil-receiver-safe and answers livedetect.ErrDetectionDisabled.
type manualDetector interface {
	CheckYouTubeVideo(ctx context.Context, channelKey, videoID string) (livedetect.VideoCheck, error)
	RunDiscoveryNow(ctx context.Context, channelKey string) (livedetect.DiscoverySummary, error)
	WatchesYouTube(channelKey string) bool
}

// manualDetect returns the detector behind the manual controls: the test
// stand-in when one is set, LiveDetect otherwise.
func (app *App) manualDetect() manualDetector {
	if app.ManualDetect != nil {
		return app.ManualDetect
	}
	return app.LiveDetect
}

// maxManualDetectBody bounds a check request. The body is one URL.
const maxManualDetectBody = 4 << 10

// youtubeVideoHosts are the hosts whose links carry a video id in the path or
// the v parameter. youtu.be is handled on its own: its whole path is the id.
var youtubeVideoHosts = map[string]bool{
	"youtube.com":              true,
	"www.youtube.com":          true,
	"m.youtube.com":            true,
	"music.youtube.com":        true,
	"youtube-nocookie.com":     true,
	"www.youtube-nocookie.com": true,
}

// errNotAVideoLink is the parser's answer for anything it cannot read an id
// out of. The message is written for the admin page.
var errNotAVideoLink = errors.New("not a YouTube video link or id: paste the video's own URL (watch?v=, youtu.be/, /live/ or /shorts/) or its 11-character id")

// parseYouTubeVideoRef extracts a video id from what an operator pastes: a
// bare id, or a watch?v=, youtu.be/, /live/, /shorts/ or /embed/ link, with or
// without the scheme, and with whatever tracking parameters came along.
//
// A channel's own /live link (youtube.com/@name/live) is refused rather than
// resolved: it redirects to whatever the channel is live on, which takes a
// search call to learn, and the operator has the stream open anyway - its
// address bar carries the real id.
func parseYouTubeVideoRef(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("url is required")
	}
	if livedetect.ValidYouTubeVideoID(s) {
		return s, nil
	}

	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errNotAVideoLink
	}
	host := strings.ToLower(u.Hostname())
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")

	var id string
	switch {
	case host == "youtu.be" || host == "www.youtu.be":
		id = segments[0]
	case youtubeVideoHosts[host]:
		switch {
		case segments[0] == "watch":
			id = u.Query().Get("v")
		case len(segments) >= 2 && (segments[0] == "live" || segments[0] == "shorts" || segments[0] == "embed"):
			id = segments[1]
		case strings.HasPrefix(segments[0], "@") || segments[0] == "channel" || segments[0] == "c":
			return "", errors.New("that is a channel link, not a video: open the stream and paste the URL of its own page")
		}
	default:
		return "", errNotAVideoLink
	}
	if !livedetect.ValidYouTubeVideoID(id) {
		return "", errNotAVideoLink
	}
	return id, nil
}

// badManualRequest is input the handler refuses before the detector sees it.
// manualDetectStatus answers it 400, and its message is the whole answer.
type badManualRequest struct{ msg string }

func (e *badManualRequest) Error() string { return e.msg }

// manualDetectError is the JSON body of a refused manual request. The admin
// page shows Error as written; RetryAfterSeconds repeats the Retry-After
// header for a page that does not read headers.
type manualDetectError struct {
	OK                bool   `json:"ok"`
	Error             string `json:"error"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

// manualDetectStatus maps an error from the manual controls to the HTTP status
// it answers with, plus how long a retry should wait where waiting helps.
//
// 400 is the request's fault (bad input, another channel's video), 404 a
// video YouTube will not return, 429 a limit (with when it lifts), 502 the
// YouTube API failing, 503 detection being unavailable for this channel, and
// 504 the discovery loop not answering in time.
func manualDetectStatus(err error) (status int, retryAfter time.Duration) {
	var tooSoon *livedetect.TooSoonError
	var other *livedetect.OtherChannelError
	var bad *badManualRequest
	switch {
	case errors.As(err, &tooSoon):
		return http.StatusTooManyRequests, tooSoon.RetryAfter
	case errors.As(err, &bad), errors.As(err, &other), errors.Is(err, livedetect.ErrInvalidVideoID):
		return http.StatusBadRequest, 0
	case errors.Is(err, livedetect.ErrVideoNotFound):
		return http.StatusNotFound, 0
	case errors.Is(err, livedetect.ErrDetectionDisabled),
		errors.Is(err, livedetect.ErrYouTubeNotRunning),
		errors.Is(err, livedetect.ErrNoYouTubeChannel):
		return http.StatusServiceUnavailable, 0
	case errors.Is(err, livedetect.ErrManualTimeout), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, 0
	case errors.Is(err, livedetect.ErrYouTubeAPI):
		return http.StatusBadGateway, 0
	default:
		return http.StatusInternalServerError, 0
	}
}

// writeManualDetectError answers a refused manual request and logs it. A
// refusal is not audited: notifyAdminAction is for what an admin changed, and
// a refused request changed nothing.
func (app *App) writeManualDetectError(w http.ResponseWriter, r *http.Request, cs *ChannelState, fn string, err error, attrs ...any) {
	attrs = append([]any{"key", cs.Key, "func", fn}, attrs...)
	if isClientGone(err) && r.Context().Err() != nil {
		// The admin closed the page; nobody is left to answer.
		slog.Info("admin live detection request abandoned by the client", append(attrs, "err", err)...)
		return
	}

	status, retryAfter := manualDetectStatus(err)
	attrs = append(attrs, "status", status, "err", err)
	var other *livedetect.OtherChannelError
	if errors.As(err, &other) {
		// Named in the log: a channel admin pointed detection at a stream
		// that is not theirs, by mistake or not.
		attrs = append(attrs, "videoChannelId", other.ChannelID, "videoChannelKey", other.ChannelKey)
	}
	if status >= 500 {
		slog.Error("admin live detection request failed", attrs...)
		metrics.Http500Errors.Inc()
	} else {
		slog.Warn("admin live detection request refused", attrs...)
		metrics.Http400Errors.Inc()
	}

	body := manualDetectError{Error: err.Error()}
	if retryAfter > 0 {
		secs := max(int(retryAfter.Round(time.Second)/time.Second), 1)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		body.RetryAfterSeconds = secs
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed to write JSON error", "func", fn, "err", err)
	}
}

// LiveDetectCheckResponse is the answer to POST
// /{channel}/admin/livedetect/check: the video as YouTube reports it and what
// detection did with it.
type LiveDetectCheckResponse struct {
	OK bool `json:"ok"`
	livedetect.VideoCheck
}

// postAdminLiveDetectCheckHandler looks up one YouTube video for this channel
// and puts it on the detection watchlist, due for an immediate state poll -
// clearing whatever negative cache hid it. Body: {"url": "<link or id>"}.
//
// It is the recovery for a stream that is live but was never detected. The
// state poll that follows claims it through the normal path, so it is queued
// and announced like any other detection - late, and the late-detection alert
// says so.
func (app *App) postAdminLiveDetectCheckHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	const fn = "postAdminLiveDetectCheckHandler"
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManualDetectBody)).Decode(&body); err != nil {
		app.writeManualDetectError(w, r, cs, fn, &badManualRequest{`the body must be JSON like {"url": "https://www.youtube.com/watch?v=..."}`})
		return
	}
	videoID, err := parseYouTubeVideoRef(body.URL)
	if err != nil {
		app.writeManualDetectError(w, r, cs, fn, &badManualRequest{err.Error()}, "input", body.URL)
		return
	}

	check, err := app.manualDetect().CheckYouTubeVideo(r.Context(), cs.Key, videoID)
	if err != nil {
		app.writeManualDetectError(w, r, cs, fn, err, "videoId", videoID)
		return
	}

	// The watchlist size and the quota spend on the page both moved, and a
	// live video is about to appear under recent detections.
	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Checked a YouTube video for live detection",
		discord.AdminField{Name: "Video", Value: check.URL},
		discord.AdminField{Name: "State", Value: check.State, Inline: true},
		discord.AdminField{Name: "Members Only", Value: yesNo(check.MembersOnly), Inline: true},
		discord.AdminField{Name: "Was Hidden From Discovery", Value: yesNo(check.Unretired), Inline: true},
		discord.AdminField{Name: "Was Already Tracked", Value: yesNo(check.WasTracked), Inline: true},
		discord.AdminField{Name: "Title", Value: check.Title},
	)
	slog.Info("admin checked a youtube video for live detection", "key", cs.Key, "func", fn,
		"videoId", check.VideoID, "state", check.State, "membersOnly", check.MembersOnly,
		"unretired", check.Unretired, "wasTracked", check.WasTracked, "checksRemaining", check.ChecksRemaining)
	writeJSON(w, LiveDetectCheckResponse{OK: true, VideoCheck: check})
}

// LiveDetectDiscoverResponse is the answer to POST
// /{channel}/admin/livedetect/discover.
//
// A pass covers every YouTube channel, whichever admin asked, so the counts
// are the whole pass's. Only this channel's finds are listed, though: this
// page is scoped to one channel, and another channel's new ids - members-only
// ones included - are that channel's business. FoundOtherChannels says how
// many there were.
type LiveDetectDiscoverResponse struct {
	OK                 bool     `json:"ok"`
	Channels           int      `json:"channels"`
	PlaylistsRead      int      `json:"playlistsRead"`
	Failures           int      `json:"failures"`
	BudgetRefused      bool     `json:"budgetRefused"`
	UnitsSpent         int      `json:"unitsSpent"`
	Found              []string `json:"found"`
	FoundMembersOnly   []string `json:"foundMembersOnly"`
	FoundOtherChannels int      `json:"foundOtherChannels"`
}

// postAdminLiveDetectDiscoverHandler runs one YouTube discovery pass now
// rather than at its next scheduled slot. At most one manual pass a minute,
// across every channel.
func (app *App) postAdminLiveDetectDiscoverHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	const fn = "postAdminLiveDetectDiscoverHandler"
	summary, err := app.manualDetect().RunDiscoveryNow(r.Context(), cs.Key)
	if err != nil {
		app.writeManualDetectError(w, r, cs, fn, err)
		return
	}

	resp := LiveDetectDiscoverResponse{
		OK:               true,
		Channels:         summary.Channels,
		PlaylistsRead:    summary.PlaylistsRead,
		Failures:         summary.Failures,
		BudgetRefused:    summary.BudgetRefused,
		UnitsSpent:       summary.UnitsSpent,
		Found:            append([]string{}, summary.Found[cs.Key]...),
		FoundMembersOnly: append([]string{}, summary.FoundMembersOnly[cs.Key]...),
	}
	for _, found := range []map[string][]string{summary.Found, summary.FoundMembersOnly} {
		for key, ids := range found {
			if key != cs.Key {
				resp.FoundOtherChannels += len(ids)
			}
		}
	}

	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Ran live detection discovery",
		discord.AdminField{Name: "New Videos Here", Value: strconv.Itoa(len(resp.Found) + len(resp.FoundMembersOnly)), Inline: true},
		discord.AdminField{Name: "Playlists Read", Value: strconv.Itoa(resp.PlaylistsRead), Inline: true},
		discord.AdminField{Name: "Quota Units", Value: strconv.Itoa(resp.UnitsSpent), Inline: true},
	)
	slog.Info("admin ran youtube discovery", "key", cs.Key, "func", fn,
		"found", resp.Found, "foundMembersOnly", resp.FoundMembersOnly, "foundOtherChannels", resp.FoundOtherChannels,
		"playlistsRead", resp.PlaylistsRead, "failures", resp.Failures, "budgetRefused", resp.BudgetRefused,
		"units", resp.UnitsSpent)
	writeJSON(w, resp)
}
