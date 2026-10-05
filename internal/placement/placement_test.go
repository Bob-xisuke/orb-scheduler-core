package placement

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

func newService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewService(st)
}

// eligibleRequest offers four nodes of which node-a and node-b are eligible;
// node-c is too small and node-d is in the wrong zone.
func eligibleRequest() *store.Placement {
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

func TestAcceptFirstSubmissionStoresPlacedRecord(t *testing.T) {
	st, svc := newService(t)

	rec, outcome, err := svc.Accept(context.Background(), eligibleRequest())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if outcome != Created {
		t.Fatalf("outcome = %v, want created", outcome)
	}
	if rec.Status != "placed" || rec.Node == nil || *rec.Node != "node-a" || rec.Reason != nil {
		t.Fatalf("unexpected decision: status=%q node=%v reason=%v", rec.Status, rec.Node, rec.Reason)
	}

	stored, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != rec.Status || *stored.Node != *rec.Node {
		t.Fatalf("stored record differs from accepted record: %+v vs %+v", stored, rec)
	}
}

func TestAcceptRejectedWhenNoEligibleNode(t *testing.T) {
	_, svc := newService(t)

	p := eligibleRequest()
	p.Nodes = []store.Node{}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if outcome != Created {
		t.Fatalf("outcome = %v, want created", outcome)
	}
	if rec.Status != "rejected" || rec.Node != nil || rec.Reason == nil || *rec.Reason != "no_eligible_node" {
		t.Fatalf("unexpected decision: status=%q node=%v reason=%v", rec.Status, rec.Node, rec.Reason)
	}
}

func TestAcceptDuplicateFillsOmittedDefaults(t *testing.T) {
	_, svc := newService(t)

	first := eligibleRequest()
	first.Selector = nil // omitted on the wire
	first.Nodes[0].Labels = nil
	if _, outcome, err := svc.Accept(context.Background(), first); err != nil || outcome != Created {
		t.Fatalf("first accept: outcome=%v err=%v", outcome, err)
	}

	second := eligibleRequest()
	second.Selector = map[string]string{} // explicit {} matches the omitted default
	second.Nodes[0].Labels = map[string]string{}
	rec, outcome, err := svc.Accept(context.Background(), second)
	if err != nil {
		t.Fatalf("second accept: %v", err)
	}
	if outcome != Duplicate {
		t.Fatalf("outcome = %v, want duplicate", outcome)
	}
	if rec.Status != "placed" || rec.Node == nil || *rec.Node != "node-a" {
		t.Fatalf("duplicate returned a different record: %+v", rec)
	}
}

func TestAcceptConflictKeepsOriginalRecord(t *testing.T) {
	st, svc := newService(t)

	first := eligibleRequest()
	first.Nodes = first.Nodes[:2] // [node-b, node-a]
	if _, _, err := svc.Accept(context.Background(), first); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	reordered := eligibleRequest()
	reordered.Nodes = []store.Node{reordered.Nodes[1], reordered.Nodes[0]} // [node-a, node-b]
	rec, outcome, err := svc.Accept(context.Background(), reordered)
	if err != nil {
		t.Fatalf("second accept: %v", err)
	}
	if outcome != Conflict {
		t.Fatalf("outcome = %v, want conflict", outcome)
	}
	if rec.Nodes[0].Name != "node-b" {
		t.Fatalf("conflict must return the pre-existing record: %+v", rec.Nodes)
	}

	stored, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Nodes[0].Name != "node-b" || stored.Nodes[1].Name != "node-a" {
		t.Fatalf("conflicting submission changed the stored record: %+v", stored.Nodes)
	}
}

func TestAcceptConcurrentIdenticalSubmissionsStoreOneRecord(t *testing.T) {
	st, svc := newService(t)

	const n = 24
	outcomes := make([]Outcome, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, outcome, err := svc.Accept(context.Background(), eligibleRequest())
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			outcomes[i] = outcome
		}(i)
	}
	wg.Wait()

	var created, duplicate int
	for _, o := range outcomes {
		switch o {
		case Created:
			created++
		case Duplicate:
			duplicate++
		default:
			t.Fatalf("unexpected outcome %v", o)
		}
	}
	if created != 1 || duplicate != n-1 {
		t.Fatalf("outcomes: created=%d duplicate=%d, want 1 and %d", created, duplicate, n-1)
	}
	recs, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("stored records = %d, want 1", len(recs))
	}
}

func TestAcceptStorageFailureReturnsError(t *testing.T) {
	st, svc := newService(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := svc.Accept(context.Background(), eligibleRequest()); err == nil {
		t.Fatal("accept on a closed store must fail")
	}
}
