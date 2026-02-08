package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"live-transcript-server/internal"

	_ "github.com/mattn/go-sqlite3"
)

func TestMigration(t *testing.T) {
	tempDir := t.TempDir()
	oldDBPath := filepath.Join(tempDir, "old.db")
	newDBPath := filepath.Join(tempDir, "new.db")

	// 1. Setup Old DB (Legacy Schema)
	oldDB, err := sql.Open("sqlite3", oldDBPath)
	if err != nil {
		t.Fatalf("failed to open old db: %v", err)
	}
	defer oldDB.Close()

	if _, err := oldDB.Exec(`
	CREATE TABLE streams (
		channel_id TEXT,
		active_id TEXT,
		active_title TEXT,
		start_time TEXT,
		is_live BOOLEAN,
		media_type TEXT,
		PRIMARY KEY (channel_id, active_id)
	);
	CREATE TABLE transcripts (
		channel_id TEXT,
		active_id TEXT,
		line_id INTEGER,
		file_id TEXT,
		timestamp INTEGER,
		segments TEXT,
		media_available BOOLEAN DEFAULT 0,
		PRIMARY KEY (channel_id, active_id, line_id)
	);
	`); err != nil {
		t.Fatalf("failed to create old schema: %v", err)
	}

	// Insert Data
	startTime := time.Now().Unix()
	startTimeStr := fmt.Sprintf("%d", startTime)
	if _, err := oldDB.Exec(`INSERT INTO streams VALUES ('ch1', 's1', 'Title 1', ?, 1, 'audio')`, startTimeStr); err != nil {
		t.Fatalf("failed to insert stream: %v", err)
	}
	if _, err := oldDB.Exec(`INSERT INTO transcripts VALUES ('ch1', 's1', 0, 'f1', 100, '[]', 1)`); err != nil {
		t.Fatalf("failed to insert transcript: %v", err)
	}

	// 2. Setup New DB (New Schema via InitDB)
	newDB, err := internal.InitDB(newDBPath, internal.DatabaseConfig{})
	if err != nil {
		t.Fatalf("failed to init new db: %v", err)
	}
	defer newDB.Close()

	// 3. Run Migration
	if err := migrateStreams(oldDB, newDB); err != nil {
		t.Fatalf("migrateStreams failed: %v", err)
	}
	if err := migrateTranscripts(oldDB, newDB); err != nil {
		t.Fatalf("migrateTranscripts failed: %v", err)
	}

	// 4. Verify
	var s internal.Stream
	row := newDB.QueryRow("SELECT channel_id, stream_id, start_time, activated_time FROM streams WHERE channel_id='ch1' AND stream_id='s1'")
	if err := row.Scan(&s.ChannelID, &s.StreamID, &s.StartTime, &s.ActivatedTime); err != nil {
		t.Fatalf("failed to scan migrated stream: %v", err)
	}

	if s.ActivatedTime != startTime {
		t.Errorf("expected activated_time %d, got %d", startTime, s.ActivatedTime)
	}
	if s.StartTime != startTimeStr {
		t.Errorf("expected start_time %s, got %s", startTimeStr, s.StartTime)
	}

	var count int
	newDB.QueryRow("SELECT count(*) FROM transcripts").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 transcript, got %d", count)
	}
}
