package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// This suite pins the transaction-visibility boundary with a real SQLite
// file. It never uses the in-memory double: the questions here — what an
// independent connection sees while the writer's transaction is open, and
// what a reader sharing the writer's single-connection pool does — are
// SQLite behavior, not storage-contract behavior.
//
// Layout of every case:
//
//	writer := Open(path)          // the production *Store: pool capped at 1
//	reader := Open(path)          // a second pool over the SAME file
//	tx, _ := writer.db.BeginTx()  // a write Submit is still inside here
//	... pre-commit observations through reader ...
//	tx.Commit() / tx.Rollback()
//	... post-decision observations through reader ...
//
// The raw INSERT binds exactly the columns Submit binds
// (internal/store/placement.go), including the canonical input_json, so the
// committed row is indistinguishable from one Submit wrote.

// visibilityRecord is a fully decided placement (placed with its node), so
// post-commit reads can be checked for the original input AND the
// scheduling conclusion, not just row existence.
func visibilityRecord() *Placement {
	p := sampleRecord()
	n := "n1"
	p.Node = &n
	p.Reason = nil
	return p
}

func canonicalPayload(t *testing.T, p *Placement) string {
	t.Helper()
	data, err := model.CanonicalInput(p)
	if err != nil {
		t.Fatalf("canonical input: %v", err)
	}
	return string(data)
}

// beginPendingInsert opens a write transaction on st and inserts p's row
// inside it WITHOUT committing, matching Submit's INSERT binding. The
// returned tx is left for the caller to Commit or Rollback.
func beginPendingInsert(t *testing.T, st *Store, p *Placement) interface {
	Commit() error
	Rollback() error
} {
	t.Helper()
	tx, err := st.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.Exec(
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		p.Namespace, p.Name, canonicalPayload(t, p), p.Status, p.Node, p.Reason,
	); err != nil {
		t.Fatalf("pending insert: %v", err)
	}
	return tx
}

func expectIdentityMissing(t *testing.T, st *Store, namespace, name string) {
	t.Helper()
	if _, err := st.Get(context.Background(), namespace, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get %s/%s = %v, want ErrNotFound while the write is uncommitted", namespace, name, err)
	}
}

func expectListExcludes(t *testing.T, st *Store, namespace, name string) {
	t.Helper()
	recs, err := st.List(context.Background(), ListFilter{Namespace: namespace})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, rec := range recs {
		if rec.Namespace == namespace && rec.Name == name {
			t.Fatalf("uncommitted %s/%s is present in an independent List: %+v", namespace, name, recs)
		}
	}
}

// Before commit an independent reader cannot see the new identity in either
// query form: Get answers ErrNotFound (the HTTP 404/PlacementNotFoundError
// case) and List omits it (the 200 {"items":[]} case). After commit both
// forms read the complete original input and the scheduling conclusion.
func TestIndependentReaderHidesUncommittedThenSeesCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "visibility.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	// The reader must exist before the write transaction opens: its schema
	// exec needs a write lock and would otherwise queue behind the open tx.
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	p := visibilityRecord()
	tx := beginPendingInsert(t, writer, p)

	// Observation 1 — before commit: the identity is simply unknown.
	expectIdentityMissing(t, reader, p.Namespace, p.Name)
	expectListExcludes(t, reader, p.Namespace, p.Name)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Observation 2 — after commit: the single-record form returns it.
	got, err := reader.Get(context.Background(), p.Namespace, p.Name)
	if err != nil {
		t.Fatalf("get after commit: %v", err)
	}
	if got.Status != "placed" || got.Node == nil || *got.Node != "n1" || got.Reason != nil {
		t.Fatalf("scheduling conclusion not visible as written: %+v", got)
	}
	if got.Queue != p.Queue || got.Priority != p.Priority || got.Resources != p.Resources {
		t.Fatalf("scalar input fields not visible as written: %+v", got)
	}
	same, err := model.SameInput(got, p)
	if err != nil || !same {
		t.Fatalf("visible record differs from the original input: same=%v err=%v", same, err)
	}

	// Observation 3 — after commit: the list form returns it too, complete.
	recs, err := reader.List(context.Background(), ListFilter{Namespace: p.Namespace})
	if err != nil {
		t.Fatalf("list after commit: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("list after commit = %d records, want 1: %+v", len(recs), recs)
	}
	if same, err := model.SameInput(recs[0], p); err != nil || !same {
		t.Fatalf("listed record differs from the original input: same=%v err=%v", same, err)
	}
}

// A rolled-back write leaves the file as it was: the independent reader
// keeps answering not-found / empty-list, and a record committed before the
// transaction is untouched by it.
func TestIndependentReaderHidesRolledBackAndKeepsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	// An already-committed record is the "existing record" that must not
	// change or disappear because another write later rolls back.
	existing := visibilityRecord()
	existing.Namespace, existing.Name = "keep", "keep"
	if _, created, err := writer.Submit(context.Background(), existing); err != nil || !created {
		t.Fatalf("seed existing: created=%v err=%v", created, err)
	}

	pending := visibilityRecord()
	tx := beginPendingInsert(t, writer, pending)
	expectIdentityMissing(t, reader, pending.Namespace, pending.Name)
	expectListExcludes(t, reader, pending.Namespace, pending.Name)

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After rollback the new identity is still unknown to both query forms.
	expectIdentityMissing(t, reader, pending.Namespace, pending.Name)
	recs, err := reader.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records after rollback = %d, want only the pre-existing one: %+v", len(recs), recs)
	}
	kept, err := reader.Get(context.Background(), existing.Namespace, existing.Name)
	if err != nil {
		t.Fatalf("existing record vanished after rollback: %v", err)
	}
	if same, err := model.SameInput(kept, existing); err != nil || !same {
		t.Fatalf("existing record changed across rollback: same=%v err=%v", same, err)
	}
}

// assertReadBlocks fails if the read result lands while the writer still
// holds its transaction. A parked reader cannot return: with
// SetMaxOpenConns(1) (internal/store/store.go Open) the read is waiting for
// the pool's single connection, which the write transaction owns.
func assertReadBlocks(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("read through the shared Store completed while the write transaction was open: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// Sharing the writer's *Store means sharing its one-connection pool: a Get
// issued while Submit's transaction is open waits for that transaction to
// end — it never observes a mid-transaction state, and once the transaction
// commits it reads the committed record. This is the case the independent
// reader (a second Open over the same file) does NOT block on.
func TestSharedStoreGetWaitsForWriterConnectionThenReadsCommitted(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// One committed record, read during the open write transaction.
	seed := visibilityRecord()
	seed.Namespace, seed.Name = "ns", "job"
	if _, created, err := st.Submit(context.Background(), seed); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}

	// The pending write concerns a DIFFERENT identity, so the row locks
	// themselves never interact with the read; only the shared connection
	// serializes them.
	pending := visibilityRecord()
	pending.Namespace, pending.Name = "ns2", "job2"
	tx := beginPendingInsert(t, st, pending)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var got *Placement
	go func() {
		got, err = st.Get(ctx, seed.Namespace, seed.Name)
		done <- err
	}()

	// While the transaction is open the shared-pool read cannot finish.
	assertReadBlocks(t, done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Once the transaction ends, the parked read completes and sees the
	// already-committed seed (it did not read a mid-transaction state).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parked get after commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked get did not complete after the writer committed")
	}
	if same, err := model.SameInput(got, seed); err != nil || !same {
		t.Fatalf("parked read returned a changed record: same=%v err=%v", same, err)
	}

	// The just-committed identity is now readable on the same Store.
	if _, err := st.Get(context.Background(), pending.Namespace, pending.Name); err != nil {
		t.Fatalf("committed identity not readable after the parked read: %v", err)
	}
}

// The counterpart for rollback: the parked shared-Store read completes after
// the rollback still seeing the committed state, and the rolled-back
// identity stays unknown.
func TestSharedStoreGetUnblocksOnRollbackWithoutSeeingRow(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "shared-rollback.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	seed := visibilityRecord()
	if _, created, err := st.Submit(context.Background(), seed); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}

	pending := visibilityRecord()
	pending.Namespace, pending.Name = "ns2", "job2"
	tx := beginPendingInsert(t, st, pending)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := st.Get(ctx, seed.Namespace, seed.Name)
		done <- err
	}()
	assertReadBlocks(t, done)

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parked get after rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked get did not complete after the writer rolled back")
	}
	if _, err := st.Get(context.Background(), pending.Namespace, pending.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back identity read as %v, want ErrNotFound", err)
	}
}

// Each List is its own statement against the last committed state: two Lists
// issued while a write transaction stays open both exclude its row, and the
// next List after commit includes it — requests do not share one frozen
// snapshot, and an open write transaction does not advance or freeze the
// reader's view. (WAL readers do not block on the writer; the single
// connection constraint belongs to the writer's own pool only.)
func TestIndependentListStatementsTakeFreshCommittedView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshots.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	p := visibilityRecord()
	tx := beginPendingInsert(t, writer, p)

	listCount := func() int {
		t.Helper()
		recs, err := reader.List(context.Background(), ListFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return len(recs)
	}
	// Two separate statements before commit both see the empty committed set.
	if n := listCount(); n != 0 {
		t.Fatalf("first list while uncommitted = %d records, want 0", n)
	}
	if n := listCount(); n != 0 {
		t.Fatalf("second list while uncommitted = %d records, want 0", n)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// A later statement, with no reader-side transaction in between, takes a
	// fresh view that now contains the committed row.
	if n := listCount(); n != 1 {
		t.Fatalf("list after commit = %d records, want 1", n)
	}
}
