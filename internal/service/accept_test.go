package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// The SQLite-backed store satisfies the storage contract. The assertion lives
// in this SQLite-linked test file rather than in store.go so the service
// package itself never imports the SQLite driver.
var _ Store = (*store.Store)(nil)

func newService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, New(st)
}

func samplePlacement() *store.Placement {
	return &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "default",
		Priority:  10,
		Resources: store.Resources{CPU: 500, Memory: 256},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []store.Node{
			{Name: "node-b", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn", "ssd": "true"}},
			{Name: "node-a", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn"}},
			{Name: "node-c", CPU: 100, Memory: 128, Labels: map[string]string{"zone": "cn"}},
			{Name: "node-d", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "us"}},
		},
	}
}

func TestAcceptFirstSubmissionIsCreatedWithDecision(t *testing.T) {
	st, svc := newService(t)
	rec, outcome, err := svc.Accept(context.Background(), samplePlacement())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if outcome != Created {
		t.Fatalf("outcome = %d, want Created", outcome)
	}
	if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "node-a" || rec.Reason != nil {
		t.Fatalf("decision not applied: %+v", rec)
	}
	got, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Node == nil || *got.Node != "node-a" || got.Status != StatusPlaced {
		t.Fatalf("stored record mismatch: %+v", got)
	}
}

func TestAcceptSameContentIsIdenticalAndReturnsOriginal(t *testing.T) {
	_, svc := newService(t)
	first, outcome, err := svc.Accept(context.Background(), samplePlacement())
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	second, outcome, err := svc.Accept(context.Background(), samplePlacement())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if outcome != Identical {
		t.Fatalf("outcome = %d, want Identical", outcome)
	}
	ab, _ := json.Marshal(first)
	bb, _ := json.Marshal(second)
	if string(ab) != string(bb) {
		t.Fatalf("identical retry changed the record:\n%s\n%s", ab, bb)
	}
}

func TestAcceptFillsOmittedDefaultsForComparison(t *testing.T) {
	st, svc := newService(t)

	first := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		// Selector omitted and node Labels omitted.
		Nodes: []store.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	rec1, outcome, err := svc.Accept(context.Background(), first)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}

	second := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	rec2, outcome, err := svc.Accept(context.Background(), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if outcome != Identical {
		t.Fatalf("outcome = %d, want Identical for defaulted content", outcome)
	}
	ab, _ := json.Marshal(rec1)
	bb, _ := json.Marshal(rec2)
	if string(ab) != string(bb) {
		t.Fatalf("defaulted retry returned a different record:\n%s\n%s", ab, bb)
	}

	// The returned and stored record renders omitted maps as {}, not null.
	got, err := st.Get(context.Background(), "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	data, _ := json.Marshal(got)
	if !json.Valid(data) || string(data) != string(ab) {
		t.Fatalf("stored JSON differs from accepted record:\n%s\n%s", data, ab)
	}
}

func TestAcceptNodeReorderConflictsEvenWhenDecisionIsSame(t *testing.T) {
	st, svc := newService(t)

	first := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 1, Memory: 1},
		Selector:  map[string]string{},
		Nodes: []store.Node{
			{Name: "n1", CPU: 10, Memory: 10, Labels: map[string]string{}},
			{Name: "n2", CPU: 10, Memory: 10, Labels: map[string]string{}},
		},
	}
	rec1, outcome, err := svc.Accept(context.Background(), first)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	if rec1.Node == nil || *rec1.Node != "n1" {
		t.Fatalf("setup: want n1, got %+v", rec1.Node)
	}

	// Only the node array order changes. Both submissions schedule onto n1,
	// so an equal scheduling result must NOT stand in for equal input.
	second := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 1, Memory: 1},
		Selector:  map[string]string{},
		Nodes: []store.Node{
			{Name: "n2", CPU: 10, Memory: 10, Labels: map[string]string{}},
			{Name: "n1", CPU: 10, Memory: 10, Labels: map[string]string{}},
		},
	}
	rec2, outcome, err := svc.Accept(context.Background(), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if outcome != Conflict {
		t.Fatalf("outcome = %d, want Conflict for reordered nodes", outcome)
	}
	if rec2.Node == nil || *rec2.Node != "n1" {
		t.Fatalf("conflict must return the original record, got %+v", rec2)
	}

	// The stored record is untouched.
	got, err := st.Get(context.Background(), "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "n1" || got.Nodes[1].Name != "n2" {
		t.Fatalf("original node order changed: %+v", got.Nodes)
	}
}

func TestAcceptDifferentContentConflictsAndLeavesOriginal(t *testing.T) {
	st, svc := newService(t)
	if _, outcome, err := svc.Accept(context.Background(), samplePlacement()); err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	different := samplePlacement()
	different.Priority = 11
	different.Nodes = []store.Node{}
	rec, outcome, err := svc.Accept(context.Background(), different)
	if err != nil {
		t.Fatalf("conflict call: %v", err)
	}
	if outcome != Conflict {
		t.Fatalf("outcome = %d, want Conflict", outcome)
	}
	if rec.Priority != 10 || len(rec.Nodes) != 4 {
		t.Fatalf("returned record is not the original: %+v", rec)
	}
	got, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Priority != 10 || len(got.Nodes) != 4 {
		t.Fatalf("stored record mutated by conflict: %+v", got)
	}
}

func TestAcceptRejectedRecordIsStoredAndQueryable(t *testing.T) {
	st, svc := newService(t)
	p := &store.Placement{
		Namespace: "team-a", Name: "job-2", Queue: "default", Priority: -1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "node-a", CPU: 1000, Memory: 512, Labels: map[string]string{}}},
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("decision = %+v", rec)
	}
	got, err := st.Get(context.Background(), "team-a", "job-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusRejected || got.Node != nil || got.Reason == nil || *got.Reason != ReasonNoNode {
		t.Fatalf("stored rejection mismatch: %+v", got)
	}
}

func TestAcceptConcurrentIdenticalSubmissionsStoreOneRecord(t *testing.T) {
	st, svc := newService(t)
	const n = 24
	var wg sync.WaitGroup
	outcomes := make([]Outcome, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, outcomes[i], errs[i] = svc.Accept(context.Background(), samplePlacement())
		}(i)
	}
	wg.Wait()

	var created, identical int
	for i := range outcomes {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		switch outcomes[i] {
		case Created:
			created++
		case Identical:
			identical++
		default:
			t.Fatalf("unexpected outcome %d", outcomes[i])
		}
	}
	if created != 1 || identical != n-1 {
		t.Fatalf("outcome counts: created=%d identical=%d, want 1 and %d", created, identical, n-1)
	}
	recs, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("stored records = %d, want 1", len(recs))
	}
}

// A rejected decision follows the same registration rules as a placed one:
// same content retries to the original record, different content conflicts,
// and neither adds nor rewrites a record — even when the new submission would
// schedule differently.
func TestAcceptRejectedRecordRetryAndConflictRules(t *testing.T) {
	st, svc := newService(t)
	rejected := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n", CPU: 1000, Memory: 512, Labels: map[string]string{}}},
	}
	first, outcome, err := svc.Accept(context.Background(), rejected)
	if err != nil || outcome != Created {
		t.Fatalf("first: outcome=%d err=%v", outcome, err)
	}
	if first.Status != StatusRejected {
		t.Fatalf("setup: want rejected, got %+v", first)
	}

	// Same content again: Identical, original record, still one row.
	retry := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 2000, Memory: 256},
		// Defaults omitted this time: nil selector and labels are the same content.
		Nodes: []store.Node{{Name: "n", CPU: 1000, Memory: 512}},
	}
	rec, outcome, err := svc.Accept(context.Background(), retry)
	if err != nil || outcome != Identical {
		t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("retry returned %+v, want the original rejection", rec)
	}

	// Different content on the same identity — this one would be placeable,
	// so the scheduling result must not stand in for content comparison.
	placeable := &store.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n", CPU: 1000, Memory: 512, Labels: map[string]string{}}},
	}
	rec, outcome, err = svc.Accept(context.Background(), placeable)
	if err != nil || outcome != Conflict {
		t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
	}
	if rec.Status != StatusRejected {
		t.Fatalf("conflict must return the untouched original rejection, got %+v", rec)
	}

	// The store still holds exactly the original rejected record.
	got, err := st.Get(context.Background(), "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusRejected || got.Resources.CPU != 2000 {
		t.Fatalf("stored record rewritten: %+v", got)
	}
	recs, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 after retry and conflict", len(recs))
	}
}

func TestAcceptStorageUnavailable(t *testing.T) {
	st, svc := newService(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, _, err := svc.Accept(context.Background(), samplePlacement())
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("err = %v, want ErrStorageUnavailable", err)
	}
}
