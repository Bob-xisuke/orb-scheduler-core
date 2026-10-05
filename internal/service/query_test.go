package service

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// seedPlacement registers one placement through Accept so query tests work on
// committed data produced by the real write path.
func seedPlacement(t *testing.T, svc *Service, namespace, name, queue string, nodes []store.Node) *store.Placement {
	t.Helper()
	p := &store.Placement{
		Namespace: namespace,
		Name:      name,
		Queue:     queue,
		Priority:  1,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     nodes,
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil {
		t.Fatalf("seed %s/%s: %v", namespace, name, err)
	}
	if outcome != Created {
		t.Fatalf("seed %s/%s: outcome = %d, want Created", namespace, name, outcome)
	}
	return rec
}

func eligibleNode(name string) []store.Node {
	return []store.Node{{Name: name, CPU: 1000, Memory: 512, Labels: map[string]string{}}}
}

func TestQuerySingleRecordReturnsAllFields(t *testing.T) {
	_, svc := newService(t)
	seeded := seedPlacement(t, svc, "team-a", "job-1", "default", eligibleNode("node-a"))

	result, err := svc.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"job-1"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record == nil || result.Items != nil {
		t.Fatalf("single success must set only Record: %+v", result)
	}
	rec := result.Record
	if rec.Namespace != seeded.Namespace || rec.Name != seeded.Name || rec.Queue != seeded.Queue ||
		rec.Priority != seeded.Priority || rec.Resources != seeded.Resources ||
		rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "node-a" || rec.Reason != nil {
		t.Fatalf("record fields not preserved: %+v", rec)
	}
	if len(rec.Nodes) != 1 || rec.Nodes[0].Name != "node-a" || rec.Selector == nil {
		t.Fatalf("request half not preserved: %+v", rec)
	}
}

func TestQuerySingleRejectedRecordKeepsNullNodeAndReason(t *testing.T) {
	_, svc := newService(t)
	// No eligible node: the stored record is a rejection.
	seedPlacement(t, svc, "team-a", "job-2", "default", []store.Node{})

	result, err := svc.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"job-2"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rec := result.Record
	if rec == nil || rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("rejected record mismatch: %+v", rec)
	}
}

func TestQuerySingleMissingIdentity(t *testing.T) {
	_, svc := newService(t)
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
}

func TestQueryInvalidParameterSets(t *testing.T) {
	_, svc := newService(t)
	cases := map[string]url.Values{
		"unknown key":          {"bogus": {"x"}},
		"known plus unknown":   {"namespace": {"a"}, "bogus": {"x"}},
		"repeated value":       {"namespace": {"a", "b"}},
		"empty value":          {"namespace": {""}},
		"name without ns":      {"name": {"job"}},
		"name with queue":      {"namespace": {"a"}, "name": {"job"}, "queue": {"q"}},
		"name with node":       {"namespace": {"a"}, "name": {"job"}, "node": {"n"}},
		"name with everything": {"namespace": {"a"}, "name": {"job"}, "queue": {"q"}, "node": {"n"}},
	}
	for label, values := range cases {
		result, err := svc.Query(context.Background(), values)
		if !errors.Is(err, ErrInvalidPlacementQuery) {
			t.Errorf("%s: err = %v, want ErrInvalidPlacementQuery", label, err)
		}
		if result.Record != nil || result.Items != nil {
			t.Errorf("%s: failure must leave both fields nil: %+v", label, result)
		}
	}
}

func TestQueryValidationFailureDoesNotTouchStorage(t *testing.T) {
	st, svc := newService(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// With the store closed, any storage access would surface as
	// ErrStorageUnavailable; an invalid query must still report the
	// validation error.
	_, err := svc.Query(context.Background(), url.Values{"bogus": {"x"}})
	if !errors.Is(err, ErrInvalidPlacementQuery) {
		t.Fatalf("err = %v, want ErrInvalidPlacementQuery", err)
	}
	if errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("invalid query reached storage: %v", err)
	}
}

func TestQueryListIntersectionFiltersAndOrder(t *testing.T) {
	_, svc := newService(t)
	// UTF-8 byte order: 'a' < 'z' < 'ä' < '日'.
	seedPlacement(t, svc, "z", "n2", "q1", eligibleNode("host-1"))
	seedPlacement(t, svc, "a", "n2", "q1", eligibleNode("host-1"))
	seedPlacement(t, svc, "ä", "n1", "q2", eligibleNode("host-2"))
	seedPlacement(t, svc, "日", "n1", "q1", eligibleNode("host-1"))
	seedPlacement(t, svc, "a", "n1", "q2", eligibleNode("host-2"))

	keys := func(items []*store.Placement) []string {
		var out []string
		for _, rec := range items {
			out = append(out, rec.Namespace+"/"+rec.Name)
		}
		return out
	}
	equal := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// Nil and empty parameter sets both list everything, sorted by
	// namespace then name in byte order.
	for label, values := range map[string]url.Values{"nil": nil, "empty": {}} {
		result, err := svc.Query(context.Background(), values)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if result.Record != nil || result.Items == nil {
			t.Fatalf("%s: list success must set only Items: %+v", label, result)
		}
		if got := keys(result.Items); !equal(got, "a/n1", "a/n2", "z/n2", "ä/n1", "日/n1") {
			t.Fatalf("%s: order = %v", label, got)
		}
	}

	single, err := svc.Query(context.Background(), url.Values{"namespace": {"a"}})
	if err != nil {
		t.Fatalf("namespace filter: %v", err)
	}
	if got := keys(single.Items); !equal(got, "a/n1", "a/n2") {
		t.Fatalf("namespace filter = %v", got)
	}

	intersection, err := svc.Query(context.Background(), url.Values{
		"namespace": {"a"},
		"queue":     {"q2"},
		"node":      {"host-2"},
	})
	if err != nil {
		t.Fatalf("intersection: %v", err)
	}
	if got := keys(intersection.Items); !equal(got, "a/n1") {
		t.Fatalf("namespace+queue+node intersection = %v", got)
	}

	// A condition that matches nothing on its own empties the intersection.
	none, err := svc.Query(context.Background(), url.Values{
		"queue": {"q1"},
		"node":  {"host-2"},
	})
	if err != nil {
		t.Fatalf("disjoint filters: %v", err)
	}
	if none.Items == nil || len(none.Items) != 0 {
		t.Fatalf("disjoint filters = %+v, want non-nil empty list", none)
	}
}

func TestQueryListEmptyResultIsNonNilEmpty(t *testing.T) {
	_, svc := newService(t)
	seedPlacement(t, svc, "team-a", "job-1", "default", eligibleNode("node-a"))

	result, err := svc.Query(context.Background(), url.Values{"queue": {"nope"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record != nil || result.Items == nil || len(result.Items) != 0 {
		t.Fatalf("empty match must be a non-nil empty Items: %+v", result)
	}
}

func TestQueryNodeFilterSkipsRejectedRecords(t *testing.T) {
	_, svc := newService(t)
	seedPlacement(t, svc, "team-a", "placed", "default", eligibleNode("node-a"))
	seedPlacement(t, svc, "team-a", "rejected", "default", []store.Node{})

	result, err := svc.Query(context.Background(), url.Values{"node": {"node-a"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "placed" {
		t.Fatalf("node filter must not match the rejected record: %+v", result.Items)
	}

	// The rejected record is still visible without the node condition.
	all, err := svc.Query(context.Background(), url.Values{"namespace": {"team-a"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("namespace filter = %d records, want 2", len(all.Items))
	}
}

func TestQueryValuesAreMatchedExactly(t *testing.T) {
	_, svc := newService(t)
	seedPlacement(t, svc, "team-a", "job-1", "default", eligibleNode("node-a"))

	// Surrounding whitespace is significant: no trimming, no re-decoding.
	result, err := svc.Query(context.Background(), url.Values{"namespace": {" team-a "}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Items == nil || len(result.Items) != 0 {
		t.Fatalf("whitespace-padded value must not match: %+v", result)
	}
	if _, err := svc.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"job-1%20"},
	}); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("err = %v, want ErrPlacementNotFound for undecoded value", err)
	}
}

func TestQueryDoesNotModifyCallerValues(t *testing.T) {
	_, svc := newService(t)
	seedPlacement(t, svc, "team-a", "job-1", "default", eligibleNode("node-a"))

	values := url.Values{"namespace": {"team-a"}, "name": {"job-1"}}
	if _, err := svc.Query(context.Background(), values); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(values) != 2 || len(values["namespace"]) != 1 || values["namespace"][0] != "team-a" ||
		len(values["name"]) != 1 || values["name"][0] != "job-1" {
		t.Fatalf("caller values modified: %v", values)
	}
}

func TestQueryStorageUnavailable(t *testing.T) {
	st, svc := newService(t)
	seedPlacement(t, svc, "team-a", "job-1", "default", eligibleNode("node-a"))
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for label, values := range map[string]url.Values{
		"single": {"namespace": {"team-a"}, "name": {"job-1"}},
		"list":   {"namespace": {"team-a"}},
	} {
		result, err := svc.Query(context.Background(), values)
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Errorf("%s: err = %v, want ErrStorageUnavailable", label, err)
		}
		if errors.Is(err, ErrPlacementNotFound) || errors.Is(err, ErrInvalidPlacementQuery) {
			t.Errorf("%s: storage failure misreported: %v", label, err)
		}
		if result.Record != nil || result.Items != nil {
			t.Errorf("%s: failure must leave both fields nil: %+v", label, result)
		}
	}
}

// TestQueryReadsCommittedDataOnly verifies a record written through the
// service is visible to a second service instance over the same store, and
// that querying never re-schedules or mutates it.
func TestQueryIsReadOnlyAcrossInstances(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	writer := New(st)
	seeded := seedPlacement(t, writer, "team-a", "job-1", "default", eligibleNode("node-a"))

	reader := New(st)
	result, err := reader.Query(context.Background(), url.Values{
		"namespace": {"team-a"},
		"name":      {"job-1"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record == nil || result.Record.Status != seeded.Status ||
		result.Record.Node == nil || *result.Record.Node != *seeded.Node {
		t.Fatalf("committed record not visible as written: %+v", result.Record)
	}

	again, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	same, err := store.SameInput(again, seeded)
	if err != nil || !same {
		t.Fatalf("query modified the stored record: same=%v err=%v", same, err)
	}
	if again.Status != seeded.Status {
		t.Fatalf("query re-scheduled the record: %q -> %q", seeded.Status, again.Status)
	}
}
