package store

import (
	"context"
	"sync"
	"testing"

	"live-transcript-server/internal/model"
)

func newDetection(platform, id, key string) model.DetectedBroadcast {
	return model.DetectedBroadcast{
		Platform:    platform,
		BroadcastID: id,
		ChannelKey:  key,
		URL:         "https://example.test/" + id,
		Title:       "a stream",
		StartedAt:   1000,
		DetectedAt:  1005,
		Mechanism:   "test",
	}
}

// The claim is the whole dedupe mechanism: the first caller wins and notifies,
// every later caller is a silent no-op no matter how many times a mechanism
// re-observes the same live broadcast.
func TestClaimDetectionOnlyFirstWins(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	won, err := st.ClaimDetection(ctx, newDetection("twitch", "123", "doki"))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !won {
		t.Fatal("expected the first claim to win")
	}

	for i := range 50 {
		won, err := st.ClaimDetection(ctx, newDetection("twitch", "123", "doki"))
		if err != nil {
			t.Fatalf("re-claim %d: %v", i, err)
		}
		if won {
			t.Fatalf("re-claim %d won; a broadcast must only ever be claimed once", i)
		}
	}
}

// Two mechanisms racing on the same broadcast is the normal case (EventSub and
// polling both see it), so exactly one must win even under concurrency.
func TestClaimDetectionConcurrentRace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const racers = 16
	var wg sync.WaitGroup
	wins := make([]bool, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := st.ClaimDetection(ctx, newDetection("youtube", "vid1", "mint"))
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			wins[i] = won
		}()
	}
	wg.Wait()

	total := 0
	for _, w := range wins {
		if w {
			total++
		}
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", total)
	}
}

// A restart mints a new platform broadcast id, and that is the only reason it
// gets detected again. Same channel, same URL, different id => a new claim.
func TestClaimDetectionRestartIsANewBroadcast(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := newDetection("twitch", "111", "doki")
	second := newDetection("twitch", "222", "doki")
	second.URL = first.URL // a Twitch channel reuses one URL for every broadcast

	if won, err := st.ClaimDetection(ctx, first); err != nil || !won {
		t.Fatalf("first broadcast: won=%v err=%v", won, err)
	}
	won, err := st.ClaimDetection(ctx, second)
	if err != nil {
		t.Fatalf("restart claim: %v", err)
	}
	if !won {
		t.Fatal("a restart has a new broadcast id and must be detected again")
	}
}

// The same id on two platforms must not collide: the key is (platform, id).
func TestClaimDetectionPlatformsAreSeparate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if won, _ := st.ClaimDetection(ctx, newDetection("twitch", "abc", "doki")); !won {
		t.Fatal("twitch claim should win")
	}
	won, err := st.ClaimDetection(ctx, newDetection("youtube", "abc", "doki"))
	if err != nil {
		t.Fatalf("youtube claim: %v", err)
	}
	if !won {
		t.Fatal("the same id on a different platform is a different broadcast")
	}
}

func TestMarkDetectionEndedIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimDetection(ctx, newDetection("twitch", "123", "doki")); err != nil {
		t.Fatalf("claim: %v", err)
	}

	ended, err := st.MarkDetectionEnded(ctx, "twitch", "123", 2000)
	if err != nil || !ended {
		t.Fatalf("first end: ended=%v err=%v", ended, err)
	}
	ended, err = st.MarkDetectionEnded(ctx, "twitch", "123", 3000)
	if err != nil {
		t.Fatalf("second end: %v", err)
	}
	if ended {
		t.Fatal("ending an already-ended broadcast must be a no-op")
	}

	got, err := st.GetDetection(ctx, "twitch", "123")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.EndedAt != 2000 {
		t.Fatalf("ended_at = %d, want the first end time 2000", got.EndedAt)
	}
}

func TestGetLiveDetectionsExcludesEnded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := st.ClaimDetection(ctx, newDetection("youtube", id, "doki")); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	if _, err := st.MarkDetectionEnded(ctx, "youtube", "b", 2000); err != nil {
		t.Fatalf("end b: %v", err)
	}

	live, err := st.GetLiveDetections(ctx)
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("got %d live detections, want 2", len(live))
	}
	for _, d := range live {
		if d.BroadcastID == "b" {
			t.Fatal("an ended broadcast must not be reported live")
		}
	}
}

// Pruning a still-live row would let it be re-claimed and re-notified, so only
// ended rows are eligible however old they are.
func TestCleanupOldDetectionsSkipsLiveRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	old := newDetection("youtube", "old", "doki")
	old.DetectedAt = 100
	if _, err := st.ClaimDetection(ctx, old); err != nil {
		t.Fatalf("claim old: %v", err)
	}
	ancientEnded := newDetection("youtube", "ancient", "doki")
	ancientEnded.DetectedAt = 100
	if _, err := st.ClaimDetection(ctx, ancientEnded); err != nil {
		t.Fatalf("claim ancient: %v", err)
	}
	if _, err := st.MarkDetectionEnded(ctx, "youtube", "ancient", 200); err != nil {
		t.Fatalf("end ancient: %v", err)
	}

	removed, err := st.CleanupOldDetections(ctx, 1000)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d rows, want 1 (only the ended one)", removed)
	}
	if got, _ := st.GetDetection(ctx, "youtube", "old"); got == nil {
		t.Fatal("a still-live row must survive pruning regardless of age")
	}
}

func TestGetRecentDetectionsIsNewestFirstAndScoped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for i, id := range []string{"x", "y", "z"} {
		d := newDetection("youtube", id, "doki")
		d.DetectedAt = int64(1000 + i)
		if _, err := st.ClaimDetection(ctx, d); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	if _, err := st.ClaimDetection(ctx, newDetection("youtube", "other", "mint")); err != nil {
		t.Fatalf("claim other: %v", err)
	}

	got, err := st.GetRecentDetections(ctx, "doki", 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 scoped to doki", len(got))
	}
	if got[0].BroadcastID != "z" {
		t.Fatalf("first row is %q, want the newest (z)", got[0].BroadcastID)
	}
}
