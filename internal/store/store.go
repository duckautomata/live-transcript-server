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

// ErrNoUpdate is returned by UpdateStream when the update carries no fields.
var ErrNoUpdate = errors.New("no fields to update")

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

	// Every transaction this package opens is a write transaction, and
	// InsertNextLine reads before it writes. A deferred BEGIN would take a read
	// snapshot on that first SELECT and then fail the upgrade to a write lock
	// with SQLITE_BUSY_SNAPSHOT ("database is locked") if another connection
	// committed in between -- a case busy_timeout does not retry, because the
	// snapshot is already stale. BEGIN IMMEDIATE takes the write lock up front,
	// so contention waits out busy_timeout instead of erroring.
	//
	// This is per-connection, so it applies to every BeginTx in the package --
	// the driver ignores sql.TxOptions, ReadOnly included. Reads run outside
	// transactions (plain db.Query), so nothing is serialized today, but a
	// future read-only BeginTx would silently take the write lock. Give reads
	// their own pool without this parameter if that day comes.
	params.Set("_txlock", "immediate")

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

	// Cookie health is worker-global, not per-channel, so it gets its own
	// single-row table rather than a column duplicated across worker_status.
	// It is persisted rather than kept in memory so that a server redeploy
	// mid-outage does not re-ping the operator about an outage they already
	// know about.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS worker_cookie_status (
		worker_id TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		since INTEGER NOT NULL,
		alerted INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating worker_cookie_status table: %w", err)
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

	// detected_broadcasts is live detection's write-once ledger. Detection
	// mechanisms produce a LEVEL signal ("this channel is live right now") and
	// re-observe the same broadcast on every poll; this table is what turns
	// that into exactly one notification per broadcast. The insert is
	// INSERT OR IGNORE and its RowsAffected IS the answer to "am I the first
	// to see this", so the race between the push and poll paths resolves
	// itself with no locking.
	//
	// Keyed on the platform's per-broadcast id, never the URL: every Twitch
	// broadcast for a login shares one URL forever, so a URL-keyed ledger
	// would report a channel's first stream and then stay silent.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS detected_broadcasts (
		platform TEXT NOT NULL,
		broadcast_id TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		started_at INTEGER NOT NULL DEFAULT 0,
		detected_at INTEGER NOT NULL,
		mechanism TEXT NOT NULL DEFAULT '',
		ended_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (platform, broadcast_id)
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating detected_broadcasts table: %w", err)
	}

	// Supports the per-channel "what is live right now" lookup the state
	// poller runs every cycle, and the detected_at pruning sweep.
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_detected_broadcasts_channel
		ON detected_broadcasts (channel_key, detected_at);
	`)
	if err != nil {
		return fmt.Errorf("error creating detected_broadcasts index: %w", err)
	}

	// detected_videos is the same write-once ledger idea for the non-live
	// observations: a stream or premiere being scheduled, a video or a short
	// being published. The poller re-derives these on every cycle and a
	// restart re-derives them from scratch, so the INSERT OR IGNORE is what
	// makes each one announce exactly once. Keyed on kind as well as id because
	// one video is legitimately "scheduled" and then, later, live.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS detected_videos (
		platform TEXT NOT NULL,
		video_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		published_at INTEGER NOT NULL DEFAULT 0,
		scheduled_at INTEGER NOT NULL DEFAULT 0,
		detected_at INTEGER NOT NULL,
		PRIMARY KEY (platform, video_id, kind)
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating detected_videos table: %w", err)
	}
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_detected_videos_channel
		ON detected_videos (channel_key, detected_at);
	`)
	if err != nil {
		return fmt.Errorf("error creating detected_videos index: %w", err)
	}

	// notification_events are the admin-configured announcement rules. The
	// list-valued columns (webhook_urls, triggers) and the embed template are
	// stored as JSON: they are only ever read and written whole, and a rule has
	// at most a handful of each.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS notification_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_key TEXT NOT NULL,
		name TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		webhook_urls TEXT NOT NULL DEFAULT '[]',
		triggers TEXT NOT NULL DEFAULT '[]',
		content TEXT NOT NULL DEFAULT '',
		embed_enabled INTEGER NOT NULL DEFAULT 1,
		embed TEXT NOT NULL DEFAULT '{}',
		cooldown_seconds INTEGER NOT NULL DEFAULT 0,
		last_sent_at INTEGER NOT NULL DEFAULT 0,
		sent_count INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		last_error_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_events table: %w", err)
	}
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_notification_events_channel
		ON notification_events (channel_key, id);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_events index: %w", err)
	}

	// notification_cooldowns holds the last send time of each (rule, trigger).
	// The minimum gap is per TRIGGER, not per rule: it exists to swallow a
	// stream restart (a second "live" minutes after the first), and a rule
	// that announces both "scheduled" and "live" must not have its go-live
	// ping eaten by the waiting-room ping that preceded it.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS notification_cooldowns (
		event_id INTEGER NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
		trigger TEXT NOT NULL,
		last_sent_at INTEGER NOT NULL,
		PRIMARY KEY (event_id, trigger)
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_cooldowns table: %w", err)
	}

	// notification_log is the per-channel delivery trail the admin page shows:
	// what was announced, to how many webhooks, and why something was not
	// (cooldown, failure). Trimmed to a fixed number of rows per channel.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS notification_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_key TEXT NOT NULL,
		event_id INTEGER NOT NULL DEFAULT 0,
		event_name TEXT NOT NULL DEFAULT '',
		trigger TEXT NOT NULL DEFAULT '',
		platform TEXT NOT NULL DEFAULT '',
		broadcast_id TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		webhooks INTEGER NOT NULL DEFAULT 0,
		delivered INTEGER NOT NULL DEFAULT 0,
		sent_at INTEGER NOT NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_log table: %w", err)
	}
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_notification_log_channel
		ON notification_log (channel_key, id);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_log index: %w", err)
	}

	// users are the self-service accounts on the live-transcript site. The
	// password is an argon2id hash (internal/auth); username_key is the
	// lowercased form that uniqueness and login go by, so "Doki" and "doki"
	// cannot both exist while the owner keeps their own capitalisation.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL,
		username_key TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		password_changed_at INTEGER NOT NULL,
		last_login_at INTEGER NOT NULL DEFAULT 0,
		disabled_at INTEGER NOT NULL DEFAULT 0,
		disabled_reason TEXT NOT NULL DEFAULT ''
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating users table: %w", err)
	}

	// site_settings holds the few operator switches that can be flipped at
	// runtime from the site admin page (closing registration during abuse)
	// without a config change and restart.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS site_settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating site_settings table: %w", err)
	}

	// sessions hold only the SHA-256 of each bearer token. The token itself
	// is handed to the browser once and never stored, so this table is
	// worthless to anyone who copies the database.
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		client TEXT NOT NULL DEFAULT '',
		address TEXT NOT NULL DEFAULT ''
	);
	`)
	if err != nil {
		return fmt.Errorf("error creating sessions table: %w", err)
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);`)
	if err != nil {
		return fmt.Errorf("error creating sessions index: %w", err)
	}
	// Columns the accounts tables grew after they were first created.
	for _, c := range []struct{ table, column, ddl string }{
		{"users", "disabled_at", "INTEGER NOT NULL DEFAULT 0"},
		{"users", "disabled_reason", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "client", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "address", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, c.table, c.column, c.ddl); err != nil {
			return err
		}
	}

	// Notification events and their log became per-account after the tables
	// first shipped. Rules from before that carry user_id 0: they keep firing
	// and the admin page can delete them, but nobody can edit them.
	if err := ensureColumn(db, "notification_events", "user_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "notification_log", "user_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_notification_events_user
		ON notification_events (user_id, channel_key, id);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_events user index: %w", err)
	}
	_, err = db.Exec(`
	CREATE INDEX IF NOT EXISTS idx_notification_log_user
		ON notification_log (user_id, channel_key, id);
	`)
	if err != nil {
		return fmt.Errorf("error creating notification_log user index: %w", err)
	}

	return nil
}

// ensureColumn adds a column to an existing table when it is missing. It is
// the whole migration story for a column added after a table shipped: SQLite
// has no ADD COLUMN IF NOT EXISTS, so the table's columns are read first.
func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("reading %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return fmt.Errorf("reading %s columns: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading %s columns: %w", table, err)
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + ddl); err != nil {
		return fmt.Errorf("adding %s.%s: %w", table, column, err)
	}
	return nil
}
