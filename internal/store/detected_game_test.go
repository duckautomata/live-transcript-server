package store

import (
	"context"
	"testing"
)

// The Twitch category is read back by every getter, since previews and test
// sends render {game} from whichever getter found the row.
func TestDetectionGameRoundTrips(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	d := newDetection("twitch", "s1", "doki")
	d.Game = "Just Chatting"
	if won, err := st.ClaimDetection(ctx, d); err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}

	got, err := st.GetDetection(ctx, "twitch", "s1")
	if err != nil || got == nil {
		t.Fatalf("GetDetection = (%+v, %v)", got, err)
	}
	if *got != d {
		t.Errorf("GetDetection = %+v, want the claimed row back: %+v", *got, d)
	}

	live, err := st.GetLiveDetections(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("GetLiveDetections = (%+v, %v), want one row", live, err)
	}
	if live[0].Game != "Just Chatting" {
		t.Errorf("GetLiveDetections game = %q", live[0].Game)
	}

	recent, err := st.GetRecentDetections(ctx, "doki", 10)
	if err != nil || len(recent) != 1 {
		t.Fatalf("GetRecentDetections = (%+v, %v), want one row", recent, err)
	}
	if recent[0].Game != "Just Chatting" {
		t.Errorf("GetRecentDetections game = %q", recent[0].Game)
	}

	// A row claimed without one - every YouTube row, and every Twitch row
	// EventSub wins - reads back empty.
	if won, err := st.ClaimDetection(ctx, newDetection("youtube", "vid1", "doki")); err != nil || !won {
		t.Fatalf("youtube claim = (%v, %v)", won, err)
	}
	if got, _ := st.GetDetection(ctx, "youtube", "vid1"); got == nil || got.Game != "" {
		t.Errorf("a row claimed with no game = %+v, want it empty", got)
	}
}

// EventSub claims a Twitch broadcast without a category; the winner's lookup
// and the poll leg both try to fill it in afterwards. Whichever lands first
// stays: the row records the category at go-live, and a streamer switching
// games an hour in must not rewrite it.
func TestFillDetectionGameOnlyFillsBlanks(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimDetection(ctx, newDetection("twitch", "s1", "doki")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	known := newDetection("twitch", "s2", "doki")
	known.Game = "recorded by the winner"
	if _, err := st.ClaimDetection(ctx, known); err != nil {
		t.Fatalf("claim: %v", err)
	}

	filled, err := st.FillDetectionGame(ctx, "twitch", "s1", "Minecraft")
	if err != nil || !filled {
		t.Fatalf("fill blank = (%v, %v), want (true, nil)", filled, err)
	}
	got, _ := st.GetDetection(ctx, "twitch", "s1")
	if got == nil || got.Game != "Minecraft" {
		t.Fatalf("after fill: %+v", got)
	}
	// Only the category moved: the title the row was claimed with is intact.
	if got.Title != newDetection("twitch", "s1", "doki").Title {
		t.Errorf("filling the game changed the title to %q", got.Title)
	}

	// Filled once; a different category later does not replace it.
	if filled, err := st.FillDetectionGame(ctx, "twitch", "s1", "Just Chatting"); err != nil || filled {
		t.Errorf("second fill = (%v, %v), want (false, nil)", filled, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s1"); got.Game != "Minecraft" {
		t.Errorf("game after second fill = %q", got.Game)
	}

	// A row that had a category from the start is untouched.
	if filled, err := st.FillDetectionGame(ctx, "twitch", "s2", "something else"); err != nil || filled {
		t.Errorf("fill known = (%v, %v), want (false, nil)", filled, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s2"); got.Game != "recorded by the winner" {
		t.Errorf("row with a game changed to %q", got.Game)
	}

	// Nothing to fill with, or nothing to fill, is a no-op.
	if filled, err := st.FillDetectionGame(ctx, "twitch", "s1", ""); err != nil || filled {
		t.Errorf("fill with empty = (%v, %v)", filled, err)
	}
	if filled, err := st.FillDetectionGame(ctx, "twitch", "missing", "x"); err != nil || filled {
		t.Errorf("fill missing = (%v, %v)", filled, err)
	}
}
