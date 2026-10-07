package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// Transaction-visibility suite. These cases observe one SQLite file through
// two independent handles: a writer Store that holds a transaction open and a
// reader Store opened separately on the same file. They pin when a submitted
// record becomes committed, queryable content — only after tx.Commit — and
// what a rollback leaves behind. No in-memory double is involved: every
// conclusion here is a SQLite fact.

// insertSQL mirrors the INSERT Submit performs, so a test-controlled
// transaction writes exactly the row shape the schema expects.
const insertSQL = "INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)"

// visibilityRecord builds the record the test transaction will carry, together
// with its canonical payload as Submit would bind it.
func visibilityRecord(t *testing.T, namespace, name string) (*Placement, string) {
	t.Helper()
	p := &Placement{
		Namespace: namespace, Name: name, Queue: "q", Priority: 7,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
	}
	payload, err := model.CanonicalInput(p)
	if err != nil {
		t.Fatalf("canonical input: %v", err)
	}
	return p, string(payload)
}

// TestUncommittedInsertInvisibleToIndependentReader walks the three
// observation moments of one write transaction on a real file: before commit
// the new identity is absent from an independent reader, after commit it is
// fully readable, and a rolled-back insert never appears while previously
// committed rows stay untouched.
func TestUncommittedInsertInvisibleToIndependentReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "visibility.db")
	ctx := context.Background()

	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()

	// A committed seed row written through the normal Submit path.
	seed, _ := visibilityRecord(t, "ns", "seed")
	node := "n1"
	seed.Node = &node
	if _, created, err := writer.Submit(ctx, seed); err != nil || !created {
		t.Fatalf("seed submit: created=%v err=%v", created, err)
	}

	// The independent reader: a separate Store on the same file, with its own
	// connection — not a second view through the writer's handle.
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	assertAbsent := func(phase, namespace, name string, wantRows int) {
		t.Helper()
		if _, err := reader.Get(ctx, namespace, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: Get(%s/%s) err = %v, want ErrNotFound", phase, namespace, name, err)
		}
		recs, err := reader.List(ctx, ListFilter{})
		if err != nil {
			t.Fatalf("%s: List: %v", phase, err)
		}
		if len(recs) != wantRows {
			t.Fatalf("%s: List rows = %d, want %d", phase, len(recs), wantRows)
		}
		for _, r := range recs {
			if r.Namespace == namespace && r.Name == name {
				t.Fatalf("%s: uncommitted/rolled-back record %s/%s visible in list", phase, namespace, name)
			}
		}
	}

	// Moment 1 — the writer holds an open transaction with the INSERT done but
	// not committed. The independent reader must not see it.
	pending, pendingPayload := visibilityRecord(t, "ns", "pending")
	tx, err := writer.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(insertSQL, pending.Namespace, pending.Name, pendingPayload, "placed", "n1", nil); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}
	assertAbsent("before commit", "ns", "pending", 1)

	// Moment 2 — after Commit the same reader sees the full record: the
	// original request content plus the scheduling columns as written.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := reader.Get(ctx, "ns", "pending")
	if err != nil {
		t.Fatalf("after commit: Get: %v", err)
	}
	if got.Queue != "q" || got.Priority != 7 || got.Status != "placed" ||
		got.Node == nil || *got.Node != "n1" || got.Reason != nil ||
		got.Selector["zone"] != "cn" || len(got.Nodes) != 1 || got.Nodes[0].Name != "n1" {
		t.Fatalf("after commit: record incomplete or altered: %+v", got)
	}
	if same, err := SameInput(got, pending); err != nil || !same {
		t.Fatalf("after commit: stored content differs from submitted input: same=%v err=%v", same, err)
	}
	recs, err := reader.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("after commit: List: %v", err)
	}
	if len(recs) != 2 || recs[0].Name != "pending" || recs[1].Name != "seed" {
		names := []string{}
		for _, r := range recs {
			names = append(names, r.Name)
		}
		t.Fatalf("after commit: list = %v, want [pending seed] (name order)", names)
	}

	// Moment 3 — a second transaction whose insert is rolled back leaves no
	// trace: the identity stays unknown and every committed row is unchanged.
	_, rolledPayload := visibilityRecord(t, "ns", "rolled-back")
	tx2, err := writer.db.Begin()
	if err != nil {
		t.Fatalf("begin rollback tx: %v", err)
	}
	if _, err := tx2.Exec(insertSQL, "ns", "rolled-back", rolledPayload, "placed", "n1", nil); err != nil {
		t.Fatalf("insert in rollback tx: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertAbsent("after rollback", "ns", "rolled-back", 2)

	seedAgain, err := reader.Get(ctx, "ns", "seed")
	if err != nil {
		t.Fatalf("seed after rollback: %v", err)
	}
	if same, err := SameInput(seedAgain, seed); err != nil || !same ||
		seedAgain.Status != "placed" || seedAgain.Node == nil || *seedAgain.Node != "n1" {
		t.Fatalf("committed seed changed across the rolled-back write: %+v", seedAgain)
	}
}

// TestSameStoreReadWaitsForOpenWriteTransaction pins the connection discipline
// inside one Store: with MaxOpenConns(1), a read issued while a write
// transaction holds the only connection does not run concurrently — it waits
// for the connection (here proven by a context deadline) and therefore never
// observes the transaction's uncommitted row. WAL does not change this: WAL
// concurrency applies between separate connections, not within this pool.
func TestSameStoreReadWaitsForOpenWriteTransaction(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "serialized.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	seed, _ := visibilityRecord(t, "ns", "seed")
	node := "n1"
	seed.Node = &node
	if _, created, err := st.Submit(ctx, seed); err != nil || !created {
		t.Fatalf("seed submit: created=%v err=%v", created, err)
	}

	// Occupy the single connection with an open write transaction carrying an
	// uncommitted insert.
	pending, pendingPayload := visibilityRecord(t, "ns", "pending")
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(insertSQL, pending.Namespace, pending.Name, pendingPayload, "placed", "n1", nil); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}

	// A read through the same Store cannot even start: the only connection is
	// held by tx, so the read blocks until its context expires. It does not
	// return the uncommitted row, nor the committed seed — it waits.
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, getErr := st.Get(waitCtx, "ns", "seed")
	cancel()
	if getErr == nil {
		t.Fatalf("Get returned while the only connection was held by an open transaction")
	}
	if !errors.Is(getErr, context.DeadlineExceeded) {
		t.Fatalf("Get err = %v, want context.DeadlineExceeded (connection wait)", getErr)
	}

	// Once the transaction ends, the same read proceeds against committed
	// state; the rolled-back insert is nowhere to be found.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err := st.Get(ctx, "ns", "seed")
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if got.Name != "seed" {
		t.Fatalf("Get after rollback = %+v, want the committed seed", got)
	}
	if _, err := st.Get(ctx, "ns", "pending"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back identity visible after connection freed: err = %v", err)
	}
}
