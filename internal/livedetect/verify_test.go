package livedetect

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func twitchSig(secret, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	mac.Write([]byte(ts))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyTwitchSignature(t *testing.T) {
	const secret = "a-secret-at-least-ten"
	id, ts := "msg-1", "2026-01-01T00:00:00Z"
	body := []byte(`{"subscription":{"id":"s"},"event":{}}`)
	good := twitchSig(secret, id, ts, body)

	if !VerifyTwitchSignature(secret, id, ts, body, good) {
		t.Fatal("a correct signature must verify")
	}

	// The signed message is id||timestamp||body with no separator, so moving a
	// byte across the boundary must invalidate it. This catches an
	// implementation that concatenated in the wrong order or added a delimiter.
	if VerifyTwitchSignature(secret, id+"x", ts, body, good) {
		t.Error("a different message id must not verify")
	}
	if VerifyTwitchSignature(secret, id, ts+"x", body, good) {
		t.Error("a different timestamp must not verify")
	}

	// The raw-bytes rule: re-encoded JSON differs in whitespace and key order
	// and must fail, which is why the handler never decodes before verifying.
	if VerifyTwitchSignature(secret, id, ts, []byte(`{"event":{},"subscription":{"id":"s"}}`), good) {
		t.Error("a re-encoded body must not verify")
	}
	if VerifyTwitchSignature("wrong-secret-value", id, ts, body, good) {
		t.Error("a different secret must not verify")
	}
	if VerifyTwitchSignature(secret, id, ts, body, "") {
		t.Error("an empty header must not verify")
	}
	if VerifyTwitchSignature("", id, ts, body, good) {
		t.Error("an empty secret must not verify")
	}

	// The prefix is part of the compared value: a bare hex digest is not a
	// valid header and must be rejected.
	if VerifyTwitchSignature(secret, id, ts, body, good[len("sha256="):]) {
		t.Error("a signature without its sha256= prefix must not verify")
	}
}

func TestTwitchTimestampFreshness(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).Format(time.RFC3339Nano)
	stale := now.Add(-11 * time.Minute).Format(time.RFC3339Nano)
	future := now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	slightlyAhead := now.Add(2 * time.Minute).Format(time.RFC3339Nano)

	if !TwitchTimestampFresh(fresh, now) {
		t.Error("a recent message must be fresh")
	}
	if TwitchTimestampFresh(stale, now) {
		t.Error("a message past the replay window must not be fresh")
	}
	if TwitchTimestampFresh(future, now) {
		t.Error("a message far in the future must not be fresh")
	}
	// Modest clock skew between Twitch and this host is normal and must not
	// reject an otherwise valid delivery.
	if !TwitchTimestampFresh(slightlyAhead, now) {
		t.Error("a slightly future timestamp must be tolerated as clock skew")
	}
	if TwitchTimestampFresh("not-a-timestamp", now) {
		t.Error("an unparseable timestamp must not be fresh")
	}
}

func TestVerifyWebSubSignature(t *testing.T) {
	const secret = "topic-secret"
	body := []byte("<feed><entry><id>x</id></entry></feed>")

	sha1mac := hmac.New(sha1.New, []byte(secret))
	sha1mac.Write(body)
	sha1Header := "sha1=" + hex.EncodeToString(sha1mac.Sum(nil))

	sha256mac := hmac.New(sha256.New, []byte(secret))
	sha256mac.Write(body)
	sha256Header := "sha256=" + hex.EncodeToString(sha256mac.Sum(nil))

	// Google's hub sends sha1 today. Hardcoding it would be a silent kill
	// switch the day the hub upgrades, so both must verify.
	if !VerifyWebSubSignature(secret, body, sha1Header) {
		t.Error("sha1 (what Google's hub sends today) must verify")
	}
	if !VerifyWebSubSignature(secret, body, sha256Header) {
		t.Error("sha256 must verify so a hub upgrade does not silently break detection")
	}

	if VerifyWebSubSignature(secret, []byte("tampered"), sha1Header) {
		t.Error("a modified body must not verify")
	}
	if VerifyWebSubSignature("other", body, sha1Header) {
		t.Error("a different secret must not verify")
	}
	if VerifyWebSubSignature(secret, body, "md5=abc") {
		t.Error("an unsupported algorithm must not verify")
	}
	if VerifyWebSubSignature(secret, body, "no-equals-sign") {
		t.Error("a malformed header must not verify")
	}
}

// Per-topic secrets mean the hub only ever learns a value scoped to one topic,
// and they must be deterministic so a restart keeps existing subscriptions
// verifiable.
func TestWebSubTopicSecretIsPerTopicAndStable(t *testing.T) {
	c := NewWebSubClient("base-secret")
	a := c.TopicSecret(YouTubeTopicURL("UC_a"))
	b := c.TopicSecret(YouTubeTopicURL("UC_b"))

	if a == "" || b == "" {
		t.Fatal("expected non-empty derived secrets")
	}
	if a == b {
		t.Error("different topics must derive different secrets")
	}
	if a != c.TopicSecret(YouTubeTopicURL("UC_a")) {
		t.Error("the derivation must be stable across calls, or a restart breaks verification")
	}
	if (&WebSubClient{}).TopicSecret("t") != "" {
		t.Error("an unconfigured client must derive no secret")
	}
}

func TestSeenCacheDedupesAndExpires(t *testing.T) {
	c := newSeenCache(time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if c.SeenOrRecord("a", now) {
		t.Fatal("a first sighting must not report as seen")
	}
	if !c.SeenOrRecord("a", now.Add(time.Second)) {
		t.Fatal("a redelivery inside the window must report as seen")
	}
	if c.SeenOrRecord("b", now) {
		t.Fatal("a different id must not report as seen")
	}
	if c.SeenOrRecord("a", now.Add(2*time.Minute)) {
		t.Error("an id past the ttl must no longer be remembered")
	}
	// An empty id carries no identity, so it can never be deduped , treating
	// it as seen would drop every message that arrived without one.
	if c.SeenOrRecord("", now) {
		t.Error("an empty id must never report as seen")
	}
}

func TestParseWebSubFeed(t *testing.T) {
	const doc = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>yt:video:VIDEO_ID</id>
    <yt:videoId>VIDEO_ID</yt:videoId>
    <yt:channelId>CHANNEL_ID</yt:channelId>
    <title>A Stream Title</title>
    <published>2026-01-01T19:00:00+00:00</published>
    <updated>2026-01-01T19:05:00+00:00</updated>
  </entry>
</feed>`

	feed, err := ParseWebSubFeed([]byte(doc))
	if err != nil {
		t.Fatalf("ParseWebSubFeed: %v", err)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(feed.Entries))
	}
	e := feed.Entries[0]
	if e.VideoID != "VIDEO_ID" || e.ChannelID != "CHANNEL_ID" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.Title != "A Stream Title" || e.Updated == "" {
		t.Fatalf("unexpected entry fields: %+v", e)
	}
}

func TestParseWebSubFeedRejectsGarbage(t *testing.T) {
	if _, err := ParseWebSubFeed([]byte("not xml at all")); err == nil {
		t.Fatal("expected an error for a non-XML body")
	}
}
