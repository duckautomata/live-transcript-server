package livedetect

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The public WebSub hub YouTube publishes through, and the topic URL form for
// a channel's upload feed.
const (
	youtubeHubURL      = "https://pubsubhubbub.appspot.com/subscribe"
	youtubeTopicPrefix = "https://www.youtube.com/xml/feeds/videos.xml?channel_id="
)

// WebSub lease handling. The hub caps leases well below what we request, and
// the granted value comes back on the verification GET rather than the
// subscribe response, so renewal is driven off a conservative fixed cadence
// rather than off a value we might never have seen.
const (
	webSubRequestedLease = 432000 * time.Second // 5 days; the hub will cap it
	webSubRenewInterval  = 12 * time.Hour       // renew far more often than needed
)

// WebSubMode values on the verification GET.
const (
	WebSubModeSubscribe   = "subscribe"
	WebSubModeUnsubscribe = "unsubscribe"
)

// WebSubClient manages hub subscriptions for YouTube channel feeds.
//
// WebSub here is a DISCOVERY hint, not a detection mechanism. The Atom payload
// carries no live-state field at all , no liveBroadcastContent, no
// liveStreamingDetails, no duration , so a push can only ever mean "a video
// record for this channel was created or changed". What it buys is the video
// id, seconds after a frame appears, which is what lets the state poller catch
// the upcoming->live transition within seconds instead of waiting for a
// discovery pass.
type WebSubClient struct {
	httpClient *http.Client
	// HubURL is injectable for tests.
	HubURL string
	// secret is the base secret; per-topic secrets are derived from it so a
	// leak of one subscription's secret does not compromise the others.
	secret string
}

// NewWebSubClient constructs a hub client.
func NewWebSubClient(secret string) *WebSubClient {
	return &WebSubClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		HubURL:     youtubeHubURL,
		secret:     secret,
	}
}

// YouTubeTopicURL is the feed topic for a channel id.
func YouTubeTopicURL(channelID string) string {
	return youtubeTopicPrefix + url.QueryEscape(channelID)
}

// TopicSecret derives the per-topic HMAC secret.
//
// Deriving rather than sharing one secret means the hub only ever learns a
// value scoped to one topic, and rotating the base secret rotates all of them
// at once. It is deterministic, so a restart recomputes the same value and
// existing subscriptions keep verifying.
func (c *WebSubClient) TopicSecret(topic string) string {
	if c == nil || c.secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(topic))
	return hex.EncodeToString(mac.Sum(nil))
}

// Subscribe asks the hub to start (or renew) delivery for a channel's feed.
//
// The hub answers 202 and then verifies asynchronously by GETting the callback
// with a challenge, so a successful return here means "the hub accepted the
// request", not "the subscription is active".
func (c *WebSubClient) Subscribe(ctx context.Context, channelID, callbackURL string) error {
	return c.request(ctx, WebSubModeSubscribe, channelID, callbackURL)
}

// Unsubscribe asks the hub to stop delivery.
func (c *WebSubClient) Unsubscribe(ctx context.Context, channelID, callbackURL string) error {
	return c.request(ctx, WebSubModeUnsubscribe, channelID, callbackURL)
}

func (c *WebSubClient) request(ctx context.Context, mode, channelID, callbackURL string) error {
	topic := YouTubeTopicURL(channelID)

	form := url.Values{}
	form.Set("hub.mode", mode)
	form.Set("hub.topic", topic)
	form.Set("hub.callback", callbackURL)
	form.Set("hub.verify", "async")
	form.Set("hub.secret", c.TopicSecret(topic))
	form.Set("hub.lease_seconds", fmt.Sprint(int(webSubRequestedLease.Seconds())))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.HubURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build websub %s request: %w", mode, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("websub %s: %w", mode, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	// 202 is the documented async answer; 204 appears for sync verification.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("websub %s returned status %d", mode, resp.StatusCode)
	}
	return nil
}

// WebSubFeed is the Atom document the hub POSTs.
//
// The complete element set is reproduced here to make the point concrete:
// there is NO live-state field anywhere in it. That is the structural reason
// WebSub cannot be the detection mechanism and is only a discovery hint.
type WebSubFeed struct {
	XMLName xml.Name        `xml:"feed"`
	Entries []WebSubEntry   `xml:"entry"`
	Deleted *WebSubDeletion `xml:"deleted-entry"`
}

// WebSubEntry is one video record in the feed.
type WebSubEntry struct {
	ID        string `xml:"id"`
	VideoID   string `xml:"videoId"`
	ChannelID string `xml:"channelId"`
	Title     string `xml:"title"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
}

// WebSubDeletion marks a removed video; we ignore these but parse the element
// so a deletion push is recognised rather than logged as malformed.
type WebSubDeletion struct {
	Ref string `xml:"ref,attr"`
}

// ParseWebSubFeed decodes an Atom push.
//
// Namespace prefixes are ignored by encoding/xml's default matching on local
// names, which is what makes `yt:videoId` bind to the VideoID field.
func ParseWebSubFeed(body []byte) (*WebSubFeed, error) {
	var f WebSubFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse websub feed: %w", err)
	}
	return &f, nil
}

// webSubDedupeTTL bounds how long a (videoId, updated) pair is remembered.
// Google's hub is documented to send duplicates and to fire on title and
// description edits, so without this every edit would trigger another burst of
// state polling.
const webSubDedupeTTL = 30 * time.Minute
