// Package store is the SQLite persistence layer. It owns the schema, the
// connection pool, and every query the server runs; callers never touch
// *sql.DB directly.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"live-transcript-server/internal/config"

	"github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned (wrapped) when a targeted row does not exist.
var ErrNotFound = errors.New("not found")

// ErrOutOfSync is returned (wrapped) by InsertNextLine when the incoming line
// ID is not exactly one past the last stored line.
var ErrOutOfSync = errors.New("out of sync")

// Store wraps the SQLite database handle.
type Store struct {
	db *sql.DB
}

// dsnConnector opens every pooled connection from the same DSN through a
// driver carrying a ConnectHook, without registering a global driver name.
type dsnConnector struct {
	dsn    string
	driver *sqlite3.SQLiteDriver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c dsnConnector) Driver() driver.Driver {
	return c.driver
}

// Open opens (creating if needed) the SQLite database at path, applies the
// performance PRAGMAs from cfg to every pooled connection, and ensures the
// schema exists. PRAGMAs are encoded as DSN parameters (or a per-connection
// hook for the two the driver has no parameter for) rather than db.Exec so
// they configure every connection in the pool, not just one.
func Open(path string, cfg config.DatabaseConfig) (*Store, error) {
	// Set defaults if not provided in config
	if cfg.JournalMode == "" {
		cfg.JournalMode = "WAL"
	}
	if cfg.BusyTimeoutMS == 0 {
		cfg.BusyTimeoutMS = 5000
	}
	if cfg.Synchronous == "" {
		cfg.Synchronous = "NORMAL"
	}
	if cfg.CacheSizeKB == 0 {
		cfg.CacheSizeKB = 200000 // 200MB
	}
	if cfg.TempStore == "" {
		cfg.TempStore = "MEMORY"
	}
	if cfg.MmapSizeBytes == 0 {
		cfg.MmapSizeBytes = 500000000 // 500MB
	}

	params := url.Values{}
	params.Set("_journal_mode", cfg.JournalMode)
	params.Set("_busy_timeout", strconv.Itoa(cfg.BusyTimeoutMS))
	params.Set("_synchronous", cfg.Synchronous)
	params.Set("_foreign_keys", "on")
	params.Set("_cache_size", strconv.Itoa(-cfg.CacheSizeKB)) // Negate to specify KB

	// temp_store and mmap_size have no DSN parameter in mattn/go-sqlite3
	// (unknown parameters are silently ignored), so apply them through a
	// per-connection hook to get the same every-connection coverage.
	drv := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			pragmas := []string{
				fmt.Sprintf("PRAGMA temp_store=%s;", cfg.TempStore),
				fmt.Sprintf("PRAGMA mmap_size=%d;", cfg.MmapSizeBytes),
			}
			for _, pragma := range pragmas {
				if _, err := conn.Exec(pragma, nil); err != nil {
					return fmt.Errorf("failed to set pragma '%s': %w", pragma, err)
				}
			}
			return nil
		},
	}

	db := sql.OpenDB(dsnConnector{dsn: path + "?" + params.Encode(), driver: drv})

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	// Connection pool settings. An in-memory database exists per connection,
	// so it must be pinned to a single never-expiring connection or the pool
	// hands out fresh empty databases.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(25)
		db.SetConnMaxLifetime(5 * time.Minute)
	}

	// Warm up the database to populate the cache
	if !cfg.SkipWarmup && cfg.JournalMode != "MEMORY" && path != ":memory:" {
		go func() {
			// Run in background to not block startup, though for small DBs it's fast.
			// A full table scan forces pages into memory.
			var count int
			if err := db.QueryRow("SELECT count(*) FROM transcripts").Scan(&count); err != nil {
				slog.Warn("failed to warm up transcripts", "err", err)
			}
			if err := db.QueryRow("SELECT count(*) FROM streams").Scan(&count); err != nil {
				slog.Warn("failed to warm up streams", "err", err)
			}
		}()
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// EnsureSchema creates the server's tables on a raw database handle if they
// don't already exist. It exists for tooling (cmd/migrate) that prepares a
// database outside the Store's own pool; the server itself gets the schema
// through Open.
func EnsureSchema(db *sql.DB) error {
	return createSchema(db)
}

func createSchema(db *sql.DB) error {
	// Create tables if not exist
	// streams table
	_, err := db.Exec(`
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
		return fmt.Errorf("error creating streams table: %w", err)
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
		vod_accurate BOOLEAN DEFAULT 0,
		PRIMARY KEY (channel_id, stream_id, line_id)
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating transcripts table: %w", err)
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
		return fmt.Errorf("error creating worker_status table: %w", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS incoming_streams (
		channel_key TEXT,
		url TEXT,
		received_at INTEGER NOT NULL,
		PRIMARY KEY (channel_key, url)
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating incoming_streams table: %w", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS worker_restart_requests (
		channel_key TEXT PRIMARY KEY,
		requested_at INTEGER NOT NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating worker_restart_requests table: %w", err)
	}

	return nil
}
