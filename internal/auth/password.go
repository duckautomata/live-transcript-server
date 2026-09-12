// Package auth holds the primitives behind the self-service accounts on the
// live-transcript site: password hashing, session tokens, credential rules
// and the request limiter that slows guessing down. It is a leaf package with
// no knowledge of HTTP or the database, so every rule in it is unit-testable
// and every handler that enforces one enforces the same one.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. This is the configuration OWASP recommends when memory
// is the constraint (19 MiB, 2 passes, 1 lane): the server shares a small box
// with the transcript pipeline, so a burst of logins must not be able to
// squeeze it, and the memory cost - not the pass count - is what keeps a
// GPU from trying billions of guesses against a leaked hash.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrInvalidHash is returned when a stored hash cannot be parsed. It means
// the row was corrupted or written by something else; treat it as a login
// failure, never as a match.
var ErrInvalidHash = errors.New("invalid password hash")

// HashPassword returns an argon2id hash of password in the standard PHC
// string format, with a fresh random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

type hashParams struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

// parseHash decodes a PHC argon2id string. The parameters travel with the
// hash so they can be raised later without invalidating existing accounts.
func parseHash(encoded string) (hashParams, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return hashParams{}, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return hashParams{}, ErrInvalidHash
	}
	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return hashParams{}, ErrInvalidHash
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return hashParams{}, ErrInvalidHash
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) == 0 {
		return hashParams{}, ErrInvalidHash
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) == 0 {
		return hashParams{}, ErrInvalidHash
	}
	return p, nil
}

// VerifyPassword reports whether password is the one encoded was made from.
// The comparison is constant-time; the work is the same for a wrong password
// as for the right one.
func VerifyPassword(encoded, password string) (bool, error) {
	p, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

// NeedsRehash reports whether a stored hash was made with parameters other
// than the current ones, so a successful login can upgrade it in place.
func NeedsRehash(encoded string) bool {
	p, err := parseHash(encoded)
	if err != nil {
		return true
	}
	return p.memory != argonMemory || p.time != argonTime || p.threads != argonThreads || len(p.key) != argonKeyLen
}

// dummyHash is verified against on a login for a username that does not
// exist, so that attempt costs the same time as one against a real account
// and the response time cannot be used to enumerate usernames.
var dummyHash = func() string {
	h, err := HashPassword("not a real account")
	if err != nil {
		panic(err)
	}
	return h
}()

// BurnVerify spends the time a real password check would, for a login
// against an account that does not exist.
func BurnVerify(password string) {
	_, _ = VerifyPassword(dummyHash, password)
}
