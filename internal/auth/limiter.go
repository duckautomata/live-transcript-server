package auth

import (
	"sync"
	"time"
)

// Limiter is a fixed-window request counter keyed by string - an address, an
// account id - for the few endpoints where a cheap request repeated fast
// enough does harm: guessing a password, creating accounts by the thousand,
// pointing test sends at a webhook. It is in-memory: a restart forgets it,
// which is fine for something that exists to slow an attacker down, not to
// meter anyone.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	now     func() time.Time
	sweeps  int
}

type bucket struct {
	count int
	start time.Time
}

// NewLimiter allows limit requests per key in each window.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow records a request for key and reports whether it is within the
// limit. When it is not, retryAfter is how long until the window resets.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep stale buckets now and then so a scan of random addresses cannot
	// grow the map without bound.
	l.sweeps++
	if l.sweeps >= 1024 {
		l.sweeps = 0
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
	}

	b, found := l.buckets[key]
	if !found || now.Sub(b.start) >= l.window {
		l.buckets[key] = &bucket{count: 1, start: now}
		return true, 0
	}
	if b.count >= l.limit {
		return false, b.start.Add(l.window).Sub(now)
	}
	b.count++
	return true, 0
}

// Account lockout after repeated wrong passwords: the first few failures are
// free (people mistype), then each further one doubles the wait, capped at an
// hour. Combined with the per-address limiter this makes an online guess
// worth a few tries an hour per account, whatever the attacker's address
// pool - while a locked-out owner is never locked out for long.
const (
	LockoutFreeFailures = 5
	lockoutBase         = time.Minute
	lockoutMax          = time.Hour
)

// LockDuration is how long an account is locked after its nth consecutive
// failed login; zero while failures are still free.
func LockDuration(failures int) time.Duration {
	if failures < LockoutFreeFailures {
		return 0
	}
	d := lockoutBase
	for i := LockoutFreeFailures; i < failures && d < lockoutMax; i++ {
		d *= 2
	}
	if d > lockoutMax {
		d = lockoutMax
	}
	return d
}
