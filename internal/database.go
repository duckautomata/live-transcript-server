package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func InitDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	// Performance optimizations
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("failed to set pragma '%s': %w", pragma, err)
		}
	}

	// Create tables if not exist
	// streams table
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS streams (
		channel_id TEXT,
		active_id TEXT,
		active_title TEXT,
		start_time TEXT,
		is_live BOOLEAN,
		media_type TEXT,
		PRIMARY KEY (channel_id, active_id)
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating streams table: %w", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS transcripts (
		channel_id TEXT,
		active_id TEXT,
		line_id INTEGER,
		file_id TEXT,
		timestamp INTEGER,
		segments TEXT,
		media_available BOOLEAN DEFAULT 0,
		PRIMARY KEY (channel_id, active_id, line_id)
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating transcripts table: %w", err)
	}

	// Connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	return db, nil
}

// GetStream returns the *active* live stream, or the most recent one if none are live
// Returns nil, nil if no stream is found.
func (a *App) GetStream(ctx context.Context, channelID string) (*Stream, error) {
	// Prioritize live stream
	row := a.DB.QueryRowContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ? AND is_live = 1 ORDER BY start_time DESC LIMIT 1", channelID)
	var s Stream
	err := row.Scan(&s.ChannelID, &s.ActiveID, &s.ActiveTitle, &s.StartTime, &s.IsLive, &s.MediaType)
	if err == nil {
		return &s, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Fallback to most recent stream if no live stream exists
	row = a.DB.QueryRowContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ? ORDER BY start_time DESC LIMIT 1", channelID)
	err = row.Scan(&s.ChannelID, &s.ActiveID, &s.ActiveTitle, &s.StartTime, &s.IsLive, &s.MediaType)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &s, nil
}

// GetStreamByID returns a specific stream by channelID and activeID.
// Returns nil, nil if no stream is found.
func (a *App) GetStreamByID(ctx context.Context, channelID string, activeID string) (*Stream, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ? AND active_id = ?", channelID, activeID)
	var s Stream
	err := row.Scan(&s.ChannelID, &s.ActiveID, &s.ActiveTitle, &s.StartTime, &s.IsLive, &s.MediaType)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetAllStreams retrieves all streams for a channel, ordered by start_time descending.
func (a *App) GetAllStreams(ctx context.Context, channelID string) ([]Stream, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ? ORDER BY start_time DESC", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		if err := rows.Scan(&s.ChannelID, &s.ActiveID, &s.ActiveTitle, &s.StartTime, &s.IsLive, &s.MediaType); err != nil {
			return nil, err
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// GetPastStreams retrieves all inactive streams for a channel, ordered by start_time descending.
func (a *App) GetPastStreams(ctx context.Context, channelID string, activeID string) ([]Stream, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ? AND is_live = 0 AND active_id != ? ORDER BY start_time DESC", channelID, activeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		if err := rows.Scan(&s.ChannelID, &s.ActiveID, &s.ActiveTitle, &s.StartTime, &s.IsLive, &s.MediaType); err != nil {
			return nil, err
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// DeleteStream deletes a specific stream from the database.
func (a *App) DeleteStream(ctx context.Context, channelID string, activeID string) error {
	_, err := a.DB.ExecContext(ctx, "DELETE FROM streams WHERE channel_id = ? AND active_id = ?", channelID, activeID)
	return err
}

// UpsertStream inserts or updates the stream data.
func (a *App) UpsertStream(ctx context.Context, s *Stream) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Since PK is (channel_id, active_id), this upsert works for specific streams
	_, err = tx.ExecContext(ctx, `
	INSERT INTO streams (channel_id, active_id, active_title, start_time, is_live, media_type)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(channel_id, active_id) DO UPDATE SET
		active_title = excluded.active_title,
		start_time = excluded.start_time,
		is_live = excluded.is_live,
		media_type = excluded.media_type;
	`, s.ChannelID, s.ActiveID, s.ActiveTitle, s.StartTime, s.IsLive, s.MediaType)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// SetStreamLive updates the is_live status of a specific stream.
func (a *App) SetStreamLive(ctx context.Context, channelID string, activeID string, isLive bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "UPDATE streams SET is_live = ? WHERE channel_id = ? AND active_id = ?", isLive, channelID, activeID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ReplaceTranscript replaces the entire transcript for a channel/stream with new lines in a transaction.
func (a *App) ReplaceTranscript(ctx context.Context, channelID string, activeID string, lines []Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Delete existing lines for this stream
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND active_id = ?", channelID, activeID); err != nil {
		return err
	}

	// 2. Insert new lines
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO transcripts (channel_id, active_id, line_id, file_id, timestamp, segments, media_available) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, line := range lines {
		// Segments is already json.RawMessage ([]byte), so we can cast it to string directly
		if _, err := stmt.ExecContext(ctx, channelID, activeID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteTranscript deletes all transcript lines for a specific stream.
func (a *App) DeleteTranscript(ctx context.Context, channelID string, activeID string) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND active_id = ?", channelID, activeID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// InsertTranscriptLine inserts a single line into the transcript for a specific stream.
func (a *App) InsertTranscriptLine(ctx context.Context, channelID string, activeID string, line Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
	INSERT INTO transcripts (channel_id, active_id, line_id, file_id, timestamp, segments, media_available)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	`, channelID, activeID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetTranscript retrieves all transcript lines for a channel/stream, ordered by line_id.
func (a *App) GetTranscript(ctx context.Context, channelID string, activeID string) ([]Line, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? AND active_id = ? ORDER BY line_id ASC", channelID, activeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []Line
	for rows.Next() {
		var l Line
		var segmentsStr string
		var fileID sql.NullString
		if err := rows.Scan(&l.ID, &fileID, &l.Timestamp, &segmentsStr, &l.MediaAvailable); err != nil {
			return nil, err
		}
		l.FileID = fileID.String
		// Assign directly to json.RawMessage
		l.Segments = json.RawMessage(segmentsStr)
		lines = append(lines, l)
	}

	return lines, nil
}

// GetLastLineID returns the ID of the last transcript line for a channel/stream.
// Returns -1 if no lines exist.
func (a *App) GetLastLineID(ctx context.Context, channelID string, activeID string) (int, error) {
	var id int
	err := a.DB.QueryRowContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? AND active_id = ? ORDER BY line_id DESC LIMIT 1", channelID, activeID).Scan(&id)
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
func (a *App) GetLastLine(ctx context.Context, channelID string, activeID string) (*Line, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? AND active_id = ? ORDER BY line_id DESC LIMIT 1", channelID, activeID)

	var l Line
	var segmentsStr string
	var fileID sql.NullString
	err := row.Scan(&l.ID, &fileID, &l.Timestamp, &segmentsStr, &l.MediaAvailable)
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

// SetMediaAvailable updates the media_available status of a transcript line for a specific stream.
func (a *App) SetMediaAvailable(ctx context.Context, channelID string, activeID string, lineID int, fileID string, available bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, "UPDATE transcripts SET media_available = ?, file_id = ? WHERE channel_id = ? AND active_id = ? AND line_id = ?", available, fileID, channelID, activeID, lineID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("line not found")
	}

	return tx.Commit()
}

// GetLastAvailableMediaFiles returns the last 'limit' line ID->FileID map that have media available for a specific stream.
// If limit is -1, returns all available media files.
func (a *App) GetLastAvailableMediaFiles(ctx context.Context, channelID string, activeID string, limit int) (map[int]string, error) {
	var rows *sql.Rows
	var err error
	if limit == -1 {
		rows, err = a.DB.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND active_id = ? AND media_available = 1 ORDER BY line_id DESC", channelID, activeID)
	} else {
		rows, err = a.DB.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND active_id = ? AND media_available = 1 ORDER BY line_id DESC LIMIT ?", channelID, activeID, limit)
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

	return files, nil
}

// GetFileIDsInRange returns the file IDs for lines in the given range [startID, endID].
func (a *App) GetFileIDsInRange(ctx context.Context, channelID string, activeID string, startID, endID int) ([]string, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT file_id FROM transcripts WHERE channel_id = ? AND active_id = ? AND line_id >= ? AND line_id <= ? AND media_available = 1 ORDER BY line_id ASC", channelID, activeID, startID, endID)
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
	return fileIDs, nil
}

// StreamExists checks if a stream exists in the database.
func (a *App) StreamExists(ctx context.Context, channelID string, activeID string) (bool, error) {
	var exists bool
	query := "SELECT EXISTS(SELECT 1 FROM streams WHERE channel_id = ? AND active_id = ?)"
	err := a.DB.QueryRowContext(ctx, query, channelID, activeID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}
