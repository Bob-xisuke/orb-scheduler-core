package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// acceptanceCrossCheck drives the same inputs through the placement service
// (the standalone acceptance entry) and the HTTP router — each backed by its
// own database — then asserts that both sides agree on the outcome and on the
// stored record.
type acceptanceCrossCheck struct {
	t   *testing.T
	svc *placement.Service
	st  *store.Store
	h   http.Handler
}

func newAcceptanceCrossCheck(t *testing.T) *acceptanceCrossCheck {
	t.Helper()
	open := func() *store.Store {
		st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	st := open()
	return &acceptanceCrossCheck{
		t:   t,
		svc: placement.NewService(st),
		st:  st,
		h:   NewRouter(open()),
	}
}

var outcomeStatus = map[placement.Outcome]int{
	placement.Created:   http.StatusCreated,
	placement.Duplicate: http.StatusOK,
	placement.Conflict:  http.StatusConflict,
}

// submit posts body over HTTP and accepts the same content through the
// service, requiring both sides to agree. It returns the accepted record
// (nil on conflict).
func (x *acceptanceCrossCheck) submit(body string) *store.Placement {
	x.t.Helper()

	p, err := parsePlacement([]byte(body))
	if err != nil {
		x.t.Fatalf("parsePlacement: %v", err)
	}
	rec, outcome, err := x.svc.Accept(context.Background(), p)
	if err != nil {
		x.t.Fatalf("accept: %v", err)
	}

	httpRec := postPlacement(x.t, x.h, body)
	if want := outcomeStatus[outcome]; httpRec.Code != want {
		x.t.Fatalf("outcome %v maps to HTTP %d, got %d: %s", outcome, want, httpRec.Code, httpRec.Body.String())
	}
	if outcome == placement.Conflict {
		expectErrorCode(x.t, httpRec, http.StatusConflict, "PlacementConflictError")
		return rec
	}

	wantJSON, err := json.Marshal(rec)
	if err != nil {
		x.t.Fatalf("marshal accepted record: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantJSON, &want); err != nil {
		x.t.Fatalf("unmarshal accepted record: %v", err)
	}
	if got := decodeBody(x.t, httpRec); !reflect.DeepEqual(got, want) {
		x.t.Fatalf("HTTP record and accepted record differ:\nHTTP:    %s\nservice: %s", httpRec.Body.String(), wantJSON)
	}
	return rec
}

// requireRecordEqualsGET fetches the identity through the service-side store
// and through HTTP GET and requires both to render the same record.
func (x *acceptanceCrossCheck) requireRecordEqualsGET(namespace, name string) {
	x.t.Helper()

	stored, err := x.st.Get(context.Background(), namespace, name)
	if err != nil {
		x.t.Fatalf("get %s/%s: %v", namespace, name, err)
	}
	rec := doRequest(x.t, x.h, http.MethodGet, "/v1/placements?namespace="+namespace+"&name="+name, "")
	if rec.Code != http.StatusOK {
		x.t.Fatalf("GET %s/%s = %d: %s", namespace, name, rec.Code, rec.Body.String())
	}
	wantJSON, err := json.Marshal(stored)
	if err != nil {
		x.t.Fatalf("marshal stored record: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantJSON, &want); err != nil {
		x.t.Fatalf("unmarshal stored record: %v", err)
	}
	if got := decodeBody(x.t, rec); !reflect.DeepEqual(got, want) {
		x.t.Fatalf("GET record and stored record differ:\nHTTP:  %s\nstore: %s", rec.Body.String(), wantJSON)
	}
}

// The same legal input must produce the same record and acceptance result
// through the standalone entry and through HTTP, including a retry that
// relies on omitted selector/labels being filled with defaults.
func TestAcceptanceEntryAndHTTPAgreeOnDefaultsRetry(t *testing.T) {
	x := newAcceptanceCrossCheck(t)

	first := `{
	  "namespace": "acme", "name": "job-defaults", "queue": "q", "priority": 1,
	  "resources": {"memory": 64, "cpu": 100},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`
	// selector omitted vs {}, node labels omitted vs {}, members reordered.
	retry := `{
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n1"}],
	  "selector": {}, "resources": {"cpu": 100, "memory": 64},
	  "priority": 1, "queue": "q", "name": "job-defaults", "namespace": "acme"
	}`

	rec := x.submit(first)
	if rec.Status != "placed" || rec.Node == nil || *rec.Node != "n1" {
		x.t.Fatalf("unexpected decision: %+v", rec)
	}
	again := x.submit(retry)
	if !reflect.DeepEqual(again, rec) {
		x.t.Fatalf("retry returned a different record:\nfirst: %+v\nretry: %+v", rec, again)
	}
	x.requireRecordEqualsGET("acme", "job-defaults")
}

// Reordering the nodes array is a content change: both the standalone entry
// and HTTP must report a conflict and leave the original record untouched.
func TestAcceptanceEntryAndHTTPAgreeOnNodeReorderConflict(t *testing.T) {
	x := newAcceptanceCrossCheck(t)

	body := `{
	  "namespace": "acme", "name": "job-pair", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	reordered := `{
	  "namespace": "acme", "name": "job-pair", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`

	original := x.submit(body)
	existing := x.submit(reordered) // conflict: returns the pre-existing record
	if !reflect.DeepEqual(existing, original) {
		x.t.Fatalf("conflict must surface the original record:\noriginal: %+v\ngot:      %+v", original, existing)
	}
	x.requireRecordEqualsGET("acme", "job-pair")
}

// A rejected record must be queryable through both entries with node null and
// reason no_eligible_node.
func TestAcceptanceEntryAndHTTPAgreeOnRejectedRecordQuery(t *testing.T) {
	x := newAcceptanceCrossCheck(t)

	body := `{
	  "namespace": "acme", "name": "job-rejected", "queue": "q", "priority": 0,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "n1", "cpu": 1000, "memory": 512, "labels": {}}]
	}`
	rec := x.submit(body)
	if rec.Status != "rejected" || rec.Node != nil || rec.Reason == nil || *rec.Reason != "no_eligible_node" {
		x.t.Fatalf("unexpected decision: %+v", rec)
	}
	x.requireRecordEqualsGET("acme", "job-rejected")

	// The rejected record must also appear in the filtered list, and must not
	// match a node filter.
	all := doRequest(x.t, x.h, http.MethodGet, "/v1/placements?namespace=acme", "")
	if items := decodeBody(x.t, all)["items"].([]any); len(items) != 1 {
		x.t.Fatalf("namespace filter returned %d records, want 1", len(items))
	}
	byNode := doRequest(x.t, x.h, http.MethodGet, "/v1/placements?node=n1", "")
	if items := decodeBody(x.t, byNode)["items"].([]any); len(items) != 0 {
		x.t.Fatalf("rejected record must not match a node filter: %v", items)
	}
}
