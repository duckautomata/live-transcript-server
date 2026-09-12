package auth

import (
	"context"
	"testing"
	"time"
)

// The guard hands out its slots, turns a caller away when none frees in
// time, and lets a cancelled caller go without waiting.
func TestGuardBoundsConcurrency(t *testing.T) {
	g := NewGuard(1, 50*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		g.Do(context.Background(), func() {
			close(started)
			<-release
		})
		close(done)
	}()
	<-started
	if g.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1", g.InFlight())
	}

	ran := false
	if g.Do(context.Background(), func() { ran = true }) || ran {
		t.Error("a second caller got a slot while the only one was held")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	begin := time.Now()
	if g.Do(cancelled, func() { ran = true }) || ran {
		t.Error("a cancelled caller ran")
	}
	if time.Since(begin) > 40*time.Millisecond {
		t.Error("a cancelled caller waited for the timer instead of leaving")
	}

	close(release)
	<-done
	if !g.Do(context.Background(), func() { ran = true }) || !ran {
		t.Error("the freed slot was not handed out")
	}
	if g.InFlight() != 0 {
		t.Errorf("InFlight after release = %d, want 0", g.InFlight())
	}

	var none *Guard
	ran = false
	if !none.Do(context.Background(), func() { ran = true }) || !ran {
		t.Error("a nil guard must run the work directly")
	}
	if NewGuard(0, time.Second).InFlight() != 0 {
		t.Error("a zero-slot guard should still be constructible with one slot")
	}
}
