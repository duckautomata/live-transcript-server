package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

const (
	testEventSubSecret = "test-eventsub-secret-value"
	testWebSubSecret   = "test-websub-base-secret"
	testYTChannelID    = "UCaaaaaaaaaaaaaaaaaaaaaa"
)

// setupDetectApp builds an App with live detection enabled. The detector's
// outbound legs never run (Start is not called), so only the inbound webhook
// paths and the sink are exercised.
func setupDetectApp(tb testing.TB) (*App, *http.ServeMux) {
	tb.Helper()

	st, err := store.Open(":memory:", config.DatabaseConfig{SkipWarmup: true})
	if err != nil {
		tb.Fatalf("failed to init db: %v", err)
	}

	cfg := config.Config{
		Credentials: config.Credentials{ApiKey: "test-api-key"},
		Storage:     config.StorageConfig{Type: "local"},
		Channels: []config.ChannelConfig{
			{Name: "doki", DisplayName: "Dokibird", TwitchLogin: "dokibird", YouTubeChannelId: testYTChannelID},
		},
		LiveDetect: config.LiveDetectConfig{
			Enabled:       true,
			PublicBaseURL: "https://example.test",
			Twitch: config.LiveDetectTwitchConfig{
				Enabled:        true,
				ClientId:       "cid",
				ClientSecret:   "csecret",
				EventSub:       true,
				EventSubSecret: testEventSubSecret,
			},
			YouTube: config.LiveDetectYouTubeConfig{
				Enabled:      true,
				ApiKey:       "yt-key",
				WebSub:       true,
				WebSubSecret: testWebSubSecret,
			},
		},
	}

	app, err := NewApp(cfg, st, tb.TempDir(), "test-version", "test-build-time")
	if err != nil {
		tb.Fatalf("failed to construct app: %v", err)
	}
	if err := app.Init(context.Background()); err != nil {
		tb.Fatalf("failed to init app: %v", err)
	}
	// The real lookup would call Helix for a title-less Twitch detection.
	// Tests that want it install a stub.
	app.TwitchTitleLookup = nil
	tb.Cleanup(func() { app.Close() })

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	return app, mux
}

// "upload" and "short" are two classifications of one publish event. A video
// announced as a short must not be announced again as a video after a restart
// reclassifies it (the probe was unsure the first time, say), while the same
// id being scheduled is a separate event with its own claim.
func TestObserveVideoUploadAndShortShareOneClaim(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	claims := func(kind string) bool {
		t.Helper()
		before, _ := app.Store.GetRecentVideoDetections(ctx, "doki", 50)
		err := app.ObserveVideo(ctx, livedetect.VideoEvent{
			Kind: kind, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "vid-1",
			URL: livedetect.YouTubeWatchURL("vid-1"), Title: "clip", PublishedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("ObserveVideo(%s): %v", kind, err)
		}
		after, _ := app.Store.GetRecentVideoDetections(ctx, "doki", 50)
		return len(after) == len(before)+1
	}

	if !claims(livedetect.VideoShort) {
		t.Fatal("the first classification must claim the event")
	}
	if claims(livedetect.VideoUpload) {
		t.Error("a later 'upload' classification of the same video must not claim a second announcement")
	}
	if claims(livedetect.VideoShort) {
		t.Error("the same kind again must not claim either")
	}
	if !claims(livedetect.VideoScheduled) {
		t.Error("'scheduled' is a different event for the same id and keeps its own claim")
	}
}

// A Twitch broadcast that arrives without a title (EventSub's payload has
// none) is enriched through the lookup after the claim, so the ledger - and
// therefore the announcement - carries the title the poll leg would have.
func TestObserveLiveLooksUpMissingTwitchTitle(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	var asked []string
	app.TwitchTitleLookup = func(_ context.Context, channelKey string) string {
		asked = append(asked, channelKey)
		return "looked-up title"
	}

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "notitle",
		URL: "https://twitch.tv/dokibird", StartedAt: time.Now(),
	}, livedetect.MechanismTwitchEventSub); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if len(asked) != 1 || asked[0] != "doki" {
		t.Fatalf("lookup calls = %v, want exactly one for doki", asked)
	}
	det, err := app.Store.GetDetection(ctx, "twitch", "notitle")
	if err != nil || det == nil {
		t.Fatalf("detection missing: %v %v", det, err)
	}
	if det.Title != "looked-up title" {
		t.Errorf("ledger title = %q, want the looked-up one", det.Title)
	}

	// A broadcast that already has a title, or a YouTube one, is never looked up.
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "titled",
		URL: "https://twitch.tv/dokibird", Title: "already titled", StartedAt: time.Now(),
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytnotitle",
		URL: livedetect.YouTubeWatchURL("ytnotitle"), StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if len(asked) != 1 {
		t.Errorf("lookup calls = %v, want no further calls", asked)
	}
}

func eventSubRequest(t *testing.T, msgType string, body []byte, opts ...func(*http.Request)) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, livedetect.TwitchEventSubPath, strings.NewReader(string(body)))
	id := "msg-" + msgType
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	req.Header.Set(livedetect.HeaderTwitchMessageID, id)
	req.Header.Set(livedetect.HeaderTwitchMessageTimestamp, ts)
	req.Header.Set(livedetect.HeaderTwitchMessageType, msgType)
	req.Header.Set(livedetect.HeaderTwitchMessageSignature, twitchTestSig(id, ts, body))
	for _, o := range opts {
		o(req)
	}
	return req
}

func twitchTestSig(id, ts string, body []byte) string {
	return signTwitch(testEventSubSecret, id, ts, body)
}

// signTwitch mirrors Twitch's construction: id || timestamp || raw body, with
// no separator, keyed by the transport secret.
func signTwitch(secret, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	mac.Write([]byte(ts))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// The verification handshake is the one message that must NEVER be deduped: a
// redelivered challenge answered with an empty body leaves the subscription in
// webhook_callback_verification_failed, which Twitch does not retry out of.
func TestEventSubVerificationAlwaysEchoesChallenge(t *testing.T) {
	_, mux := setupDetectApp(t)

	body, _ := json.Marshal(map[string]any{
		"challenge":    "pogchamp-kappa-123",
		"subscription": map[string]any{"id": "sub-1", "type": "stream.online"},
	})

	for attempt := range 3 {
		req := eventSubRequest(t, livedetect.TwitchMsgVerification, body)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", attempt, rr.Code)
		}
		if got := rr.Body.String(); got != "pogchamp-kappa-123" {
			t.Fatalf("attempt %d: body = %q, want the raw challenge echoed", attempt, got)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
			t.Fatalf("attempt %d: Content-Type = %q, want text/plain", attempt, ct)
		}
	}
}

func TestEventSubRejectsBadSignature(t *testing.T) {
	_, mux := setupDetectApp(t)
	body := []byte(`{"subscription":{"id":"s","type":"stream.online"},"event":{}}`)

	req := eventSubRequest(t, livedetect.TwitchMsgNotification, body, func(r *http.Request) {
		r.Header.Set(livedetect.HeaderTwitchMessageSignature, "sha256=deadbeef")
	})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unauthenticated request", rr.Code)
	}
}

func TestEventSubRejectsMissingHeaders(t *testing.T) {
	_, mux := setupDetectApp(t)
	req := httptest.NewRequest(http.MethodPost, livedetect.TwitchEventSubPath, strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the signature headers are absent", rr.Code)
	}
}

// A stale-but-correctly-signed message must be ACKNOWLEDGED, not rejected.
// Twitch counts a non-2xx as a delivery failure and enough of them revoke the
// subscription - so a drifting clock would cost us detection entirely, to
// defend against a replay the ledger already makes a no-op.
func TestEventSubAcknowledgesStaleButSignedMessage(t *testing.T) {
	_, mux := setupDetectApp(t)
	body := []byte(`{"subscription":{"id":"s","type":"stream.online"},"event":{}}`)

	id := "old-msg"
	ts := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodPost, livedetect.TwitchEventSubPath, strings.NewReader(string(body)))
	req.Header.Set(livedetect.HeaderTwitchMessageID, id)
	req.Header.Set(livedetect.HeaderTwitchMessageTimestamp, ts)
	req.Header.Set(livedetect.HeaderTwitchMessageType, livedetect.TwitchMsgNotification)
	req.Header.Set(livedetect.HeaderTwitchMessageSignature, signTwitch(testEventSubSecret, id, ts, body))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; rejecting would walk the subscription toward revocation", rr.Code)
	}
}

func TestEventSubStreamOnlineIsDetectedOnce(t *testing.T) {
	app, mux := setupDetectApp(t)

	startedAt := time.Now().Add(-4 * time.Second).UTC().Format(time.RFC3339)
	event, _ := json.Marshal(map[string]any{
		"id":                     "42424242",
		"broadcaster_user_id":    "1234",
		"broadcaster_user_login": "dokibird",
		"broadcaster_user_name":  "Dokibird",
		"type":                   "live",
		"started_at":             startedAt,
	})
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{
			"id":        "sub-1",
			"type":      "stream.online",
			"condition": map[string]string{"broadcaster_user_id": "1234"},
		},
		"event": json.RawMessage(event),
	})

	req := eventSubRequest(t, livedetect.TwitchMsgNotification, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}

	det := waitForDetection(t, app, "twitch", "42424242")
	if det.ChannelKey != "doki" {
		t.Errorf("channel = %q, want doki", det.ChannelKey)
	}
	if det.Mechanism != livedetect.MechanismTwitchEventSub {
		t.Errorf("mechanism = %q, want %q", det.Mechanism, livedetect.MechanismTwitchEventSub)
	}
	if det.URL != "https://twitch.tv/dokibird" {
		t.Errorf("url = %q", det.URL)
	}
	if det.StartedAt == 0 {
		t.Error("startedAt must be carried through from the event")
	}
	if delay := det.DetectedAt - det.StartedAt; delay < 0 || delay > 60 {
		t.Errorf("implausible measured delay %ds", delay)
	}
}

// Delivery is at-least-once, so a redelivered notification must not produce a
// second detection.
func TestEventSubDuplicateNotificationIsDropped(t *testing.T) {
	app, mux := setupDetectApp(t)

	event, _ := json.Marshal(map[string]any{
		"id": "555", "broadcaster_user_id": "1234",
		"broadcaster_user_login": "dokibird", "type": "live",
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{"id": "sub-1", "type": "stream.online",
			"condition": map[string]string{"broadcaster_user_id": "1234"}},
		"event": json.RawMessage(event),
	})

	for range 4 {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, eventSubRequest(t, livedetect.TwitchMsgNotification, body))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rr.Code)
		}
	}
	waitForDetection(t, app, "twitch", "555")

	dets, err := app.Store.GetRecentDetections(context.Background(), "doki", 50)
	if err != nil {
		t.Fatalf("recent detections: %v", err)
	}
	if len(dets) != 1 {
		t.Fatalf("got %d detections for one broadcast, want exactly 1", len(dets))
	}
}

// A rerun is not a live broadcast and must be dropped BEFORE the ledger claim:
// there is no claim-without-notify path, so filtering later is impossible.
func TestEventSubIgnoresNonLiveType(t *testing.T) {
	app, mux := setupDetectApp(t)

	event, _ := json.Marshal(map[string]any{
		"id": "999", "broadcaster_user_id": "1234",
		"broadcaster_user_login": "dokibird", "type": "rerun",
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{"id": "sub-1", "type": "stream.online",
			"condition": map[string]string{"broadcaster_user_id": "1234"}},
		"event": json.RawMessage(event),
	})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, eventSubRequest(t, livedetect.TwitchMsgNotification, body))

	time.Sleep(150 * time.Millisecond)
	if got, _ := app.Store.GetDetection(context.Background(), "twitch", "999"); got != nil {
		t.Fatal("a rerun must never be recorded as a detection")
	}
}

// An event for a channel this server does not know must be dropped quietly
// rather than reaching the sink, which treats an unknown channel as an error.
func TestEventSubIgnoresUnconfiguredChannel(t *testing.T) {
	app, mux := setupDetectApp(t)

	event, _ := json.Marshal(map[string]any{
		"id": "777", "broadcaster_user_id": "9999",
		"broadcaster_user_login": "somebodyelse", "type": "live",
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	body, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{"id": "sub-x", "type": "stream.online",
			"condition": map[string]string{"broadcaster_user_id": "9999"}},
		"event": json.RawMessage(event),
	})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, eventSubRequest(t, livedetect.TwitchMsgNotification, body))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}

	time.Sleep(150 * time.Millisecond)
	if got, _ := app.Store.GetDetection(context.Background(), "twitch", "777"); got != nil {
		t.Fatal("an unconfigured channel must not produce a detection")
	}
}

func webSubBody(videoID, channelID, updated string) []byte {
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>yt:video:%s</id>
    <yt:videoId>%s</yt:videoId>
    <yt:channelId>%s</yt:channelId>
    <title>Test Stream</title>
    <published>2026-01-01T19:00:00+00:00</published>
    <updated>%s</updated>
  </entry>
</feed>`, videoID, videoID, channelID, updated)
}

func webSubSig(t *testing.T, app *App, body []byte) string {
	t.Helper()
	secret := app.LiveDetect.WebSubTopicSecret(livedetect.YouTubeTopicURL(testYTChannelID))
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebSubVerificationEchoesChallengeForKnownTopic(t *testing.T) {
	_, mux := setupDetectApp(t)

	url := livedetect.YouTubeWebSubPath +
		"?hub.mode=subscribe&hub.challenge=chal-123&hub.lease_seconds=432000" +
		"&hub.topic=" + strings.ReplaceAll(livedetect.YouTubeTopicURL(testYTChannelID), ":", "%3A")

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "chal-123" {
		t.Fatalf("body = %q, want the challenge echoed verbatim", got)
	}
}

// Confirming an arbitrary topic would let anyone aim the hub's traffic here.
func TestWebSubVerificationRefusesUnknownTopic(t *testing.T) {
	_, mux := setupDetectApp(t)

	req := httptest.NewRequest(http.MethodGet,
		livedetect.YouTubeWebSubPath+"?hub.mode=subscribe&hub.challenge=x&hub.topic=https%3A%2F%2Fevil.test%2Ffeed", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatal("a topic we never subscribed to must not be confirmed")
	}
}

func TestWebSubPushSeedsWatchlist(t *testing.T) {
	app, mux := setupDetectApp(t)
	body := webSubBody("vid-abc", testYTChannelID, "2026-01-01T19:00:00+00:00")

	req := httptest.NewRequest(http.MethodPost, livedetect.YouTubeWebSubPath, strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature", webSubSig(t, app, body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.LiveDetect.Status().WatchlistSize > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a verified push must seed the watchlist")
}

// WebSub says an unverifiable message must be ignored locally, and the hub
// retries a non-2xx for the whole lease. Answering 2xx and dropping avoids an
// infinite retry storm from a rotated secret.
func TestWebSubBadSignatureIsAcknowledgedNotRejected(t *testing.T) {
	app, mux := setupDetectApp(t)
	body := webSubBody("vid-bad", testYTChannelID, "2026-01-01T19:00:00+00:00")

	req := httptest.NewRequest(http.MethodPost, livedetect.YouTubeWebSubPath, strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature", "sha1=00000000000000000000000000000000000000ff")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 so the hub does not retry forever", rr.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if app.LiveDetect.Status().WatchlistSize != 0 {
		t.Fatal("an unverifiable push must not seed anything")
	}
}

// SHADOW MODE is the whole premise of this build: detection observes and
// reports, and must never put work in front of the worker.
func TestDetectionNeverQueuesForTheWorker(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform:   livedetect.PlatformTwitch,
		ChannelKey: "doki",
		ID:         "shadow-1",
		URL:        "https://twitch.tv/dokibird",
		StartedAt:  time.Now(),
	}, livedetect.MechanismTwitchPoll)
	if err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	urls, err := app.Store.GetIncomingStreams(ctx, "doki")
	if err != nil {
		t.Fatalf("incoming: %v", err)
	}
	if len(urls) != 0 {
		t.Fatalf("detection queued %v for the worker; shadow mode must never do that", urls)
	}
	if s, _ := app.Store.GetRecentStream(ctx, "doki"); s != nil {
		t.Fatal("detection must never write to the streams table")
	}
}

func TestObserveLiveRejectsUnknownChannelAndEmptyID(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "nope", ID: "x",
	}, "test"); err == nil {
		t.Error("an unknown channel must be rejected")
	}
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "",
	}, "test"); err == nil {
		t.Error("a broadcast with no platform id must be rejected")
	}
}

// A platform that reports no start time makes the delay unknowable. Storing a
// zero is what lets the notification say "unknown" instead of rendering a
// perfect-looking zero-second delay.
func TestObserveLiveKeepsUnknownStartTimeZero(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "nostart",
		URL: "https://www.youtube.com/watch?v=nostart",
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	det, err := app.Store.GetDetection(ctx, "youtube", "nostart")
	if err != nil || det == nil {
		t.Fatalf("detection missing: %v %v", det, err)
	}
	if det.StartedAt != 0 {
		t.Fatalf("startedAt = %d, want 0 so the delay reads as unknown", det.StartedAt)
	}
}

// waitForDetection polls for a ledger row written by the async webhook worker.
func waitForDetection(tb testing.TB, app *App, platform, id string) *model.DetectedBroadcast {
	tb.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		det, err := app.Store.GetDetection(context.Background(), platform, id)
		if err != nil {
			tb.Fatalf("GetDetection: %v", err)
		}
		if det != nil {
			return det
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatalf("no detection recorded for %s/%s", platform, id)
	return nil
}

// Verification is unauthenticated by design - the hub has no shared secret at
// that point - so confirming an unsubscribe would let anyone who learns the
// callback URL ask the hub to drop the subscription and have us agree,
// silently killing the push leg. This server never unsubscribes.
func TestWebSubRefusesUnsubscribeVerification(t *testing.T) {
	_, mux := setupDetectApp(t)

	url := livedetect.YouTubeWebSubPath +
		"?hub.mode=unsubscribe&hub.challenge=chal-999" +
		"&hub.topic=" + strings.ReplaceAll(livedetect.YouTubeTopicURL(testYTChannelID), ":", "%3A")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))

	if rr.Code == http.StatusOK {
		t.Fatal("an unsubscribe challenge must never be confirmed")
	}
	if strings.Contains(rr.Body.String(), "chal-999") {
		t.Fatal("the challenge must not be echoed for an unsubscribe")
	}
}

// Background work started from a handler must be refused once shutdown has
// begun, so a WaitGroup Add can never race the Wait that Close is already in.
func TestBackgroundWorkIsRefusedAfterClose(t *testing.T) {
	app, _ := setupDetectApp(t)

	if !app.goBackground(func() {}) {
		t.Fatal("background work should be accepted while running")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if app.goBackground(func() { t.Error("this must never run") }) {
		t.Fatal("background work must be refused after Close")
	}
}

// The reachability marker must be present on EVERY response from both
// callbacks, including the rejection paths - it is what the probe uses to tell
// "our handler answered" from "the edge answered with a decoy". An unsigned
// request is the probe's own shape, so that case matters most.
func TestCallbackMarkerIsAlwaysSet(t *testing.T) {
	_, mux := setupDetectApp(t)

	cases := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "twitch unsigned (the probe's own request)",
			req:  httptest.NewRequest(http.MethodPost, livedetect.TwitchEventSubPath, strings.NewReader("{}")),
		},
		{
			name: "twitch bad signature",
			req: eventSubRequest(t, livedetect.TwitchMsgNotification, []byte(`{}`), func(r *http.Request) {
				r.Header.Set(livedetect.HeaderTwitchMessageSignature, "sha256=bad")
			}),
		},
		{
			name: "twitch valid verification",
			req: eventSubRequest(t, livedetect.TwitchMsgVerification,
				[]byte(`{"challenge":"c","subscription":{"id":"s","type":"stream.online"}}`)),
		},
		{
			name: "websub unsigned push",
			req:  httptest.NewRequest(http.MethodPost, livedetect.YouTubeWebSubPath, strings.NewReader("<feed></feed>")),
		},
		{
			name: "websub verification for an unknown topic",
			req: httptest.NewRequest(http.MethodGet,
				livedetect.YouTubeWebSubPath+"?hub.mode=subscribe&hub.challenge=c&hub.topic=https%3A%2F%2Fevil.test%2Ffeed", nil),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, tc.req)
			if rr.Header().Get(livedetect.HeaderCallbackMarker) == "" {
				t.Fatalf("no %s header on a %d response; the probe could not tell this from an intercepted request",
					livedetect.HeaderCallbackMarker, rr.Code)
			}
		})
	}
}

// The probe must not walk the real push path: its throwaway body would trip a
// parse warning on every run, and a warning that always fires is a warning
// nobody reads.
func TestProbeRequestsAreAnsweredWithoutSideEffects(t *testing.T) {
	for _, path := range []string{livedetect.TwitchEventSubPath, livedetect.YouTubeWebSubPath} {
		t.Run(path, func(t *testing.T) {
			_, mux := setupDetectApp(t)

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			req.Header.Set(livedetect.HeaderCallbackProbe, "1")
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Errorf("status = %d, want 204", rr.Code)
			}
			// The marker is the whole point - without it the probe cannot tell
			// this from an intercepted request.
			if rr.Header().Get(livedetect.HeaderCallbackMarker) == "" {
				t.Error("a probe response must still carry the handler marker")
			}
		})
	}
}

// The scheduled flag must survive the whole path from the poller to the
// notification, since it is what makes the reported delay interpretable.
func TestScheduledFlagReachesTheNotification(t *testing.T) {
	app, _ := setupDetectApp(t)
	ctx := context.Background()

	for _, tc := range []struct {
		id        string
		scheduled bool
	}{
		{id: "was-scheduled", scheduled: true},
		{id: "was-surprise", scheduled: false},
	} {
		err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform:     livedetect.PlatformYouTube,
			ChannelKey:   "doki",
			ID:           tc.id,
			URL:          livedetect.YouTubeWatchURL(tc.id),
			StartedAt:    time.Now(),
			SawScheduled: tc.scheduled,
		}, livedetect.MechanismYouTubeState)
		if err != nil {
			t.Fatalf("ObserveLive(%s): %v", tc.id, err)
		}

		// The ledger deliberately does not carry it - the schema has no
		// ALTER TABLE path, so persisting it would work in tests and silently
		// never apply to a deployed database.
		det, err := app.Store.GetDetection(ctx, "youtube", tc.id)
		if err != nil || det == nil {
			t.Fatalf("detection missing for %s: %v", tc.id, err)
		}
		if det.Mechanism != livedetect.MechanismYouTubeState {
			t.Errorf("mechanism = %q", det.Mechanism)
		}
	}
}
