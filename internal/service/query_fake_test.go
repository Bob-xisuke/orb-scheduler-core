package service

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

// This suite runs the Query business flow against the in-memory fakestore
// double: no SQLite file is created, the records and errors storage returns
// are controlled directly, and call recording proves which queries never
// reach storage. The SQLite-backed tests in query_test.go remain the
// regression for the real storage path.

func mustValues(t *testing.T, raw string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return values
}

// placedRecord builds a stored-looking record without running Accept, so
// tests control exactly what the storage layer holds.
func placedRecord(namespace, name, queue, node string) *model.Placement {
	return &model.Placement{
		Namespace: namespace,
		Name:      name,
		Queue:     queue,
		Priority:  1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: node, CPU: 1000, Memory: 512, Labels: map[string]string{}}},
		Status:    StatusPlaced,
		Node:      &node,
	}
}

func rejectedRecord(namespace, name, queue string) *model.Placement {
	reason := ReasonNoNode
	return &model.Placement{
		Namespace: namespace,
		Name:      name,
		Queue:     queue,
		Priority:  1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{},
		Status:    StatusRejected,
		Reason:    &reason,
	}
}

func TestQueryWithFakeInvalidParameterSetsNeverReachStorage(t *testing.T) {
	fake, svc := newFakeService()
	fake.Seed(placedRecord("team-a", "job-1", "default", "node-a"))

	cases := map[string]url.Values{
		"unknown key":        {"bogus": {"x"}},
		"known plus unknown": {"namespace": {"a"}, "bogus": {"x"}},
		"repeated value":     {"namespace": {"a", "b"}},
		"empty value":        {"namespace": {""}},
		"name without ns":    {"name": {"job"}},
		"name with queue":    {"namespace": {"a"}, "name": {"job"}, "queue": {"q"}},
		"name with node":     {"namespace": {"a"}, "name": {"job"}, "node": {"n"}},
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
	if len(fake.Gets) != 0 || len(fake.Lists) != 0 || len(fake.Submits) != 0 {
		t.Fatalf("invalid queries reached storage: gets=%d lists=%d submits=%d",
			len(fake.Gets), len(fake.Lists), len(fake.Submits))
	}
}

func TestQueryWithFakeSingleRecordReturnedAsStored(t *testing.T) {
	fake, svc := newFakeService()
	// The stored decision points at a node the request never offered and
	// asks for more than any node has: if Query re-scheduled, the record
	// could not come back like this.
	seeded := placedRecord("team-a", "job-1", "default", "node-not-offered")
	seeded.Resources = model.Resources{CPU: 9999, Memory: 9999}
	fake.Seed(seeded)

	result, err := svc.Query(context.Background(), mustValues(t, "namespace=team-a&name=job-1"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Record == nil || result.Items != nil {
		t.Fatalf("single success must set only Record: %+v", result)
	}
	rec := result.Record
	if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "node-not-offered" ||
		rec.Resources != seeded.Resources {
		t.Fatalf("query did not return the stored record verbatim: %+v", rec)
	}
	if len(fake.Gets) != 1 || fake.Gets[0] != (fakestore.GetCall{Namespace: "team-a", Name: "job-1"}) {
		t.Fatalf("gets = %+v, want one get for team-a/job-1", fake.Gets)
	}
	if len(fake.Lists) != 0 || len(fake.Submits) != 0 {
		t.Fatalf("single query must not list or write: %+v %+v", fake.Lists, fake.Submits)
	}
}

func TestQueryWithFakeSingleMissingIdentity(t *testing.T) {
	fake, svc := newFakeService()
	fake.Seed(placedRecord("team-a", "job-1", "default", "node-a"))

	result, err := svc.Query(context.Background(), mustValues(t, "namespace=team-a&name=missing"))
	if !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("err = %v, want ErrPlacementNotFound", err)
	}
	if errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("not-found misreported as storage failure: %v", err)
	}
	if result.Record != nil || result.Items != nil {
		t.Fatalf("failure must leave both fields nil: %+v", result)
	}
}

func TestQueryWithFakeListPassesFilterThrough(t *testing.T) {
	fake, svc := newFakeService()
	// Byte order: 'a' < 'z' < 'ä'.
	fake.Seed(
		placedRecord("z", "n2", "q1", "host-1"),
		placedRecord("a", "n2", "q1", "host-1"),
		placedRecord("ä", "n1", "q2", "host-2"),
		placedRecord("a", "n1", "q2", "host-2"),
		rejectedRecord("a", "n0", "q2"),
	)

	keys := func(items []*model.Placement) []string {
		var out []string
		for _, rec := range items {
			out = append(out, rec.Namespace+"/"+rec.Name)
		}
		return out
	}

	// An empty parameter set lists everything, sorted by namespace then
	// name in UTF-8 byte order, rejected records included.
	all, err := svc.Query(context.Background(), nil)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got := keys(all.Items); !equalKeys(got, "a/n0", "a/n1", "a/n2", "z/n2", "ä/n1") {
		t.Fatalf("list all = %v", got)
	}
	if len(fake.Lists) != 1 || fake.Lists[0] != (model.ListFilter{}) {
		t.Fatalf("empty query must pass an empty filter, got %+v", fake.Lists)
	}

	// Conditions intersect and reach storage exactly as queried.
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=a&queue=q2&node=host-2"))
	if err != nil {
		t.Fatalf("intersection: %v", err)
	}
	if got := keys(result.Items); !equalKeys(got, "a/n1") {
		t.Fatalf("intersection = %v", got)
	}
	wantFilter := model.ListFilter{Namespace: "a", Queue: "q2", Node: "host-2"}
	if fake.Lists[1] != wantFilter {
		t.Fatalf("filter = %+v, want %+v", fake.Lists[1], wantFilter)
	}

	// A node filter never matches a rejected record.
	byNode, err := svc.Query(context.Background(), mustValues(t, "node=host-1"))
	if err != nil {
		t.Fatalf("node filter: %v", err)
	}
	if got := keys(byNode.Items); !equalKeys(got, "a/n2", "z/n2") {
		t.Fatalf("node filter = %v", got)
	}

	// No match is a non-nil empty list.
	none, err := svc.Query(context.Background(), mustValues(t, "queue=nope"))
	if err != nil {
		t.Fatalf("empty match: %v", err)
	}
	if none.Record != nil || none.Items == nil || len(none.Items) != 0 {
		t.Fatalf("empty match must be a non-nil empty Items: %+v", none)
	}
}

func TestQueryWithFakeStorageFailure(t *testing.T) {
	fake, svc := newFakeService()
	fake.Seed(placedRecord("team-a", "job-1", "default", "node-a"))
	root := errors.New("io error")

	fake.GetErr = root
	result, err := svc.Query(context.Background(), mustValues(t, "namespace=team-a&name=job-1"))
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("get failure: err = %v, want ErrStorageUnavailable", err)
	}
	if !errors.Is(err, root) {
		t.Fatalf("get failure: err = %v, want the injected root error to stay recognizable", err)
	}
	if errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("storage failure misreported as not found: %v", err)
	}
	if result.Record != nil || result.Items != nil {
		t.Fatalf("failure must leave both fields nil: %+v", result)
	}

	fake.GetErr = nil
	fake.ListErr = root
	result, err = svc.Query(context.Background(), mustValues(t, "namespace=team-a"))
	if !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, root) {
		t.Fatalf("list failure: err = %v, want ErrStorageUnavailable wrapping the root error", err)
	}
	if result.Record != nil || result.Items != nil {
		t.Fatalf("failure must leave both fields nil: %+v", result)
	}
}

func TestQueryWithFakeDoesNotModifyCallerValues(t *testing.T) {
	fake, svc := newFakeService()
	fake.Seed(placedRecord("team-a", "job-1", "default", "node-a"))

	values := url.Values{"namespace": {"team-a"}, "name": {"job-1"}}
	if _, err := svc.Query(context.Background(), values); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(values) != 2 || len(values["namespace"]) != 1 || values["namespace"][0] != "team-a" ||
		len(values["name"]) != 1 || values["name"][0] != "job-1" {
		t.Fatalf("caller values modified: %v", values)
	}
}

func equalKeys(got []string, want ...string) bool {
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
