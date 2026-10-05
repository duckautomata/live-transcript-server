package livedetect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"live-transcript-server/internal/config"
)

// Two channels, so a test can give one a members-only playlist and leave the
// other without, the way most channels are.
const (
	membersDoki = "UCaaaaaaaaaaaaaaaaaaaaaa"
	membersMint = "UCbbbbbbbbbbbbbbbbbbbbbb"
)

// membersFakeAPI fakes the two Data API endpoints discovery and the state poll
// call. A playlist it has no entry for answers 404, as YouTube does for a
// members-only playlist that does not exist; a video it has no entry for is
// left out of the videos.list answer.
type membersFakeAPI struct {
	mu        sync.Mutex
	playlists map[string][]string
	failures  map[string]membersFailure
	videos    map[string]YTVideo
	requests  map[string]int
}

// membersFailure is a canned non-2xx answer for one playlist.
type membersFailure struct {
	status int
	body   string
}

func newMembersFakeAPI(t *testing.T) (*membersFakeAPI, string) {
	t.Helper()
	api := &membersFakeAPI{
		// Both channels have public uploads (empty unless a test fills them),
		// and neither has a members-only playlist unless a test adds one.
		playlists: map[string][]string{
			UploadsPlaylistID(membersDoki): {},
			UploadsPlaylistID(membersMint): {},
		},
		failures: map[string]membersFailure{},
		videos:   map[string]YTVideo{},
		requests: map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv.URL
}

func (a *membersFakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch r.URL.Path {
	case "/playlistItems":
		id := r.URL.Query().Get("playlistId")
		a.requests[id]++
		if f, ok := a.failures[id]; ok {
			w.WriteHeader(f.status)
			w.Write([]byte(f.body))
			return
		}
		ids, ok := a.playlists[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":404,"errors":[{"reason":"playlistNotFound"}]}}`))
			return
		}
		type item struct {
			ContentDetails struct {
				VideoID string `json:"videoId"`
			} `json:"contentDetails"`
		}
		items := make([]item, len(ids))
		for i, v := range ids {
			items[i].ContentDetails.VideoID = v
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})

	case "/videos":
		var items []YTVideo
		for _, id := range strings.Split(r.URL.Query().Get("id"), ",") {
			if v, ok := a.videos[id]; ok {
				items = append(items, v)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})

	default:
		http.NotFound(w, r)
	}
}

func (a *membersFakeAPI) setPlaylist(id string, videoIDs ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.playlists[id] = videoIDs
}

func (a *membersFakeAPI) failPlaylist(id string, status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[id] = membersFailure{status: status, body: body}
}

func (a *membersFakeAPI) setVideo(v YTVideo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.videos[v.ID] = v
}

func (a *membersFakeAPI) count(playlistID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests[playlistID]
}

// membersDetector builds a YouTube-only detector for both channels against
// the fake API. Nothing is started: tests drive the legs directly.
func membersDetector(t *testing.T, sink Sink, baseURL string) *Detector {
	t.Helper()
	return membersDetectorFor(t, sink, baseURL, []config.ChannelConfig{
		{Name: "doki", YouTubeChannelId: membersDoki},
		{Name: "mint", YouTubeChannelId: membersMint},
	})
}

func membersDetectorFor(t *testing.T, sink Sink, baseURL string, channels []config.ChannelConfig) *Detector {
	t.Helper()
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, channels, sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	d.youtube.BaseURL = baseURL
	return d
}

// membersBroadcast builds a videos.list item for a broadcast on doki's channel.
func membersBroadcast(id, lbc string, published, scheduled, started time.Time) YTVideo {
	v := notifVideo(id, lbc, published)
	v.Snippet.ChannelID = membersDoki
	format := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	v.LiveStreamingDetails = liveDetails(format(started), "", format(scheduled))
	return v
}

// membersMakeDue pulls an entry's next poll forward, standing in for the time
// a real state loop would wait.
func membersMakeDue(d *Detector, videoID string) {
	d.watch.mu.Lock()
	defer d.watch.mu.Unlock()
	if e, ok := d.watch.entries[videoID]; ok {
		e.NextDue = time.Now().Add(-time.Second)
	}
}

// membersHealth snapshots one leg's health record.
func membersHealth(d *Detector, mechanism string) legHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.health[mechanism]; ok {
		return *h
	}
	return legHealth{}
}

func TestMembersOnlyPlaylistID(t *testing.T) {
	if got := MembersOnlyPlaylistID("UCabcdefghijklmnopqrstuv"); got != "UUMOabcdefghijklmnopqrstuv" {
		t.Errorf("MembersOnlyPlaylistID = %q", got)
	}
	if got := MembersOnlyPlaylistID("notachannel"); got != "" {
		t.Errorf("expected empty for a non-UC id, got %q", got)
	}
	if got := MembersOnlyPlaylistID(""); got != "" {
		t.Errorf("expected empty for an empty id, got %q", got)
	}
}

// Discovery tells "this playlist does not exist" from a failure by the status
// alone, so the status has to survive the error - and the body, which echoes
// the request, must not.
func TestPlaylistItemsErrorKeepsOnlyTheStatus(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"no playlist for key=SECRETKEY","errors":[{"reason":"playlistNotFound"}]}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewYouTubeClient("SECRETKEY")
	c.BaseURL = srv.URL
	_, err := c.PlaylistItems(t.Context(), "UUMOabcdefghijklmnopqrstuv", 50)
	if err == nil {
		t.Fatal("a 404 must be an error")
	}
	if status := youtubeStatus(err); status != http.StatusNotFound {
		t.Errorf("youtubeStatus = %d, want 404", status)
	}
	if strings.Contains(err.Error(), "SECRETKEY") {
		t.Errorf("error %q leaks the response body", err)
	}

	q := got.URL.Query()
	if q.Get("playlistId") != "UUMOabcdefghijklmnopqrstuv" || q.Get("part") != "contentDetails" || q.Get("maxResults") != "50" {
		t.Errorf("query = %v", q)
	}
	if q.Has("key") || got.Header.Get("X-goog-api-key") != "SECRETKEY" {
		t.Error("the key must travel in the header, never the query string")
	}

	// Anything that is not a non-2xx answer has no status.
	if status := youtubeStatus(errYouTubeQuota); status != 0 {
		t.Errorf("youtubeStatus(quota) = %d, want 0", status)
	}
}

// The bug this exists for: members-only streams, waiting rooms and videos are
// never on the uploads playlist, so a detector that only read it never saw one
// and never sent a "scheduled" or "live" for any of them. Most channels have no
// members-only playlist at all, and that 404 must cost neither an alert nor a
// call every pass.
func TestDiscoveryReadsTheMembersOnlyPlaylist(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(UploadsPlaylistID(membersDoki), "doki-public")
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "doki-members")
	api.setPlaylist(UploadsPlaylistID(membersMint), "mint-public")
	// mint has no members-only playlist: the fake answers 404.

	d := membersDetector(t, &recordingSink{}, baseURL)
	d.youtubeDiscoverOnce()

	for id, wantMembers := range map[string]bool{"doki-public": false, "doki-members": true, "mint-public": false} {
		if !d.watch.Known(id) {
			t.Fatalf("%s was not seeded", id)
		}
		if got := d.watch.MembersOnly(id); got != wantMembers {
			t.Errorf("MembersOnly(%s) = %v, want %v", id, got, wantMembers)
		}
	}
	if got := d.watch.ChannelOf("doki-members"); got != "doki" {
		t.Errorf("ChannelOf(doki-members) = %q, want doki", got)
	}

	for _, mech := range []string{MechanismYouTubeDiscover, MechanismYouTubeMembersDiscover} {
		h := membersHealth(d, mech)
		if h.failures != 0 || h.lastErr != "" {
			t.Errorf("%s: a missing members-only playlist was recorded as a failure: failures=%d lastErr=%q",
				mech, h.failures, h.lastErr)
		}
		if h.lastSuccess.IsZero() {
			t.Errorf("%s: the pass must record the leg as healthy", mech)
		}
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 4 {
		t.Errorf("unitsSpent = %d, want 4 (two playlists for each of two channels)", spent)
	}

	// Inside the backoff, mint's missing playlist is not asked for again.
	d.youtubeDiscoverOnce()
	if n := api.count(MembersOnlyPlaylistID(membersMint)); n != 1 {
		t.Errorf("mint's members-only playlist was requested %d times, want 1 inside the backoff", n)
	}
	if n := api.count(MembersOnlyPlaylistID(membersDoki)); n != 2 {
		t.Errorf("doki's members-only playlist was requested %d times, want every pass", n)
	}
	if spent := d.gov.Snapshot(time.Now()).UnitsSpent; spent != 7 {
		t.Errorf("unitsSpent = %d, want 7 after a pass that skipped mint's members-only playlist", spent)
	}

	// Once the backoff lapses it is asked again, and a playlist that has
	// appeared in the meantime is read normally.
	api.setPlaylist(MembersOnlyPlaylistID(membersMint), "mint-members")
	d.ytMembersAbsent[membersMint] = membersAbsence{retry: time.Now().Add(-time.Second), status: http.StatusNotFound}
	d.youtubeDiscoverOnce()
	if !d.watch.MembersOnly("mint-members") {
		t.Error("a members-only playlist that appeared after the backoff must be read")
	}
	if _, still := d.ytMembersAbsent[membersMint]; still {
		t.Error("a channel whose members-only playlist answered must leave the backoff")
	}
}

// The uploads and members-only playlists never share an id, so whichever one
// lists it now decides. A creator who opens a members-only waiting room to
// everyone has made it an ordinary stream, and it must be queued like one.
func TestMembersOnlyFlagFollowsThePlaylist(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "frame")

	d := membersDetector(t, &recordingSink{}, baseURL)
	d.youtubeDiscoverOnce()
	if !d.watch.MembersOnly("frame") {
		t.Fatal("an id found on the members-only playlist must be flagged")
	}

	api.setPlaylist(MembersOnlyPlaylistID(membersDoki))
	api.setPlaylist(UploadsPlaylistID(membersDoki), "frame")
	d.youtubeDiscoverOnce()
	if d.watch.MembersOnly("frame") {
		t.Error("an id that moved to the uploads playlist must no longer be members-only")
	}

	api.setPlaylist(UploadsPlaylistID(membersDoki))
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "frame")
	d.youtubeDiscoverOnce()
	if !d.watch.MembersOnly("frame") {
		t.Error("an id that moved back to the members-only playlist must be flagged again")
	}

	// Caught mid-move, listed on both: public wins, because treating a public
	// stream as members-only loses its transcript.
	api.setPlaylist(UploadsPlaylistID(membersDoki), "frame")
	d.youtubeDiscoverOnce()
	if d.watch.MembersOnly("frame") {
		t.Error("an id listed on both playlists must be treated as public")
	}
}

// End to end: a members-only waiting room is found, announced as scheduled,
// and its go-live reported - flagged, so the sink knows not to queue it. A
// public stream in the same poll is the control.
func TestMembersOnlyStreamIsAnnouncedScheduledThenLive(t *testing.T) {
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	now := time.Now()
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "waiting-room")
	api.setPlaylist(UploadsPlaylistID(membersDoki), "public-live")
	api.setVideo(membersBroadcast("waiting-room", "upcoming", now.Add(-time.Minute), now.Add(2*time.Hour), time.Time{}))
	api.setVideo(membersBroadcast("public-live", "live", now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Minute)))

	d := membersDetector(t, sink, baseURL)
	d.youtubeDiscoverOnce()
	d.youtubeStateOnce()

	events := sink.videoEvents()
	if len(events) != 1 || events[0].Kind != VideoScheduled || events[0].ID != "waiting-room" {
		t.Fatalf("video events = %+v, want one scheduled event for the members-only waiting room", events)
	}
	if events[0].ChannelKey != "doki" {
		t.Errorf("scheduled event channel = %q, want doki", events[0].ChannelKey)
	}

	api.setVideo(membersBroadcast("waiting-room", "live", now.Add(-time.Minute), now.Add(2*time.Hour), now))
	membersMakeDue(d, "waiting-room")
	d.youtubeStateOnce()

	sink.mu.Lock()
	live := map[string]Broadcast{}
	for _, b := range sink.live {
		live[b.ID] = b
	}
	sink.mu.Unlock()

	members, ok := live["waiting-room"]
	if !ok {
		t.Fatalf("the members-only go-live was never reported; live = %+v", live)
	}
	if !members.MembersOnly {
		t.Error("a broadcast found on the members-only playlist must be reported as members-only")
	}
	if !members.SawScheduled {
		t.Error("the members-only stream was watched as a waiting room first")
	}
	if public, ok := live["public-live"]; !ok || public.MembersOnly {
		t.Errorf("public stream = %+v (reported %v), want reported and not members-only", public, ok)
	}
}

// If videos.list ever declines to answer for members-only ids, the id still
// sits on the members-only playlist. Dropping it without remembering it would
// mean re-seeding it on every pass and polling it hot for nothing, forever.
func TestMembersOnlyVideoThatIsNeverReturnedStopsChurning(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki), "hidden")
	// No videos entry: videos.list leaves it out of every answer.

	d := membersDetector(t, &recordingSink{}, baseURL)
	d.youtubeDiscoverOnce()
	for range ytMaxConsecutiveMisses {
		membersMakeDue(d, "hidden")
		d.youtubeStateOnce()
	}
	if d.watch.Known("hidden") {
		t.Fatalf("an id videos.list never returns must be dropped after %d misses", ytMaxConsecutiveMisses)
	}

	d.youtubeDiscoverOnce()
	if d.watch.Known("hidden") {
		t.Fatal("discovery must not re-seed a members-only id videos.list will not answer for")
	}

	// Made public, it is on the uploads playlist, and the members-only
	// negative cache must not stand in its way.
	api.setPlaylist(MembersOnlyPlaylistID(membersDoki))
	api.setPlaylist(UploadsPlaylistID(membersDoki), "hidden")
	d.youtubeDiscoverOnce()
	if !d.watch.Known("hidden") || d.watch.MembersOnly("hidden") {
		t.Error("a retired members-only id that turns up on the uploads playlist must be seeded as public")
	}
}

// Only "this playlist cannot be read" backs off quietly. A rate limit passes on
// its own and a server error may too, so both are retried next pass; a quota
// refusal also trips the budget. Every failure lands on the members-only leg,
// never on the uploads leg whose read just succeeded.
func TestMembersOnlyPlaylistFailuresAreClassified(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantBackoff bool
		wantFailure bool
		wantBlocked bool
	}{
		{name: "not found", status: http.StatusNotFound, body: `{}`, wantBackoff: true},
		{name: "forbidden", status: http.StatusForbidden,
			body: `{"error":{"errors":[{"reason":"playlistItemsNotAccessible"}]}}`, wantBackoff: true},
		{name: "rate limited", status: http.StatusForbidden,
			body: `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, wantFailure: true},
		{name: "quota", status: http.StatusForbidden,
			body: `{"error":{"errors":[{"reason":"quotaExceeded"}]}}`, wantFailure: true, wantBlocked: true},
		{name: "unsupported", status: http.StatusBadRequest,
			body: `{"error":{"errors":[{"reason":"playlistOperationUnsupported"}]}}`, wantFailure: true},
		{name: "server error", status: http.StatusInternalServerError, body: `{}`, wantFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, baseURL := newMembersFakeAPI(t)
			api.failPlaylist(MembersOnlyPlaylistID(membersDoki), tc.status, tc.body)
			api.setPlaylist(MembersOnlyPlaylistID(membersMint))

			d := membersDetector(t, &recordingSink{}, baseURL)
			d.youtubeDiscoverOnce()

			_, backoff := d.ytMembersAbsent[membersDoki]
			if backoff != tc.wantBackoff {
				t.Errorf("backoff = %v, want %v", backoff, tc.wantBackoff)
			}
			members := membersHealth(d, MechanismYouTubeMembersDiscover)
			if gotFailure := members.failures > 0; gotFailure != tc.wantFailure {
				t.Errorf("members-only failure recorded = %v (failures=%d lastErr=%q), want %v",
					gotFailure, members.failures, members.lastErr, tc.wantFailure)
			}
			if tc.wantFailure && !strings.Contains(members.lastErr, "doki") {
				t.Errorf("lastErr = %q, want it to name the channel", members.lastErr)
			}
			if uploads := membersHealth(d, MechanismYouTubeDiscover); uploads.failures != 0 || uploads.lastErr != "" {
				t.Errorf("the uploads leg was charged with a members-only failure: %+v", uploads)
			}
			if blocked := d.gov.Snapshot(time.Now()).UnitsBlocked; blocked != tc.wantBlocked {
				t.Errorf("unit budget blocked = %v, want %v", blocked, tc.wantBlocked)
			}
		})
	}
}

// One verdict per pass, under its own leg. Charged per channel against the
// uploads leg, a broken members-only playlist never alerted with a few channels
// (the uploads success reset the count every pass) and, with five or more,
// alerted and recovered inside every single pass.
func TestMembersOnlyFailuresAlertOncePerOutage(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	var channels []config.ChannelConfig
	for i := range 6 {
		letter := string(rune('c' + i))
		id := "UC" + strings.Repeat(letter, 22)
		channels = append(channels, config.ChannelConfig{Name: "ch" + letter, YouTubeChannelId: id})
		api.setPlaylist(UploadsPlaylistID(id))
		api.failPlaylist(MembersOnlyPlaylistID(id), http.StatusInternalServerError, `{}`)
	}
	d := membersDetectorFor(t, &recordingSink{}, baseURL, channels)

	d.youtubeDiscoverOnce()
	if h := membersHealth(d, MechanismYouTubeMembersDiscover); h.failures != 1 || h.alerted {
		t.Fatalf("after one pass: failures=%d alerted=%v, want one failure and no alert yet", h.failures, h.alerted)
	}
	for range failuresBeforeAlert {
		d.youtubeDiscoverOnce()
	}
	h := membersHealth(d, MechanismYouTubeMembersDiscover)
	if h.failures != failuresBeforeAlert+1 || !h.alerted {
		t.Errorf("after %d passes: failures=%d alerted=%v, want the outage counted per pass and alerted",
			failuresBeforeAlert+1, h.failures, h.alerted)
	}
	if !strings.Contains(h.lastErr, "6 members-only playlists could not be read") {
		t.Errorf("lastErr = %q, want the pass summarised", h.lastErr)
	}
	if uploads := membersHealth(d, MechanismYouTubeDiscover); uploads.failures != 0 || uploads.alerted {
		t.Errorf("the uploads leg must stay healthy: %+v", uploads)
	}
}

// The backoff remembers which answer put a channel there, so a channel that
// had no members-only playlist and is later refused one is noticed rather than
// swallowed by the backoff it was already in.
func TestMembersOnlyBackoffTracksTheAnswer(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setPlaylist(MembersOnlyPlaylistID(membersMint))

	d := membersDetector(t, &recordingSink{}, baseURL)
	d.youtubeDiscoverOnce()
	if a := d.ytMembersAbsent[membersDoki]; a.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", a.status)
	}

	api.failPlaylist(MembersOnlyPlaylistID(membersDoki), http.StatusForbidden,
		`{"error":{"errors":[{"reason":"playlistItemsNotAccessible"}]}}`)
	d.ytMembersAbsent[membersDoki] = membersAbsence{retry: time.Now().Add(-time.Second), status: http.StatusNotFound}
	d.youtubeDiscoverOnce()
	a := d.ytMembersAbsent[membersDoki]
	if a.status != http.StatusForbidden {
		t.Errorf("status = %d, want the new 403 recorded", a.status)
	}
	if !a.retry.After(time.Now().Add(ytMembersAbsentRetry - time.Minute)) {
		t.Errorf("retry = %v, want a fresh backoff", a.retry)
	}
}
