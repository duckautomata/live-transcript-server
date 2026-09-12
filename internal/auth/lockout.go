package auth

import (
	"sync"
	"time"
)

// Lockout slows password guessing down: after a few free failures, each
// further one doubles the wait before the next attempt is even considered.
//
// It is keyed by the caller, not by the account alone - (username, address)
// - so a stranger who knows a username cannot lock its owners out, which
// matters when a whole mod team shares one account. Unknown usernames are
// keyed exactly like real ones, so the lockout answer does not reveal which
// usernames exist. Failures decay: a key that has been quiet for as long as
// the longest lock starts from its free attempts again, so an attacker has
// to re-earn every lock and a typo months later does not cost an hour.
//
// In-memory on purpose: a restart forgets it, which is fine for something
// that exists to slow an attacker down, and it never touches the accounts
// table.
type Lockout struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
	now     func() time.Time
	sweeps  int
}

type lockEntry struct {
	failures int
	until    time.Time
	last     time.Time
}

// NewLockout creates an empty lockout table.
func NewLockout() *Lockout {
	return &Lockout{entries: map[string]*lockEntry{}, now: time.Now}
}

// Locked reports how long the key must still wait, or zero.
func (l *Lockout) Locked(key string) time.Duration {
	if l == nil {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return 0
	}
	if l.decayed(e, now) {
		delete(l.entries, key)
		return 0
	}
	if e.until.After(now) {
		return e.until.Sub(now)
	}
	return 0
}

// Fail records a wrong password for the key and returns the wait it now
// carries (zero while failures are still free).
func (l *Lockout) Fail(key string) time.Duration {
	if l == nil {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	e, ok := l.entries[key]
	if !ok || l.decayed(e, now) {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.last = now
	d := LockDuration(e.failures)
	if d > 0 {
		e.until = now.Add(d)
	}
	return d
}

// Reset forgets the key: the right password was given.
func (l *Lockout) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// ResetPrefix forgets every key with the prefix - every address's failures
// for one username, when the operator unlocks an account - and returns how
// many it forgot.
func (l *Lockout) ResetPrefix(prefix string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for k := range l.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(l.entries, k)
			n++
		}
	}
	return n
}

// decayed reports whether an entry has been quiet long enough to forget:
// the longest lock's worth of time since its last failure or since its lock
// ended, whichever is later. An attacker who tries again the moment a lock
// lifts therefore keeps escalating; an owner who leaves it alone starts over.
func (l *Lockout) decayed(e *lockEntry, now time.Time) bool {
	since := e.last
	if e.until.After(since) {
		since = e.until
	}
	return now.Sub(since) >= lockoutMax
}

// sweep drops decayed entries now and then so a scan of usernames cannot
// grow the table without bound.
func (l *Lockout) sweep(now time.Time) {
	l.sweeps++
	if l.sweeps < 1024 {
		return
	}
	l.sweeps = 0
	for k, e := range l.entries {
		if l.decayed(e, now) {
			delete(l.entries, k)
		}
	}
}
