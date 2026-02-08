package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func InitDB(path string, config DatabaseConfig) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	// Set defaults if not provided in config
	if config.JournalMode == "" {
		config.JournalMode = "WAL"
	}
	if config.BusyTimeoutMS == 0 {
		config.BusyTimeoutMS = 5000
	}
	if config.Synchronous == "" {
		config.Synchronous = "NORMAL"
	}
	if config.CacheSizeKB == 0 {
		config.CacheSizeKB = 200000 // 200MB
	}
	if config.TempStore == "" {
		config.TempStore = "MEMORY"
	}
	if config.MmapSizeBytes == 0 {
		config.MmapSizeBytes = 500000000 // 500MB
	}

	// Performance optimizations
	pragmas := []string{
		fmt.Sprintf("PRAGMA journal_mode=%s;", config.JournalMode),
		fmt.Sprintf("PRAGMA busy_timeout=%d;", config.BusyTimeoutMS),
		fmt.Sprintf("PRAGMA synchronous=%s;", config.Synchronous),
		"PRAGMA foreign_keys=ON;",
		fmt.Sprintf("PRAGMA cache_size=%d;", -config.CacheSizeKB), // Negate to specify KB
		fmt.Sprintf("PRAGMA temp_store=%s;", config.TempStore),
		fmt.Sprintf("PRAGMA mmap_size=%d;", config.MmapSizeBytes),
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
		stream_id TEXT,
		stream_title TEXT,
		start_time TEXT,
		is_live BOOLEAN,
		media_type TEXT,
		activated_time INTEGER DEFAULT 0,
		PRIMARY KEY (channel_id, stream_id)
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating streams table: %w", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS transcripts (
		channel_id TEXT,
		stream_id TEXT,
		line_id INTEGER,
		file_id TEXT,
		timestamp INTEGER,
		segments TEXT,
		media_available BOOLEAN DEFAULT 0,
		PRIMARY KEY (channel_id, stream_id, line_id)
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating transcripts table: %w", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS worker_status (
		channel_key TEXT PRIMARY KEY,
		worker_version TEXT,
		worker_build_time TEXT,
		last_seen INTEGER
	);
	`)
	if err != nil {
		return nil, fmt.Errorf("error creating worker_status table: %w", err)
	}

	// Connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Warm up the database to populate the cache
	if config.JournalMode != "MEMORY" && path != ":memory:" {
		go func() {
			// Run in background to not block startup, though for small DBs it's fast.
			// A full table scan forces pages into memory.
			var count int
			if err := db.QueryRow("SELECT count(*) FROM transcripts").Scan(&count); err != nil {
				fmt.Printf("failed to warm up transcripts: %v\n", err) // using fmt as slog not imported here specifically for this, or could import slog
			}
			if err := db.QueryRow("SELECT count(*) FROM streams").Scan(&count); err != nil {
				fmt.Printf("failed to warm up streams: %v\n", err)
			}
		}()
	}

	return db, nil
}

// GetRecentStream returns the stream with the most recent activated_time.
// Returns nil, nil if no stream is found.
func (a *App) GetRecentStream(ctx context.Context, channelID string) (*Stream, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? ORDER BY activated_time DESC LIMIT 1", channelID)
	var s Stream
	err := row.Scan(&s.ChannelID, &s.StreamID, &s.StreamTitle, &s.StartTime, &s.IsLive, &s.MediaType, &s.ActivatedTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &s, nil
}

// GetStreamByID returns a specific stream by channelID and streamID.
// Returns nil, nil if no stream is found.
func (a *App) GetStreamByID(ctx context.Context, channelID string, streamID string) (*Stream, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	var s Stream
	err := row.Scan(&s.ChannelID, &s.StreamID, &s.StreamTitle, &s.StartTime, &s.IsLive, &s.MediaType, &s.ActivatedTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetAllStreams retrieves all streams for a channel, ordered by activated_time descending.
func (a *App) GetAllStreams(ctx context.Context, channelID string) ([]Stream, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? ORDER BY activated_time DESC", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		if err := rows.Scan(&s.ChannelID, &s.StreamID, &s.StreamTitle, &s.StartTime, &s.IsLive, &s.MediaType, &s.ActivatedTime); err != nil {
			return nil, err
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// GetPastStreams retrieves all inactive streams for a channel, ordered by activated_time descending.
func (a *App) GetPastStreams(ctx context.Context, channelID string, excludeStreamID string) ([]Stream, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time FROM streams WHERE channel_id = ? AND is_live = 0 AND stream_id != ? ORDER BY activated_time DESC", channelID, excludeStreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		if err := rows.Scan(&s.ChannelID, &s.StreamID, &s.StreamTitle, &s.StartTime, &s.IsLive, &s.MediaType, &s.ActivatedTime); err != nil {
			return nil, err
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// DeleteStream deletes a specific stream from the database.
func (a *App) DeleteStream(ctx context.Context, channelID string, streamID string) error {
	_, err := a.DB.ExecContext(ctx, "DELETE FROM streams WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	return err
}

// UpsertStream inserts or updates the stream data.
func (a *App) UpsertStream(ctx context.Context, s *Stream) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Since PK is (channel_id, stream_id), this upsert works for specific streams
	// We do NOT update activated_time on conflict, to preserve the original activation time.
	_, err = tx.ExecContext(ctx, `
	INSERT INTO streams (channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(channel_id, stream_id) DO UPDATE SET
		stream_title = excluded.stream_title,
		start_time = excluded.start_time,
		is_live = excluded.is_live,
		media_type = excluded.media_type;
	`, s.ChannelID, s.StreamID, s.StreamTitle, s.StartTime, s.IsLive, s.MediaType, s.ActivatedTime)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// SetStreamLive updates the is_live status of a specific stream.
func (a *App) SetStreamLive(ctx context.Context, channelID string, streamID string, isLive bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "UPDATE streams SET is_live = ? WHERE channel_id = ? AND stream_id = ?", isLive, channelID, streamID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ReplaceTranscript replaces the entire transcript for a channel/stream with new lines in a transaction.
func (a *App) ReplaceTranscript(ctx context.Context, channelID string, streamID string, lines []Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Delete existing lines for this stream
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND stream_id = ?", channelID, streamID); err != nil {
		return err
	}

	// 2. Insert new lines
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO transcripts (channel_id, stream_id, line_id, file_id, timestamp, segments, media_available) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, line := range lines {
		// Segments is already json.RawMessage ([]byte), so we can cast it to string directly
		if _, err := stmt.ExecContext(ctx, channelID, streamID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteTranscript deletes all transcript lines for a specific stream.
func (a *App) DeleteTranscript(ctx context.Context, channelID string, streamID string) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "DELETE FROM transcripts WHERE channel_id = ? AND stream_id = ?", channelID, streamID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// InsertTranscriptLine inserts a single line into the transcript for a specific stream.
func (a *App) InsertTranscriptLine(ctx context.Context, channelID string, streamID string, line Line) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
	INSERT INTO transcripts (channel_id, stream_id, line_id, file_id, timestamp, segments, media_available)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	`, channelID, streamID, line.ID, line.FileID, line.Timestamp, string(line.Segments), line.MediaAvailable)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetTranscript retrieves all transcript lines for a channel/stream, ordered by line_id.
func (a *App) GetTranscript(ctx context.Context, channelID string, streamID string) ([]Line, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id ASC", channelID, streamID)
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
func (a *App) GetLastLineID(ctx context.Context, channelID string, streamID string) (int, error) {
	var id int
	err := a.DB.QueryRowContext(ctx, "SELECT line_id FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id DESC LIMIT 1", channelID, streamID).Scan(&id)
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
func (a *App) GetLastLine(ctx context.Context, channelID string, streamID string) (*Line, error) {
	row := a.DB.QueryRowContext(ctx, "SELECT line_id, file_id, timestamp, segments, media_available FROM transcripts WHERE channel_id = ? AND stream_id = ? ORDER BY line_id DESC LIMIT 1", channelID, streamID)

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
func (a *App) SetMediaAvailable(ctx context.Context, channelID string, streamID string, lineID int, fileID string, available bool) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, "UPDATE transcripts SET media_available = ?, file_id = ? WHERE channel_id = ? AND stream_id = ? AND line_id = ?", available, fileID, channelID, streamID, lineID)
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
func (a *App) GetLastAvailableMediaFiles(ctx context.Context, channelID string, streamID string, limit int) (map[int]string, error) {
	var rows *sql.Rows
	var err error
	if limit == -1 {
		rows, err = a.DB.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND media_available = 1 ORDER BY line_id DESC", channelID, streamID)
	} else {
		rows, err = a.DB.QueryContext(ctx, "SELECT line_id, file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND media_available = 1 ORDER BY line_id DESC LIMIT ?", channelID, streamID, limit)
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
func (a *App) GetFileIDsInRange(ctx context.Context, channelID string, streamID string, startID, endID int) ([]string, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT file_id FROM transcripts WHERE channel_id = ? AND stream_id = ? AND line_id >= ? AND line_id <= ? AND media_available = 1 ORDER BY line_id ASC", channelID, streamID, startID, endID)
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
func (a *App) StreamExists(ctx context.Context, channelID string, streamID string) (bool, error) {
	var exists bool
	query := "SELECT EXISTS(SELECT 1 FROM streams WHERE channel_id = ? AND stream_id = ?)"
	err := a.DB.QueryRowContext(ctx, query, channelID, streamID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// UpsertWorkerStatus updates the status of a worker for a specific key.
func (a *App) UpsertWorkerStatus(ctx context.Context, key, version, buildTime string, lastSeen int64) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
	INSERT INTO worker_status (channel_key, worker_version, worker_build_time, last_seen)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(channel_key) DO UPDATE SET
		worker_version = excluded.worker_version,
		worker_build_time = excluded.worker_build_time,
		last_seen = excluded.last_seen;
	`, key, version, buildTime, lastSeen)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetAllWorkerStatus retrieves the status of all workers.
func (a *App) GetAllWorkerStatus(ctx context.Context) ([]WorkerStatus, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT channel_key, worker_version, worker_build_time, last_seen FROM worker_status ORDER BY last_seen DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []WorkerStatus
	for rows.Next() {
		var s WorkerStatus
		if err := rows.Scan(&s.ChannelKey, &s.WorkerVersion, &s.WorkerBuildTime, &s.LastSeen); err != nil {
			return nil, err
		}
		statuses = append(statuses, s)
	}

	return statuses, nil
}

// ResetWorkerStatus clears the worker_status table.
func (a *App) ResetWorkerStatus(ctx context.Context) error {
	_, err := a.DB.ExecContext(ctx, "DELETE FROM worker_status")
	return err
}

// CleanupOrphanedTranscripts deletes transcript lines that do not have a corresponding stream in the streams table.
func (a *App) CleanupOrphanedTranscripts(ctx context.Context) error {
	_, err := a.DB.ExecContext(ctx, "DELETE FROM transcripts WHERE (channel_id, stream_id) NOT IN (SELECT channel_id, stream_id FROM streams)")
	return err
}
