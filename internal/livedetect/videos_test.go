package livedetect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"live-transcript-server/internal/config"
)

// notifChannelID is the YouTube channel id testChannels() configures for "doki".
const notifChannelID = "UCaaaaaaaaaaaaaaaaaaaaaa"

// notifVideo builds one videos.list item for the configured channel. The
// shared snippet helper leaves publishedAt blank, and the announcement paths
// key on it, so this sets it explicitly. A zero publishedAt is left unset.
func notifVideo(id, lbc string, publishedAt time.Time) YTVideo {
	v := YTVideo{ID: id, Snippet: snippet(lbc)}
	v.Snippet.ChannelID = notifChannelID
	v.Snippet.Title = "title of " + id
	if !publishedAt.IsZero() {
		v.Snippet.PublishedAt = publishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// notifShortsSite fakes youtube.com's /shorts/{id} behaviour: the answer for
// a given id is whatever the test says it is, and every request is counted so
// a test can prove the probe was (or was not) consulted.
type notifShortsSite struct {
	URL    string
	hits   atomic.Int32
	answer func(id string) (status int, location string)
}

func notifNewShortsSite(t *testing.T, answer func(id string) (int, string)) *notifShortsSite {
	t.Helper()
	site := &notifShortsSite{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.hits.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/shorts/")
		if id == r.URL.Path {
			http.Error(w, "not a shorts url", http.StatusNotFound)
			return
		}
		status, location := site.answer(id)
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
	}))
	// Registered before the detector is built, so the detector's Close (and
	// its wait for the classification goroutine) runs before the site goes.
	t.Cleanup(srv.Close)
	site.URL = srv.URL
	return site
}

// notifAnswerVideo is youtube.com's answer for an ordinary video.
func notifAnswerVideo(id string) (int, string) {
	return http.StatusSeeOther, "/watch?v=" + id
}

// notifAnswerShort is youtube.com's answer for a short.
func notifAnswerShort(string) (int, string) { return http.StatusOK, "" }

// notifYouTubeDetector builds a detector with only the YouTube leg enabled
// and points its shorts probe at the fake site. Nothing is started: tests
// drive applyYouTubeVideo directly.
func notifYouTubeDetector(t *testing.T, sink Sink, site *notifShortsSite) *Detector {
	t.Helper()
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, testChannels(), sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if site != nil {
		d.shorts.BaseURL = site.URL
	}
	return d
}

// notifWaitFor polls cond until it holds or a deadline passes.
func notifWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// notifSettle gives any spawned goroutine a moment to do the wrong thing, so
// a "nothing happened" assertion is worth something.
func notifSettle() { time.Sleep(60 * time.Millisecond) }

// notifFlakySink fails ObserveVideo a configured number of times before
// behaving like a recordingSink, and counts every attempt.
type notifFlakySink struct {
	recordingSink
	failuresLeft atomic.Int32
	attempts     atomic.Int32
}

func (s *notifFlakySink) ObserveVideo(ctx context.Context, v VideoEvent) error {
	s.attempts.Add(1)
	if s.failuresLeft.Add(-1) >= 0 {
		return errFailingSink
	}
	return s.recordingSink.ObserveVideo(ctx, v)
}

func TestYTVideoPublishedAt(t *testing.T) {
	if got := (YTVideo{}).PublishedAt(); !got.IsZero() {
		t.Errorf("nil snippet: PublishedAt = %v, want zero", got)
	}
	if got := (YTVideo{Snippet: snippet("none")}).PublishedAt(); !got.IsZero() {
		t.Errorf("empty publishedAt: got %v, want zero", got)
	}

	garbled := YTVideo{Snippet: snippet("none")}
	garbled.Snippet.PublishedAt = "yesterday-ish"
	if got := garbled.PublishedAt(); !got.IsZero() {
		t.Errorf("unparseable publishedAt: got %v, want zero rather than a guess", got)
	}

	want := time.Date(2026, 9, 10, 6, 30, 0, 0, time.UTC)
	v := notifVideo("vid", "none", want)
	if got := v.PublishedAt(); !got.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", got, want)
	}
}

// liveStreamingDetails is attached to exactly the videos that are, were or
// will be broadcasts. Its presence, not its contents, is the discriminator: a
// finished stream's VOD keeps an object whose fields are all populated, and a
// scheduled frame has one with only scheduledStartTime.
func TestYTVideoIsBroadcast(t *testing.T) {
	if (YTVideo{Snippet: snippet("none")}).IsBroadcast() {
		t.Error("a plain upload has no liveStreamingDetails and is not a broadcast")
	}
	if (YTVideo{}).IsBroadcast() {
		t.Error("an item with nothing on it is not a broadcast")
	}
	upcoming := YTVideo{Snippet: snippet("upcoming"), LiveStreamingDetails: liveDetails("", "", "2026-01-01T19:00:00Z")}
	if !upcoming.IsBroadcast() {
		t.Error("a scheduled frame is a broadcast")
	}
	vod := YTVideo{Snippet: snippet("none"), LiveStreamingDetails: liveDetails("2026-01-01T19:00:00Z", "2026-01-01T21:00:00Z", "2026-01-01T19:00:00Z")}
	if !vod.IsBroadcast() {
		t.Error("a finished stream's VOD keeps liveStreamingDetails and stays a broadcast")
	}
	// Even an empty object counts: presence is the signal.
	bare := YTVideo{Snippet: snippet("none"), LiveStreamingDetails: liveDetails("", "", "")}
	if !bare.IsBroadcast() {
		t.Error("an empty liveStreamingDetails object still marks a broadcast")
	}
}

// The first discovery pass after enabling reads the whole uploads playlist.
// Without a recency bound it would announce every video on it.
func TestRecentlyPublishedWindow(t *testing.T) {
	now := base

	if recentlyPublished(time.Time{}, now) {
		t.Error("a video with no publish time can never be new")
	}
	if !recentlyPublished(now.Add(-time.Minute), now) {
		t.Error("published a minute ago is new")
	}
	if !recentlyPublished(now.Add(-announceMaxAge), now) {
		t.Errorf("published exactly %v ago is still inside the window", announceMaxAge)
	}
	if recentlyPublished(now.Add(-announceMaxAge-time.Second), now) {
		t.Errorf("published %v ago is outside the window", announceMaxAge+time.Second)
	}
	if recentlyPublished(now.Add(-365*24*time.Hour), now) {
		t.Error("the back catalogue is not news")
	}
	// A publish time slightly ahead of our clock is clock skew, not the future.
	if !recentlyPublished(now.Add(time.Minute), now) {
		t.Error("a publish time a minute ahead of our clock must still count as new")
	}
}

func TestShortsProbeClassifies(t *testing.T) {
	site := notifNewShortsSite(t, func(id string) (int, string) {
		switch id {
		case "short1":
			return notifAnswerShort(id)
		case "video1":
			return notifAnswerVideo(id)
		case "consent1":
			// The interstitial keeps the original target url-encoded in a query
			// parameter, so the literal "/watch" never appears.
			return http.StatusFound, "https://consent.youtube.com/m?continue=https%3A%2F%2Fwww.youtube.com%2Fshorts%2Fconsent1&gl=DE"
		case "limited1":
			return http.StatusTooManyRequests, ""
		default:
			return http.StatusNotFound, ""
		}
	})
	p := NewShortsProbe()
	p.BaseURL = site.URL
	ctx := context.Background()

	t.Run("200 is a short", func(t *testing.T) {
		isShort, err := p.IsShort(ctx, "short1")
		if err != nil {
			t.Fatalf("IsShort: %v", err)
		}
		if !isShort {
			t.Error("a 200 from /shorts/ means the video is a short")
		}
	})

	t.Run("redirect to /watch is a video", func(t *testing.T) {
		isShort, err := p.IsShort(ctx, "video1")
		if err != nil {
			t.Fatalf("IsShort: %v", err)
		}
		if isShort {
			t.Error("a redirect to /watch means an ordinary video")
		}
	})

	t.Run("consent interstitial is unknown", func(t *testing.T) {
		isShort, err := p.IsShort(ctx, "consent1")
		if !errors.Is(err, ErrShortUnknown) {
			t.Fatalf("err = %v, want ErrShortUnknown: a consent page is neither answer", err)
		}
		if isShort {
			t.Error("an unknown answer must never read as a short")
		}
		if !strings.Contains(err.Error(), "consent.youtube.com") {
			t.Errorf("the error should name the redirect host for the log, got %q", err)
		}
		if strings.Contains(err.Error(), "continue=") {
			t.Errorf("the error should carry only the host, not the whole redirect target: %q", err)
		}
	})

	t.Run("rate limit is unknown", func(t *testing.T) {
		isShort, err := p.IsShort(ctx, "limited1")
		if !errors.Is(err, ErrShortUnknown) {
			t.Fatalf("err = %v, want ErrShortUnknown", err)
		}
		if isShort {
			t.Error("a 429 must never read as a short")
		}
		if !strings.Contains(err.Error(), "429") {
			t.Errorf("the error should carry the status, got %q", err)
		}
	})

	t.Run("nil probe and empty id are unknown without a request", func(t *testing.T) {
		before := site.hits.Load()
		var nilProbe *ShortsProbe
		if isShort, err := nilProbe.IsShort(ctx, "short1"); isShort || !errors.Is(err, ErrShortUnknown) {
			t.Errorf("nil probe: (%v, %v), want (false, ErrShortUnknown)", isShort, err)
		}
		if isShort, err := p.IsShort(ctx, ""); isShort || !errors.Is(err, ErrShortUnknown) {
			t.Errorf("empty id: (%v, %v), want (false, ErrShortUnknown)", isShort, err)
		}
		if got := site.hits.Load(); got != before {
			t.Errorf("made %d requests for inputs that cannot be classified", got-before)
		}
	})

	// The probe must not follow the redirect: the redirect IS the answer, and
	// following it to a 200 watch page would classify every video as a short.
	if got := site.hits.Load(); got != 4 {
		t.Errorf("site saw %d requests, want exactly 4 (one per classified id, no redirects followed)", got)
	}
}

// The probe runs on the detector's context so shutdown can abandon it. A
// youtube.com that hangs must not hold the classification goroutine for the
// full probe timeout after Close.
func TestShortsProbeRespectsContextCancellation(t *testing.T) {
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(srv.Close)

	p := NewShortsProbe()
	p.BaseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)

	start := time.Now()
	isShort, err := p.IsShort(ctx, "hanging")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrShortUnknown) {
		t.Fatalf("err = %v, want ErrShortUnknown after cancellation", err)
	}
	if isShort {
		t.Error("a cancelled probe must not read as a short")
	}
	if elapsed > 2*time.Second {
		t.Errorf("IsShort took %v after cancellation; it must return promptly, not wait out the %v timeout", elapsed, shortsProbeTimeout)
	}
}

func TestClaimAnnounceLatch(t *testing.T) {
	w := newWatchlist()

	if w.ClaimAnnounce("untracked", VideoScheduled) {
		t.Error("an id not on the watchlist has nothing to latch and must not be claimable")
	}
	w.ReleaseAnnounce("untracked", VideoScheduled) // must not panic

	w.Seed("v1", "doki", base, false)
	if !w.ClaimAnnounce("v1", VideoScheduled) {
		t.Fatal("the first claim must succeed")
	}
	if w.ClaimAnnounce("v1", VideoScheduled) {
		t.Error("the second claim of the same (video, kind) must fail")
	}

	// Kinds are independent: the same frame can be announced as scheduled
	// and later, if it turns out to be something else, as an upload.
	if !w.ClaimAnnounce("v1", VideoUpload) {
		t.Error("a different kind on the same id is a separate latch")
	}
	if w.ClaimAnnounce("v1", VideoUpload) {
		t.Error("the upload latch must now be taken too")
	}

	// A release hands back only the kind released.
	w.ReleaseAnnounce("v1", VideoScheduled)
	if !w.ClaimAnnounce("v1", VideoScheduled) {
		t.Error("after a release the scheduled latch must be claimable again")
	}
	if w.ClaimAnnounce("v1", VideoUpload) {
		t.Error("releasing the scheduled latch must not release the upload latch")
	}
}

// A waiting room that has just gone public is announced once. The poll is
// level-triggered and sees the same frame every few seconds until it starts,
// so the latch is what keeps that from becoming a notification per cycle.
func TestScheduledFrameIsAnnouncedOnce(t *testing.T) {
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, nil)

	now := time.Now()
	published := now.Add(-10 * time.Minute).UTC().Truncate(time.Second)
	scheduled := now.Add(2 * time.Hour).UTC().Truncate(time.Second)
	v := notifVideo("frame1", "upcoming", published)
	v.LiveStreamingDetails = liveDetails("", "", scheduled.Format(time.RFC3339))

	d.watch.Seed("frame1", "doki", now, true)
	for range 3 {
		d.applyYouTubeVideo(context.Background(), v, now)
	}

	events := sink.videoEvents()
	if len(events) != 1 {
		t.Fatalf("got %d video events across three polls, want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != VideoScheduled {
		t.Errorf("Kind = %q, want %q", ev.Kind, VideoScheduled)
	}
	if ev.Platform != PlatformYouTube || ev.ChannelKey != "doki" || ev.ID != "frame1" {
		t.Errorf("event identity = %s/%s/%s, want youtube/doki/frame1", ev.Platform, ev.ChannelKey, ev.ID)
	}
	if ev.URL != YouTubeWatchURL("frame1") {
		t.Errorf("URL = %q, want the watch url", ev.URL)
	}
	if ev.Title != "title of frame1" {
		t.Errorf("Title = %q", ev.Title)
	}
	if !ev.PublishedAt.Equal(published) {
		t.Errorf("PublishedAt = %v, want %v", ev.PublishedAt, published)
	}
	if !ev.ScheduledAt.Equal(scheduled) {
		t.Errorf("ScheduledAt = %v, want %v", ev.ScheduledAt, scheduled)
	}
	if len(sink.live) != 0 || len(sink.endedIDs()) != 0 {
		t.Error("a scheduled frame is neither live nor ended")
	}
}

// A frame that went public days ago is already on the uploads playlist when
// detection is enabled. It is not news, however soon it is scheduled to start.
func TestOldScheduledFrameIsNotAnnounced(t *testing.T) {
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, nil)

	now := time.Now()
	v := notifVideo("old-frame", "upcoming", now.Add(-announceMaxAge-time.Hour))
	v.LiveStreamingDetails = liveDetails("", "", now.Add(30*time.Minute).UTC().Format(time.RFC3339))

	d.watch.Seed("old-frame", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)

	if events := sink.videoEvents(); len(events) != 0 {
		t.Fatalf("an old frame must not be announced, got %+v", events)
	}
	// And nothing was latched, so the watchlist still reflects a clean state.
	if !d.watch.ClaimAnnounce("old-frame", VideoScheduled) {
		t.Error("a frame that was never announced must not hold the latch")
	}

	// A frame with no publish time at all is treated the same way.
	sink2 := &recordingSink{}
	d2 := notifYouTubeDetector(t, sink2, nil)
	unknown := notifVideo("undated", "upcoming", time.Time{})
	unknown.LiveStreamingDetails = liveDetails("", "", now.Add(30*time.Minute).UTC().Format(time.RFC3339))
	d2.watch.Seed("undated", "doki", now, true)
	d2.applyYouTubeVideo(context.Background(), unknown, now)
	if events := sink2.videoEvents(); len(events) != 0 {
		t.Fatalf("a frame with no publish time must not be announced, got %+v", events)
	}
}

// An ordinary upload reads as "none" with no liveStreamingDetails from its very
// first poll. It is classified off the poll goroutine and announced as a video
// when youtube.com redirects /shorts/{id} to the watch page.
func TestUploadIsAnnouncedAsVideo(t *testing.T) {
	site := notifNewShortsSite(t, notifAnswerVideo)
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	published := now.Add(-5 * time.Minute).UTC().Truncate(time.Second)
	v := notifVideo("upload1", "none", published)

	d.watch.Seed("upload1", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)

	notifWaitFor(t, "the upload to be announced", func() bool { return len(sink.videoEvents()) == 1 })
	ev := sink.videoEvents()[0]
	if ev.Kind != VideoUpload {
		t.Errorf("Kind = %q, want %q", ev.Kind, VideoUpload)
	}
	if ev.URL != YouTubeWatchURL("upload1") {
		t.Errorf("URL = %q, want the watch url", ev.URL)
	}
	if ev.ChannelKey != "doki" || ev.ID != "upload1" || ev.Platform != PlatformYouTube {
		t.Errorf("event identity = %s/%s/%s, want youtube/doki/upload1", ev.Platform, ev.ChannelKey, ev.ID)
	}
	if ev.Title != "title of upload1" {
		t.Errorf("Title = %q", ev.Title)
	}
	if !ev.PublishedAt.Equal(published) {
		t.Errorf("PublishedAt = %v, want %v", ev.PublishedAt, published)
	}
	if !ev.ScheduledAt.IsZero() {
		t.Errorf("ScheduledAt = %v, want zero for an upload", ev.ScheduledAt)
	}
	if got := site.hits.Load(); got != 1 {
		t.Errorf("the probe was consulted %d times, want once", got)
	}

	// The poll keeps seeing the same upload as "none" on every cycle; the
	// latch must hold, and the probe must not be asked again.
	for range 3 {
		d.applyYouTubeVideo(context.Background(), v, now.Add(time.Minute))
	}
	notifSettle()
	if events := sink.videoEvents(); len(events) != 1 {
		t.Fatalf("got %d events after repeated polls, want the single announcement to hold: %+v", len(events), events)
	}
	if got := site.hits.Load(); got != 1 {
		t.Errorf("the probe was consulted %d times across repeated polls, want once", got)
	}
	// An upload was never live, so its "ended" state is not an end worth
	// reporting.
	if ended := sink.endedIDs(); len(ended) != 0 {
		t.Errorf("an upload must not be reported as an ended broadcast, got %v", ended)
	}
}

func TestUploadIsAnnouncedAsShort(t *testing.T) {
	site := notifNewShortsSite(t, notifAnswerShort)
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	v := notifVideo("short1", "none", now.Add(-5*time.Minute))

	d.watch.Seed("short1", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)

	notifWaitFor(t, "the short to be announced", func() bool { return len(sink.videoEvents()) == 1 })
	ev := sink.videoEvents()[0]
	if ev.Kind != VideoShort {
		t.Errorf("Kind = %q, want %q", ev.Kind, VideoShort)
	}
	if ev.URL != YouTubeShortsURL("short1") {
		t.Errorf("URL = %q, want the shorts url", ev.URL)
	}
	if ev.ChannelKey != "doki" || ev.ID != "short1" {
		t.Errorf("event identity = %s/%s, want doki/short1", ev.ChannelKey, ev.ID)
	}
}

// When youtube.com will not say, the video is announced as an ordinary upload:
// calling a short "a video" is the mistake nobody minds, and never announcing
// it is the one that matters.
func TestUnclassifiableUploadIsAnnouncedAsVideo(t *testing.T) {
	site := notifNewShortsSite(t, func(string) (int, string) { return http.StatusTooManyRequests, "" })
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	v := notifVideo("mystery1", "none", now.Add(-5*time.Minute))
	d.watch.Seed("mystery1", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)

	notifWaitFor(t, "the unclassifiable upload to be announced", func() bool { return len(sink.videoEvents()) == 1 })
	ev := sink.videoEvents()[0]
	if ev.Kind != VideoUpload {
		t.Errorf("Kind = %q, want %q when the probe cannot tell", ev.Kind, VideoUpload)
	}
	if ev.URL != YouTubeWatchURL("mystery1") {
		t.Errorf("URL = %q, want the watch url", ev.URL)
	}
}

// A finished stream's VOD also reads as "none", but it keeps its
// liveStreamingDetails. It was a broadcast, not an upload, and announcing it
// as one would ping the audience for a stream they already watched.
func TestFinishedStreamVODIsNotAnUpload(t *testing.T) {
	site := notifNewShortsSite(t, notifAnswerVideo)
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	v := notifVideo("vod1", "none", now.Add(-5*time.Minute))
	v.LiveStreamingDetails = liveDetails(
		now.Add(-3*time.Hour).UTC().Format(time.RFC3339),
		now.Add(-time.Hour).UTC().Format(time.RFC3339),
		now.Add(-3*time.Hour).UTC().Format(time.RFC3339),
	)

	d.watch.Seed("vod1", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)
	notifSettle()

	if events := sink.videoEvents(); len(events) != 0 {
		t.Fatalf("a finished stream must not be announced as an upload, got %+v", events)
	}
	if got := site.hits.Load(); got != 0 {
		t.Errorf("the shorts probe was consulted %d times for a VOD, want never", got)
	}
	if d.watch.ClaimAnnounce("vod1", VideoUpload) != true {
		t.Error("nothing should have latched the upload kind for a VOD")
	}
}

// An old upload sitting on the playlist when detection is enabled must not be
// announced, and must not cost a probe request either.
func TestOldUploadIsNotAnnounced(t *testing.T) {
	site := notifNewShortsSite(t, notifAnswerVideo)
	sink := &recordingSink{}
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	v := notifVideo("ancient", "none", now.Add(-48*time.Hour))
	d.watch.Seed("ancient", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)
	notifSettle()

	if events := sink.videoEvents(); len(events) != 0 {
		t.Fatalf("an old upload must not be announced, got %+v", events)
	}
	if got := site.hits.Load(); got != 0 {
		t.Errorf("the shorts probe was consulted %d times for an old upload, want never", got)
	}
}

// A sink failure must not lose the announcement for the life of the process:
// the latch is handed back so the next poll retries, and the sink's ledger
// is what makes the retry safe.
func TestSinkErrorReleasesTheScheduledLatch(t *testing.T) {
	sink := &notifFlakySink{}
	sink.failuresLeft.Store(1)
	d := notifYouTubeDetector(t, sink, nil)

	now := time.Now()
	v := notifVideo("retry-frame", "upcoming", now.Add(-time.Minute))
	v.LiveStreamingDetails = liveDetails("", "", now.Add(time.Hour).UTC().Format(time.RFC3339))
	d.watch.Seed("retry-frame", "doki", now, true)

	// First poll: the sink refuses.
	d.applyYouTubeVideo(context.Background(), v, now)
	if got := sink.attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d after the first poll, want 1", got)
	}
	if events := sink.videoEvents(); len(events) != 0 {
		t.Fatalf("a refused observation must not be recorded, got %+v", events)
	}

	// Second poll: the latch was released, so it is retried and succeeds.
	d.applyYouTubeVideo(context.Background(), v, now.Add(3*time.Second))
	if got := sink.attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d after the second poll, want 2 (the latch must have been released)", got)
	}
	events := sink.videoEvents()
	if len(events) != 1 || events[0].Kind != VideoScheduled || events[0].ID != "retry-frame" {
		t.Fatalf("events = %+v, want one scheduled event for retry-frame", events)
	}

	// Once recorded, the latch holds.
	d.applyYouTubeVideo(context.Background(), v, now.Add(6*time.Second))
	if got := sink.attempts.Load(); got != 2 {
		t.Errorf("attempts = %d after a third poll, want the latch to hold at 2", got)
	}
}

// The same retry contract holds for the asynchronous upload path.
func TestSinkErrorReleasesTheUploadLatch(t *testing.T) {
	site := notifNewShortsSite(t, notifAnswerVideo)
	sink := &notifFlakySink{}
	sink.failuresLeft.Store(1)
	d := notifYouTubeDetector(t, sink, site)

	now := time.Now()
	v := notifVideo("retry-upload", "none", now.Add(-time.Minute))
	d.watch.Seed("retry-upload", "doki", now, true)

	d.applyYouTubeVideo(context.Background(), v, now)
	notifWaitFor(t, "the first (refused) attempt", func() bool { return sink.attempts.Load() == 1 })
	// The release happens after the sink returns; wait for the latch to be
	// visibly free before polling again, exactly as a later poll cycle would.
	notifWaitFor(t, "the latch to be released", func() bool {
		if !d.watch.ClaimAnnounce("retry-upload", VideoUpload) {
			return false
		}
		d.watch.ReleaseAnnounce("retry-upload", VideoUpload)
		return true
	})
	if events := sink.videoEvents(); len(events) != 0 {
		t.Fatalf("a refused observation must not be recorded, got %+v", events)
	}

	d.applyYouTubeVideo(context.Background(), v, now.Add(3*time.Second))
	notifWaitFor(t, "the retried upload to be announced", func() bool { return len(sink.videoEvents()) == 1 })
	if got := sink.attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if ev := sink.videoEvents()[0]; ev.Kind != VideoUpload || ev.ID != "retry-upload" {
		t.Errorf("event = %+v, want an upload event for retry-upload", ev)
	}
	// Each attempt re-classifies, since the answer was never recorded.
	if got := site.hits.Load(); got != 2 {
		t.Errorf("the probe was consulted %d times, want once per attempt", got)
	}
}

// Close must wait for an in-flight classification rather than leaving the
// goroutine to report into a sink whose store is being torn down.
func TestCloseWaitsForAnInFlightClassification(t *testing.T) {
	released := make(chan struct{})
	var releaseOnce atomic.Bool
	release := func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(released)
		}
	}
	t.Cleanup(release)

	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-released:
		}
		w.Header().Set("Location", "/watch?v=x")
		w.WriteHeader(http.StatusSeeOther)
	}))
	t.Cleanup(srv.Close)

	sink := &recordingSink{}
	d, err := New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k"},
	}, testChannels(), sink, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.shorts.BaseURL = srv.URL

	now := time.Now()
	v := notifVideo("slow1", "none", now.Add(-time.Minute))
	d.watch.Seed("slow1", "doki", now, true)
	d.applyYouTubeVideo(context.Background(), v, now)

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the classification request never reached the site")
	}

	closed := make(chan struct{})
	go func() {
		d.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return: cancelling the context must abandon the hung probe")
	}
	// The abandoned classification must not have produced an announcement
	// (the ledger would otherwise record something nobody was told about),
	// and must have handed the latch back.
	if events := sink.videoEvents(); len(events) != 0 {
		t.Errorf("an abandoned classification must not announce, got %+v", events)
	}
	if !d.watch.ClaimAnnounce("slow1", VideoUpload) {
		t.Error("an abandoned classification must release its latch")
	}
}
