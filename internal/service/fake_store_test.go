package service

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// fakeStore is a programmable Store substitute: each method's response and
// failure are set per test, and every access is recorded so tests can prove
// when the business flow did or did not reach storage. It carries no
// acceptance or query rules of its own and never touches a SQLite file.
type fakeStore struct {
	submitFunc func(ctx context.Context, p *store.Placement) (*store.Placement, bool, error)
	getFunc    func(ctx context.Context, namespace, name string) (*store.Placement, error)
	listFunc   func(ctx context.Context, f store.ListFilter) ([]*store.Placement, error)
	calls      []string
}

var errUnexpectedCall = errors.New("unexpected storage call")

func (f *fakeStore) Submit(ctx context.Context, p *store.Placement) (*store.Placement, bool, error) {
	f.calls = append(f.calls, "submit")
	if f.submitFunc == nil {
		return nil, false, errUnexpectedCall
	}
	return f.submitFunc(ctx, p)
}

func (f *fakeStore) Get(ctx context.Context, namespace, name string) (*store.Placement, error) {
	f.calls = append(f.calls, "get")
	if f.getFunc == nil {
		return nil, errUnexpectedCall
	}
	return f.getFunc(ctx, namespace, name)
}

func (f *fakeStore) List(ctx context.Context, filter store.ListFilter) ([]*store.Placement, error) {
	f.calls = append(f.calls, "list")
	if f.listFunc == nil {
		return nil, errUnexpectedCall
	}
	return f.listFunc(ctx, filter)
}

func (f *fakeStore) callCount() int { return len(f.calls) }

// Accept normalizes defaults and schedules before Submit is invoked, and a
// first-time registration is reported as Created with the stored record.
func TestAcceptCreatedThroughFakeStore(t *testing.T) {
	fake := &fakeStore{}
	var received *store.Placement
	fake.submitFunc = func(_ context.Context, p *store.Placement) (*store.Placement, bool, error) {
		received = p
		out := *p
		return &out, true, nil
	}
	svc := New(fake)

	p := &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "default",
		Priority:  10,
		Resources: store.Resources{CPU: 500, Memory: 256},
		// Selector and node Labels omitted: Accept must default them.
		Nodes: []store.Node{
			{Name: "node-b", CPU: 1000, Memory: 512},
			{Name: "node-a", CPU: 1000, Memory: 512},
			{Name: "node-c", CPU: 100, Memory: 128},
		},
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if outcome != Created {
		t.Fatalf("outcome = %d, want Created", outcome)
	}
	if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "node-a" || rec.Reason != nil {
		t.Fatalf("decision not applied: %+v", rec)
	}

	// The store saw the normalized, already-scheduled placement.
	if received == nil {
		t.Fatal("submit was not called")
	}
	if received.Selector == nil || received.Nodes == nil {
		t.Fatalf("defaults not filled before submit: %+v", received)
	}
	for i, n := range received.Nodes {
		if n.Labels == nil {
			t.Fatalf("node %d labels not defaulted before submit: %+v", i, n)
		}
	}
	if received.Status != StatusPlaced || received.Node == nil || *received.Node != "node-a" {
		t.Fatalf("scheduling did not happen before submit: %+v", received)
	}
	if fake.callCount() != 1 {
		t.Fatalf("storage calls = %v, want exactly one submit", fake.calls)
	}
}

// A repeated identity returns the record the store already holds; the
// service-side content comparison classifies it as Identical or Conflict
// without the substitute re-implementing any rule.
func TestAcceptRetryOutcomesThroughFakeStore(t *testing.T) {
	original := &store.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n", CPU: 200, Memory: 128, Labels: map[string]string{}}},
		Status:    StatusPlaced,
		Node:      ptrString("n"),
	}
	fake := &fakeStore{
		submitFunc: func(_ context.Context, _ *store.Placement) (*store.Placement, bool, error) {
			return original, false, nil
		},
	}
	svc := New(fake)

	// Same content with defaults omitted: Identical, original record back.
	same := &store.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Nodes:     []store.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	rec, outcome, err := svc.Accept(context.Background(), same)
	if err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if outcome != Identical {
		t.Fatalf("outcome = %d, want Identical", outcome)
	}
	if rec != original {
		t.Fatalf("identical retry must return the stored record, got %+v", rec)
	}

	// Different content: Conflict, still the untouched original back.
	different := &store.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  2,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Nodes:     []store.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	rec, outcome, err = svc.Accept(context.Background(), different)
	if err != nil {
		t.Fatalf("conflict retry: %v", err)
	}
	if outcome != Conflict {
		t.Fatalf("outcome = %d, want Conflict", outcome)
	}
	if rec != original || rec.Priority != 1 {
		t.Fatalf("conflict must return the untouched original, got %+v", rec)
	}
}

// A registration failure surfaces as ErrStorageUnavailable while the
// underlying cause stays recognizable with errors.Is.
func TestAcceptSubmitFailureThroughFakeStore(t *testing.T) {
	errBackend := errors.New("backend connection lost")
	fake := &fakeStore{
		submitFunc: func(_ context.Context, _ *store.Placement) (*store.Placement, bool, error) {
			return nil, false, errBackend
		},
	}
	svc := New(fake)

	rec, _, err := svc.Accept(context.Background(), samplePlacement())
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("err = %v, want ErrStorageUnavailable", err)
	}
	if !errors.Is(err, errBackend) {
		t.Fatalf("err = %v, want the underlying cause preserved", err)
	}
	if rec != nil {
		t.Fatalf("failure must not return a record: %+v", rec)
	}
}

// An invalid query is rejected before any storage access.
func TestQueryInvalidDoesNotTouchFakeStore(t *testing.T) {
	fake := &fakeStore{}
	svc := New(fake)

	for label, values := range map[string]url.Values{
		"unknown key":     {"bogus": {"x"}},
		"repeated value":  {"namespace": {"a", "b"}},
		"name without ns": {"name": {"job"}},
		"name with queue": {"namespace": {"a"}, "name": {"job"}, "queue": {"q"}},
	} {
		result, err := svc.Query(context.Background(), values)
		if !errors.Is(err, ErrInvalidPlacementQuery) {
			t.Errorf("%s: err = %v, want ErrInvalidPlacementQuery", label, err)
		}
		if result.Record != nil || result.Items != nil {
			t.Errorf("%s: failure must leave both fields nil: %+v", label, result)
		}
	}
	if fake.callCount() != 0 {
		t.Fatalf("invalid queries reached storage: %v", fake.calls)
	}
}

// The single-record form maps an unknown identity to ErrPlacementNotFound.
func TestQuerySingleNotFoundThroughFakeStore(t *testing.T) {
	fake := &fakeStore{
		getFunc: func(_ context.Context, _, _ string) (*store.Placement, error) {
			return nil, store.ErrNotFound
		},
	}
	svc := New(fake)

	result, err := svc.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"missing"},
	})
	if !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("err = %v, want ErrPlacementNotFound", err)
	}
	if result.Record != nil || result.Items != nil {
		t.Fatalf("failure must leave both fields nil: %+v", result)
	}
	if fake.callCount() != 1 || fake.calls[0] != "get" {
		t.Fatalf("storage calls = %v, want exactly one get", fake.calls)
	}
}

// Read failures surface as ErrStorageUnavailable with the cause preserved,
// for both the single-record and the list form.
func TestQueryReadFailuresThroughFakeStore(t *testing.T) {
	errBackend := errors.New("backend connection lost")
	fake := &fakeStore{
		getFunc: func(_ context.Context, _, _ string) (*store.Placement, error) {
			return nil, errBackend
		},
		listFunc: func(_ context.Context, _ store.ListFilter) ([]*store.Placement, error) {
			return nil, errBackend
		},
	}
	svc := New(fake)

	for label, values := range map[string]url.Values{
		"single": {"namespace": {"team-a"}, "name": {"job-1"}},
		"list":   {"namespace": {"team-a"}},
	} {
		result, err := svc.Query(context.Background(), values)
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("%s: err = %v, want ErrStorageUnavailable", label, err)
		}
		if !errors.Is(err, errBackend) {
			t.Errorf("%s: err = %v, want the underlying cause preserved", label, err)
		}
		if errors.Is(err, ErrPlacementNotFound) || errors.Is(err, ErrInvalidPlacementQuery) {
			t.Errorf("%s: storage failure misreported: %v", label, err)
		}
		if result.Record != nil || result.Items != nil {
			t.Errorf("%s: failure must leave both fields nil: %+v", label, result)
		}
	}
}

// The single-record form returns the stored record untouched: querying never
// re-schedules it.
func TestQuerySingleReturnsStoredRecordThroughFakeStore(t *testing.T) {
	stored := &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "default",
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{},
		Status:    StatusRejected,
		Reason:    ptrString(ReasonNoNode),
	}
	fake := &fakeStore{
		getFunc: func(_ context.Context, namespace, name string) (*store.Placement, error) {
			if namespace != "team-a" || name != "job-1" {
				t.Fatalf("get called with %s/%s", namespace, name)
			}
			return stored, nil
		},
	}
	svc := New(fake)

	result, err := svc.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"job-1"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record != stored || result.Items != nil {
		t.Fatalf("single success must return the stored record only: %+v", result)
	}
	if result.Record.Status != StatusRejected || result.Record.Node != nil {
		t.Fatalf("query re-scheduled the record: %+v", result.Record)
	}
}

// The list form maps the parameter set onto the store filter exactly, never
// modifies the caller's values, and renders an empty match as a non-nil
// empty list.
func TestQueryListThroughFakeStore(t *testing.T) {
	var gotFilter store.ListFilter
	seeded := &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "q1",
		Status:    StatusPlaced,
		Node:      ptrString("node-a"),
	}
	fake := &fakeStore{
		listFunc: func(_ context.Context, f store.ListFilter) ([]*store.Placement, error) {
			gotFilter = f
			if f.Queue == "empty" {
				return nil, nil
			}
			return []*store.Placement{seeded}, nil
		},
	}
	svc := New(fake)

	values := url.Values{"namespace": {"team-a"}, "queue": {"q1"}, "node": {"node-a"}}
	result, err := svc.Query(context.Background(), values)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record != nil || len(result.Items) != 1 || result.Items[0] != seeded {
		t.Fatalf("list result = %+v", result)
	}
	if gotFilter != (store.ListFilter{Namespace: "team-a", Queue: "q1", Node: "node-a"}) {
		t.Fatalf("filter = %+v, want exact parameter mapping", gotFilter)
	}
	if len(values) != 3 || values.Get("namespace") != "team-a" || values.Get("queue") != "q1" ||
		values.Get("node") != "node-a" {
		t.Fatalf("caller values modified: %v", values)
	}

	empty, err := svc.Query(context.Background(), url.Values{"queue": {"empty"}})
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	if empty.Record != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty match must be a non-nil empty Items: %+v", empty)
	}
}

func ptrString(s string) *string { return &s }
