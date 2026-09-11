package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"live-transcript-server/internal/model"
)

// notificationEventColumns is the SELECT list shared by every event read, in
// scanNotificationEvent's order.
const notificationEventColumns = `id, channel_key, name, enabled, webhook_urls, triggers, content,
	embed_enabled, embed, cooldown_seconds, last_sent_at, sent_count, last_error, last_error_at,
	created_at, updated_at`

// notificationLogKeep is how many log rows are retained per channel. The log
// is a recent-activity view for the admin page, not an archive.
const notificationLogKeep = 200

type notificationEventScanner interface {
	Scan(dest ...any) error
}

func scanNotificationEvent(row notificationEventScanner) (*model.NotificationEvent, error) {
	var ev model.NotificationEvent
	var webhooks, triggers, embed string
	if err := row.Scan(&ev.ID, &ev.ChannelKey, &ev.Name, &ev.Enabled, &webhooks, &triggers, &ev.Content,
		&ev.EmbedEnabled, &embed, &ev.CooldownSeconds, &ev.LastSentAt, &ev.SentCount, &ev.LastError, &ev.LastErrorAt,
		&ev.CreatedAt, &ev.UpdatedAt); err != nil {
		return nil, err
	}
	hooks, err := decodeWebhooks(webhooks)
	if err != nil {
		return nil, fmt.Errorf("decode webhook_urls for event %d: %w", ev.ID, err)
	}
	ev.Webhooks = hooks
	if err := json.Unmarshal([]byte(triggers), &ev.Triggers); err != nil {
		return nil, fmt.Errorf("decode triggers for event %d: %w", ev.ID, err)
	}
	if err := json.Unmarshal([]byte(embed), &ev.Embed); err != nil {
		return nil, fmt.Errorf("decode embed for event %d: %w", ev.ID, err)
	}
	if ev.Triggers == nil {
		ev.Triggers = []string{}
	}
	return &ev, nil
}

// decodeWebhooks reads the webhook_urls column. It holds a list of
// {name, url} objects; a row written before webhooks had names holds a plain
// list of URL strings, which is read back as unnamed webhooks.
func decodeWebhooks(raw string) ([]model.Webhook, error) {
	var hooks []model.Webhook
	if err := json.Unmarshal([]byte(raw), &hooks); err == nil {
		if hooks == nil {
			hooks = []model.Webhook{}
		}
		return hooks, nil
	}
	var urls []string
	if err := json.Unmarshal([]byte(raw), &urls); err != nil {
		return nil, err
	}
	return model.WebhooksFromURLs(urls...), nil
}

// encodeNotificationEvent renders the JSON-valued columns. A nil slice is
// stored as [] so a later read never yields null.
func encodeNotificationEvent(ev model.NotificationEvent) (webhooks, triggers, embed string, err error) {
	hooks := ev.Webhooks
	if hooks == nil {
		hooks = []model.Webhook{}
	}
	trig := ev.Triggers
	if trig == nil {
		trig = []string{}
	}
	w, err := json.Marshal(hooks)
	if err != nil {
		return "", "", "", err
	}
	t, err := json.Marshal(trig)
	if err != nil {
		return "", "", "", err
	}
	e, err := json.Marshal(ev.Embed)
	if err != nil {
		return "", "", "", err
	}
	return string(w), string(t), string(e), nil
}

// ListNotificationEvents returns every announcement rule for a channel in
// creation order.
func (s *Store) ListNotificationEvents(ctx context.Context, channelKey string) ([]model.NotificationEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+notificationEventColumns+" FROM notification_events WHERE channel_key = ? ORDER BY id ASC", channelKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.NotificationEvent
	for rows.Next() {
		ev, err := scanNotificationEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// GetNotificationEvent returns one rule, or nil when no rule with that id
// belongs to the channel. The channel is part of the lookup so one channel's
// admin can never read another's webhooks by guessing an id.
func (s *Store) GetNotificationEvent(ctx context.Context, channelKey string, id int64) (*model.NotificationEvent, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+notificationEventColumns+" FROM notification_events WHERE channel_key = ? AND id = ?", channelKey, id)
	ev, err := scanNotificationEvent(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// CreateNotificationEvent inserts a rule and returns its new id. The delivery
// trail columns start at zero regardless of what the caller passed.
func (s *Store) CreateNotificationEvent(ctx context.Context, ev model.NotificationEvent, now int64) (int64, error) {
	webhooks, triggers, embed, err := encodeNotificationEvent(ev)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
	INSERT INTO notification_events
		(channel_key, name, enabled, webhook_urls, triggers, content, embed_enabled, embed, cooldown_seconds, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`, ev.ChannelKey, ev.Name, ev.Enabled, webhooks, triggers, ev.Content, ev.EmbedEnabled, embed, ev.CooldownSeconds, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNotificationEvent replaces a rule's admin-editable fields. The
// delivery trail (last sent, counts, last error) is left alone so an edit does
// not erase history or reset a running cooldown. Returns ErrNotFound when the
// id does not belong to the channel.
func (s *Store) UpdateNotificationEvent(ctx context.Context, ev model.NotificationEvent, now int64) error {
	webhooks, triggers, embed, err := encodeNotificationEvent(ev)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
	UPDATE notification_events SET
		name = ?, enabled = ?, webhook_urls = ?, triggers = ?, content = ?,
		embed_enabled = ?, embed = ?, cooldown_seconds = ?, updated_at = ?
	WHERE channel_key = ? AND id = ?;
	`, ev.Name, ev.Enabled, webhooks, triggers, ev.Content, ev.EmbedEnabled, embed, ev.CooldownSeconds, now,
		ev.ChannelKey, ev.ID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("notification event %d for %s: %w", ev.ID, ev.ChannelKey, ErrNotFound)
	}
	return nil
}

// DeleteNotificationEvent removes a rule. Returns the number of rows deleted
// so the caller can answer 404 for an id that was not the channel's.
func (s *Store) DeleteNotificationEvent(ctx context.Context, channelKey string, id int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM notification_events WHERE channel_key = ? AND id = ?", channelKey, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClaimNotificationSend atomically decides whether a rule may send for a
// trigger right now, and if so starts that trigger's cooldown. It runs in a
// write transaction (every transaction here is BEGIN IMMEDIATE), so two
// detections racing for the same rule serialize and exactly one passes. The
// cooldown is measured from the claim rather than from a successful delivery,
// so a send that fails is not retried into the same window it was meant to
// suppress.
//
// The gap is per (rule, trigger): it exists to swallow a stream restart - a
// second "live" minutes after the first - and must not let a rule's
// "scheduled" ping eat the "live" ping that follows it.
//
// Returns (allowed, remainingSeconds): when not allowed, remaining is how long
// the cooldown has left, for the log. A rule that has been disabled or deleted
// since it was listed is simply not allowed.
//
// A zero cooldown is always allowed, and a last send in the future (the wall
// clock stepped backwards under an NTP correction) never extends a cooldown:
// the guard is about a gap that has been waited out, not about the clock's
// monotonicity.
func (s *Store) ClaimNotificationSend(ctx context.Context, id int64, trigger string, now int64) (bool, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	var enabled bool
	var cooldown int64
	err = tx.QueryRowContext(ctx, "SELECT enabled, cooldown_seconds FROM notification_events WHERE id = ?", id).
		Scan(&enabled, &cooldown)
	if err == sql.ErrNoRows {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !enabled {
		return false, 0, nil
	}

	var lastSent int64
	err = tx.QueryRowContext(ctx,
		"SELECT last_sent_at FROM notification_cooldowns WHERE event_id = ? AND trigger = ?", id, trigger).Scan(&lastSent)
	if err != nil && err != sql.ErrNoRows {
		return false, 0, err
	}
	if cooldown > 0 && lastSent <= now && lastSent+cooldown > now {
		return false, lastSent + cooldown - now, nil
	}

	if _, err := tx.ExecContext(ctx, `
	INSERT INTO notification_cooldowns (event_id, trigger, last_sent_at) VALUES (?, ?, ?)
	ON CONFLICT(event_id, trigger) DO UPDATE SET last_sent_at = excluded.last_sent_at;
	`, id, trigger, now); err != nil {
		return false, 0, err
	}
	// last_sent_at on the rule itself is the "last sent" the admin page shows,
	// across every trigger.
	if _, err := tx.ExecContext(ctx, "UPDATE notification_events SET last_sent_at = ? WHERE id = ?", now, id); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}

// RecordNotificationResult updates a rule's delivery trail after an attempt.
// delivered means at least one webhook accepted the message; errText carries
// the (masked) failure summary for the rest, or is empty when everything went
// through. A fully successful send clears the last error.
func (s *Store) RecordNotificationResult(ctx context.Context, id int64, delivered bool, errText string, now int64) error {
	errAt := int64(0)
	if errText != "" {
		errAt = now
	}
	if delivered {
		_, err := s.db.ExecContext(ctx, `
		UPDATE notification_events SET sent_count = sent_count + 1, last_error = ?, last_error_at = ?
		WHERE id = ?;`, errText, errAt, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE notification_events SET last_error = ?, last_error_at = ? WHERE id = ?", errText, errAt, id)
	return err
}

// InsertNotificationLog appends a log entry and trims the channel's log to
// notificationLogKeep rows. The trim runs on every insert because inserts are
// rare (one per announcement) and it keeps the table bounded without a sweep.
func (s *Store) InsertNotificationLog(ctx context.Context, e model.NotificationLogEntry) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO notification_log
		(channel_key, event_id, event_name, trigger, platform, broadcast_id, title, url, status, detail, webhooks, delivered, sent_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`, e.ChannelKey, e.EventID, e.EventName, e.Trigger, e.Platform, e.BroadcastID, e.Title, e.URL, e.Status, e.Detail,
		e.Webhooks, e.Delivered, e.SentAt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
	DELETE FROM notification_log WHERE channel_key = ? AND id NOT IN (
		SELECT id FROM notification_log WHERE channel_key = ? ORDER BY id DESC LIMIT ?
	);`, e.ChannelKey, e.ChannelKey, notificationLogKeep)
	return err
}

// ClearNotificationLog empties a channel's delivery log. The log is a display
// of what happened, not something the dispatcher reads back, so clearing it
// changes nothing about future sends.
func (s *Store) ClearNotificationLog(ctx context.Context, channelKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM notification_log WHERE channel_key = ?", channelKey)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListNotificationLog returns a channel's newest log entries, most recent
// first.
func (s *Store) ListNotificationLog(ctx context.Context, channelKey string, limit int) ([]model.NotificationLogEntry, error) {
	if limit <= 0 || limit > notificationLogKeep {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT id, channel_key, event_id, event_name, trigger, platform, broadcast_id, title, url, status, detail, webhooks, delivered, sent_at
	FROM notification_log WHERE channel_key = ? ORDER BY id DESC LIMIT ?;`, channelKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.NotificationLogEntry
	for rows.Next() {
		var e model.NotificationLogEntry
		if err := rows.Scan(&e.ID, &e.ChannelKey, &e.EventID, &e.EventName, &e.Trigger, &e.Platform, &e.BroadcastID,
			&e.Title, &e.URL, &e.Status, &e.Detail, &e.Webhooks, &e.Delivered, &e.SentAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
