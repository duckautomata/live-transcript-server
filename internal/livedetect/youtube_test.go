package livedetect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func liveDetails(actualStart, actualEnd, scheduled string) *struct {
	ScheduledStartTime string `json:"scheduledStartTime"`
	ActualStartTime    string `json:"actualStartTime"`
	ActualEndTime      string `json:"actualEndTime"`
	ConcurrentViewers  string `json:"concurrentViewers"`
} {
	return &struct {
		ScheduledStartTime string `json:"scheduledStartTime"`
		ActualStartTime    string `json:"actualStartTime"`
		ActualEndTime      string `json:"actualEndTime"`
		ConcurrentViewers  string `json:"concurrentViewers"`
	}{ScheduledStartTime: scheduled, ActualStartTime: actualStart, ActualEndTime: actualEnd}
}

func snippet(lbc string) *struct {
	ChannelID            string `json:"channelId"`
	Title                string `json:"title"`
	LiveBroadcastContent string `json:"liveBroadcastContent"`
	PublishedAt          string `json:"publishedAt"`
} {
	return &struct {
		ChannelID            string `json:"channelId"`
		Title                string `json:"title"`
		LiveBroadcastContent string `json:"liveBroadcastContent"`
		PublishedAt          string `json:"publishedAt"`
	}{ChannelID: "UC0123456789012345678901", Title: "a stream", LiveBroadcastContent: lbc}
}

func TestYTVideoState(t *testing.T) {
	tests := []struct {
		name string
		v    YTVideo
		want State
	}{
		{
			name: "plain VOD",
			v:    YTVideo{Snippet: snippet("none")},
			want: StateEnded,
		},
		{
			name: "scheduled waiting room",
			v:    YTVideo{Snippet: snippet("upcoming"), LiveStreamingDetails: liveDetails("", "", "2026-01-01T19:00:00Z")},
			want: StateUpcoming,
		},
		{
			name: "livestream in progress",
			v:    YTVideo{Snippet: snippet("live"), LiveStreamingDetails: liveDetails("2026-01-01T19:02:00Z", "", "2026-01-01T19:00:00Z")},
			want: StateLive,
		},
		{
			name: "ended livestream still cached as live",
			v:    YTVideo{Snippet: snippet("live"), LiveStreamingDetails: liveDetails("2026-01-01T19:00:00Z", "2026-01-01T21:00:00Z", "")},
			want: StateEnded,
		},
		{
			// The premiere trap: Google's reference scopes liveStreamingDetails
			// to live BROADCASTS and never mentions premieres. If the object is
			// ever absent for a premiering video, a rule that required it would
			// silently drop every premiere. The nil check must be permissive.
			name: "premiere in progress with no liveStreamingDetails at all",
			v:    YTVideo{Snippet: snippet("live")},
			want: StateLive,
		},
		{
			name: "nil snippet is unknown, never not-live",
			v:    YTVideo{},
			want: StateUnknown,
		},
		{
			name: "unrecognised value is unknown",
			v:    YTVideo{Snippet: snippet("something-new")},
			want: StateUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.State(); got != tc.want {
				t.Errorf("State() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The measured delay is the number the whole shadow-mode soak exists to
// produce, so it must never be computed from a scheduled time. "Scheduled
// 19:00, actually started 21:30" would otherwise report a stream detected four
// seconds late as a two-and-a-half-hour miss.
func TestYTVideoStartedAtIgnoresScheduledTime(t *testing.T) {
	v := YTVideo{
		Snippet:              snippet("live"),
		LiveStreamingDetails: liveDetails("", "", "2026-01-01T19:00:00Z"),
	}
	if got := v.StartedAt(); !got.IsZero() {
		t.Fatalf("StartedAt() = %v, want zero; scheduledStartTime must never be used as the actual start", got)
	}

	v.LiveStreamingDetails = liveDetails("2026-01-01T21:30:00Z", "", "2026-01-01T19:00:00Z")
	want := time.Date(2026, 1, 1, 21, 30, 0, 0, time.UTC)
	if got := v.StartedAt(); !got.Equal(want) {
		t.Fatalf("StartedAt() = %v, want %v", got, want)
	}
}

func TestUploadsPlaylistID(t *testing.T) {
	if got := UploadsPlaylistID("UCabcdefghijklmnopqrstuv"); got != "UUabcdefghijklmnopqrstuv" {
		t.Errorf("UploadsPlaylistID = %q", got)
	}
	if got := UploadsPlaylistID("notachannel"); got != "" {
		t.Errorf("expected empty for a non-UC id, got %q", got)
	}
	if got := UploadsPlaylistID(""); got != "" {
		t.Errorf("expected empty for an empty id, got %q", got)
	}
}

func TestCanonicalURLs(t *testing.T) {
	// These exact forms are what internal/discord/webhook.go already builds,
	// so detection rows and announcement links agree.
	if got := YouTubeWatchURL("abc123"); got != "https://www.youtube.com/watch?v=abc123" {
		t.Errorf("YouTubeWatchURL = %q", got)
	}
	if got := TwitchChannelURL("DokiBird"); got != "https://twitch.tv/dokibird" {
		t.Errorf("TwitchChannelURL = %q", got)
	}
}

func newYouTubeTestServer(t *testing.T, handler http.HandlerFunc) *YouTubeClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewYouTubeClient("test-key")
	c.BaseURL = srv.URL
	return c
}

func TestVideosListBatchesAndDecodes(t *testing.T) {
	var gotIDs, gotPart string
	c := newYouTubeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotIDs = r.URL.Query().Get("id")
		gotPart = r.URL.Query().Get("part")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"id":                   "vid1",
					"snippet":              map[string]any{"liveBroadcastContent": "live", "title": "hello", "channelId": "UC1"},
					"liveStreamingDetails": map[string]any{"actualStartTime": "2026-01-01T19:00:00Z"},
				},
			},
		})
	})

	videos, err := c.VideosList(context.Background(), []string{"vid1", "vid2"})
	if err != nil {
		t.Fatalf("VideosList: %v", err)
	}
	if gotIDs != "vid1,vid2" {
		t.Errorf("id param = %q, want comma-joined ids", gotIDs)
	}
	// contentDetails is deliberately not requested: nothing reads duration, and
	// asking for it invites re-adding a premiere heuristic YouTube already broke.
	if gotPart != "snippet,liveStreamingDetails" {
		t.Errorf("part = %q, want snippet,liveStreamingDetails only", gotPart)
	}
	if len(videos) != 1 || videos[0].State() != StateLive {
		t.Fatalf("unexpected videos: %+v", videos)
	}
}

func TestVideosListRejectsOversizedBatch(t *testing.T) {
	c := NewYouTubeClient("k")
	ids := make([]string, videosListBatchSize+1)
	if _, err := c.VideosList(context.Background(), ids); err == nil {
		t.Fatal("expected an error for more than the documented batch size")
	}
}

// A 403 quotaExceeded must be distinguishable from any other 403, because only
// the former means "wait for the daily reset" rather than "the key is wrong".
func TestVideosListClassifiesQuotaExceeded(t *testing.T) {
	c := newYouTubeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"errors": []map[string]any{{"reason": "quotaExceeded"}},
			},
		})
	})
	_, err := c.VideosList(context.Background(), []string{"v"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isQuotaError(err) {
		t.Fatalf("error %v was not classified as a quota error", err)
	}

	other := newYouTubeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"errors": []map[string]any{{"reason": "keyInvalid"}}},
		})
	})
	if _, err := other.VideosList(context.Background(), []string{"v"}); isQuotaError(err) {
		t.Fatal("a non-quota 403 must not trip the quota breaker")
	}
}

// isQuotaError is the test's view of the classification the poller relies on
// to decide whether to trip the daily circuit breaker.
func isQuotaError(err error) bool { return errors.Is(err, errYouTubeQuota) }

// The API key must never reach an error string. Transport errors return a
// *url.Error that stringifies the whole URL, and that error is logged, stored
// on the leg health record the admin endpoint serves as JSON, and rendered
// into a Discord alert , so a key in the query string would leak to all three.
func TestYouTubeAPIKeyNeverAppearsInErrors(t *testing.T) {
	const sentinel = "SUPER-SECRET-API-KEY-SENTINEL"

	cases := []struct {
		name    string
		baseURL string
	}{
		// An unroutable host exercises the transport (*url.Error) path.
		{name: "transport failure", baseURL: "http://127.0.0.1:1"},
		// A malformed base exercises http.NewRequestWithContext's error path.
		{name: "malformed base url", baseURL: "http://[::1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewYouTubeClient(sentinel)
			c.BaseURL = tc.baseURL

			_, err := c.VideosList(context.Background(), []string{"vid"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("the api key leaked into the error: %v", err)
			}
		})
	}
}

// The key must travel as a header, which is what keeps it out of every
// URL-formatting path.
func TestYouTubeAPIKeyIsSentAsAHeader(t *testing.T) {
	var gotHeader, gotQuery string
	c := newYouTubeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-goog-api-key")
		gotQuery = r.URL.Query().Get("key")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})

	if _, err := c.VideosList(context.Background(), []string{"vid"}); err != nil {
		t.Fatalf("VideosList: %v", err)
	}
	if gotHeader != "test-key" {
		t.Errorf("X-goog-api-key = %q, want the configured key", gotHeader)
	}
	if gotQuery != "" {
		t.Errorf("key leaked into the query string as %q", gotQuery)
	}
}
