package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// Session lifetimes. A session slides: every authenticated request past the
// touch interval pushes its expiry out to SessionIdle again, so an account in
// regular use never logs itself out, while one left alone does. SessionMax is
// the hard ceiling from creation, past which even an active session has to
// sign in again.
const (
	SessionIdle          = 30 * 24 * time.Hour
	SessionMax           = 180 * 24 * time.Hour
	SessionTouchInterval = 6 * time.Hour
	tokenBytes           = 32
)

// NewSessionToken returns a fresh bearer token and the hash by which the
// server stores it. Only the hash is ever written down: a copy of the
// database is then worth nothing to whoever has it, and the token itself
// exists in exactly one place - the browser that signed in.
func NewSessionToken() (token, hash string, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generating session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is the storage form of a session token. Tokens carry 256 bits of
// entropy, so an unsalted SHA-256 is the right tool: there is nothing to
// guess through the hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
