package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
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
		channel_id TEXT PRIMARY KEY,
		active_id TEXT,
		active_title TEXT,
		start_time TEXT,
		is_live BOOLEAN,
		media_type TEXT
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating streams table: %w", err)
	}

	// transcripts table
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS transcripts (
		channel_id TEXT,
		line_id INTEGER,
		timestamp INTEGER,
		segments TEXT,
		media_available BOOLEAN DEFAULT 0,
		PRIMARY KEY (channel_id, line_id)
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

// GetStream retrieves the stream state for a channel.
// Returns nil, nil if no stream is found.
func (a *App) GetStream(ctx context.Context, channelID string) (*Stream, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams WHERE channel_id = ?", channelID)
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

// UpsertStream inserts or updates the stream data.
func (a *App) UpsertStream(ctx context.Context, s *Stream) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
	INSERT INTO streams (channel_id, active_id, active_title, start_time, is_live, media_type)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(channel_id) DO UPDATE SET
		active_id = excluded.active_id,
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

// SetStreamLive updates the is_live status of a stream.
func (a *App) SetStreamLive(ctx context.Context, channelID string, isLive bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "UPDATE streams SET is_live = ? WHERE channel_id = ?", isLive, channelID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ReplaceTranscript replaces the entire transcript for a channel with new lines in a transaction.
func (a *App) ReplaceTranscript(ctx context.Context, channelID string, lines []Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Delete existing lines
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ?", channelID); err != nil {
		return err
	}

	// 2. Insert new lines
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO transcripts (channel_id, line_id, timestamp, segments, media_available) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, line := range lines {
		segmentsBytes, err := json.Marshal(line.Segments)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, channelID, line.ID, line.Timestamp, string(segmentsBytes), line.MediaAvailable); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ClearTranscript deletes all transcript lines for a channel.
func (a *App) ClearTranscript(ctx context.Context, channelID string) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ?", channelID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// InsertTranscriptLine inserts a single line into the transcript.
func (a *App) InsertTranscriptLine(ctx context.Context, channelID string, line Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	segmentsBytes, err := json.Marshal(line.Segments)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO transcripts (channel_id, line_id, timestamp, segments, media_available)
	VALUES (?, ?, ?, ?, ?)
	`, channelID, line.ID, line.Timestamp, string(segmentsBytes), line.MediaAvailable)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetTranscript retrieves all transcript lines for a channel, ordered by line_id.
func (a *App) GetTranscript(ctx context.Context, channelID string) ([]Line, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT line_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? ORDER BY line_id ASC", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []Line
	for rows.Next() {
		var l Line
		var segmentsStr string
		if err := rows.Scan(&l.ID, &l.Timestamp, &segmentsStr, &l.MediaAvailable); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(segmentsStr), &l.Segments); err != nil {
			slog.Error("failed to unmarshal segments", "err", err)
			l.Segments = []Segment{}
		}
		lines = append(lines, l)
	}

	return lines, nil
}

// GetLastLineID returns the ID of the last transcript line for a channel.
// Returns -1 if no lines exist.
func (a *App) GetLastLineID(ctx context.Context, channelID string) (int, error) {
	var id int
	err := a.DB.QueryRowContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? ORDER BY line_id DESC LIMIT 1", channelID).Scan(&id)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}

	return id, nil
}

// GetLastLine retrieves the last transcript line for a channel.
// Returns nil, nil if no lines exist.
func (a *App) GetLastLine(ctx context.Context, channelID string) (*Line, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT line_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? ORDER BY line_id DESC LIMIT 1", channelID)

	var l Line
	var segmentsStr string
	err := row.Scan(&l.ID, &l.Timestamp, &segmentsStr, &l.MediaAvailable)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(segmentsStr), &l.Segments); err != nil {
		slog.Error("failed to unmarshal segments", "err", err)
		l.Segments = []Segment{}
	}

	return &l, nil
}

// SetMediaAvailable updates the media_available status of a transcript line.
func (a *App) SetMediaAvailable(ctx context.Context, channelID string, lineID int, available bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, "UPDATE transcripts SET media_available = ? WHERE channel_id = ? AND line_id = ?", available, channelID, lineID)
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

// GetLastAvailableMediaIDs returns the last 'limit' line IDs that have media available.
func (a *App) GetLastAvailableMediaIDs(ctx context.Context, channelID string, limit int) ([]int, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? AND media_available = 1 ORDER BY line_id DESC LIMIT ?", channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	// Reverse the list so it is in ascending order
	slices.Reverse(ids)

	return ids, nil
}
