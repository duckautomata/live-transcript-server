package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"live-transcript-server/internal/model"

	"github.com/mattn/go-sqlite3"
)

// ErrUsernameTaken is returned by CreateUser when the username key exists.
var ErrUsernameTaken = errors.New("username taken")

const userColumns = "id, username, username_key, password_hash, created_at, password_changed_at, last_login_at, disabled_at, disabled_reason"

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	var u model.User
	var key string
	if err := row.Scan(&u.ID, &u.Username, &key, &u.PasswordHash, &u.CreatedAt, &u.PasswordChangedAt, &u.LastLoginAt,
		&u.DisabledAt, &u.DisabledReason); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns every account, newest first, for the operator.
func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userColumns+" FROM users ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetUserDisabled disables (disabledAt > 0) or re-enables (0) an account.
// Disabling also ends its sessions. Returns whether the account exists.
func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabledAt int64, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE users SET disabled_at = ?, disabled_reason = ? WHERE id = ?", disabledAt, reason, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if disabledAt > 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SessionSummary is what the operator sees of an account's sessions.
type SessionSummary struct {
	Count       int
	LastSeenAt  int64
	LastAddress string
}

// SessionSummaries summarises every account's live sessions in one pass.
func (s *Store) SessionSummaries(ctx context.Context, now int64) (map[int64]SessionSummary, error) {
	out := map[int64]SessionSummary{}
	rows, err := s.db.QueryContext(ctx, `
	SELECT user_id, COUNT(*), MAX(last_seen_at) FROM sessions WHERE expires_at > ? GROUP BY user_id`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var sum SessionSummary
		if err := rows.Scan(&id, &sum.Count, &sum.LastSeenAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The address of each account's most recent session, live or not.
	addr, err := s.db.QueryContext(ctx, `
	SELECT user_id, address FROM sessions WHERE id IN (SELECT MAX(id) FROM sessions GROUP BY user_id)`)
	if err != nil {
		return nil, err
	}
	defer addr.Close()
	for addr.Next() {
		var id int64
		var address string
		if err := addr.Scan(&id, &address); err != nil {
			return nil, err
		}
		sum := out[id]
		sum.LastAddress = address
		out[id] = sum
	}
	return out, addr.Err()
}

// CreateUser inserts an account. usernameKey is the lowercased form the
// UNIQUE constraint applies to; a clash is reported as ErrUsernameTaken.
func (s *Store) CreateUser(ctx context.Context, username, usernameKey, passwordHash string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
	INSERT INTO users (username, username_key, password_hash, created_at, password_changed_at)
	VALUES (?, ?, ?, ?, ?);`, username, usernameKey, passwordHash, now, now)
	if err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
			return 0, ErrUsernameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByUsernameKey returns the account with that lowercased username,
// or nil.
func (s *Store) GetUserByUsernameKey(ctx context.Context, usernameKey string) (*model.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE username_key = ?", usernameKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// GetUserByID returns an account by id, or nil.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*model.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// RecordLoginSuccess stamps the login time. A new password hash may be
// supplied when the stored one was made with older parameters.
func (s *Store) RecordLoginSuccess(ctx context.Context, id int64, now int64, rehash string) error {
	if rehash != "" {
		_, err := s.db.ExecContext(ctx,
			"UPDATE users SET last_login_at = ?, password_hash = ? WHERE id = ?", now, rehash, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE users SET last_login_at = ? WHERE id = ?", now, id)
	return err
}

// ChangePassword stores a new hash and ends every session but the one
// making the change, in one transaction: whoever else had the old password
// is out the moment the new one is in, with no window where the hash has
// changed but their sessions still stand. Returns how many sessions ended.
func (s *Store) ChangePassword(ctx context.Context, id int64, passwordHash string, now int64, keepSession int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET password_hash = ?, password_changed_at = ? WHERE id = ?", passwordHash, now, id); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id != ?", id, keepSession)
	if err != nil {
		return 0, err
	}
	ended, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return ended, tx.Commit()
}

// DeleteUser removes an account and everything it owns: its sessions (by
// cascade), its notification events (and their cooldowns, by cascade) and
// its delivery log. Nothing of the account is left behind for anyone to read.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM notification_log WHERE user_id = ?",
		"DELETE FROM notification_events WHERE user_id = ?",
		"DELETE FROM sessions WHERE user_id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Usernames resolves account ids to usernames, for the operator's view of
// whose rules and deliveries these are. Unknown ids are simply absent.
// UserLabel is what Usernames returns per id: the name to show and whether
// the account is currently disabled, so operator views can say so.
type UserLabel struct {
	Username string
	Disabled bool
}

func (s *Store) Usernames(ctx context.Context, ids []int64) (map[int64]UserLabel, error) {
	out := map[int64]UserLabel{}
	if len(ids) == 0 {
		return out, nil
	}
	seen := map[int64]bool{}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, username, disabled_at FROM users WHERE id IN (?"+strings.Repeat(",?", len(args)-1)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, disabledAt int64
		var name string
		if err := rows.Scan(&id, &name, &disabledAt); err != nil {
			return nil, err
		}
		out[id] = UserLabel{Username: name, Disabled: disabledAt != 0}
	}
	return out, rows.Err()
}

// CountUsers is for the admin page's account tally.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CreateSession records a signed-in browser by the hash of its token, with
// the browser description and address the sessions list shows.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash string, now, expiresAt int64, client, address string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
	INSERT INTO sessions (user_id, token_hash, created_at, last_seen_at, expires_at, client, address)
	VALUES (?, ?, ?, ?, ?, ?, ?);`, userID, tokenHash, now, now, expiresAt, client, address)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const sessionColumns = "id, user_id, token_hash, created_at, last_seen_at, expires_at, client, address"

func scanSession(row interface{ Scan(...any) error }, extra ...any) (*model.Session, error) {
	var sess model.Session
	dest := append([]any{&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.Client, &sess.Address}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &sess, nil
}

// GetSessionByTokenHash returns the live session for a token hash and the
// account it belongs to, or nils when there is no such session or it has
// expired.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string, now int64) (*model.Session, *model.User, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT `+prefixed("s.", sessionColumns)+`, `+prefixed("u.", userColumns)+`
	FROM sessions s JOIN users u ON u.id = s.user_id
	WHERE s.token_hash = ? AND s.expires_at > ?;`, tokenHash, now)

	var u model.User
	var key string
	sess, err := scanSession(row,
		&u.ID, &u.Username, &key, &u.PasswordHash, &u.CreatedAt, &u.PasswordChangedAt, &u.LastLoginAt, &u.DisabledAt, &u.DisabledReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return sess, &u, nil
}

// ListUserSessions returns an account's live sessions, most recently active
// first: everyone signed in to it, on every device.
func (s *Store) ListUserSessions(ctx context.Context, userID int64, now int64) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+sessionColumns+" FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC, id DESC",
		userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// DeleteUserSession ends one of an account's sessions by id. A session that
// is not the account's own is simply not found.
func (s *Store) DeleteUserSession(ctx context.Context, userID, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id = ?", userID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// prefixed qualifies every column in a comma-separated list with a table
// alias, for a join.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ", ")
	for i := range parts {
		parts[i] = alias + parts[i]
	}
	return strings.Join(parts, ", ")
}

// TouchSession records activity on a session and slides its expiry.
func (s *Store) TouchSession(ctx context.Context, id int64, now, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?", now, expiresAt, id)
	return err
}

// DeleteSession signs one browser out. Returns whether a session existed.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUserSessions signs an account out everywhere, optionally keeping one
// session (the browser doing the signing out). Returns how many were ended.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keep int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id != ?", userID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupExpiredSessions removes sessions past their expiry. Expired rows
// are already refused at lookup; this only keeps the table small.
func (s *Store) CleanupExpiredSessions(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountUserSessions is for the account view ("signed in on 3 browsers").
func (s *Store) CountUserSessions(ctx context.Context, userID int64, now int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?", userID, now).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting sessions: %w", err)
	}
	return n, nil
}
