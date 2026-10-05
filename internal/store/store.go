// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	// SQLite allows a single writer; one connection serializes access instead of
	// surfacing SQLITE_BUSY to callers under concurrent submissions.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	for _, statement := range []string{metadataSchema, placementsSchema} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SavePlacement inserts a placement record identified by (namespace, name). When a
// record already exists the insert is skipped and the stored request and record are
// returned so the caller can decide whether the resubmission matches. The insert is
// atomic, so concurrent submissions can only ever create one row.
func (s *Store) SavePlacement(namespace, name, queue string, node *string, request, record []byte) (existingRequest, existingRecord []byte, created bool, err error) {
	var nodeArg any
	if node != nil {
		nodeArg = *node
	}
	res, err := s.db.Exec(
		`INSERT INTO placements (namespace, name, queue, node, request, record)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (namespace, name) DO NOTHING`,
		namespace, name, queue, nodeArg, string(request), string(record),
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("insert placement: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, nil, false, fmt.Errorf("insert placement: %w", err)
	}
	if affected == 1 {
		return nil, nil, true, nil
	}
	var storedRequest, storedRecord string
	err = s.db.QueryRow(
		`SELECT request, record FROM placements WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&storedRequest, &storedRecord)
	if err != nil {
		return nil, nil, false, fmt.Errorf("load placement: %w", err)
	}
	return []byte(storedRequest), []byte(storedRecord), false, nil
}

// GetPlacement returns the stored record for (namespace, name).
func (s *Store) GetPlacement(namespace, name string) (record []byte, found bool, err error) {
	var stored string
	err = s.db.QueryRow(
		`SELECT record FROM placements WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load placement: %w", err)
	}
	return []byte(stored), true, nil
}

// ListPlacements returns the stored records matching every non-nil filter, ordered by
// namespace then name in UTF-8 byte order (SQLite BINARY collation).
func (s *Store) ListPlacements(namespace, queue, node *string) ([][]byte, error) {
	query := `SELECT record FROM placements`
	var conditions []string
	var args []any
	if namespace != nil {
		conditions = append(conditions, `namespace = ?`)
		args = append(args, *namespace)
	}
	if queue != nil {
		conditions = append(conditions, `queue = ?`)
		args = append(args, *queue)
	}
	if node != nil {
		conditions = append(conditions, `node = ?`)
		args = append(args, *node)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY namespace, name`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list placements: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return nil, fmt.Errorf("list placements: %w", err)
		}
		records = append(records, []byte(stored))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list placements: %w", err)
	}
	return records, nil
}

const metadataSchema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

const placementsSchema = `
CREATE TABLE IF NOT EXISTS placements (
	namespace TEXT NOT NULL,
	name      TEXT NOT NULL,
	queue     TEXT NOT NULL,
	node      TEXT,
	request   TEXT NOT NULL,
	record    TEXT NOT NULL,
	PRIMARY KEY (namespace, name)
);
`
