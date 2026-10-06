package service

import (
	"context"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// The same boundary scenarios as accept_boundary_test.go, run against the
// in-memory fakestore double. The double deep-copies every record through
// JSON on the way in and out (fakestore.go:87-104, 159-169), so its Created
// return shares NOTHING with the caller's input object — a real difference
// from the SQLite store's shallow copy (store/placement.go:36-37), pinned
// down by TestAcceptBoundaryFakeCreatedReturnIsDetached below. The
// caller-side normalization, the retry/conflict rules and the re-query
// guarantees are identical on both storage paths; the fake results must
// never be read as proof of SQLite behavior where the two differ.

func TestAcceptBoundaryFakeCallerObjectIsNormalizedAndScheduledInPlace(t *testing.T) {
	_, svc := newFakeService()
	p := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		// Selector omitted; node Labels omitted.
		Nodes: []model.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	if _, outcome, err := svc.Accept(context.Background(), p); err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	// Normalization and the trial write-back happen in package service,
	// before storage is involved, so they look exactly like the SQLite run.
	if p.Selector == nil || len(p.Selector) != 0 {
		t.Fatalf("caller selector not defaulted in place: %+v", p.Selector)
	}
	if p.Nodes[0].Labels == nil || len(p.Nodes[0].Labels) != 0 {
		t.Fatalf("caller labels not defaulted in place: %+v", p.Nodes[0].Labels)
	}
	if p.Status != StatusPlaced || p.Node == nil || *p.Node != "n1" || p.Reason != nil {
		t.Fatalf("trial decision not written back to the caller object: %+v", p)
	}
}

// Unlike the SQLite store, the double returns a deep copy on creation:
// mutating the input object afterwards changes none of the returned record's
// members. This is the one place where fake and SQLite disagree; the
// committed-record guarantees below it are the same on both.
func TestAcceptBoundaryFakeCreatedReturnIsDetached(t *testing.T) {
	_, svc := newFakeService()
	p := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []model.Node{
			{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
		},
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}

	p.Queue = "mutated"
	p.Selector["injected"] = "yes"
	p.Nodes[0].CPU = 1
	p.Nodes[0].Labels["injected"] = "yes"
	*p.Node = "hijacked"

	if rec.Queue != "q" || rec.Nodes[0].CPU != 200 {
		t.Fatalf("input mutation leaked into the fake created return: %+v", rec)
	}
	if _, ok := rec.Selector["injected"]; ok {
		t.Fatalf("selector leaked into the fake created return: %+v", rec.Selector)
	}
	if _, ok := rec.Nodes[0].Labels["injected"]; ok {
		t.Fatalf("labels leaked into the fake created return: %+v", rec.Nodes[0].Labels)
	}
	if rec.Node == nil || *rec.Node != "n1" {
		t.Fatalf("node pointer leaked into the fake created return: %+v", rec.Node)
	}

	// Same as SQLite: re-querying is unaffected by any in-memory mutation.
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=ns&name=job"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := result.Record
	if got.Queue != "q" || got.Nodes[0].CPU != 200 || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("stored record changed by input mutation: %+v", got)
	}
	if _, ok := got.Selector["injected"]; ok {
		t.Fatalf("stored selector gained a key: %+v", got.Selector)
	}
}

// Retry, conflict and query results are detached on the fake path too, and
// the outcomes match the SQLite run exactly.
func TestAcceptBoundaryFakeRetryConflictAndQueryAreDetached(t *testing.T) {
	_, svc := newFakeService()
	mk := func() *model.Placement {
		return &model.Placement{
			Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
			Resources: model.Resources{CPU: 100, Memory: 64},
			Selector:  map[string]string{"zone": "cn"},
			Nodes: []model.Node{
				{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
			},
		}
	}
	if _, outcome, err := svc.Accept(context.Background(), mk()); err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	retry := mk()
	rec2, outcome, err := svc.Accept(context.Background(), retry)
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	retry.Selector["injected"] = "yes"
	*retry.Node = "hijacked"
	if _, ok := rec2.Selector["injected"]; ok {
		t.Fatalf("retry input leaked into the returned record: %+v", rec2.Selector)
	}
	if rec2.Node == nil || *rec2.Node != "n1" {
		t.Fatalf("retry node pointer leaked: %+v", rec2.Node)
	}

	conflicting := mk()
	conflicting.Priority = 99
	rec3, outcome, err := svc.Accept(context.Background(), conflicting)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}
	if rec3.Priority != 1 {
		t.Fatalf("conflict did not return the original record: %+v", rec3)
	}

	// Mutating a query result changes nothing for later queries.
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=ns&name=job"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	result.Record.Queue = "mutated"
	*result.Record.Node = "hijacked"
	again, err := svc.Query(context.Background(), mustValues(t, "namespace=ns&name=job"))
	if err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if again.Record.Queue != "q" || again.Record.Node == nil || *again.Record.Node != "n1" {
		t.Fatalf("stored record changed through a query result: %+v", again.Record)
	}
}
