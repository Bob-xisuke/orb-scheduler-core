package fakestore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

// This test is the guarantee that the business entry points — strict parsing,
// trial placement, idempotency/conflict and query — run their full path over
// the in-memory double without ever loading a SQL driver. The test binary for
// this package imports neither internal/store nor modernc.org/sqlite, so the
// "sqlite" driver must not be registered here; `go list -deps` on this test
// package is the build-time counterpart of the runtime assertion.
func TestBusinessPathRunsWithoutSQLiteDriver(t *testing.T) {
	if slices.Contains(sql.Drivers(), "sqlite") {
		t.Fatalf("sqlite driver registered in the driver-free business test: %v", sql.Drivers())
	}

	fake := &fakestore.Store{}
	svc := service.New(fake)
	ctx := context.Background()

	// First registration: 201-equivalent Created, full record with the
	// trial decision. Omitted selector/labels are defaulted by the parser.
	firstBody := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`
	p, err := placement.ParsePlacementInput([]byte(firstBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rec1, outcome, err := svc.Accept(ctx, p)
	if err != nil || outcome != service.Created {
		t.Fatalf("first accept: outcome=%d err=%v", outcome, err)
	}
	if rec1.Status != service.StatusPlaced || rec1.Node == nil || *rec1.Node != "n" || rec1.Reason != nil {
		t.Fatalf("trial decision wrong: %+v", rec1)
	}
	if rec1.Selector == nil || rec1.Nodes[0].Labels == nil {
		t.Fatalf("defaults not filled on the stored record: %+v", rec1)
	}

	// Same content with members reordered, escaped member names and explicit
	// defaults: 200-equivalent Identical, byte-identical original record.
	variant := `{
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}],
	  "resources": {"cpu": 100, "memory": 64},
	  "selector": {}, "priority": 1, "queue": "q", "name": "job",
	  "namespace": "ns"
	}`
	p2, err := placement.ParsePlacementInput([]byte(variant))
	if err != nil {
		t.Fatalf("parse variant: %v", err)
	}
	rec2, outcome, err := svc.Accept(ctx, p2)
	if err != nil || outcome != service.Identical {
		t.Fatalf("retry: outcome=%d err=%v", outcome, err)
	}
	if !equalJSON(rec1, rec2) {
		t.Fatalf("identical retry returned a different record")
	}

	// Node order alone changes: 409-equivalent Conflict, original untouched.
	reordered := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64}, "selector": {},
	  "nodes": [
	    {"name": "n2", "cpu": 999, "memory": 999, "labels": {}},
	    {"name": "n", "cpu": 200, "memory": 128, "labels": {}}
	  ]
	}`
	p3, err := placement.ParsePlacementInput([]byte(reordered))
	if err != nil {
		t.Fatalf("parse reordered: %v", err)
	}
	rec3, outcome, err := svc.Accept(ctx, p3)
	if err != nil || outcome != service.Conflict {
		t.Fatalf("reorder: outcome=%d err=%v", outcome, err)
	}
	if rec3.Nodes[0].Name != "n" {
		t.Fatalf("conflict must return the original record: %+v", rec3)
	}

	// Invalid input is the parser sentinel and never reaches storage.
	if _, perr := placement.ParsePlacementInput([]byte(`{"namespace":null}`)); !errors.Is(perr, placement.ErrInvalidPlacementInput) {
		t.Fatalf("parse error = %v, want ErrInvalidPlacementInput", perr)
	}
	if len(fake.Submits) != 3 {
		t.Fatalf("invalid parse must not submit; submits=%d want 3", len(fake.Submits))
	}

	// Query path: single record, intersection list, empty-list semantics,
	// 404 not-found and 400 invalid-parameter business errors.
	got, qerr := svc.Query(ctx, url.Values{"namespace": {"ns"}, "name": {"job"}})
	if qerr != nil || got.Record == nil || got.Record.Priority != 1 {
		t.Fatalf("single query: %+v err=%v", got, qerr)
	}
	list, qerr := svc.Query(ctx, nil)
	if qerr != nil || len(list.Items) != 1 || list.Items[0].Name != "job" {
		t.Fatalf("list query: %+v err=%v", list, qerr)
	}
	none, qerr := svc.Query(ctx, url.Values{"queue": {"nope"}})
	if qerr != nil || none.Items == nil || len(none.Items) != 0 {
		t.Fatalf("empty match must be a non-nil empty list: %+v err=%v", none, qerr)
	}
	if _, qerr = svc.Query(ctx, url.Values{"namespace": {"ns"}, "name": {"missing"}}); !errors.Is(qerr, service.ErrPlacementNotFound) {
		t.Fatalf("missing = %v, want ErrPlacementNotFound", qerr)
	}
	if _, qerr = svc.Query(ctx, url.Values{"bogus": {"x"}}); !errors.Is(qerr, service.ErrInvalidPlacementQuery) {
		t.Fatalf("invalid query = %v, want ErrInvalidPlacementQuery", qerr)
	}

	// The record the business stored is the same one the contract returns.
	if !equalJSON(got.Record, rec1) {
		t.Fatalf("queried record differs from the accepted record")
	}

	// Still no SQL driver after the whole flow.
	if slices.Contains(sql.Drivers(), "sqlite") {
		t.Fatalf("sqlite driver registered during the business flow: %v", sql.Drivers())
	}
}

// TestFakeStoreSatisfiesContract is a compile-time check that the in-memory
// double is an interchangeable Store with the SQLite implementation's
// contract, both expressed through model.Store.
func TestFakeStoreSatisfiesContract(t *testing.T) {
	var _ model.Store = (*fakestore.Store)(nil)
}

func equalJSON(a, b *model.Placement) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
