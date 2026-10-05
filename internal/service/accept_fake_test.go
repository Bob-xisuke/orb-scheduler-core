package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

// This suite runs the Accept business flow against the in-memory fakestore
// double: no SQLite file is created, every storage interaction is recorded,
// and storage failures are injected directly. The SQLite-backed tests in
// accept_test.go remain the regression for the real storage path.

func newFakeService() (*fakestore.Store, *Service) {
	fake := &fakestore.Store{}
	return fake, New(fake)
}

func TestAcceptWithFakeFillsDefaultsBeforeStorage(t *testing.T) {
	fake, svc := newFakeService()
	p := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		// Selector and node Labels omitted.
		Nodes: []model.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if len(fake.Submits) != 1 {
		t.Fatalf("submits = %d, want 1", len(fake.Submits))
	}
	sent := fake.Submits[0].Record
	if sent.Selector == nil || len(sent.Selector) != 0 {
		t.Fatalf("selector default not filled before storage: %+v", sent.Selector)
	}
	if sent.Nodes[0].Labels == nil || len(sent.Nodes[0].Labels) != 0 {
		t.Fatalf("labels default not filled before storage: %+v", sent.Nodes[0].Labels)
	}
	// The trial saw the same normalized input: the unlabeled node matches
	// the empty selector.
	if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "n" || rec.Reason != nil {
		t.Fatalf("decision = %+v", rec)
	}
}

func TestAcceptWithFakeSchedulingRules(t *testing.T) {
	fake, svc := newFakeService()
	// Eligible means labels contain every selector pair and both capacities
	// fit. Byte order: "z-node" < "ä-node".
	nodes := []model.Node{
		{Name: "ä-node", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn"}},
		{Name: "z-node", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn"}},
		{Name: "small", CPU: 100, Memory: 128, Labels: map[string]string{"zone": "cn"}},
		{Name: "a-other-zone", CPU: 2000, Memory: 1024, Labels: map[string]string{"zone": "us"}},
	}
	mk := func(name string, priority int32) *model.Placement {
		return &model.Placement{
			Namespace: "ns", Name: name, Queue: "q", Priority: priority,
			Resources: model.Resources{CPU: 1000, Memory: 512},
			Selector:  map[string]string{"zone": "cn"},
			Nodes:     nodes,
		}
	}

	// Different priorities, each asking for the node's full capacity: both
	// land on z-node, proving priority does not steer the choice, the
	// smallest eligible name in UTF-8 byte order wins, and the trial never
	// deducts capacity between submissions.
	for i, name := range []string{"job-1", "job-2"} {
		rec, outcome, err := svc.Accept(context.Background(), mk(name, int32(100-99*i)))
		if err != nil || outcome != Created {
			t.Fatalf("%s: outcome=%d err=%v", name, outcome, err)
		}
		if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "z-node" {
			t.Fatalf("%s: decision = %+v, want placed on z-node", name, rec)
		}
	}
	if len(fake.Submits) != 2 {
		t.Fatalf("submits = %d, want 2", len(fake.Submits))
	}
}

func TestAcceptWithFakeCreatedIdenticalConflict(t *testing.T) {
	fake, svc := newFakeService()

	first := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		// Selector and labels omitted; the retry spells them out as {}.
		Nodes: []model.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	rec1, outcome, err := svc.Accept(context.Background(), first)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	retry := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	rec2, outcome, err := svc.Accept(context.Background(), retry)
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	ab, _ := json.Marshal(rec1)
	bb, _ := json.Marshal(rec2)
	if string(ab) != string(bb) {
		t.Fatalf("identical retry returned a different record:\n%s\n%s", ab, bb)
	}

	conflicting := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 2,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	rec3, outcome, err := svc.Accept(context.Background(), conflicting)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}
	if rec3.Priority != 1 {
		t.Fatalf("conflict must return the original record, got %+v", rec3)
	}

	// The conflict did not overwrite the stored record: reading it back
	// through the service still returns the original content.
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=ns&name=job"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record == nil || result.Record.Priority != 1 {
		t.Fatalf("stored record rewritten by conflict: %+v", result.Record)
	}
	if len(fake.Submits) != 3 {
		t.Fatalf("submits = %d, want 3", len(fake.Submits))
	}
}

func TestAcceptWithFakeRejectionIsStoredAndReused(t *testing.T) {
	_, svc := newFakeService()
	rejected := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 2000, Memory: 256},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n", CPU: 1000, Memory: 512, Labels: map[string]string{}}},
	}
	rec, outcome, err := svc.Accept(context.Background(), rejected)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("decision = %+v, want rejection with %q", rec, ReasonNoNode)
	}

	// Same content again: Identical with the original rejection; a
	// placeable retry on the same identity conflicts and changes nothing.
	again, outcome, err := svc.Accept(context.Background(), rejected)
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	if again.Status != StatusRejected || again.Reason == nil || *again.Reason != ReasonNoNode {
		t.Fatalf("retry returned %+v, want the original rejection", again)
	}

	placeable := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n", CPU: 1000, Memory: 512, Labels: map[string]string{}}},
	}
	rec, outcome, err = svc.Accept(context.Background(), placeable)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}
	if rec.Status != StatusRejected {
		t.Fatalf("conflict must return the untouched original rejection, got %+v", rec)
	}
}

func TestAcceptWithFakeStorageFailure(t *testing.T) {
	fake, svc := newFakeService()
	root := errors.New("connection lost")
	fake.SubmitErr = root

	rec, _, err := svc.Accept(context.Background(), &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Nodes:     []model.Node{},
	})
	if rec != nil {
		t.Fatalf("failure must not return a record: %+v", rec)
	}
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("err = %v, want ErrStorageUnavailable", err)
	}
	if !errors.Is(err, root) {
		t.Fatalf("err = %v, want the injected root error to stay recognizable", err)
	}
	// The failing attempt did reach storage exactly once.
	if len(fake.Submits) != 1 {
		t.Fatalf("submits = %d, want 1", len(fake.Submits))
	}
}
