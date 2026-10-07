// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	// Serialize access through one connection: combined with WAL this keeps
	// concurrent submissions free of busy errors while the UNIQUE constraint
	// remains the guarantee that one identity is stored exactly once.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS placements (
	namespace  TEXT NOT NULL,
	name       TEXT NOT NULL,
	input_json TEXT NOT NULL,
	status     TEXT NOT NULL,
	node       TEXT,
	reason     TEXT,
	queue      TEXT NOT NULL GENERATED ALWAYS AS (json_extract(input_json, '$.queue')) VIRTUAL,
	PRIMARY KEY (namespace, name)
);

CREATE INDEX IF NOT EXISTS idx_placements_ns   ON placements (namespace);
CREATE INDEX IF NOT EXISTS idx_placements_queue ON placements (queue);
CREATE INDEX IF NOT EXISTS idx_placements_node  ON placements (node);
`
