package store

import (
	"context"
	"database/sql"
	"errors"
)

// Site settings are the few operator switches that can be flipped at
// runtime from the site admin page. Each is a string under a key; absence
// means "not overridden".

// SettingRegistrationClosed, when "1", closes sign-ups regardless of the
// config's accounts.disableRegistration.
const SettingRegistrationClosed = "registration_closed"

// GetSetting returns a setting's value and whether it is set.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM site_settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string, now int64) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO site_settings (key, value, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now)
	return err
}

// DeleteSetting clears a setting, returning to the configured default.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM site_settings WHERE key = ?", key)
	return err
}
