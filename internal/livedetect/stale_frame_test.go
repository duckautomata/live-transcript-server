package livedetect

import (
	"slices"
	"testing"
	"time"
)

// End to end through discovery and the state poll: a waiting room re-published
// a week after it was made, still carrying its original schedule, must be
// watched until it goes live and then reported. Before the fix, the sweep that
// closes a discovery pass retired it as "abandoned", discovery skipped it for a
// day, and its go-live was never seen (FdwrQid9ulg, 2026-10-11).
func TestRepublishedWaitingRoomWithStaleScheduleIsDetectedWhenItGoesLive(t *testing.T) {
	const id = "reused-frame"
	sink := &recordingSink{}
	api, baseURL := newMembersFakeAPI(t)
	now := time.Now()
	created := now.Add(-7 * 24 * time.Hour)
	stale := created.Add(11 * time.Minute)

	api.setPlaylist(UploadsPlaylistID(membersDoki), id)
	api.setVideo(membersBroadcast(id, "upcoming", created, stale, time.Time{}))

	d := membersDetector(t, sink, baseURL)
	d.youtubeDiscoverOnce() // seeds it
	d.youtubeStateOnce()    // first poll: "upcoming", schedule a week old
	d.youtubeDiscoverOnce() // closing sweep used to retire it here
	d.youtubeDiscoverOnce() // ...and discovery then skipped it

	if !d.watch.Known(id) {
		t.Fatal("a pending frame with a stale schedule was dropped from the watchlist")
	}
	if d.watch.Retired(id, time.Now()) {
		t.Fatal("a pending frame was put in the negative cache")
	}
	// A week-old frame is not scheduling news, so nothing is announced yet.
	if ev := sink.videoEvents(); len(ev) != 0 {
		t.Fatalf("video events = %+v, want none for a re-published old frame", ev)
	}

	api.setVideo(membersBroadcast(id, "live", created, stale, time.Now()))
	membersMakeDue(d, id)
	d.youtubeStateOnce()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.live) != 1 || sink.live[0].ID != id {
		t.Fatalf("live = %+v, want the re-published frame's go-live reported", sink.live)
	}
	if sink.mechs[0] != MechanismYouTubeState {
		t.Errorf("mechanism = %q, want %q", sink.mechs[0], MechanismYouTubeState)
	}
	if !sink.live[0].SawScheduled {
		t.Error("the frame was watched as a waiting room before it went live")
	}
}

// A frame the sweep ages out is rediscovered on the next pass - it must be, a
// creator can still go live on it - but without the seed boost. Boosted, every
// waiting room a creator left up was polled every three seconds for ten
// minutes once a day: about 200 units a frame a day, far-future frames and
// long-overdue ones alike, to watch something the watchlist had been watching
// a moment before.
func TestAgedOutFrameIsRediscoveredWithoutTheBoost(t *testing.T) {
	const (
		aged  = "aged-frame"
		fresh = "fresh-frame"
	)
	api, baseURL := newMembersFakeAPI(t)
	now := time.Now()
	stale := now.Add(-3 * 24 * time.Hour)
	api.setPlaylist(UploadsPlaylistID(membersDoki), aged)
	api.setVideo(membersBroadcast(aged, "upcoming", stale.Add(-time.Hour), stale, time.Time{}))
	api.setVideo(membersBroadcast(fresh, "upcoming", now, now.Add(48*time.Hour), time.Time{}))

	d := membersDetector(t, &recordingSink{}, baseURL)
	// Watched for a day already, as discovery would have left it.
	first := now.Add(-25 * time.Hour)
	d.watch.Seed(aged, "doki", first, true)
	d.watch.Observe(aged, StateUpcoming, stale, first)

	d.youtubeDiscoverOnce() // its closing sweep ages the frame out
	if d.watch.Known(aged) {
		t.Fatal("test setup: the sweep should have aged the frame out")
	}
	if d.watch.Retired(aged, time.Now()) {
		t.Fatal("an aged-out frame must not be negative-cached")
	}

	api.setPlaylist(UploadsPlaylistID(membersDoki), fresh, aged)
	d.youtubeDiscoverOnce()

	if !d.watch.Known(aged) {
		t.Fatal("the next pass must re-seed an aged-out frame that is still listed")
	}
	if boosted(d, aged) {
		t.Error("the aged-out frame was re-seeded with the ten-minute hot boost")
	}
	if !boosted(d, fresh) {
		t.Error("a genuinely new id must still be boosted: it may already be live")
	}
	if due := d.watch.Due(time.Now()); !slices.Contains(due, aged) {
		t.Errorf("due = %v, want the re-seeded frame polled at once", due)
	}

	// After that first poll the ladder decides, and a frame days past its
	// schedule belongs on the cold rung.
	d.youtubeStateOnce()
	d.watch.mu.Lock()
	wait := time.Until(d.watch.entries[aged].NextDue)
	d.watch.mu.Unlock()
	if wait < ytIntervalCold-time.Minute {
		t.Errorf("next poll in %s, want the cold rung (%s)", wait, ytIntervalCold)
	}
}
