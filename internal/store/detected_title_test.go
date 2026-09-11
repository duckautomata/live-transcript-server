package store

import (
	"context"
	"testing"
)

// A later observation may know the title the claim winner did not (the
// Twitch poll leg after EventSub), so a blank title can be filled in - but a
// title that is already recorded is never overwritten.
func TestFillDetectionTitleOnlyFillsBlanks(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	untitled := newDetection("twitch", "s1", "doki")
	untitled.Title = ""
	if _, err := st.ClaimDetection(ctx, untitled); err != nil {
		t.Fatalf("claim: %v", err)
	}
	titled := newDetection("twitch", "s2", "doki")
	titled.Title = "recorded by the winner"
	if _, err := st.ClaimDetection(ctx, titled); err != nil {
		t.Fatalf("claim: %v", err)
	}

	filled, err := st.FillDetectionTitle(ctx, "twitch", "s1", "Playing games")
	if err != nil || !filled {
		t.Fatalf("fill blank = (%v, %v), want (true, nil)", filled, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s1"); got == nil || got.Title != "Playing games" {
		t.Fatalf("after fill: %+v", got)
	}

	// Filled once; a different title later does not replace it.
	if filled, err := st.FillDetectionTitle(ctx, "twitch", "s1", "Renamed"); err != nil || filled {
		t.Errorf("second fill = (%v, %v), want (false, nil)", filled, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s1"); got.Title != "Playing games" {
		t.Errorf("title after second fill = %q", got.Title)
	}

	// A row that had a title from the start is untouched.
	if filled, err := st.FillDetectionTitle(ctx, "twitch", "s2", "something else"); err != nil || filled {
		t.Errorf("fill titled = (%v, %v), want (false, nil)", filled, err)
	}
	if got, _ := st.GetDetection(ctx, "twitch", "s2"); got.Title != "recorded by the winner" {
		t.Errorf("titled row changed to %q", got.Title)
	}

	// Nothing to fill with, or nothing to fill, is a no-op.
	if filled, err := st.FillDetectionTitle(ctx, "twitch", "s1", ""); err != nil || filled {
		t.Errorf("fill with empty = (%v, %v)", filled, err)
	}
	if filled, err := st.FillDetectionTitle(ctx, "twitch", "missing", "x"); err != nil || filled {
		t.Errorf("fill missing = (%v, %v)", filled, err)
	}
}
