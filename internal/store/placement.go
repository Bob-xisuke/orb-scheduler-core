package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// Submit stores p the first time its (namespace, name) identity is seen. On a
// repeated submission it returns the previously stored record. created reports
// whether the row was inserted by this call.
func (s *Store) Submit(ctx context.Context, p *Placement) (stored *Placement, created bool, err error) {
	payload, err := model.CanonicalInput(p)
	if err != nil {
		return nil, false, fmt.Errorf("encode placement: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin submit: %w", err)
	}
	defer tx.Rollback()

	_, execErr := tx.ExecContext(ctx,
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		p.Namespace, p.Name, string(payload), p.Status, p.Node, p.Reason,
	)
	if execErr == nil {
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("commit submit: %w", err)
		}
		out := *p
		return &out, true, nil
	}
	if !isUniqueViolation(execErr) {
		return nil, false, fmt.Errorf("insert placement: %w", execErr)
	}

	row := tx.QueryRowContext(ctx,
		"SELECT input_json, status, node, reason FROM placements WHERE namespace = ? AND name = ?",
		p.Namespace, p.Name,
	)
	var inputJSON, status string
	var node, reason sql.NullString
	if err := row.Scan(&inputJSON, &status, &node, &reason); err != nil {
		return nil, false, fmt.Errorf("load existing placement: %w", err)
	}
	existing, err := decodeRecord(inputJSON, status, node, reason)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// Get fetches a single placement by its identity. It returns ErrNotFound when
// the identity is unknown.
func (s *Store) Get(ctx context.Context, namespace, name string) (*Placement, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT input_json, status, node, reason FROM placements WHERE namespace = ? AND name = ?",
		namespace, name,
	)
	var inputJSON, status string
	var node, reason sql.NullString
	if err := row.Scan(&inputJSON, &status, &node, &reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get placement: %w", err)
	}
	return decodeRecord(inputJSON, status, node, reason)
}

// List returns committed placements matching f, sorted by namespace then name
// in UTF-8 byte order (SQLite BINARY text ordering).
func (s *Store) List(ctx context.Context, f ListFilter) ([]*Placement, error) {
	var clauses []string
	var args []any
	if f.Namespace != "" {
		clauses = append(clauses, "namespace = ?")
		args = append(args, f.Namespace)
	}
	if f.Queue != "" {
		clauses = append(clauses, "queue = ?")
		args = append(args, f.Queue)
	}
	if f.Node != "" {
		clauses = append(clauses, "node = ?")
		args = append(args, f.Node)
	}
	query := "SELECT input_json, status, node, reason FROM placements"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY namespace, name"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list placements: %w", err)
	}
	defer rows.Close()

	var out []*Placement
	for rows.Next() {
		var inputJSON, status string
		var node, reason sql.NullString
		if err := rows.Scan(&inputJSON, &status, &node, &reason); err != nil {
			return nil, fmt.Errorf("scan placement: %w", err)
		}
		rec, err := decodeRecord(inputJSON, status, node, reason)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate placements: %w", err)
	}
	return out, nil
}

func decodeRecord(inputJSON, status string, node, reason sql.NullString) (*Placement, error) {
	p, err := model.DecodeInput([]byte(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("decode stored placement: %w", err)
	}
	p.Status = status
	if node.Valid {
		v := node.String
		p.Node = &v
	}
	if reason.Valid {
		v := reason.String
		p.Reason = &v
	}
	return p, nil
}

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed")
}
