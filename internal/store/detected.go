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
// - which is the entire point of the shadow-mode soak.
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

// UpdateDetectionTitle fills in a title the claiming mechanism did not carry.
// Twitch EventSub wins the race for nearly every Twitch go-live and its
// payload has no title; the winner looks it up afterwards and records it here
// so the ledger and the admin page show what the audience was told.
func (s *Store) UpdateDetectionTitle(ctx context.Context, platform, broadcastID, title string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE detected_broadcasts SET title = ? WHERE platform = ? AND broadcast_id = ?", title, platform, broadcastID)
	return err
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

// ClaimVideoDetection records a newly observed non-live video event (a
// scheduled stream or premiere, an upload, a short) and reports whether this
// call was the one that recorded it. Same INSERT OR IGNORE design as
// ClaimDetection: the poller sees the same video on every cycle, and a restart
// sees the whole uploads playlist again, so exactly one caller may announce.
//
// "upload" and "short" are two classifications of ONE event - a video being
// published - so they share a claim: whichever the probe answered first is
// the announcement, and a restart that classifies the same video differently
// (the probe was unsure the first time, say) loses. "scheduled" is a separate
// event and keeps its own claim, since the same id legitimately goes on to
// be scheduled and then live.
func (s *Store) ClaimVideoDetection(ctx context.Context, v model.DetectedVideo) (bool, error) {
	sibling := v.Kind
	switch v.Kind {
	case "upload":
		sibling = "short"
	case "short":
		sibling = "upload"
	}
	res, err := s.db.ExecContext(ctx, `
	INSERT OR IGNORE INTO detected_videos
		(platform, video_id, kind, channel_key, url, title, published_at, scheduled_at, detected_at)
	SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
	WHERE NOT EXISTS (
		SELECT 1 FROM detected_videos WHERE platform = ? AND video_id = ? AND kind IN (?, ?)
	);
	`, v.Platform, v.VideoID, v.Kind, v.ChannelKey, v.URL, v.Title, v.PublishedAt, v.ScheduledAt, v.DetectedAt,
		v.Platform, v.VideoID, v.Kind, sibling)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// GetRecentVideoDetections returns the newest non-live video observations for
// a channel, most recent first, for the admin page.
func (s *Store) GetRecentVideoDetections(ctx context.Context, channelKey string, limit int) ([]model.DetectedVideo, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT platform, video_id, kind, channel_key, url, title, published_at, scheduled_at, detected_at
	FROM detected_videos WHERE channel_key = ? ORDER BY detected_at DESC, rowid DESC LIMIT ?;
	`, channelKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.DetectedVideo
	for rows.Next() {
		var v model.DetectedVideo
		if err := rows.Scan(&v.Platform, &v.VideoID, &v.Kind, &v.ChannelKey, &v.URL, &v.Title,
			&v.PublishedAt, &v.ScheduledAt, &v.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ClearDetectionHistory removes a channel's detection rows from the admin
// page's history, as far as that is safe: the ledger is also what stops a
// broadcast from being claimed and announced twice, so a broadcast still live
// or only just ended, and a video observed inside the announcement recency
// window, are kept. Returns how many broadcast and video rows were removed.
func (s *Store) ClearDetectionHistory(ctx context.Context, channelKey string, endedBefore, videosDetectedBefore int64) (broadcasts, videos int64, err error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM detected_broadcasts WHERE channel_key = ? AND ended_at != 0 AND ended_at < ?", channelKey, endedBefore)
	if err != nil {
		return 0, 0, err
	}
	broadcasts, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx,
		"DELETE FROM detected_videos WHERE channel_key = ? AND detected_at < ?", channelKey, videosDetectedBefore)
	if err != nil {
		return broadcasts, 0, err
	}
	videos, _ = res.RowsAffected()
	return broadcasts, videos, nil
}

// CleanupOldVideoDetections drops non-live ledger rows detected before cutoff.
// Retention must exceed the announcement recency window by a wide margin, or a
// video could be re-announced once its row is gone while it still qualifies.
func (s *Store) CleanupOldVideoDetections(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM detected_videos WHERE detected_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
