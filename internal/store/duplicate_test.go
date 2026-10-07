package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Regression suite for the duplicate-identity judgment in Submit. The verdict
// must come from the driver's structured result code alone, so these cases
// pin the boundary: genuine primary-key conflicts (even wrapped) enter the
// read-original path, while look-alike message text and non-unique constraint
// failures stay storage failures.

func sampleRecord() *Placement {
	return &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
	}
}

// captureConstraintError runs a failing INSERT against a real database and
// returns the driver error exactly as Submit receives it.
func captureConstraintError(t *testing.T, st *Store, stmt string, args ...any) error {
	t.Helper()
	_, err := st.db.Exec(stmt, args...)
	if err == nil {
		t.Fatalf("statement did not fail: %s", stmt)
	}
	return err
}

func TestDuplicateIdentityClassification(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "classify.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	p := sampleRecord()
	if _, _, err := st.Submit(context.Background(), p); err != nil {
		t.Fatalf("seed submit: %v", err)
	}

	// The real primary-key conflict, captured from the driver itself. The
	// payload carries a queue so the generated NOT NULL column is satisfied
	// and the primary key is the only constraint that can fire.
	pkErr := captureConstraintError(t, st,
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		"ns", "job", `{"queue":"q"}`, "placed", nil, nil,
	)
	var sqlErr *sqlite.Error
	if !errors.As(pkErr, &sqlErr) {
		t.Fatalf("driver error is %T, want *sqlite.Error — test premise broken", pkErr)
	}
	if code := sqlErr.Code(); code != sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY && code != sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		t.Fatalf("primary-key conflict code = %d, want %d or %d — test premise broken",
			code, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}

	// A different constraint on the same table: input_json is NOT NULL.
	notNullErr := captureConstraintError(t, st,
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		"ns", "other", nil, "placed", nil, nil,
	)

	// A plain error whose text imitates the SQLite wording.
	lookalike := errors.New("UNIQUE constraint failed: placements.namespace, placements.name")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"real primary-key conflict", pkErr, true},
		{"wrapped primary-key conflict", fmt.Errorf("insert placement: %w", pkErr), true},
		{"double-wrapped primary-key conflict", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", pkErr)), true},
		{"plain error with unique-constraint wording", lookalike, false},
		{"wrapped look-alike text", fmt.Errorf("insert placement: %w", lookalike), false},
		{"not-null constraint violation", notNullErr, false},
		{"wrapped not-null constraint violation", fmt.Errorf("insert placement: %w", notNullErr), false},
		{"unrelated error", errors.New("connection reset by peer"), false},
	}
	for _, tc := range cases {
		if got := isDuplicateIdentity(tc.err); got != tc.want {
			t.Errorf("%s: isDuplicateIdentity = %v, want %v (err: %v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// A genuine duplicate identity goes down the read-original path: the stored
// record, its scheduling result and the row count are all untouched.
func TestSubmitDuplicateIdentityReadsOriginalAndChangesNothing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "dup.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	first := sampleRecord()
	node := "n1"
	first.Node = &node
	if _, created, err := st.Submit(ctx, first); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}

	// Different content on the same identity: must not insert or overwrite.
	second := sampleRecord()
	second.Priority = 99
	second.Nodes = []Node{}
	stored, created, err := st.Submit(ctx, second)
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if created {
		t.Fatalf("duplicate identity reported as created")
	}
	if stored.Priority != 1 || len(stored.Nodes) != 1 || stored.Node == nil || *stored.Node != "n1" {
		t.Fatalf("returned record is not the original: %+v", stored)
	}

	got, err := st.Get(ctx, "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Priority != 1 || len(got.Nodes) != 1 || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("stored record changed by duplicate submit: %+v", got)
	}
	recs, err := st.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
}

// A database file written before this refactor keeps working after a reopen:
// the stored row is queryable and a retry against its identity still takes
// the duplicate path instead of inserting.
func TestSubmitDuplicateIdentityAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, created, err := st.Submit(ctx, sampleRecord()); err != nil || !created {
		t.Fatalf("seed submit: created=%v err=%v", created, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()

	got, err := st.Get(ctx, "ns", "job")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Queue != "q" || got.Priority != 1 {
		t.Fatalf("record decoded wrong after reopen: %+v", got)
	}

	stored, created, err := st.Submit(ctx, sampleRecord())
	if err != nil {
		t.Fatalf("retry after reopen: %v", err)
	}
	if created {
		t.Fatalf("retry after reopen inserted a new row")
	}
	if same, err := SameInput(stored, got); err != nil || !same {
		t.Fatalf("stored record changed by retry: same=%v err=%v", same, err)
	}
	recs, err := st.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
}
