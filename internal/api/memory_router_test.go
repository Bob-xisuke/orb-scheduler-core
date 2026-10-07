package api

// These regressions run the complete HTTP surface against the in-memory
// storage double (internal/service/fakestore): no SQLite file is created
// and the router is wired through the same service.Store contract the
// production store satisfies. They pin accept verdicts (201/200/409),
// the single/list reads (200/404, intersection filters, UTF-8 order),
// 400s that never reach storage, independently injected storage failures
// (submit/get/list/ping -> 503), and the health probe — including that a
// failed probe never blocks placement requests.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

func newMemoryRouter() (*fakestore.Store, http.Handler) {
	fs := &fakestore.Store{}
	return fs, NewRouter(fs)
}

// First submission schedules through the same service rules and is stored.
func TestMemoryCreatePlacementSelectsSmallestEligibleNode(t *testing.T) {
	_, h := newMemoryRouter()
	rec := postPlacement(t, h, validBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "placed" || body["node"] != "node-a" || body["reason"] != nil {
		t.Fatalf("unexpected scheduling decision: %s", rec.Body.String())
	}
	if body["namespace"] != "team-a" || body["queue"] != "default" || body["priority"].(float64) != 10 {
		t.Fatalf("input fields not retained: %s", rec.Body.String())
	}
}

// Identical content retries return 200 and the original record byte for
// byte; omitted defaults are filled the same way on both passes.
func TestMemoryRepeatSubmissionIsIdempotent(t *testing.T) {
	_, h := newMemoryRouter()
	first := postPlacement(t, h, validBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	second := postPlacement(t, h, validBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("repeated submission returned a different record:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

// Object member order is irrelevant and omitted selector/labels take the
// documented empty defaults without changing the content.
func TestMemoryComparisonIgnoresMemberOrderAndFillsDefaults(t *testing.T) {
	_, h := newMemoryRouter()
	first := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"memory": 64, "cpu": 100},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`
	if rec := postPlacement(t, h, first); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, postPlacement(t, h, first)); got["selector"] == nil {
		t.Fatalf("selector default not stored: %s", first)
	}
	second := `{
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}],
	  "resources": {"cpu": 100, "memory": 64},
	  "selector": {},
	  "priority": 1, "queue": "q", "name": "job", "namespace": "ns"
	}`
	if rec := postPlacement(t, h, second); rec.Code != http.StatusOK {
		t.Fatalf("second = %d, want 200 for defaulted/reordered content: %s", rec.Code, rec.Body.String())
	}
}

// The candidate array order is part of the request content.
func TestMemoryComparisonKeepsArrayOrder(t *testing.T) {
	_, h := newMemoryRouter()
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	postPlacement(t, h, body)
	reordered := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	expectErrorCode(t, postPlacement(t, h, reordered), http.StatusConflict, "PlacementConflictError")
}

// Different content on the same identity conflicts and leaves the stored
// record untouched; scheduling-result fields never enter the comparison.
func TestMemoryDifferentContentConflictsAndLeavesRecord(t *testing.T) {
	_, h := newMemoryRouter()
	postPlacement(t, h, validBody)
	different := `{
	  "namespace": "team-a", "name": "job-1", "queue": "default", "priority": 11,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn"}, "nodes": []
	}`
	expectErrorCode(t, postPlacement(t, h, different), http.StatusConflict, "PlacementConflictError")

	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["priority"].(float64) != 10 {
		t.Fatalf("conflicting write replaced the original record: %s", rec.Body.String())
	}
}

// Single-record read hits 200; an unknown identity hits 404 and the
// published error shape.
func TestMemoryGetSingleRecord(t *testing.T) {
	fs, h := newMemoryRouter()
	postPlacement(t, h, validBody)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["name"] != "job-1" {
		t.Fatalf("unexpected record: %s", rec.Body.String())
	}
	if len(fs.Gets) != 1 {
		t.Fatalf("get calls = %d, want 1", len(fs.Gets))
	}

	missing := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", "")
	expectErrorCode(t, missing, http.StatusNotFound, "PlacementNotFoundError")
}

// Listing keeps intersection filters, UTF-8 byte ordering and the
// {"items":[]} empty shape; a node filter never matches a rejected record.
func TestMemoryListFiltersSortsAndEmpties(t *testing.T) {
	_, h := newMemoryRouter()
	submit := func(ns, name, queue, node string) {
		t.Helper()
		body := fmt.Sprintf(`{
		  "namespace": %q, "name": %q, "queue": %q, "priority": 0,
		  "resources": {"cpu": 1, "memory": 1},
		  "nodes": [{"name": %q, "cpu": 10, "memory": 10, "labels": {}}]
		}`, ns, name, queue, node)
		if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
			t.Fatalf("create %s/%s: %d %s", ns, name, rec.Code, rec.Body.String())
		}
	}
	// UTF-8 byte order: 'a' < 'z' < 'ä' < '日'.
	submit("z", "n2", "q1", "host-1")
	submit("a", "n2", "q1", "host-1")
	submit("ä", "n1", "q2", "host-2")
	submit("日", "n1", "q1", "host-1")
	submit("a", "n1", "q2", "host-2")
	// Rejected record: its node is nil, so the node filter must skip it.
	rejected := `{
	  "namespace": "rj", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "host-9", "cpu": 100, "memory": 50, "labels": {}}]
	}`
	if rec := postPlacement(t, h, rejected); rec.Code != http.StatusCreated {
		t.Fatalf("rejected create = %d: %s", rec.Code, rec.Body.String())
	}

	all := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	var order []string
	for _, item := range decodeBody(t, all)["items"].([]any) {
		m := item.(map[string]any)
		order = append(order, m["namespace"].(string)+"/"+m["name"].(string))
	}
	// The rejected record is stored too; it sorts with the rest by
	// identity and is only excluded by the node filter below.
	want := []string{"a/n1", "a/n2", "rj/job", "z/n2", "ä/n1", "日/n1"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}

	checkCount := func(target string, n int) {
		t.Helper()
		rec := doRequest(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
		if len(decodeBody(t, rec)["items"].([]any)) != n {
			t.Fatalf("GET %s items = %d, want %d: %s", target, len(decodeBody(t, rec)["items"].([]any)), n, rec.Body.String())
		}
	}
	checkCount("/v1/placements?queue=q2", 2)
	checkCount("/v1/placements?node=host-2&queue=q2", 2) // intersection
	checkCount("/v1/placements?namespace=a", 2)
	checkCount("/v1/placements?node=host-9", 0) // rejected record excluded
	empty := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if got := decodeBody(t, empty)["items"].([]any); len(got) != 0 {
		t.Fatalf("want empty items, got %v", got)
	}
}

// Invalid request bodies are rejected with 400 before storage is touched.
func TestMemoryInvalidBodiesReachNoStorage(t *testing.T) {
	fs, h := newMemoryRouter()
	for i, body := range []string{
		``,
		`not json`,
		`[]`,
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[],"bogus":1}`,
		`{"name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // missing namespace
		`{"namespace":"ns","name":"job","queue":"q","priority":"1",
		  "resources":{"cpu":0,"memory":64},"nodes":[]}`, // zero cpu
	} {
		rec := postPlacement(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400; body %s", i, rec.Code, rec.Body.String())
			continue
		}
		expectErrorCode(t, rec, http.StatusBadRequest, "InvalidPlacementInputError")
	}
	if len(fs.Submits) != 0 {
		t.Fatalf("invalid bodies reached Submit %d times", len(fs.Submits))
	}
}

// Invalid query strings are rejected with 400 and perform no reads.
func TestMemoryInvalidQueriesReachNoStorage(t *testing.T) {
	fs, h := newMemoryRouter()
	for _, target := range []string{
		"/v1/placements?unknown=1",
		"/v1/placements?namespace=",
		"/v1/placements?namespace=a&namespace=b",
		"/v1/placements?name=x&queue=q",
		"/v1/placements?%zz",
	} {
		expectErrorCode(t, doRequest(t, h, http.MethodGet, target, ""),
			http.StatusBadRequest, "InvalidPlacementInputError")
	}
	if len(fs.Gets) != 0 || len(fs.Lists) != 0 {
		t.Fatalf("invalid queries reached storage: gets=%d lists=%d", len(fs.Gets), len(fs.Lists))
	}
}

// A submit-side failure is its own 503, independent of the other methods,
// and the published message hides the underlying error.
func TestMemorySubmitFailureReturns503(t *testing.T) {
	fs := &fakestore.Store{SubmitErr: errors.New("dial tcp 10.0.0.9:6379: submit disk fault")}
	h := NewRouter(fs)
	rec := postPlacement(t, h, validBody)
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsAny(body, "10.0.0.9", "dial", "disk fault") {
		t.Fatalf("503 body leaks the underlying error: %s", body)
	}
	if len(fs.Submits) != 1 {
		t.Fatalf("submit calls = %d, want 1", len(fs.Submits))
	}
	if fs.Pings != 0 {
		t.Fatalf("placement submit must not ping: pings=%d", fs.Pings)
	}
}

// A get-side failure is its own 503 for the single-record form.
func TestMemoryGetFailureReturns503(t *testing.T) {
	fs := &fakestore.Store{GetErr: errors.New("get row: internal driver crash at /var/lib/x.db")}
	h := NewRouter(fs)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsAny(body, "/var/lib", "driver crash") {
		t.Fatalf("503 body leaks the underlying error: %s", body)
	}
	if len(fs.Gets) != 1 {
		t.Fatalf("get calls = %d, want 1", len(fs.Gets))
	}
	if fs.Pings != 0 {
		t.Fatalf("placement query must not ping: pings=%d", fs.Pings)
	}
}

// A list-side failure is its own 503 for the list form.
func TestMemoryListFailureReturns503(t *testing.T) {
	fs := &fakestore.Store{ListErr: errors.New("scan placements: locked table at secret.db")}
	h := NewRouter(fs)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsAny(body, "secret.db", "locked table") {
		t.Fatalf("503 body leaks the underlying error: %s", body)
	}
	if len(fs.Lists) != 1 {
		t.Fatalf("list calls = %d, want 1", len(fs.Lists))
	}
	if fs.Pings != 0 {
		t.Fatalf("placement list must not ping: pings=%d", fs.Pings)
	}
}

// The health probe answers 200/503 from Ping alone, counts exactly one
// Ping per probe, and a failing probe never blocks accept or query: those
// paths do not call Ping at all.
func TestMemoryHealthzReflectsPingWithoutBlockingPlacements(t *testing.T) {
	fs, h := newMemoryRouter()

	ok := doRequest(t, h, http.MethodGet, "/healthz", "")
	if ok.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200: %s", ok.Code, ok.Body.String())
	}
	if got := ok.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz body = %s", got)
	}

	fs.PingErr = errors.New("ping: refused at db.internal:9999")
	down := doRequest(t, h, http.MethodGet, "/healthz", "")
	expectErrorCode(t, down, http.StatusServiceUnavailable, "storage_unavailable")
	if body := down.Body.String(); containsAny(body, "db.internal", "refused") {
		t.Fatalf("503 body leaks the ping error: %s", body)
	}

	// Probe failure must not pre-empt the placement surface.
	if rec := postPlacement(t, h, validBody); rec.Code != http.StatusCreated {
		t.Fatalf("post under failed ping = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("single get under failed ping = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, h, http.MethodGet, "/v1/placements", ""); rec.Code != http.StatusOK {
		t.Fatalf("list under failed ping = %d: %s", rec.Code, rec.Body.String())
	}

	if fs.Pings != 2 {
		t.Fatalf("ping calls = %d, want exactly 2 (the two healthz probes)", fs.Pings)
	}
	if len(fs.Submits) != 1 || len(fs.Gets) != 1 || len(fs.Lists) != 1 {
		t.Fatalf("placement calls under failed ping: submits=%d gets=%d lists=%d, want 1/1/1",
			len(fs.Submits), len(fs.Gets), len(fs.Lists))
	}

	// Recovery is observed purely through Ping.
	fs.PingErr = nil
	if rec := doRequest(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz after recovery = %d: %s", rec.Code, rec.Body.String())
	}
}

// Unknown paths keep the published route_not_found shape over the double.
func TestMemoryUnknownRouteReturns404(t *testing.T) {
	_, h := newMemoryRouter()
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/missing", ""),
		http.StatusNotFound, "route_not_found")
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
