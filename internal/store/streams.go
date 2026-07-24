package store

import (
	"context"
	"database/sql"

	"live-transcript-server/internal/model"
)

// GetRecentStream returns the stream with the most recent activated_time.
// Returns nil, nil if no stream is found.
func (s *Store) GetRecentStream(ctx context.Context, channelID string) (*model.Stream, error) {
	row := s.db.QueryRowContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? ORDER BY activated_time DESC LIMIT 1", channelID)
	var st model.Stream
	err := row.Scan(&st.ChannelID, &st.StreamID, &st.StreamTitle, &st.StartTime, &st.IsLive, &st.MediaType, &st.ActivatedTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &st, nil
}

// GetStreamByID returns a specific stream by channelID and streamID.
// Returns nil, nil if no stream is found.
func (s *Store) GetStreamByID(ctx context.Context, channelID string, streamID string) (*model.Stream, error) {
	row := s.db.QueryRowContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	var st model.Stream
	err := row.Scan(&st.ChannelID, &st.StreamID, &st.StreamTitle, &st.StartTime, &st.IsLive, &st.MediaType, &st.ActivatedTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetAllStreams retrieves all streams for a channel, ordered by activated_time descending.
func (s *Store) GetAllStreams(ctx context.Context, channelID string) ([]model.Stream, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? ORDER BY activated_time DESC", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []model.Stream
	for rows.Next() {
		var st model.Stream
		if err := rows.Scan(&st.ChannelID, &st.StreamID, &st.StreamTitle, &st.StartTime, &st.IsLive, &st.MediaType, &st.ActivatedTime); err != nil {
			return nil, err
		}
		streams = append(streams, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return streams, nil
}

// GetPastStreams retrieves all inactive streams for a channel, ordered by activated_time descending.
func (s *Store) GetPastStreams(ctx context.Context, channelID string, excludeStreamID string) ([]model.Stream, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? AND is_live = 0 AND stream_id != ? ORDER BY activated_time DESC", channelID, excludeStreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []model.Stream
	for rows.Next() {
		var st model.Stream
		if err := rows.Scan(&st.ChannelID, &st.StreamID, &st.StreamTitle, &st.StartTime, &st.IsLive, &st.MediaType, &st.ActivatedTime); err != nil {
			return nil, err
		}
		streams = append(streams, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return streams, nil
}

// DeleteStream deletes a specific stream from the database.
func (s *Store) DeleteStream(ctx context.Context, channelID string, streamID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM streams WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	return err
}

// DeleteStreamCascade deletes a stream and all of its transcript lines in a
// single transaction, so a crash between the two deletes cannot orphan lines.
func (s *Store) DeleteStreamCascade(ctx context.Context, channelID string, streamID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM streams WHERE channel_id = ? AND stream_id = ?", channelID, streamID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND stream_id = ?", channelID, streamID); err != nil {
		return err
	}

	return tx.Commit()
}

// UpsertStream inserts or updates the stream data.
func (s *Store) UpsertStream(ctx context.Context, st *model.Stream) error {
	// Since PK is (channel_id, stream_id), this upsert works for specific streams
	// We do NOT update activated_time on conflict, to preserve the original activation time.
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO streams (channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(channel_id, stream_id) DO UPDATE SET
		stream_title = excluded.stream_title,
		start_time = excluded.start_time,
		is_live = excluded.is_live,
		media_type = excluded.media_type;
	`, st.ChannelID, st.StreamID, st.StreamTitle, st.StartTime, st.IsLive, st.MediaType, st.ActivatedTime)
	return err
}

// SetStreamLive updates the is_live status of a specific stream.
func (s *Store) SetStreamLive(ctx context.Context, channelID string, streamID string, isLive bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE streams SET is_live = ? WHERE channel_id = ? AND stream_id = ?", isLive, channelID, streamID)
	return err
}

// StreamExists checks if a stream exists in the database.
func (s *Store) StreamExists(ctx context.Context, channelID string, streamID string) (bool, error) {
	var exists bool
	query := "SELECT EXISTS(SELECT 1 FROM streams WHERE channel_id = ? AND stream_id = ?)"
	err := s.db.QueryRowContext(ctx, query, channelID, streamID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}
