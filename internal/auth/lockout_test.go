package auth

import (
	"testing"
	"time"
)

// The lockout is per key, escalates, is free again after a quiet spell, and
// says nothing about other keys.
func TestLockoutIsPerKeyAndDecays(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLockout()
	l.now = func() time.Time { return now }

	for i := 0; i < LockoutFreeFailures-1; i++ {
		if d := l.Fail("doki|1"); d != 0 {
			t.Fatalf("failure %d locked for %v; the first few must be free", i+1, d)
		}
	}
	if d := l.Fail("doki|1"); d != time.Minute {
		t.Fatalf("failure %d locked for %v, want a minute", LockoutFreeFailures, d)
	}
	if got := l.Locked("doki|1"); got != time.Minute {
		t.Errorf("Locked = %v, want a minute", got)
	}
	// Another address, or another name, is untouched.
	if l.Locked("doki|2") != 0 || l.Locked("mint|1") != 0 {
		t.Error("the lock leaked to another key")
	}

	now = now.Add(30 * time.Second)
	if got := l.Locked("doki|1"); got != 30*time.Second {
		t.Errorf("mid-lock Locked = %v, want 30s", got)
	}
	now = now.Add(31 * time.Second)
	if got := l.Locked("doki|1"); got != 0 {
		t.Errorf("after the lock Locked = %v, want 0", got)
	}
	// The failures are remembered: the next one doubles the wait.
	if d := l.Fail("doki|1"); d != 2*time.Minute {
		t.Errorf("next failure locked for %v, want two minutes", d)
	}

	// The right password forgets everything.
	l.Reset("doki|1")
	if l.Locked("doki|1") != 0 {
		t.Error("Reset left a lock")
	}
	if d := l.Fail("doki|1"); d != 0 {
		t.Errorf("after Reset a failure locked for %v; the free attempts should be back", d)
	}

	// Left alone for as long as the longest lock, a key starts over.
	for i := 0; i < LockoutFreeFailures; i++ {
		l.Fail("quiet|1")
	}
	if l.Locked("quiet|1") == 0 {
		t.Fatal("expected a lock")
	}
	now = now.Add(lockoutMax + time.Minute)
	if got := l.Locked("quiet|1"); got != 0 {
		t.Errorf("a decayed key is still locked for %v", got)
	}
	if d := l.Fail("quiet|1"); d != 0 {
		t.Errorf("a decayed key still escalates: %v", d)
	}

	var none *Lockout
	if none.Fail("x") != 0 || none.Locked("x") != 0 {
		t.Error("a nil lockout must be a no-op")
	}
	none.Reset("x")
}

// The wait caps at an hour and is held there, so a sustained attacker
// gets one guess an hour and the owner is never out for longer.
func TestLockoutCapsAtAnHour(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLockout()
	l.now = func() time.Time { return now }
	var last time.Duration
	for i := 0; i < 30; i++ {
		last = l.Fail("k")
		now = now.Add(last)
	}
	if last != lockoutMax {
		t.Errorf("30th failure locked for %v, want the cap %v", last, lockoutMax)
	}
}
