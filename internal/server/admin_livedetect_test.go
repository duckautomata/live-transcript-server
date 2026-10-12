package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/livedetect"
)

func TestParseYouTubeVideoRef(t *testing.T) {
	const id = "FdwrQid9ulg"
	ok := []string{
		id,
		"  " + id + "\n",
		"https://www.youtube.com/watch?v=" + id,
		"https://www.youtube.com/watch?v=" + id + "&t=120s&si=abc",
		"https://youtube.com/watch?feature=share&v=" + id,
		"http://m.youtube.com/watch?v=" + id,
		"https://music.youtube.com/watch?v=" + id,
		"www.youtube.com/watch?v=" + id,
		"youtube.com/watch?v=" + id,
		"https://youtu.be/" + id,
		"https://youtu.be/" + id + "?si=tracking",
		"youtu.be/" + id,
		"https://www.youtube.com/live/" + id,
		"https://www.youtube.com/live/" + id + "?si=abc",
		"https://youtube.com/shorts/" + id,
		"https://www.youtube.com/shorts/" + id + "/",
		"https://www.youtube.com/embed/" + id,
		"https://www.youtube-nocookie.com/embed/" + id,
		"HTTPS://WWW.YOUTUBE.COM/watch?v=" + id,
	}
	for _, in := range ok {
		got, err := parseYouTubeVideoRef(in)
		if err != nil || got != id {
			t.Errorf("parseYouTubeVideoRef(%q) = %q, %v; want %q", in, got, err, id)
		}
	}

	bad := []string{
		"",
		"   ",
		"short",
		id + "x",
		"https://www.youtube.com/watch",
		"https://www.youtube.com/watch?v=short",
		"https://www.youtube.com/watch?v=" + id + "x",
		"https://www.youtube.com/@beri/live",
		"https://www.youtube.com/channel/UCaaaaaaaaaaaaaaaaaaaaaa",
		"https://www.youtube.com/live",
		"https://www.youtube.com/playlist?list=PL123",
		"https://youtu.be/",
		"https://evil.example/watch?v=" + id,
		"https://youtube.com.evil.example/watch?v=" + id,
		"https://twitch.tv/dokibird",
		"javascript:alert(1)",
		"https://www.youtube.com/watch?v=../../etc",
	}
	for _, in := range bad {
		if got, err := parseYouTubeVideoRef(in); err == nil {
			t.Errorf("parseYouTubeVideoRef(%q) = %q, want an error", in, got)
		}
	}

	// A channel link gets an answer that says what to paste instead.
	if _, err := parseYouTubeVideoRef("https://www.youtube.com/@beri/live"); err == nil || !strings.Contains(err.Error(), "channel link") {
		t.Errorf("channel link error = %v, want it called a channel link", err)
	}
}

// fakeManualDetector stands in for the discovery goroutine. Each call is
// recorded so a test can prove a refused request never reached it.
type fakeManualDetector struct {
	check    func(channelKey, videoID string) (livedetect.VideoCheck, error)
	discover func() (livedetect.DiscoverySummary, error)
	watches  bool

	checks        []string
	discoverCalls int
	// discoverKeys is the channel each pass was run for, which the detector
	// counts against that channel's daily cap.
	discoverKeys []string
}

func (f *fakeManualDetector) CheckYouTubeVideo(_ context.Context, channelKey, videoID string) (livedetect.VideoCheck, error) {
	f.checks = append(f.checks, channelKey+"="+videoID)
	if f.check == nil {
		return livedetect.VideoCheck{}, errors.New("unexpected check")
	}
	return f.check(channelKey, videoID)
}

func (f *fakeManualDetector) RunDiscoveryNow(_ context.Context, channelKey string) (livedetect.DiscoverySummary, error) {
	f.discoverCalls++
	f.discoverKeys = append(f.discoverKeys, channelKey)
	if f.discover == nil {
		return livedetect.DiscoverySummary{}, errors.New("unexpected discovery")
	}
	return f.discover()
}

func (f *fakeManualDetector) WatchesYouTube(string) bool { return f.watches }

const testVideoID = "FdwrQid9ulg"

// Every error the detector can answer with has one status, and a limit says
// when to come back. None of them is an admin action, so none is audited.
func TestAdminLiveDetectErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		want       int
		retryAfter string
	}{
		{"too soon", &livedetect.TooSoonError{Reason: "slow down", RetryAfter: 42*time.Second + 400*time.Millisecond}, http.StatusTooManyRequests, "42"},
		{"sub-second wait", &livedetect.TooSoonError{Reason: "slow down", RetryAfter: 100 * time.Millisecond}, http.StatusTooManyRequests, "1"},
		{"other channel", &livedetect.OtherChannelError{VideoID: testVideoID, ChannelID: "UCbbbbbbbbbbbbbbbbbbbbbb", ChannelKey: "mint"}, http.StatusBadRequest, ""},
		{"invalid id", livedetect.ErrInvalidVideoID, http.StatusBadRequest, ""},
		{"not found", livedetect.ErrVideoNotFound, http.StatusNotFound, ""},
		{"detection disabled", livedetect.ErrDetectionDisabled, http.StatusServiceUnavailable, ""},
		{"youtube leg off", livedetect.ErrYouTubeNotRunning, http.StatusServiceUnavailable, ""},
		{"no youtube channel", livedetect.ErrNoYouTubeChannel, http.StatusServiceUnavailable, ""},
		{"timeout", livedetect.ErrManualTimeout, http.StatusGatewayTimeout, ""},
		{"api failure", fmt.Errorf("%w: youtube /videos returned status 500", livedetect.ErrYouTubeAPI), http.StatusBadGateway, ""},
		{"anything else", errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, mux := setupTestApp(t, []string{"doki"})
			fake := &fakeManualDetector{
				check:    func(string, string) (livedetect.VideoCheck, error) { return livedetect.VideoCheck{}, tc.err },
				discover: func() (livedetect.DiscoverySummary, error) { return livedetect.DiscoverySummary{}, tc.err },
			}
			app.ManualDetect = fake
			payloads := captureAdminWebhook(t, app)
			before := app.Channels["doki"].AdminChangeCounter.Load()

			for _, req := range []struct {
				path string
				body any
			}{
				{"/doki/admin/livedetect/check", map[string]string{"url": "https://youtu.be/" + testVideoID}},
				{"/doki/admin/livedetect/discover", nil},
			} {
				rec := adminReq(t, mux, http.MethodPost, req.path, "admin-doki", req.body)
				if rec.Code != tc.want {
					t.Fatalf("%s: status=%d want %d body=%s", req.path, rec.Code, tc.want, rec.Body.String())
				}
				if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
					t.Errorf("%s: Retry-After=%q want %q", req.path, got, tc.retryAfter)
				}
				var body manualDetectError
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("%s: decode: %v", req.path, err)
				}
				if body.OK || body.Error != tc.err.Error() {
					t.Errorf("%s: body=%+v, want ok=false and the detector's message", req.path, body)
				}
				if tc.retryAfter != "" && strconv.Itoa(body.RetryAfterSeconds) != tc.retryAfter {
					t.Errorf("%s: retryAfterSeconds=%d want %s", req.path, body.RetryAfterSeconds, tc.retryAfter)
				}
			}
			if !slices.Equal(fake.checks, []string{"doki=" + testVideoID}) {
				t.Errorf("checks = %v, want the parsed id for this channel", fake.checks)
			}
			if after := app.Channels["doki"].AdminChangeCounter.Load(); after != before {
				t.Errorf("a refused request bumped the admin change counter (%d -> %d)", before, after)
			}
			expectNoAdminAudit(t, payloads)
		})
	}
}

// Input the handler can judge on its own is refused before the detector -
// and the discovery goroutine behind it - is bothered at all.
func TestAdminLiveDetectCheckRefusesBadInputItself(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	fake := &fakeManualDetector{}
	app.ManualDetect = fake

	for _, body := range []any{
		map[string]string{"url": ""},
		map[string]string{"url": "https://www.youtube.com/@beri/live"},
		map[string]string{"url": "https://twitch.tv/dokibird"},
		map[string]int{"url": 5},
	} {
		rec := adminReq(t, mux, http.MethodPost, "/doki/admin/livedetect/check", "admin-doki", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %v: status=%d want 400", body, rec.Code)
		}
		var resp manualDetectError
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil || resp.Error == "" {
			t.Errorf("body %v: want a JSON error, got %v / %+v", body, err, resp)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/doki/admin/livedetect/check", strings.NewReader("{not json"))
	req.Header.Set("X-Admin-Key", "admin-doki")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d want 400", rec.Code)
	}
	if len(fake.checks) != 0 {
		t.Errorf("the detector was asked %v for input the handler should have refused", fake.checks)
	}
}

func TestAdminLiveDetectCheckSucceeds(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.ManualDetect = &fakeManualDetector{
		check: func(channelKey, videoID string) (livedetect.VideoCheck, error) {
			return livedetect.VideoCheck{
				VideoID: videoID, ChannelKey: channelKey, URL: livedetect.YouTubeWatchURL(videoID),
				State: "live", Title: "late stream", ActualStart: 1760212980,
				Unretired: true, Tracked: true, ChecksRemaining: 49,
			}, nil
		},
	}
	payloads := captureAdminWebhook(t, app)
	before := app.Channels["doki"].AdminChangeCounter.Load()

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/livedetect/check", "admin-doki",
		map[string]string{"url": "https://www.youtube.com/live/" + testVideoID + "?si=x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The check's fields sit at the top level next to ok, as the page reads
	// them.
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != true || got["videoId"] != testVideoID || got["state"] != "live" ||
		got["unretired"] != true || got["tracked"] != true || got["checksRemaining"] != float64(49) {
		t.Errorf("response = %v", got)
	}
	if after := app.Channels["doki"].AdminChangeCounter.Load(); after <= before {
		t.Error("a successful check must wake the admin page")
	}

	title, fields := waitAdminAudit(t, payloads)
	if title != "Admin: Checked a YouTube video for live detection" {
		t.Errorf("title=%q", title)
	}
	for name, want := range map[string]string{
		"Video":                     livedetect.YouTubeWatchURL(testVideoID),
		"State":                     "live",
		"Was Hidden From Discovery": "yes",
		"Was Already Tracked":       "no",
		"Endpoint":                  "POST /doki/admin/livedetect/check",
	} {
		if fields[name] != want {
			t.Errorf("field %q=%q want %q", name, fields[name], want)
		}
	}
}

// A pass covers every channel, but a channel's page lists only its own finds.
func TestAdminLiveDetectDiscoverListsOnlyThisChannel(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})
	fake := &fakeManualDetector{
		discover: func() (livedetect.DiscoverySummary, error) {
			return livedetect.DiscoverySummary{
				Channels: 2, PlaylistsRead: 3, UnitsSpent: 4,
				Found:            map[string][]string{"doki": {"a"}, "mint": {"b", "c"}},
				FoundMembersOnly: map[string][]string{"mint": {"secret"}},
			}, nil
		},
	}
	app.ManualDetect = fake
	payloads := captureAdminWebhook(t, app)
	before := app.Channels["doki"].AdminChangeCounter.Load()

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/livedetect/discover", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), `"b"`) {
		t.Errorf("body %s lists another channel's ids", rec.Body.String())
	}
	var resp LiveDetectDiscoverResponse
	if err := json.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Channels != 2 || resp.PlaylistsRead != 3 || resp.UnitsSpent != 4 {
		t.Errorf("response = %+v", resp)
	}
	if !slices.Equal(resp.Found, []string{"a"}) || resp.FoundMembersOnly == nil || len(resp.FoundMembersOnly) != 0 || resp.FoundOtherChannels != 3 {
		t.Errorf("found = %v / %v / %d other, want [a] / [] / 3", resp.Found, resp.FoundMembersOnly, resp.FoundOtherChannels)
	}
	if after := app.Channels["doki"].AdminChangeCounter.Load(); after <= before {
		t.Error("a discovery pass must wake the admin page")
	}
	if !slices.Equal(fake.discoverKeys, []string{"doki"}) {
		t.Errorf("pass run for %v, want [doki]: the daily cap is counted per channel", fake.discoverKeys)
	}

	title, fields := waitAdminAudit(t, payloads)
	if title != "Admin: Ran live detection discovery" {
		t.Errorf("title=%q", title)
	}
	if fields["New Videos Here"] != "1" || fields["Quota Units"] != "4" {
		t.Errorf("fields = %v", fields)
	}
}

// Through the real detector: with detection off, or with the YouTube leg not
// running, both endpoints answer 503 at once rather than waiting out the
// 45-second timeout for a goroutine that does not exist. The admin info says
// whether the page should offer the controls at all.
func TestAdminLiveDetectWithoutARunningLeg(t *testing.T) {
	adminInfo := func(t *testing.T, mux *http.ServeMux) AdminInfoResponse {
		t.Helper()
		rec := adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("info: status=%d", rec.Code)
		}
		var info AdminInfoResponse
		if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
			t.Fatalf("decode info: %v", err)
		}
		return info
	}
	refused := func(t *testing.T, mux *http.ServeMux) {
		t.Helper()
		start := time.Now()
		for _, path := range []string{"/doki/admin/livedetect/check", "/doki/admin/livedetect/discover"} {
			rec := adminReq(t, mux, http.MethodPost, path, "admin-doki", map[string]string{"url": testVideoID})
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: status=%d want 503, body=%s", path, rec.Code, rec.Body.String())
			}
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("took %s to refuse", waited)
		}
	}

	t.Run("detection disabled", func(t *testing.T) {
		_, mux := setupTestApp(t, []string{"doki"})
		refused(t, mux)
		if adminInfo(t, mux).LiveDetectYouTube {
			t.Error("liveDetectYouTube must be false with detection off")
		}
	})
	t.Run("youtube leg not started", func(t *testing.T) {
		app, mux := setupDetectApp(t)
		app.Channels["doki"].AdminKey = "admin-doki"
		refused(t, mux)
		if !adminInfo(t, mux).LiveDetectYouTube {
			t.Error("liveDetectYouTube must be true for a channel with a youtubeChannelId and the leg enabled")
		}
	})
}

func TestAdminLiveDetectNeedsTheAdminKey(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki", "mint"})
	fake := &fakeManualDetector{}
	app.ManualDetect = fake
	for _, path := range []string{"/doki/admin/livedetect/check", "/doki/admin/livedetect/discover"} {
		for _, key := range []string{"", "admin-mint"} {
			rec := adminReq(t, mux, http.MethodPost, path, key, map[string]string{"url": testVideoID})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with key %q: status=%d want 403", path, key, rec.Code)
			}
		}
	}
	if len(fake.checks) != 0 || fake.discoverCalls != 0 {
		t.Error("an unauthenticated request reached the detector")
	}
}
