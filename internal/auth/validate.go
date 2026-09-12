package auth

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Credential rules. The password rules follow NIST SP 800-63B: a length
// floor, a generous ceiling, no composition rules (they push people toward
// predictable substitutions), and a check against the passwords everyone
// picks first. Length is counted in characters, not bytes, so a passphrase
// in any script is measured fairly.
const (
	MinUsernameLength = 3
	MaxUsernameLength = 32
	MinPasswordLength = 8
	MaxPasswordLength = 128
)

var (
	ErrUsernameLength  = errors.New("username must be 3 to 32 characters")
	ErrUsernameChars   = errors.New("username may contain letters, digits, dots, underscores and hyphens, and must start with a letter or digit")
	ErrPasswordLength  = errors.New("password must be 8 to 128 characters")
	ErrPasswordCommon  = errors.New("that password is too common; pick something less guessable")
	ErrPasswordIsLogin = errors.New("password must not be the username")
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// NormalizeUsername validates a username and returns the form to display
// (as typed, trimmed) and the key it is unique by (lowercased), so "Doki" and
// "doki" are one account and the owner still sees their own capitalisation.
func NormalizeUsername(raw string) (display, key string, err error) {
	display = strings.TrimSpace(raw)
	n := utf8.RuneCountInString(display)
	if n < MinUsernameLength || n > MaxUsernameLength {
		return "", "", ErrUsernameLength
	}
	if !usernamePattern.MatchString(display) {
		return "", "", ErrUsernameChars
	}
	return display, strings.ToLower(display), nil
}

// ValidatePassword applies the password rules. The username is needed so a
// password that is just the login can be refused.
func ValidatePassword(password, username string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength || n > MaxPasswordLength {
		return ErrPasswordLength
	}
	lower := strings.ToLower(strings.TrimSpace(password))
	if username != "" && lower == strings.ToLower(strings.TrimSpace(username)) {
		return ErrPasswordIsLogin
	}
	if _, common := commonPasswords[lower]; common {
		return ErrPasswordCommon
	}
	return nil
}

// commonPasswords is the top of every breached-password list: the handful
// of strings that an online guesser tries first. A full breach corpus is out
// of scope for this server; refusing these costs nothing and removes the
// guesses that succeed most often.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "passw0rd": {}, "12345678": {}, "123456789": {},
	"1234567890": {}, "qwerty123": {}, "qwertyuiop": {}, "iloveyou": {}, "11111111": {}, "00000000": {},
	"abc12345": {}, "letmein1": {}, "welcome1": {}, "admin123": {}, "sunshine": {}, "princess": {},
	"football": {}, "baseball": {}, "trustno1": {}, "superman": {}, "dragon12": {}, "monkey12": {},
	"changeme": {}, "whatever": {}, "starwars": {}, "computer": {}, "internet": {}, "1q2w3e4r": {},
	"1qaz2wsx": {}, "asdfghjkl": {}, "zxcvbnm1": {}, "aaaaaaaa": {}, "12341234": {}, "livetranscript": {},
}
