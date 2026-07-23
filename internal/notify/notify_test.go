package notify

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitReadyImmediately(t *testing.T) {
	n := New()
	got := n.Wait(context.Background(), time.Now().Add(200*time.Millisecond), func() bool { return true })
	if got != Ready {
		t.Errorf("Wait() = %v, want Ready", got)
	}
}

func TestWaitReadyWithExpiredDeadline(t *testing.T) {
	// check runs before the deadline test, so an already-true condition wins.
	n := New()
	got := n.Wait(context.Background(), time.Now().Add(-time.Second), func() bool { return true })
	if got != Ready {
		t.Errorf("Wait() = %v, want Ready", got)
	}
}

func TestNotifyWakesParkedWait(t *testing.T) {
	n := New()
	var flag atomic.Bool
	done := make(chan Outcome, 1)

	go func() {
		done <- n.Wait(context.Background(), time.Now().Add(200*time.Millisecond), func() bool { return flag.Load() })
	}()

	// Give the waiter a moment to park, then flip the condition and notify.
	time.Sleep(20 * time.Millisecond)
	flag.Store(true)
	n.Notify()

	select {
	case got := <-done:
		if got != Ready {
			t.Errorf("Wait() = %v, want Ready", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return after Notify")
	}
}

func TestNotifyWithoutStateChangeLoopsUntilTimeout(t *testing.T) {
	n := New()
	var checks atomic.Int64
	done := make(chan Outcome, 1)

	go func() {
		done <- n.Wait(context.Background(), time.Now().Add(100*time.Millisecond), func() bool {
			checks.Add(1)
			return false
		})
	}()

	time.Sleep(20 * time.Millisecond)
	n.Notify() // spurious wakeup: condition still false, waiter must re-park

	select {
	case got := <-done:
		if got != Timeout {
			t.Errorf("Wait() = %v, want Timeout", got)
		}
		if checks.Load() < 2 {
			t.Errorf("expected a recheck after Notify, got %d checks", checks.Load())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not time out")
	}
}

func TestWaitTimeout(t *testing.T) {
	n := New()
	got := n.Wait(context.Background(), time.Now().Add(50*time.Millisecond), func() bool { return false })
	if got != Timeout {
		t.Errorf("Wait() = %v, want Timeout", got)
	}
}

func TestReleaseUnblocksParkedWaiters(t *testing.T) {
	n := New()
	done := make(chan Outcome, 2)

	for range 2 {
		go func() {
			done <- n.Wait(context.Background(), time.Now().Add(200*time.Millisecond), func() bool { return false })
		}()
	}

	time.Sleep(20 * time.Millisecond)
	n.Release()
	n.Release() // idempotent: second call must not panic

	for range 2 {
		select {
		case got := <-done:
			if got != Released {
				t.Errorf("Wait() = %v, want Released", got)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Wait did not return after Release")
		}
	}
}

func TestContextCancelProducesCanceled(t *testing.T) {
	n := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)

	go func() {
		done <- n.Wait(ctx, time.Now().Add(200*time.Millisecond), func() bool { return false })
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got != Canceled {
			t.Errorf("Wait() = %v, want Canceled", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return after cancel")
	}
}
