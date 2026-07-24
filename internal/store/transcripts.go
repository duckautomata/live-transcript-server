package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"live-transcript-server/internal/model"
)

// ReplaceTranscript replaces the entire transcript for a channel/stream with new lines in a transaction.
func (s *Store) ReplaceTranscript(ctx context.Context, channelID string, streamID string, lines []model.Line) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Delete existing lines for this stream
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND stream_id = ?", channelID, streamID); err != nil {
		return err
	}

	// 2. Insert new lines
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO transcripts (channel_id, stream_id, line_id, file_id, timestamp, segments, media_available, vod_accurate) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, line := range lines {
		// Segments is already json.RawMessage ([]byte), so we can cast it to string directly
		if _, err := stmt.ExecContext(ctx, channelID, streamID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable, line.VodAccurate); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteTranscript deletes all transcript lines for a specific stream.
func (s *Store) DeleteTranscript(ctx context.Context, channelID string, streamID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	return err
}

// InsertNextLine appends a line to the transcript, enforcing that its ID is
// exactly one past the last stored line (or 0 for an empty transcript). The
// check and insert share one transaction so concurrent appends cannot race.
// Returns an error wrapping ErrOutOfSync when the ID does not match.
func (s *Store) InsertNextLine(ctx context.Context, channelID string, streamID string, line model.Line) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// An empty transcript reads as last ID -1, so the first expected ID is 0.
	last := -1
	err = tx.QueryRowContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id DESC LIMIT 1", channelID, streamID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if line.ID != last+1 {
		return fmt.Errorf("expected line id %d, got %d: %w", last+1, line.ID, ErrOutOfSync)
	}

	if _, err := tx.ExecContext(ctx, `
	INSERT INTO transcripts (channel_id, stream_id, line_id, file_id, timestamp, segments, media_available, vod_accurate)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, channelID, streamID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable, line.VodAccurate); err != nil {
		return err
	}

	return tx.Commit()
}

// GetTranscript retrieves all transcript lines for a channel/stream, ordered by line_id.
func (s *Store) GetTranscript(ctx context.Context, channelID string, streamID string) ([]model.Line, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available, vod_accurate FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id ASC", channelID, streamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []model.Line
	for rows.Next() {
		var l model.Line
		var segmentsStr string
		var fileID sql.NullString
		if err := rows.Scan(&l.ID, &fileID, &l.Timestamp, &segmentsStr, &l.MediaAvailable, &l.VodAccurate); err != nil {
			return nil, err
		}
		l.FileID = fileID.String
		// Assign directly to json.RawMessage
		l.Segments = json.RawMessage(segmentsStr)
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

// GetLastLineID returns the ID of the last transcript line for a channel/stream.
// Returns -1 if no lines exist.
func (s *Store) GetLastLineID(ctx context.Context, channelID string, streamID string) (int, error) {
	var id int
	err := s.db.QueryRowContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id DESC LIMIT 1", channelID, streamID).Scan(&id)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}

	return id, nil
}

// GetLastLine retrieves the last transcript line for a channel/stream.
// Returns nil, nil if no lines exist.
func (s *Store) GetLastLine(ctx context.Context, channelID string, streamID string) (*model.Line, error) {
	row := s.db.QueryRowContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available, vod_accurate FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id DESC LIMIT 1", channelID, streamID)

	var l model.Line
	var segmentsStr string
	var fileID sql.NullString
	err := row.Scan(&l.ID, &fileID, &l.Timestamp, &segmentsStr, &l.MediaAvailable, &l.VodAccurate)
	l.FileID = fileID.String
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	l.Segments = json.RawMessage(segmentsStr)

	return &l, nil
}

// SetMediaAvailable updates the media_available status of a transcript line
// for a specific stream. Returns an error wrapping ErrNotFound when the line
// does not exist.
func (s *Store) SetMediaAvailable(ctx context.Context, channelID string, streamID string, lineID int, fileID string, available bool) error {
	result, err := s.db.ExecContext(ctx, "UPDATE transcripts SET media_available = ?, file_id = ? WHERE channel_id = ? AND stream_id = ? AND line_id = ?", available, fileID, channelID, streamID, lineID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("line %d for stream %s/%s: %w", lineID, channelID, streamID, ErrNotFound)
	}
	return nil
}

// GetLastAvailableMediaFiles returns the last 'limit' line ID->FileID map that have media available for a specific stream.
// If limit is -1, returns all available media files.
func (s *Store) GetLastAvailableMediaFiles(ctx context.Context, channelID string, streamID string, limit int) (map[int]string, error) {
	var rows *sql.Rows
	var err error
	if limit == -1 {
		rows, err = s.db.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND media_available = 1 ORDER BY line_id DESC", channelID, streamID)
	} else {
		rows, err = s.db.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND media_available = 1 ORDER BY line_id DESC LIMIT ?", channelID, streamID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	files := make(map[int]string)
	for rows.Next() {
		var id int
		var fileID sql.NullString
		if err := rows.Scan(&id, &fileID); err != nil {
			return nil, err
		}
		files[id] = fileID.String
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return files, nil
}

// GetFileIDsInRange returns the file IDs for lines in the given range [startID, endID].
func (s *Store) GetFileIDsInRange(ctx context.Context, channelID string, streamID string, startID, endID int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND line_id >= ? AND line_id <= ? AND media_available = 1 ORDER BY line_id ASC", channelID, streamID, startID, endID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fileIDs []string
	for rows.Next() {
		var fileID sql.NullString
		if err := rows.Scan(&fileID); err != nil {
			return nil, err
		}
		if fileID.Valid && fileID.String != "" {
			fileIDs = append(fileIDs, fileID.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return fileIDs, nil
}

// CleanupOrphanedTranscripts deletes transcript lines that do not have a corresponding stream in the streams table.
func (s *Store) CleanupOrphanedTranscripts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM transcripts WHERE (channel_id, stream_id) NOT IN (SELECT channel_id, stream_id FROM streams)")
	return err
}
