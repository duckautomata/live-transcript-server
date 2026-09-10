package store

import (
	"context"
	"database/sql"

	"live-transcript-server/internal/model"
)

// ClaimDetection records a newly observed live broadcast and reports whether
// this call was the one that recorded it.
//
// This is the whole concurrency design of live detection. Every mechanism
// (Twitch EventSub, Twitch polling, YouTube WebSub, YouTube state polling)
// calls it on every observation, and the INSERT OR IGNORE means exactly one of
// them wins: the winner returns true and sends the notification, everyone else
// returns false and does nothing. No locking, no leader election, and the
// mechanism recorded on the row is a real measurement of which path is fastest
// , which is the entire point of the shadow-mode soak.
//
// It is also why a restart is handled with no special case: a restarted
// broadcast has a new platform id, so it is a new row and it notifies again.
func (s *Store) ClaimDetection(ctx context.Context, d model.DetectedBroadcast) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
	INSERT OR IGNORE INTO detected_broadcasts
		(platform, broadcast_id, channel_key, url, title, started_at, detected_at, mechanism, ended_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0);
	`, d.Platform, d.BroadcastID, d.ChannelKey, d.URL, d.Title, d.StartedAt, d.DetectedAt, d.Mechanism)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// GetDetection returns a single ledger row, or nil when the broadcast has
// never been claimed.
func (s *Store) GetDetection(ctx context.Context, platform, broadcastID string) (*model.DetectedBroadcast, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT platform, broadcast_id, channel_key, url, title, started_at, detected_at, mechanism, ended_at
	FROM detected_broadcasts WHERE platform = ? AND broadcast_id = ?;
	`, platform, broadcastID)

	var d model.DetectedBroadcast
	err := row.Scan(&d.Platform, &d.BroadcastID, &d.ChannelKey, &d.URL, &d.Title,
		&d.StartedAt, &d.DetectedAt, &d.Mechanism, &d.EndedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// MarkDetectionEnded stamps a broadcast as finished, once. The guard on
// ended_at = 0 makes it idempotent, so the mechanism that notices the end
// first sets the timestamp and later observations are no-ops. Returns whether
// this call was the one that ended it, which is what triggers the accelerated
// re-poll that catches a quick restart.
func (s *Store) MarkDetectionEnded(ctx context.Context, platform, broadcastID string, endedAt int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
	UPDATE detected_broadcasts SET ended_at = ?
	WHERE platform = ? AND broadcast_id = ? AND ended_at = 0;
	`, endedAt, platform, broadcastID)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// GetLiveDetections returns every broadcast currently believed live (claimed,
// not yet ended), oldest first. The detector re-seeds its in-memory watchlist
// from this on startup so a restart mid-broadcast does not re-notify and does
// not lose track of a stream it already saw.
func (s *Store) GetLiveDetections(ctx context.Context) ([]model.DetectedBroadcast, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT platform, broadcast_id, channel_key, url, title, started_at, detected_at, mechanism, ended_at
	FROM detected_broadcasts WHERE ended_at = 0 ORDER BY detected_at ASC;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.DetectedBroadcast
	for rows.Next() {
		var d model.DetectedBroadcast
		if err := rows.Scan(&d.Platform, &d.BroadcastID, &d.ChannelKey, &d.URL, &d.Title,
			&d.StartedAt, &d.DetectedAt, &d.Mechanism, &d.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetRecentDetections returns the newest ledger rows for a channel, most
// recent first. It backs the admin page's detection history, which is how the
// operator reads the soak results without opening the database.
func (s *Store) GetRecentDetections(ctx context.Context, channelKey string, limit int) ([]model.DetectedBroadcast, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT platform, broadcast_id, channel_key, url, title, started_at, detected_at, mechanism, ended_at
	FROM detected_broadcasts WHERE channel_key = ? ORDER BY detected_at DESC LIMIT ?;
	`, channelKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.DetectedBroadcast
	for rows.Next() {
		var d model.DetectedBroadcast
		if err := rows.Scan(&d.Platform, &d.BroadcastID, &d.ChannelKey, &d.URL, &d.Title,
			&d.StartedAt, &d.DetectedAt, &d.Mechanism, &d.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CleanupOldDetections drops ledger rows first detected before cutoff.
//
// Retention must comfortably exceed the longest plausible single broadcast: if
// a row is pruned while its stream is still live, the next observation claims
// it again and re-notifies. Only ended broadcasts are eligible for exactly
// that reason.
func (s *Store) CleanupOldDetections(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM detected_broadcasts WHERE detected_at < ? AND ended_at != 0", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
