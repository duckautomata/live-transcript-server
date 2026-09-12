package auth

import (
	"context"
	"time"
)

// Guard bounds how many password hashes run at once. Every argon2id call
// pins 19 MiB and a core for tens of milliseconds; without a bound, a flood
// of sign-in attempts from enough addresses to slip past the per-address
// limiter would allocate that per in-flight request until the process was
// killed. With it, the flood queues briefly and then gets a 503 with a
// Retry-After, and the transcript pipeline sharing the box keeps its memory.
type Guard struct {
	slots chan struct{}
	wait  time.Duration
}

// NewGuard allows n concurrent hashes and lets a caller wait up to maxWait
// for a slot before giving up.
func NewGuard(n int, maxWait time.Duration) *Guard {
	if n < 1 {
		n = 1
	}
	return &Guard{slots: make(chan struct{}, n), wait: maxWait}
}

// Do runs fn under a slot. It returns false, without running fn, when no
// slot came free in time or the context ended first. A nil Guard runs fn
// directly.
func (g *Guard) Do(ctx context.Context, fn func()) bool {
	if g == nil {
		fn()
		return true
	}
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
	defer func() { <-g.slots }()
	fn()
	return true
}

// InFlight is how many hashes are running, for tests and diagnostics.
func (g *Guard) InFlight() int {
	if g == nil {
		return 0
	}
	return len(g.slots)
}
