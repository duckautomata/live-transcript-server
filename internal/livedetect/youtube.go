package livedetect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const youtubeAPIURL = "https://www.googleapis.com/youtube/v3"

// videosListBatchSize is the documented maximum number of ids accepted by
// videos.list in one call. The call costs one quota unit regardless of how
// many ids it carries, which is what decouples poll frequency from channel
// count and makes seconds-scale polling affordable.
const videosListBatchSize = 50

// errYouTubeQuota reports a 403 quotaExceeded. It is wrapped so callers can
// trip the right circuit breaker: videos.list and playlistItems.list share the
// 10,000-unit daily budget, while search.list has its own 100-call bucket, and
// exhausting one must not stop the other.
var errYouTubeQuota = errors.New("youtube: quota exceeded")

// YouTubeClient calls the YouTube Data API v3 with an API key.
type YouTubeClient struct {
	apiKey     string
	httpClient *http.Client

	// BaseURL is injectable for tests.
	BaseURL string
}

// NewYouTubeClient constructs a Data API client. An empty key leaves it
// unconfigured; see Configured.
func NewYouTubeClient(apiKey string) *YouTubeClient {
	return &YouTubeClient{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		BaseURL:    youtubeAPIURL,
	}
}

// Configured reports whether an API key is set.
func (c *YouTubeClient) Configured() bool { return c != nil && c.apiKey != "" }

// YTVideo is one item from videos.list.
type YTVideo struct {
	ID      string `json:"id"`
	Snippet *struct {
		ChannelID string `json:"channelId"`
		Title     string `json:"title"`
		// LiveBroadcastContent is "live", "upcoming" or "none". Livestreams and
		// premieres are INDISTINGUISHABLE here - both report "live" while
		// running - which is exactly what we want, since both are in scope.
		LiveBroadcastContent string `json:"liveBroadcastContent"`
		PublishedAt          string `json:"publishedAt"`
	} `json:"snippet"`
	LiveStreamingDetails *struct {
		ScheduledStartTime string `json:"scheduledStartTime"`
		ActualStartTime    string `json:"actualStartTime"`
		ActualEndTime      string `json:"actualEndTime"`
		ConcurrentViewers  string `json:"concurrentViewers"`
	} `json:"liveStreamingDetails"`
}

// State classifies a video for detection purposes.
//
// The nil check on LiveStreamingDetails is deliberately PERMISSIVE. Google's
// reference scopes that object to "an upcoming, live, or completed live
// broadcast" and never mentions premieres, so requiring it - or requiring
// actualStartTime - would silently drop every premiere the day the API stops
// attaching it. A missing object means "queue it anyway"; the object is read
// only to detect an already-ended broadcast and to supply the start time.
//
// Snippet is nil-checked too: a response item can arrive without it (a partial
// response, a fields mask, an unavailable video), and dereferencing would
// panic the poll goroutine and take detection down silently. A nil snippet is
// "no observation", never "not live".
func (v YTVideo) State() State {
	if v.Snippet == nil {
		return StateUnknown
	}
	switch v.Snippet.LiveBroadcastContent {
	case "live":
		// A stale cache can report "live" for something already finished.
		if v.LiveStreamingDetails != nil && v.LiveStreamingDetails.ActualEndTime != "" {
			return StateEnded
		}
		return StateLive
	case "upcoming":
		return StateUpcoming
	case "none":
		return StateEnded
	default:
		return StateUnknown
	}
}

// StartedAt returns the platform-reported actual start time.
//
// ONLY actualStartTime is accepted. scheduledStartTime is deliberately NOT a
// fallback: the common real case is "scheduled 19:00, actually starts 21:30",
// and using the scheduled time would report a stream detected four seconds
// after going live as a two-and-a-half hour delay - worse than reporting
// nothing, because it is indistinguishable from a genuine miss and the delay
// is the number this whole exercise exists to measure.
func (v YTVideo) StartedAt() time.Time {
	if v.LiveStreamingDetails == nil || v.LiveStreamingDetails.ActualStartTime == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v.LiveStreamingDetails.ActualStartTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ScheduledStartTime returns the announced start time, which is reported to
// the operator as context but is never used to compute a detection delay.
func (v YTVideo) ScheduledStartTime() time.Time {
	if v.LiveStreamingDetails == nil || v.LiveStreamingDetails.ScheduledStartTime == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v.LiveStreamingDetails.ScheduledStartTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Title returns the video title, or "" when no snippet was returned.
func (v YTVideo) Title() string {
	if v.Snippet == nil {
		return ""
	}
	return v.Snippet.Title
}

// ChannelID returns the owning channel, or "" when no snippet was returned.
func (v YTVideo) ChannelID() string {
	if v.Snippet == nil {
		return ""
	}
	return v.Snippet.ChannelID
}

// get performs one Data API request and decodes it.
//
// The API key travels in the X-goog-api-key HEADER, never the query string.
// A transport failure returns a *url.Error that stringifies the whole URL, and
// that error is logged, stored on the leg's health record (which the admin
// endpoint serves as JSON) and rendered into a Discord alert embed - so a key
// in the query string would leak into all three. Keeping it in a header means
// no error-formatting path can ever carry it, without any redaction to
// remember.
func (c *YouTubeClient) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("build youtube request: %w", err)
	}
	req.Header.Set("X-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call youtube %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a bounded amount of the error body to classify quota errors,
		// but never surface it: it echoes the request, which contains the key.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode == http.StatusForbidden && isQuotaExceeded(body) {
			return fmt.Errorf("%w (%s)", errYouTubeQuota, path)
		}
		return fmt.Errorf("youtube %s returned status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode youtube response: %w", err)
	}
	return nil
}

// isQuotaExceeded reports whether a 403 body carries the quotaExceeded reason,
// distinguishing an exhausted quota (wait until the daily reset) from a key or
// permission problem (retrying will never help either, but the operator needs
// to hear a different message).
func isQuotaExceeded(body []byte) bool {
	var e struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	for _, item := range e.Error.Errors {
		if item.Reason == "quotaExceeded" || item.Reason == "dailyLimitExceeded" {
			return true
		}
	}
	return false
}

// VideosList looks up the live state of up to videosListBatchSize video ids.
//
// This is the detection oracle. It costs ONE quota unit per call regardless of
// how many ids it carries, so watching four videos costs exactly as much as
// watching one - which is what makes a three-second poll interval affordable.
//
// contentDetails is deliberately not requested: nothing in the classification
// reads duration, and asking for it invites re-adding a premiere-vs-livestream
// heuristic that YouTube already broke once.
func (c *YouTubeClient) VideosList(ctx context.Context, ids []string) ([]YTVideo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > videosListBatchSize {
		return nil, fmt.Errorf("videos.list accepts at most %d ids, got %d", videosListBatchSize, len(ids))
	}

	q := url.Values{}
	q.Set("part", "snippet,liveStreamingDetails")
	q.Set("id", strings.Join(ids, ","))
	q.Set("maxResults", "50")

	var resp struct {
		Items []YTVideo `json:"items"`
	}
	if err := c.get(ctx, "/videos", q, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// UploadsPlaylistID derives a channel's uploads playlist id from its channel
// id. The mapping is the documented "UC" -> "UU" prefix swap, so it needs no
// channels.list call at runtime.
func UploadsPlaylistID(channelID string) string {
	if len(channelID) < 2 || !strings.HasPrefix(channelID, "UC") {
		return ""
	}
	return "UU" + channelID[2:]
}

// PlaylistItems returns the most recent video ids on a playlist, newest first.
// Used against the uploads playlist to discover video ids we have not seen.
//
// Costs one unit per call and cannot batch channels, so this is the slow
// discovery tier; the whole returned page is diffed rather than just the top
// entry, because a stream scheduled in advance sorts by publish date and can
// land partway down the list.
func (c *YouTubeClient) PlaylistItems(ctx context.Context, playlistID string, maxResults int) ([]string, error) {
	if playlistID == "" {
		return nil, errors.New("empty playlist id")
	}
	if maxResults <= 0 || maxResults > 50 {
		maxResults = 50
	}

	q := url.Values{}
	q.Set("part", "contentDetails")
	q.Set("playlistId", playlistID)
	q.Set("maxResults", fmt.Sprint(maxResults))

	var resp struct {
		Items []struct {
			ContentDetails struct {
				VideoID string `json:"videoId"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/playlistItems", q, &resp); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		if it.ContentDetails.VideoID != "" {
			ids = append(ids, it.ContentDetails.VideoID)
		}
	}
	return ids, nil
}

// SearchLive asks directly which video a channel is live on.
//
// This draws on search.list's OWN quota bucket - 100 calls per day for the
// whole project, separate from the 10,000 units everything else shares. That
// makes it useless as a primary detector but valuable as a low-rate audit and
// as the contingency for a channel whose uploads playlist does not surface an
// in-progress broadcast.
func (c *YouTubeClient) SearchLive(ctx context.Context, channelID string) ([]string, error) {
	q := url.Values{}
	q.Set("part", "snippet")
	q.Set("channelId", channelID)
	q.Set("eventType", "live")
	q.Set("type", "video")
	q.Set("maxResults", "5")

	var resp struct {
		Items []struct {
			ID struct {
				VideoID string `json:"videoId"`
			} `json:"id"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/search", q, &resp); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		if it.ID.VideoID != "" {
			ids = append(ids, it.ID.VideoID)
		}
	}
	return ids, nil
}

// YouTubeWatchURL is the canonical watch URL for a video id. This exact form
// is what internal/discord/webhook.go already builds for YouTube streams.
func YouTubeWatchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// TwitchChannelURL is the canonical URL for a Twitch channel. This exact form
// is what internal/discord/webhook.go already builds for Twitch streams.
func TwitchChannelURL(login string) string {
	return "https://twitch.tv/" + strings.ToLower(login)
}
