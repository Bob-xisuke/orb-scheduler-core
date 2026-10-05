package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Resources is a workload request or a node capacity. CPU is in millicores and
// memory in MiB; both are non-negative 64-bit integers on the wire.
type Resources struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
}

// Node is one candidate node offered by a placement request.
type Node struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"`
	Memory int64             `json:"memory"`
	Labels map[string]string `json:"labels"`
}

// Placement is one stored scheduling decision: the request payload plus its
// status. Node and Reason are pointers so a rejected record renders them as
// JSON null.
type Placement struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Queue     string            `json:"queue"`
	Priority  int32             `json:"priority"`
	Resources Resources         `json:"resources"`
	Selector  map[string]string `json:"selector"`
	Nodes     []Node            `json:"nodes"`
	Status    string            `json:"status"`
	Node      *string           `json:"node"`
	Reason    *string           `json:"reason"`
}

// ErrNotFound is returned by Get when no placement matches the identity.
var ErrNotFound = errors.New("placement not found")

// Submit stores p the first time its (namespace, name) identity is seen. On a
// repeated submission it returns the previously stored record. created reports
// whether the row was inserted by this call.
func (s *Store) Submit(ctx context.Context, p *Placement) (stored *Placement, created bool, err error) {
	payload, err := canonicalInput(p)
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

// ListFilter narrows List; empty fields are ignored. Node never matches a
// rejected record.
type ListFilter struct {
	Namespace string
	Queue     string
	Node      string
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
	var in inputPayload
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return nil, fmt.Errorf("decode stored placement: %w", err)
	}
	p := &Placement{
		Namespace: in.Namespace,
		Name:      in.Name,
		Queue:     in.Queue,
		Priority:  in.Priority,
		Resources: in.Resources,
		Selector:  in.Selector,
		Nodes:     in.Nodes,
		Status:    status,
	}
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
