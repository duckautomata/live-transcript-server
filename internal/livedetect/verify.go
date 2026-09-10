package livedetect

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"strings"
	"sync"
	"time"
)

// Twitch EventSub request headers. Go canonicalises incoming header names, so
// these are written in canonical form and read with r.Header.Get.
const (
	HeaderTwitchMessageID        = "Twitch-Eventsub-Message-Id"
	HeaderTwitchMessageTimestamp = "Twitch-Eventsub-Message-Timestamp"
	HeaderTwitchMessageSignature = "Twitch-Eventsub-Message-Signature"
	HeaderTwitchMessageType      = "Twitch-Eventsub-Message-Type"
	HeaderTwitchSubscriptionType = "Twitch-Eventsub-Subscription-Type"
)

// HeaderCallbackMarker is set on every response from the inbound callback
// handlers. Its absence in a probe response means something between the
// internet and this process answered the request.
const HeaderCallbackMarker = "X-Eventsub-Handler"

// Twitch EventSub message types.
const (
	TwitchMsgVerification = "webhook_callback_verification"
	TwitchMsgNotification = "notification"
	TwitchMsgRevocation   = "revocation"
)

// twitchReplayMaxAge is how old a signed message may be before we treat it as
// a replay. Twitch documents 10 minutes.
const twitchReplayMaxAge = 10 * time.Minute

// twitchClockSkewAllowance tolerates a message timestamped slightly in the
// future relative to our clock.
const twitchClockSkewAllowance = 5 * time.Minute

// VerifyTwitchSignature checks an EventSub HMAC.
//
// The signed message is the concatenation of the message id, the message
// timestamp, and the RAW request body, with no separator, in that order. The
// body must be the exact bytes received: decoding and re-encoding JSON changes
// key order and whitespace and every signature then fails.
func VerifyTwitchSignature(secret, msgID, msgTimestamp string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msgID))
	mac.Write([]byte(msgTimestamp))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

// TwitchMessageAge returns how long ago a message was sent, and whether the
// timestamp parsed at all.
func TwitchMessageAge(msgTimestamp string, now time.Time) (time.Duration, bool) {
	sent, err := time.Parse(time.RFC3339Nano, msgTimestamp)
	if err != nil {
		return 0, false
	}
	return now.Sub(sent), true
}

// TwitchTimestampFresh reports whether a message timestamp is inside the
// replay window.
//
// A stale-but-correctly-signed message is NOT rejected with an error status by
// the handler: a non-2xx response counts as a delivery failure in Twitch's
// accounting and enough of them revoke the subscription. Since ClaimDetection
// is insert-or-ignore and MarkDetectionEnded is guarded, replaying an old
// message is already a no-op, so the handler acknowledges it and moves on.
func TwitchTimestampFresh(msgTimestamp string, now time.Time) bool {
	age, ok := TwitchMessageAge(msgTimestamp, now)
	if !ok {
		return false
	}
	return age <= twitchReplayMaxAge && age >= -twitchClockSkewAllowance
}

// VerifyWebSubSignature checks a WebSub X-Hub-Signature header against the raw
// body.
//
// The algorithm is read from the header rather than hardcoded. Google's hub
// sends sha1= today; the W3C spec permits sha1/sha256/sha384/sha512 and
// encourages stronger ones, so pinning either choice is a silent kill switch
// the day the hub changes. There is no downgrade risk: forcing sha1 still
// requires forging an HMAC under a secret the attacker does not have.
func VerifyWebSubSignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	method, sig, ok := strings.Cut(header, "=")
	if !ok {
		return false
	}

	var newHash func() hash.Hash
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "sha1":
		newHash = sha1.New
	case "sha256":
		newHash = sha256.New
	case "sha384":
		newHash = sha512.New384
	case "sha512":
		newHash = sha512.New
	default:
		return false
	}

	mac := hmac.New(newHash, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.ToLower(strings.TrimSpace(sig))))
}

// seenCache remembers recently handled message ids so an at-least-once
// delivery is processed once. Entries expire after ttl, which only needs to
// exceed the replay window: anything older is rejected before it gets here.
type seenCache struct {
	ttl time.Duration

	mu    sync.Mutex
	seen  map[string]time.Time
	swept time.Time
}

func newSeenCache(ttl time.Duration) *seenCache {
	return &seenCache{ttl: ttl, seen: make(map[string]time.Time)}
}

// SeenOrRecord reports whether id was already recorded, recording it if not.
//
// Callers must apply this ONLY to notifications. Running it over verification
// handshakes would answer a redelivered challenge with an empty body, which
// puts the subscription into webhook_callback_verification_failed - a state
// Twitch does not retry out of and that requires manual repair.
func (c *seenCache) SeenOrRecord(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Amortised sweep: cheap, and bounds the map without a background
	// goroutine that would need its own lifecycle.
	if now.Sub(c.swept) > c.ttl {
		for k, t := range c.seen {
			if now.Sub(t) > c.ttl {
				delete(c.seen, k)
			}
		}
		c.swept = now
	}

	if t, ok := c.seen[id]; ok && now.Sub(t) <= c.ttl {
		return true
	}
	c.seen[id] = now
	return false
}
