package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"live-transcript-server/internal"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	oldDBPath := flag.String("old", "", "Path to the old database file")
	newDBPath := flag.String("new", "", "Path to the new database file")
	flag.Parse()

	if *oldDBPath == "" || *newDBPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	if _, err := os.Stat(*oldDBPath); os.IsNotExist(err) {
		log.Fatalf("Old database file does not exist: %s", *oldDBPath)
	}

	// 1. Initialize New DB (creates schema)
	// We use internal.InitDB to insure schema is correct being created
	// Passing an empty config as we only need the DB connection and schema creation
	newDB, err := internal.InitDB(*newDBPath, internal.DatabaseConfig{})
	if err != nil {
		log.Fatalf("Failed to initialize new database: %v", err)
	}
	defer newDB.Close()

	// 2. Open Old DB
	oldDB, err := sql.Open("sqlite3", *oldDBPath)
	if err != nil {
		log.Fatalf("Failed to open old database: %v", err)
	}
	defer oldDB.Close()

	log.Println("Migrating streams...")
	if err := migrateStreams(oldDB, newDB); err != nil {
		log.Fatalf("Failed to migrate streams: %v", err)
	}

	log.Println("Migrating transcripts...")
	if err := migrateTranscripts(oldDB, newDB); err != nil {
		log.Fatalf("Failed to migrate transcripts: %v", err)
	}

	log.Println("Migrating worker_status...")
	if err := migrateWorkerStatus(oldDB, newDB); err != nil {
		log.Printf("Warning: Failed to migrate worker_status (migth not exist in old DB): %v", err)
	}

	log.Println("Migration completed successfully.")
}

func migrateStreams(oldDB, newDB *sql.DB) error {
	// Select columns that exist in the old schema
	rows, err := oldDB.Query("SELECT channel_id, active_id, active_title, start_time, is_live, media_type FROM streams")
	if err != nil {
		return fmt.Errorf("query streams: %w", err)
	}
	defer rows.Close()

	tx, err := newDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO streams (channel_id, stream_id, stream_title, start_time, is_live, media_type, activated_time) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rows.Next() {
		var s internal.Stream
		// Scan into struct fields (ActiveID -> StreamID, ActiveTitle -> StreamTitle)
		if err := rows.Scan(&s.ChannelID, &s.StreamID, &s.StreamTitle, &s.StartTime, &s.IsLive, &s.MediaType); err != nil {
			return err
		}

		// Parse start_time to int64 for activated_time
		activatedTime, err := strconv.ParseInt(s.StartTime, 10, 64)
		if err != nil {
			log.Printf("Warning: failed to parse start_time '%s' for stream %s/%s, defaulting activated_time to 0", s.StartTime, s.ChannelID, s.StreamID)
			activatedTime = 0
		}

		if _, err := stmt.Exec(s.ChannelID, s.StreamID, s.StreamTitle, s.StartTime, s.IsLive, s.MediaType, activatedTime); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func migrateTranscripts(oldDB, newDB *sql.DB) error {
	// transcripts schema: channel_id, active_id, line_id, file_id, timestamp, segments, media_available
	rows, err := oldDB.Query("SELECT channel_id, active_id, line_id, file_id, timestamp, segments, media_available FROM transcripts")
	if err != nil {
		return fmt.Errorf("query transcripts: %w", err)
	}
	defer rows.Close()

	tx, err := newDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO transcripts (channel_id, stream_id, line_id, file_id, timestamp, segments, media_available) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rows.Next() {
		var channelID, streamID, fileID, segments string
		var lineID, timestamp int64
		var mediaAvailable bool

		if err := rows.Scan(&channelID, &streamID, &lineID, &fileID, &timestamp, &segments, &mediaAvailable); err != nil {
			return err
		}

		if _, err := stmt.Exec(channelID, streamID, lineID, fileID, timestamp, segments, mediaAvailable); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func migrateWorkerStatus(oldDB, newDB *sql.DB) error {
	rows, err := oldDB.Query("SELECT channel_key, worker_version, worker_build_time, last_seen FROM worker_status")
	if err != nil {
		// Table might not exist in old DB, which is fine
		return err
	}
	defer rows.Close()

	tx, err := newDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO worker_status (channel_key, worker_version, worker_build_time, last_seen) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rows.Next() {
		var key, ver, build string
		var lastSeen int64
		if err := rows.Scan(&key, &ver, &build, &lastSeen); err != nil {
			return err
		}
		if _, err := stmt.Exec(key, ver, build, lastSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}
