package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

func mustAccept(t *testing.T, svc *Service, p *store.Placement) *store.Placement {
	t.Helper()
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	return rec
}

// queryPlacementFor builds a minimal record with the given identity, queue and
// a single candidate node named nodeName.
func queryPlacementFor(ns, name, queue, nodeName string) *store.Placement {
	p := &store.Placement{
		Namespace: ns, Name: name, Queue: queue, Priority: 0,
		Resources: store.Resources{CPU: 1, Memory: 1},
		Selector:  map[string]string{},
	}
	if nodeName != "" {
		p.Nodes = []store.Node{{Name: nodeName, CPU: 10, Memory: 10, Labels: map[string]string{}}}
	} else {
		p.Nodes = []store.Node{} // no candidates -> rejected, node stays NULL
	}
	return p
}

func TestQuerySingleRecordHitReturnsRecordOnly(t *testing.T) {
	st, svc := newService(t)
	stored := mustAccept(t, svc, samplePlacement())

	res, err := svc.Query(context.Background(),
		url.Values{"namespace": {"team-a"}, "name": {"job-1"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Record == nil || res.Items != nil {
		t.Fatalf("single hit: Record=%v Items=%v, want Record set, Items nil", res.Record, res.Items)
	}
	ab, _ := json.Marshal(stored)
	bb, _ := json.Marshal(res.Record)
	if string(ab) != string(bb) {
		t.Fatalf("record not the original:\n%s\n%s", ab, bb)
	}

	got, err := st.Get(context.Background(), "team-a", "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(res.Record, got) {
		t.Fatalf("queried record differs from stored: %+v vs %+v", res.Record, got)
	}
}

func TestQuerySingleRecordMissingIsNotFound(t *testing.T) {
	_, svc := newService(t)
	res, err := svc.Query(context.Background(),
		url.Values{"namespace": {"team-a"}, "name": {"missing"}})
	if !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("err = %v, want ErrPlacementNotFound", err)
	}
	if res != nil {
		t.Fatalf("failure result = %+v, want nil", res)
	}
}

func TestQueryInvalidParams(t *testing.T) {
	cases := map[string]url.Values{
		"unknown key":                 {"unknown": {"1"}},
		"name without namespace":      {"name": {"job-1"}},
		"name with queue":             {"namespace": {"team-a"}, "name": {"job-1"}, "queue": {"q"}},
		"name with node":              {"namespace": {"team-a"}, "name": {"job-1"}, "node": {"n"}},
		"empty namespace":             {"namespace": {""}, "name": {"job-1"}},
		"empty name":                  {"namespace": {"team-a"}, "name": {""}},
		"repeated namespace":          {"namespace": {"a", "b"}, "name": {"job"}},
		"repeated name":               {"namespace": {"a"}, "name": {"job", "job2"}},
		"repeated queue in list":      {"queue": {"q1", "q2"}},
		"empty value in list":         {"queue": {""}},
		"node key with empty value":   {"node": {""}},
		"unknown key alongside valid": {"namespace": {"a"}, "bogus": {"x"}},
	}
	for desc, values := range cases {
		t.Run(desc, func(t *testing.T) {
			_, svc := newService(t)
			res, err := svc.Query(context.Background(), values)
			if !errors.Is(err, ErrInvalidPlacementQuery) {
				t.Fatalf("err = %v, want ErrInvalidPlacementQuery", err)
			}
			if res != nil {
				t.Fatalf("result = %+v, want nil", res)
			}
		})
	}
}

// Invalid queries must be rejected from pure validation: with the store closed
// they still return ErrInvalidPlacementQuery rather than a storage error, so
// no storage access happened.
func TestQueryInvalidParamsDoNotReachStorage(t *testing.T) {
	st, svc := newService(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, values := range []url.Values{
		{"unknown": {"1"}},
		{"name": {"job"}},
		{"namespace": {"a"}, "name": {"job"}, "queue": {"q"}},
		{"queue": {""}},
		{"queue": {"q", "r"}},
	} {
		if _, err := svc.Query(context.Background(), values); !errors.Is(err, ErrInvalidPlacementQuery) {
			t.Fatalf("values %v: err = %v, want ErrInvalidPlacementQuery before storage", values, err)
		}
	}
}

// Query must not rewrite the caller's url.Values, including rejecting without
// stripping duplicate entries.
func TestQueryDoesNotMutateInput(t *testing.T) {
	_, svc := newService(t)
	values := url.Values{}
	values.Add("namespace", "a")
	values.Add("namespace", "b")
	values.Add("name", "job")
	snapshot := url.Values{}
	for k, v := range values {
		snapshot[k] = append([]string(nil), v...)
	}
	if _, err := svc.Query(context.Background(), values); !errors.Is(err, ErrInvalidPlacementQuery) {
		t.Fatalf("err = %v, want ErrInvalidPlacementQuery", err)
	}
	if !reflect.DeepEqual(values, snapshot) {
		t.Fatalf("input mutated: %v -> %v", snapshot, values)
	}
}

func TestQueryListIntersectionSortAndEmpty(t *testing.T) {
	_, svc := newService(t)
	mustAccept(t, svc, queryPlacementFor("z", "n2", "q1", "host-1"))
	mustAccept(t, svc, queryPlacementFor("a", "n2", "q1", "host-1"))
	mustAccept(t, svc, queryPlacementFor("ä", "n1", "q2", "host-2"))
	mustAccept(t, svc, queryPlacementFor("日", "n1", "q1", "host-1"))
	mustAccept(t, svc, queryPlacementFor("a", "n1", "q2", "host-2"))

	identities := func(items []*store.Placement) []string {
		out := make([]string, len(items))
		for i, p := range items {
			out[i] = p.Namespace + "/" + p.Name
		}
		return out
	}

	// nil url.Values queries everything, sorted by namespace then name in
	// UTF-8 byte order.
	all, err := svc.Query(context.Background(), nil)
	if err != nil {
		t.Fatalf("query nil: %v", err)
	}
	if all.Record != nil || all.Items == nil {
		t.Fatalf("list: Record must be nil, Items non-nil: %+v", all)
	}
	want := []string{"a/n1", "a/n2", "z/n2", "ä/n1", "日/n1"}
	if got := identities(all.Items); !reflect.DeepEqual(got, want) {
		t.Fatalf("all order = %v, want %v", got, want)
	}

	// Empty map behaves the same as nil.
	empty, err := svc.Query(context.Background(), url.Values{})
	if err != nil {
		t.Fatalf("query empty: %v", err)
	}
	if got := identities(empty.Items); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty order = %v, want %v", got, want)
	}

	// Intersection: namespace + queue.
	nsQueue, err := svc.Query(context.Background(),
		url.Values{"namespace": {"a"}, "queue": {"q2"}})
	if err != nil {
		t.Fatalf("query ns+queue: %v", err)
	}
	if got := identities(nsQueue.Items); !reflect.DeepEqual(got, []string{"a/n1"}) {
		t.Fatalf("ns+queue = %v, want [a/n1]", got)
	}

	// node + queue intersect.
	nodeQueue, err := svc.Query(context.Background(),
		url.Values{"node": {"host-2"}, "queue": {"q2"}})
	if err != nil {
		t.Fatalf("query node+queue: %v", err)
	}
	if got := identities(nodeQueue.Items); !reflect.DeepEqual(got, []string{"a/n1", "ä/n1"}) {
		t.Fatalf("node+queue = %v, want [a/n1 ä/n1]", got)
	}

	// No match is a non-nil empty list, not an error.
	none, err := svc.Query(context.Background(), url.Values{"queue": {"nope"}})
	if err != nil {
		t.Fatalf("query no match: %v", err)
	}
	if none.Record != nil || none.Items == nil || len(none.Items) != 0 {
		t.Fatalf("no match = %+v, want non-nil empty Items", none)
	}
}

// node filters on the scheduled column, which is NULL for rejected records:
// a rejected record never matches even if the value equals its candidate name.
func TestQueryListNodeFilterExcludesRejected(t *testing.T) {
	_, svc := newService(t)
	mustAccept(t, svc, queryPlacementFor("ns", "placed", "q", "host-1"))
	mustAccept(t, svc, queryPlacementFor("ns", "rejected", "q", ""))

	byNode, err := svc.Query(context.Background(), url.Values{"node": {"host-1"}})
	if err != nil {
		t.Fatalf("query node: %v", err)
	}
	if len(byNode.Items) != 1 || byNode.Items[0].Name != "placed" {
		t.Fatalf("node filter = %+v, want only placed", byNode.Items)
	}

	all, err := svc.Query(context.Background(), url.Values{"namespace": {"ns"}})
	if err != nil {
		t.Fatalf("query ns: %v", err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("namespace filter = %d records, want 2 (rejected included)", len(all.Items))
	}
}

// Values arrive already percent-decoded; Query matches them exactly: it neither
// trims whitespace nor decodes a second time.
func TestQueryMatchesDecodedValuesExactly(t *testing.T) {
	_, svc := newService(t)
	mustAccept(t, svc, queryPlacementFor("team-a", "job-1", "default", "node-a"))

	// url.ParseQuery decodes %20 but leaves the spaces in; the stored value
	// "default" must not match it.
	values, err := url.ParseQuery("queue=def%61ult&namespace=team%2Da")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := svc.Query(context.Background(), values)
	if err != nil {
		t.Fatalf("query decoded: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "job-1" {
		t.Fatalf("decoded values should match exactly, got %+v", res.Items)
	}

	padded, err := url.ParseQuery("queue=%20default%20")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := padded.Get("queue"); got != " default " {
		t.Fatalf("test setup: %q", got)
	}
	none, err := svc.Query(context.Background(), padded)
	if err != nil {
		t.Fatalf("query padded: %v", err)
	}
	if len(none.Items) != 0 {
		t.Fatalf("whitespace must not be trimmed into a match: %+v", none.Items)
	}
}

func TestQueryStorageUnavailable(t *testing.T) {
	st, svc := newService(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := svc.Query(context.Background(),
		url.Values{"namespace": {"a"}, "name": {"job"}}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("single err = %v, want ErrStorageUnavailable", err)
	}
	if _, err := svc.Query(context.Background(),
		url.Values{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("list err = %v, want ErrStorageUnavailable", err)
	}
}

// Records committed by Accept (the same shape old SQLite rows already have)
// read back through Query with every field intact, including a rejection's
// null node and reason — no conversion step.
func TestQueryRejectedRecordReadsBackComplete(t *testing.T) {
	_, svc := newService(t)
	mustAccept(t, svc, queryPlacementFor("ns", "rej", "q", ""))

	res, err := svc.Query(context.Background(),
		url.Values{"namespace": {"ns"}, "name": {"rej"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rec := res.Record
	if rec == nil {
		t.Fatalf("record missing")
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil || *rec.Reason != ReasonNoNode {
		t.Fatalf("rejected fields incomplete: %+v", rec)
	}
	if rec.Namespace != "ns" || rec.Name != "rej" || rec.Queue != "q" || len(rec.Nodes) != 0 {
		t.Fatalf("input fields incomplete: %+v", rec)
	}
}

// Ensure the standalone-open path (fresh store on an existing database file)
// queries old rows directly.
func TestQueryReadsRowsAfterReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustAccept(t, New(st), samplePlacement())
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	res, err := New(st2).Query(context.Background(),
		url.Values{"namespace": {"team-a"}, "name": {"job-1"}})
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if res.Record == nil || res.Record.Status != StatusPlaced {
		t.Fatalf("old row not readable without conversion: %+v", res)
	}
}
