package service

import (
	"context"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Boundary tests for the record-immutability contract, run against the real
// SQLite-backed store. They trace one legal request through parse-time
// defaults, Accept's in-place normalization and trial scheduling, storage,
// and re-query, then mutate one object at a time to pin down which memory
// objects share state and which are detached. The fakestore counterparts in
// accept_boundary_fake_test.go run the same scenarios against the in-memory
// double; where the two disagree the test names say so explicitly.
//
// See docs/record-boundary.md for the source-level walkthrough these cases
// back.

// boundaryInput builds a valid placed input with explicit maps so tests can
// observe sharing on every member kind.
func boundaryInput() *store.Placement {
	return &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []store.Node{
			{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
		},
	}
}

func boundaryQuerySingle(t *testing.T, svc *Service) *store.Placement {
	t.Helper()
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=ns&name=job"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record == nil {
		t.Fatalf("query returned no record")
	}
	return result.Record
}

// Accept fills defaults and writes the trial decision into the caller's own
// input object before anything is stored (service.go:63-64). This happens on
// every call, for placed and rejected inputs alike.
func TestAcceptBoundaryCallerObjectIsNormalizedAndScheduledInPlace(t *testing.T) {
	_, svc := newService(t)

	p := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		// Selector omitted; node Labels omitted.
		Nodes: []store.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	if _, outcome, err := svc.Accept(context.Background(), p); err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if p.Selector == nil || len(p.Selector) != 0 {
		t.Fatalf("caller selector not defaulted in place: %+v", p.Selector)
	}
	if p.Nodes[0].Labels == nil || len(p.Nodes[0].Labels) != 0 {
		t.Fatalf("caller labels not defaulted in place: %+v", p.Nodes[0].Labels)
	}
	if p.Status != StatusPlaced || p.Node == nil || *p.Node != "n1" || p.Reason != nil {
		t.Fatalf("trial decision not written back to the caller object: %+v", p)
	}

	// A rejected input is rewritten the same way: Reason is set on the
	// caller's object, Node stays nil.
	r := &store.Placement{
		Namespace: "ns", Name: "job-2", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		Nodes:     []store.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	if _, outcome, err := svc.Accept(context.Background(), r); err != nil || outcome != Created {
		t.Fatalf("accept rejected: outcome=%d err=%v", outcome, err)
	}
	if r.Status != StatusRejected || r.Node != nil || r.Reason == nil || *r.Reason != ReasonNoNode {
		t.Fatalf("rejection not written back to the caller object: %+v", r)
	}
}

// The SQLite store's created return is a shallow struct copy of the
// submission (store/placement.go:36-37): scalar fields are detached, but the
// selector map, the nodes array, the label maps and the scheduling-result
// pointers are shared with the caller's input object. None of these mutations
// reaches the committed record.
func TestAcceptBoundaryCreatedReturnSharesMutableStateWithInput(t *testing.T) {
	_, svc := newService(t)
	p := boundaryInput()
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}

	// Plain scalar field: copied by value, not shared.
	p.Queue = "mutated"
	if rec.Queue != "q" {
		t.Fatalf("scalar field shared with input: rec.Queue = %q", rec.Queue)
	}

	// Selector map: shared.
	p.Selector["injected"] = "yes"
	if rec.Selector["injected"] != "yes" {
		t.Fatalf("selector map not shared with the created return: %+v", rec.Selector)
	}

	// Nodes array element: shared backing array.
	p.Nodes[0].CPU = 1
	if rec.Nodes[0].CPU != 1 {
		t.Fatalf("nodes array not shared with the created return: %+v", rec.Nodes)
	}

	// Node labels map: shared.
	p.Nodes[0].Labels["injected"] = "yes"
	if rec.Nodes[0].Labels["injected"] != "yes" {
		t.Fatalf("labels map not shared with the created return: %+v", rec.Nodes[0].Labels)
	}

	// Scheduling-result pointer: same *string.
	*p.Node = "hijacked"
	if rec.Node == nil || *rec.Node != "hijacked" {
		t.Fatalf("node pointer not shared with the created return: %+v", rec.Node)
	}

	// The committed record is untouched by every mutation above: re-querying
	// decodes the stored bytes fresh.
	got := boundaryQuerySingle(t, svc)
	if got.Queue != "q" || got.Resources.CPU != 100 {
		t.Fatalf("committed record changed by input mutation: %+v", got)
	}
	if _, ok := got.Selector["injected"]; ok {
		t.Fatalf("committed selector gained a key: %+v", got.Selector)
	}
	if got.Nodes[0].CPU != 200 {
		t.Fatalf("committed nodes changed: %+v", got.Nodes)
	}
	if _, ok := got.Nodes[0].Labels["injected"]; ok {
		t.Fatalf("committed labels gained a key: %+v", got.Nodes[0].Labels)
	}
	if got.Node == nil || *got.Node != "n1" {
		t.Fatalf("committed decision changed: %+v", got.Node)
	}
}

// On an identical retry the SQLite store returns a record freshly decoded
// from the committed bytes (store/placement.go:43-56, 125-140): it shares
// nothing with the retry's input object, and mutating it does not reach the
// store.
func TestAcceptBoundaryIdenticalRetryReturnsDetachedOriginal(t *testing.T) {
	_, svc := newService(t)
	rec1, outcome, err := svc.Accept(context.Background(), boundaryInput())
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	retry := boundaryInput()
	rec2, outcome, err := svc.Accept(context.Background(), retry)
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	if rec1 == rec2 {
		t.Fatalf("created and identical returns must be distinct objects")
	}

	// Mutating the retry's input object does not leak into the returned
	// record, member by member.
	retry.Queue = "mutated"
	retry.Selector["injected"] = "yes"
	retry.Nodes[0].CPU = 1
	retry.Nodes[0].Labels["injected"] = "yes"
	*retry.Node = "hijacked"
	if rec2.Queue != "q" || rec2.Nodes[0].CPU != 200 {
		t.Fatalf("retry input leaked into the returned record: %+v", rec2)
	}
	if _, ok := rec2.Selector["injected"]; ok {
		t.Fatalf("retry selector leaked: %+v", rec2.Selector)
	}
	if _, ok := rec2.Nodes[0].Labels["injected"]; ok {
		t.Fatalf("retry labels leaked: %+v", rec2.Nodes[0].Labels)
	}
	if rec2.Node == nil || *rec2.Node != "n1" {
		t.Fatalf("retry node pointer leaked: %+v", rec2.Node)
	}

	// Mutating the returned record does not reach the committed content.
	rec2.Queue = "mutated"
	*rec2.Node = "hijacked"
	got := boundaryQuerySingle(t, svc)
	if got.Queue != "q" || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("committed record changed through the returned object: %+v", got)
	}
}

// A conflicting submission returns the untouched original record, decoded
// fresh; the conflicting input object itself is still normalized and
// trial-scheduled in place (service.go:63-64 run before the conflict is
// detected), but nothing it carries reaches the store.
func TestAcceptBoundaryConflictLeavesStoreAndReturnsUntouched(t *testing.T) {
	_, svc := newService(t)
	if _, outcome, err := svc.Accept(context.Background(), boundaryInput()); err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	conflicting := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 99,
		Resources: store.Resources{CPU: 100, Memory: 64},
		// Selector omitted: the conflicting input is still normalized in place.
		Nodes: []store.Node{{Name: "n9", CPU: 200, Memory: 128}},
	}
	rec, outcome, err := svc.Accept(context.Background(), conflicting)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}

	// The caller's conflicting object was defaulted and trial-scheduled even
	// though its content was rejected.
	if conflicting.Selector == nil || conflicting.Status != StatusPlaced ||
		conflicting.Node == nil || *conflicting.Node != "n9" {
		t.Fatalf("conflicting input not normalized/scheduled in place: %+v", conflicting)
	}

	// The returned record is the original, detached from the conflicting
	// input.
	if rec.Priority != 1 || len(rec.Nodes) != 1 || rec.Nodes[0].Name != "n1" {
		t.Fatalf("conflict did not return the original record: %+v", rec)
	}
	conflicting.Nodes[0].Labels = map[string]string{"injected": "yes"}
	conflicting.Selector["injected"] = "yes"
	if _, ok := rec.Selector["injected"]; ok {
		t.Fatalf("conflicting input leaked into the returned record: %+v", rec.Selector)
	}

	// The committed record is the original.
	got := boundaryQuerySingle(t, svc)
	if got.Priority != 1 || got.Nodes[0].Name != "n1" || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("committed record rewritten by conflict: %+v", got)
	}
}

// A rejected record follows the same boundary rules as a placed one: the
// created return shares the reason pointer with the caller's object, the
// committed record is immune, same-content retries are Identical, and a
// placeable retry on the same identity conflicts without changing anything.
func TestAcceptBoundaryRejectedRecordFollowsSameRules(t *testing.T) {
	_, svc := newService(t)
	p := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("decision = %+v, want rejected", rec)
	}

	// The created return shares the reason pointer with the caller's object
	// (shallow copy, store/placement.go:36-37); the committed record is
	// unaffected.
	*p.Reason = "tampered"
	if rec.Reason == nil || *rec.Reason != "tampered" {
		t.Fatalf("reason pointer not shared with the created return: %+v", rec.Reason)
	}
	got := boundaryQuerySingle(t, svc)
	if got.Reason == nil || *got.Reason != ReasonNoNode {
		t.Fatalf("committed reason changed: %+v", got.Reason)
	}

	// Same content again: Identical with the original rejection.
	again, outcome, err := svc.Accept(context.Background(), &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	})
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	if again.Status != StatusRejected || again.Reason == nil || *again.Reason != ReasonNoNode {
		t.Fatalf("retry returned %+v, want the original rejection", again)
	}

	// A placeable submission on the same identity conflicts; the stored
	// rejection is untouched.
	placeable := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	rec, outcome, err = svc.Accept(context.Background(), placeable)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}
	if rec.Status != StatusRejected {
		t.Fatalf("conflict must return the untouched rejection, got %+v", rec)
	}
	got = boundaryQuerySingle(t, svc)
	if got.Status != StatusRejected || got.Node != nil || got.Resources.CPU != 2000 {
		t.Fatalf("committed rejection rewritten: %+v", got)
	}
}

// Query results are freshly decoded on every call (store/placement.go:
// 61-123): mutating a returned single record or list item changes nothing
// for later queries.
func TestAcceptBoundaryMutatingQueryResultDoesNotTouchStore(t *testing.T) {
	_, svc := newService(t)
	if _, outcome, err := svc.Accept(context.Background(), boundaryInput()); err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	single := boundaryQuerySingle(t, svc)
	single.Queue = "mutated"
	single.Selector["injected"] = "yes"
	single.Nodes[0].CPU = 1
	*single.Node = "hijacked"

	list, err := svc.Query(context.Background(), mustValues(t, "namespace=ns"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("list items = %d, want 1", len(list.Items))
	}
	list.Items[0].Queue = "mutated"
	*list.Items[0].Node = "hijacked"

	got := boundaryQuerySingle(t, svc)
	if got.Queue != "q" || got.Nodes[0].CPU != 200 || got.Node == nil || *got.Node != "n1" {
		t.Fatalf("committed record changed through query results: %+v", got)
	}
	if _, ok := got.Selector["injected"]; ok {
		t.Fatalf("committed selector changed through query results: %+v", got.Selector)
	}
}

// Content comparison itself has no side effects: SameInput works on copies
// (model/input.go:83-89), unlike NormalizeInput which fills defaults into the
// caller's object.
func TestAcceptBoundarySameInputHasNoSideEffects(t *testing.T) {
	a := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	b := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	same, err := model.SameInput(a, b)
	if err != nil || !same {
		t.Fatalf("same=%v err=%v, want equal content", same, err)
	}
	if a.Selector != nil || a.Nodes[0].Labels != nil {
		t.Fatalf("SameInput filled defaults into its argument: %+v", a)
	}
}
