// Package store persists events, durable controller state, rolling hourly
// statistics, and config history in SQLite (ADR 0008). It uses the pure-Go
// modernc.org/sqlite driver so Windows binaries build without cgo. The
// active config itself lives in config.json (internal/config); the database
// is never its only copy.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// Registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"
)

// busyTimeoutMS is how long a statement waits on a lock held by another
// connection (another process such as an operator's sqlite3 shell) before
// failing with SQLITE_BUSY.
const busyTimeoutMS = 5000

// Store is an open database. Methods are safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path in WAL mode and applies any
// pending migrations.
func Open(path string) (*Store, error) {
	if path == "" || strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: invalid database path %q", path)
	}
	// Without a "file:" prefix the driver strips the query and opens path
	// verbatim, so Windows paths need no URI escaping. BEGIN IMMEDIATE
	// takes the write lock up front, so a transaction never fails midway
	// upgrading a read lock.
	dsn := path + fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate", busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One connection serialises access inside the process. The write volume
	// is a few rows a second at most, and it rules out SQLITE_BUSY between
	// our own goroutines entirely.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db}
	if err := s.checkWAL(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) checkWAL() error {
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: read journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal mode is %q, want wal", mode)
	}
	return nil
}

// migrations are applied in order; migrations[i] brings the schema to
// version i+1. Never edit a released migration; append a new one.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	time_ms INTEGER NOT NULL,
	type    TEXT    NOT NULL,
	body    TEXT    NOT NULL
);
CREATE INDEX events_time ON events (time_ms);
CREATE INDEX events_type ON events (type, id);

CREATE TABLE documents (
	key        TEXT PRIMARY KEY,
	body       TEXT    NOT NULL,
	updated_ms INTEGER NOT NULL
);

CREATE TABLE stats_hourly (
	hour                 INTEGER PRIMARY KEY,
	loads                INTEGER NOT NULL DEFAULT 0,
	load_seconds_total   REAL    NOT NULL DEFAULT 0,
	load_seconds_max     REAL    NOT NULL DEFAULT 0,
	available_seconds    REAL    NOT NULL DEFAULT 0,
	unavailable_seconds  REAL    NOT NULL DEFAULT 0,
	drains               INTEGER NOT NULL DEFAULT 0,
	forced_preemptions   INTEGER NOT NULL DEFAULT 0,
	crashes              INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE config_history (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	time_ms INTEGER NOT NULL,
	actor   TEXT    NOT NULL,
	config  TEXT    NOT NULL,
	impact  TEXT    NOT NULL
);
CREATE INDEX config_history_time ON config_history (time_ms);
`,
}

// SchemaVersion is the schema version this build migrates to.
func SchemaVersion() int { return len(migrations) }

// migrate brings the schema up to date. Each migration and its version row
// commit together, so an interrupted upgrade resumes where it stopped.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_version: %w", err)
	}
	v, err := s.version()
	if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this build (%d); refusing to downgrade", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		err := s.tx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Store) version() (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// tx runs fn in a transaction, committing on nil and rolling back otherwise.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func ms(t time.Time) int64 { return t.UnixMilli() }
