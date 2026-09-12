package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("swordfish tacos")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q, want the PHC argon2id form with the current parameters", h)
	}
	if ok, err := VerifyPassword(h, "swordfish tacos"); err != nil || !ok {
		t.Errorf("right password: ok=%v err=%v", ok, err)
	}
	if ok, err := VerifyPassword(h, "swordfish taco"); err != nil || ok {
		t.Errorf("wrong password: ok=%v err=%v", ok, err)
	}
	if NeedsRehash(h) {
		t.Error("a fresh hash must not need rehashing")
	}
	// Every hash has its own salt.
	h2, _ := HashPassword("swordfish tacos")
	if h2 == h {
		t.Error("two hashes of one password are identical: no salt")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=19456,t=2,p=1$$aGFzaA"} {
		if ok, err := VerifyPassword(bad, "x"); err == nil || ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want an error and no match", bad, ok, err)
		}
		if !NeedsRehash(bad) {
			t.Errorf("NeedsRehash(%q) = false, want true", bad)
		}
	}
}

// A hash made with other parameters still verifies and is flagged for an
// upgrade, so parameters can be raised without locking anyone out.
func TestNeedsRehashOnOldParameters(t *testing.T) {
	h, _ := HashPassword("swordfish tacos")
	old := strings.Replace(h, "m=19456,t=2,p=1", "m=19456,t=1,p=1", 1)
	// The key was derived with t=2, so it will not match under t=1 - but the
	// string parses, which is what NeedsRehash looks at.
	if !NeedsRehash(old) {
		t.Error("a hash with different parameters must need rehashing")
	}
	if _, err := VerifyPassword(old, "swordfish tacos"); err != nil {
		t.Errorf("an old-parameter hash must still be verifiable: %v", err)
	}
}

func TestSessionTokens(t *testing.T) {
	tok, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if len(tok) < 40 || strings.ContainsAny(tok, "+/=") {
		t.Errorf("token = %q, want ~43 url-safe base64 chars", tok)
	}
	if hash != HashToken(tok) || len(hash) != 64 {
		t.Errorf("hash = %q, want the sha256 hex of the token", hash)
	}
	tok2, _, _ := NewSessionToken()
	if tok2 == tok {
		t.Error("two tokens are identical")
	}
}

func TestNormalizeUsername(t *testing.T) {
	display, key, err := NormalizeUsername("  Doki_Fan.99  ")
	if err != nil || display != "Doki_Fan.99" || key != "doki_fan.99" {
		t.Errorf("NormalizeUsername = %q %q %v", display, key, err)
	}
	for _, bad := range []string{"ab", strings.Repeat("a", 33), "doki fan", ".doki", "-doki", "dôki", "doki/fan", ""} {
		if _, _, err := NormalizeUsername(bad); err == nil {
			t.Errorf("NormalizeUsername(%q) accepted", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("swordfish tacos", "doki"); err != nil {
		t.Errorf("a good passphrase was refused: %v", err)
	}
	if err := ValidatePassword("pässwörd ok", "doki"); err != nil {
		t.Errorf("a non-ascii password was refused: %v", err)
	}
	cases := map[string]error{
		"short":                  ErrPasswordLength,
		strings.Repeat("a", 129): ErrPasswordLength,
		"password123":            ErrPasswordCommon,
		"PASSWORD123":            ErrPasswordCommon,
		"DokiDoki":               ErrPasswordIsLogin,
		"1234567890":             ErrPasswordCommon,
		strings.Repeat("ab", 4):  nil, // 8 characters, not common
		"ééééééé":                ErrPasswordLength,
		"éééééééé":               nil, // 8 runes, 16 bytes
	}
	for pw, want := range cases {
		got := ValidatePassword(pw, "dokidoki")
		if got != want {
			t.Errorf("ValidatePassword(%q) = %v, want %v", pw, got, want)
		}
	}
}

func TestLimiterWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d refused inside the limit", i+1)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry != time.Minute {
		t.Errorf("4th request: ok=%v retry=%v, want refused with the whole window left", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("another key shares the bucket")
	}
	now = now.Add(30 * time.Second)
	if ok, retry := l.Allow("a"); ok || retry != 30*time.Second {
		t.Errorf("mid-window: ok=%v retry=%v", ok, retry)
	}
	now = now.Add(30 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("the window did not reset")
	}
	var none *Limiter
	if ok, _ := none.Allow("x"); !ok {
		t.Error("a nil limiter must allow")
	}
}

func TestLockDuration(t *testing.T) {
	cases := map[int]time.Duration{
		0: 0, 4: 0,
		5: time.Minute, 6: 2 * time.Minute, 7: 4 * time.Minute, 10: 32 * time.Minute,
		11: time.Hour, 50: time.Hour,
	}
	for failures, want := range cases {
		if got := LockDuration(failures); got != want {
			t.Errorf("LockDuration(%d) = %v, want %v", failures, got, want)
		}
	}
}
