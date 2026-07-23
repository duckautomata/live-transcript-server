package store

import "context"

// UpsertIncomingStream records an incoming stream URL for a channel. The
// received_at timestamp is refreshed whenever the same (channel_key, url) is
// re-announced so it does not get pruned by TTL while the stream is still live.
func (s *Store) UpsertIncomingStream(ctx context.Context, channelKey, url string, receivedAt int64) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO incoming_streams (channel_key, url, received_at)
	VALUES (?, ?, ?)
	ON CONFLICT(channel_key, url) DO UPDATE SET received_at = excluded.received_at;
	`, channelKey, url, receivedAt)
	return err
}

// GetIncomingStreams returns every URL queued for the given channel ordered by oldest first.
func (s *Store) GetIncomingStreams(ctx context.Context, channelKey string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT url FROM incoming_streams WHERE channel_key = ? ORDER BY received_at ASC", channelKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return urls, nil
}

// GetLatestIncomingTime returns the newest received_at among the URLs queued
// for a channel, or 0 when the queue is empty. Used by the GET /events long
// poll as an edge-trigger cursor so a queue that stays populated for a whole
// stream is only reported once.
func (s *Store) GetLatestIncomingTime(ctx context.Context, channelKey string) (int64, error) {
	var latest int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(received_at), 0) FROM incoming_streams WHERE channel_key = ?", channelKey).Scan(&latest)
	if err != nil {
		return 0, err
	}
	return latest, nil
}

// DeleteIncomingStream removes a single (channel_key, url) entry. Returns the
// number of rows deleted so callers can distinguish "not found" from "removed".
func (s *Store) DeleteIncomingStream(ctx context.Context, channelKey, url string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM incoming_streams WHERE channel_key = ? AND url = ?", channelKey, url)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupExpiredIncomingStreams deletes incoming stream entries whose received_at is older than the cutoff.
func (s *Store) CleanupExpiredIncomingStreams(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM incoming_streams WHERE received_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearIncomingStreams removes every queued URL for a channel. Used by the
// admin "stop current stream" action.
func (s *Store) ClearIncomingStreams(ctx context.Context, channelKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM incoming_streams WHERE channel_key = ?", channelKey)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
