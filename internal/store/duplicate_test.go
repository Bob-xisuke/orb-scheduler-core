package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// The duplicate-identity judgment must rest on the driver's structured
// result code, not on message text. These cases pin the boundaries: a real
// primary-key collision is a duplicate even when wrapped; lookalike message
// text, other constraint failures and unrelated errors never are.
func TestDuplicateIdentityClassification(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "classify.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	seed := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []Node{},
		Status:    "placed",
	}
	if _, _, err := st.Submit(ctx, seed); err != nil {
		t.Fatalf("seed submit: %v", err)
	}

	// A genuine primary-key violation raised by SQLite itself. Submit treats
	// it as a retry, so provoke the raw driver error with a direct insert
	// against the stored identity.
	_, realDuplicate := st.db.ExecContext(ctx,
		`INSERT INTO placements (namespace, name, input_json, status) VALUES ('ns', 'job', '{"queue":"q"}', 'placed')`)
	if realDuplicate == nil {
		t.Fatalf("setup: direct duplicate insert unexpectedly succeeded")
	}

	// A different constraint failure from the same engine: NOT NULL.
	_, notNull := st.db.ExecContext(ctx,
		`INSERT INTO placements (namespace, name, input_json, status) VALUES ('ns', NULL, '{"queue":"q"}', 'placed')`)
	if notNull == nil {
		t.Fatalf("setup: NOT NULL violation unexpectedly succeeded")
	}

	lookalike := errors.New("constraint failed: UNIQUE constraint failed: placements.namespace, placements.name (1555)")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"real primary key violation", realDuplicate, true},
		{"wrapped real violation", fmt.Errorf("insert placement: %w", realDuplicate), true},
		{"double wrapped real violation", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", realDuplicate)), true},
		{"not-null constraint violation", notNull, false},
		{"wrapped not-null violation", fmt.Errorf("insert placement: %w", notNull), false},
		{"plain error with unique-constraint text", lookalike, false},
		{"wrapped lookalike text", fmt.Errorf("insert placement: %w", lookalike), false},
		{"unrelated error", errors.New("disk I/O error"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isDuplicateIdentity(tc.err); got != tc.want {
			t.Errorf("%s: isDuplicateIdentity = %v, want %v (err=%v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// A genuine primary-key collision goes through the read-original path: the
// stored record comes back unchanged and no second row appears, whether the
// retry carries the same or different content.
func TestSubmitRealDuplicateKeepsOriginalRecord(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "dup.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	node := "n1"
	original := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
		Node:      &node,
	}
	if _, created, err := st.Submit(ctx, original); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}

	// Different content on the same identity: the original record is
	// returned, nothing is inserted or rewritten.
	different := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 9,
		Resources: Resources{CPU: 50, Memory: 32},
		Selector:  map[string]string{},
		Nodes:     []Node{},
		Status:    "rejected",
	}
	stored, created, err := st.Submit(ctx, different)
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if created {
		t.Fatalf("duplicate submit reported created")
	}
	if stored.Priority != 1 || stored.Status != "placed" || stored.Node == nil || *stored.Node != "n1" {
		t.Fatalf("duplicate submit did not return the original record: %+v", stored)
	}

	// Same content on the same identity: same rule.
	again := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
		Node:      &node,
	}
	if _, created, err := st.Submit(ctx, again); err != nil || created {
		t.Fatalf("identical retry: created=%v err=%v", created, err)
	}

	// Exactly one committed row, and a fresh read returns the original.
	recs, err := st.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	got, err := st.Get(ctx, "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Priority != 1 || got.Status != "placed" || got.Node == nil || *got.Node != "n1" || got.Reason != nil {
		t.Fatalf("stored record changed by retries: %+v", got)
	}
	if same, err := SameInput(got, original); err != nil || !same {
		t.Fatalf("stored content changed by retries: same=%v err=%v", same, err)
	}
}

// A storage fault that is not a duplicate — here a closed database — must
// surface as an error from Submit, never as a created record or a retry.
func TestSubmitStorageFailureIsError(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "down.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	p := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []Node{},
		Status:    "placed",
	}
	stored, created, err := st.Submit(context.Background(), p)
	if err == nil {
		t.Fatalf("submit against closed store succeeded: stored=%+v created=%v", stored, created)
	}
	if created || stored != nil {
		t.Fatalf("failed submit reported stored=%+v created=%v", stored, created)
	}
	if isDuplicateIdentity(err) {
		t.Fatalf("storage fault misclassified as duplicate: %v", err)
	}
}

// A database file written before this refactor keeps working after reopen:
// the record is queryable and a retry hits the duplicate path instead of
// inserting a second row.
func TestReopenedDatabaseReadsAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	node := "n1"
	p := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
		Node:      &node,
	}
	if _, created, err := st.Submit(ctx, p); err != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Get(ctx, "ns", "job")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Status != "placed" || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("record after reopen: %+v", got)
	}

	retry := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
	}
	stored, created, err := reopened.Submit(ctx, retry)
	if err != nil || created {
		t.Fatalf("retry after reopen: created=%v err=%v", created, err)
	}
	if stored.Node == nil || *stored.Node != "n1" {
		t.Fatalf("retry after reopen returned %+v, want the original record", stored)
	}
	recs, err := reopened.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 after reopen and retry", len(recs))
	}
}
