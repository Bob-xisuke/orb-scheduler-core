package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

func newTestRouter(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewRouter(st)
}

func doRequest(t *testing.T, h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postPlacement(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, "/v1/placements", body)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func expectErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	body := decodeBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object missing: %s", rec.Body.String())
	}
	if got := errObj["code"]; got != code {
		t.Fatalf("code = %v, want %s", got, code)
	}
	if msg, ok := errObj["message"].(string); !ok || msg == "" {
		t.Fatalf("message must be a non-empty string: %s", rec.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("error response must have only the error key: %s", rec.Body.String())
	}
}

const validBody = `{
  "namespace": "team-a",
  "name": "job-1",
  "queue": "default",
  "priority": 10,
  "resources": {"cpu": 500, "memory": 256},
  "selector": {"zone": "cn"},
  "nodes": [
    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn", "ssd": "true"}},
    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}},
    {"name": "node-c", "cpu": 100, "memory": 128, "labels": {"zone": "cn"}},
    {"name": "node-d", "cpu": 1000, "memory": 512, "labels": {"zone": "us"}}
  ]
}`

func TestCreatePlacementSelectsSmallestEligibleNode(t *testing.T) {
	_, h := newTestRouter(t)
	rec := postPlacement(t, h, validBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "placed" {
		t.Fatalf("status = %v, want placed", body["status"])
	}
	if body["node"] != "node-a" {
		t.Fatalf("node = %v, want node-a (byte-smallest eligible; node-c too small, node-d wrong zone)", body["node"])
	}
	if body["reason"] != nil {
		t.Fatalf("reason = %v, want null", body["reason"])
	}
	if body["namespace"] != "team-a" || body["queue"] != "default" || body["priority"].(float64) != 10 {
		t.Fatalf("input fields not retained: %s", rec.Body.String())
	}
}

func TestCreatePlacementRejectsWhenNoEligibleNode(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{
	  "namespace": "team-a", "name": "job-2", "queue": "default", "priority": -1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512, "labels": {}}]
	}`
	rec := postPlacement(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if got["status"] != "rejected" || got["node"] != nil || got["reason"] != "no_eligible_node" {
		t.Fatalf("unexpected decision: %s", rec.Body.String())
	}
}

func TestEmptyNodesAndOmittedSelectorDefaultToRejected(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1}, "nodes": []
	}`
	rec := postPlacement(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if got["status"] != "rejected" || got["reason"] != "no_eligible_node" {
		t.Fatalf("want rejected: %s", rec.Body.String())
	}
}

func TestRepeatSubmissionIsIdempotent(t *testing.T) {
	_, h := newTestRouter(t)
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

func TestComparisonIgnoresMemberOrderAndFillsDefaults(t *testing.T) {
	_, h := newTestRouter(t)
	first := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"memory": 64, "cpu": 100},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`
	// selector omitted vs explicit {}, node labels omitted vs {}, object members reordered.
	second := `{
	  "resources": {"cpu": 100, "memory": 64},
	  "priority": 1, "queue": "q", "name": "job", "namespace": "ns",
	  "selector": {},
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}]
	}`
	if rec := postPlacement(t, h, first); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postPlacement(t, h, second); rec.Code != http.StatusOK {
		t.Fatalf("second = %d, want 200 for defaulted/reordered content: %s", rec.Code, rec.Body.String())
	}
}

func TestComparisonKeepsArrayOrder(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	reordered := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	postPlacement(t, h, body)
	rec := postPlacement(t, h, reordered)
	expectErrorCode(t, rec, http.StatusConflict, "PlacementConflictError")
}

func TestDifferentContentConflictsAndIsNotStored(t *testing.T) {
	_, h := newTestRouter(t)
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
		t.Fatalf("conflicting write must not replace the original record: %s", rec.Body.String())
	}
}

func TestConcurrentIdenticalSubmissionsStoreOneRecord(t *testing.T) {
	_, h := newTestRouter(t)
	const n = 24
	var wg sync.WaitGroup
	statuses := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			statuses[i] = postPlacement(t, h, validBody).Code
		}(i)
	}
	wg.Wait()

	var created, ok int
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			ok++
		default:
			t.Fatalf("unexpected status %d", s)
		}
	}
	if created != 1 || ok != n-1 {
		t.Fatalf("status counts: created=%d ok=%d, want 1 and %d", created, ok, n-1)
	}
	rec := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("stored records = %d, want 1", len(items))
	}
}

func TestGetSingleRecord(t *testing.T) {
	_, h := newTestRouter(t)
	postPlacement(t, h, validBody)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["name"] != "job-1" {
		t.Fatalf("unexpected record: %s", rec.Body.String())
	}
}

func TestGetMissingRecordReturnsNotFound(t *testing.T) {
	_, h := newTestRouter(t)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", "")
	expectErrorCode(t, rec, http.StatusNotFound, "PlacementNotFoundError")
}

func TestGetByNameRequiresNamespaceAlone(t *testing.T) {
	_, h := newTestRouter(t)
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?name=job-1", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1&queue=default", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1&node=n1", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
}

func TestListFiltersAndSortsByNamespaceThenName(t *testing.T) {
	_, h := newTestRouter(t)
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
	// UTF-8 byte order: 'a'(0x61) < 'z'(0x7A) < 'ä'(0xC3..) < '日'(0xE6..).
	submit("z", "n2", "q1", "host-1")
	submit("a", "n2", "q1", "host-1")
	submit("ä", "n1", "q2", "host-2")
	submit("日", "n1", "q1", "host-1")
	submit("a", "n1", "q2", "host-2")

	all := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	var order []string
	for _, item := range decodeBody(t, all)["items"].([]any) {
		m := item.(map[string]any)
		order = append(order, m["namespace"].(string)+"/"+m["name"].(string))
	}
	want := []string{"a/n1", "a/n2", "z/n2", "ä/n1", "日/n1"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}

	byQueue := doRequest(t, h, http.MethodGet, "/v1/placements?queue=q2", "")
	if len(decodeBody(t, byQueue)["items"].([]any)) != 2 {
		t.Fatalf("queue filter: %s", byQueue.Body.String())
	}
	byNode := doRequest(t, h, http.MethodGet, "/v1/placements?node=host-2&queue=q2", "")
	if len(decodeBody(t, byNode)["items"].([]any)) != 2 {
		t.Fatalf("node+queue intersection: %s", byNode.Body.String())
	}
	byNamespace := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a", "")
	if len(decodeBody(t, byNamespace)["items"].([]any)) != 2 {
		t.Fatalf("namespace filter: %s", byNamespace.Body.String())
	}
	empty := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if got := decodeBody(t, empty)["items"].([]any); len(got) != 0 {
		t.Fatalf("want empty items, got %v", got)
	}
}

func TestInvalidQueryParams(t *testing.T) {
	_, h := newTestRouter(t)
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
}

func TestInvalidBodies(t *testing.T) {
	_, h := newTestRouter(t)
	valid := func() string {
		body := map[string]any{
			"namespace": "ns", "name": "job", "queue": "q", "priority": 1,
			"resources": map[string]any{"cpu": 100, "memory": 64},
			"nodes":     []any{map[string]any{"name": "n1", "cpu": 100, "memory": 64, "labels": map[string]any{}}},
		}
		data, _ := json.Marshal(body)
		return string(data)
	}

	rawCases := []string{
		``,
		`not json`,
		`[]`,
		`null`,
		`123`,
		valid() + ` {"x":1}`, // trailing data
		valid() + `tail`,
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[],"unknown":true}`,
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[],"namespace":"ns2"}`, // duplicate key
		`{"name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // missing namespace
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100},"nodes":[]}`, // missing memory
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64,"extra":1},"nodes":[]}`, // unknown resource field
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"n1","cpu":100,"memory":64,"labels":{},"x":1}]}`, // unknown node field
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"n1","cpu":100,"memory":64,"labels":{},"name":"n2"}]}`, // duplicate node key
		`{"namespace":"","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // empty namespace
		`{"namespace":" ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // leading whitespace
		`{"namespace":"ns","name":"job ","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // trailing whitespace
		`{"namespace":"ns","name":"job","queue":"q","priority":1.5,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // fractional priority
		`{"namespace":"ns","name":"job","queue":"q","priority":2147483648,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // int32 overflow
		`{"namespace":"ns","name":"job","queue":"q","priority":-2147483649,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // int32 underflow
		`{"namespace":"ns","name":"job","queue":"q","priority":"1",
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // string priority
		`{"namespace":"ns","name":"job","queue":"q","priority":null,
		  "resources":{"cpu":100,"memory":64},"nodes":[]}`, // null priority
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":0,"memory":64},"nodes":[]}`, // zero cpu
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":-1},"nodes":[]}`, // negative memory
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":9223372036854775808,"memory":1},"nodes":[]}`, // int64 overflow
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":{}}`, // nodes not array
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"nodes":null}`, // null nodes
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"n1","cpu":-1,"memory":64}]}`, // negative node cpu
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"n1","cpu":100,"memory":64},
		           {"name":"n1","cpu":100,"memory":64}]}`, // duplicate node names
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"","cpu":100,"memory":64}]}`, // empty node name
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":123,"cpu":100,"memory":64}]}`, // numeric node name
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},
		  "nodes":[{"name":"n1","cpu":100,"memory":64,"labels":null}]}`, // null labels
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"selector":null,"nodes":[]}`, // null selector
		`{"namespace":"ns","name":"job","queue":"q","priority":1,
		  "resources":{"cpu":100,"memory":64},"selector":{"k":1},"nodes":[]}`, // non-string label value
	}
	for i, body := range rawCases {
		rec := postPlacement(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400; body %s", i, rec.Code, rec.Body.String())
			continue
		}
		expectErrorCode(t, rec, http.StatusBadRequest, "InvalidPlacementInputError")
	}

	// Rejected input must not have been stored.
	list := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns", "")
	if items := decodeBody(t, list)["items"].([]any); len(items) != 0 {
		t.Fatalf("invalid bodies stored %d records", len(items))
	}
}

func TestBoundaryValuesAccepted(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q",
	  "priority": 2147483647,
	  "resources": {"cpu": 9223372036854775807, "memory": 1},
	  "nodes": [{"name": "n", "cpu": 0, "memory": 0, "labels": {}}]
	}`
	if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
		t.Fatalf("max int values rejected: %d %s", rec.Code, rec.Body.String())
	}
	body2 := `{
	  "namespace": "ns2", "name": "job", "queue": "q",
	  "priority": -2147483648,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [{"name": "n", "cpu": 0, "memory": 0}]
	}`
	if rec := postPlacement(t, h, body2); rec.Code != http.StatusCreated {
		t.Fatalf("min int32 priority rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStorageFailureReturns503(t *testing.T) {
	st, h := newTestRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := postPlacement(t, h, validBody)
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if bytes.Contains(rec.Body.Bytes(), []byte("SQLITE")) || bytes.Contains(rec.Body.Bytes(), []byte(".go")) {
		t.Fatalf("503 body leaks internals: %s", rec.Body.String())
	}
}

func TestRecordsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	h := NewRouter(st)
	if rec := postPlacement(t, h, validBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	h2 := NewRouter(st2)
	rec := doRequest(t, h2, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get after reopen = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["status"] != "placed" || got["node"] != "node-a" {
		t.Fatalf("record changed across reopen: %s", rec.Body.String())
	}
}

// A resubmission that differs only textually — members reordered, member
// names written with Unicode escapes, omitted defaults spelled out — carries
// the same content: 200 with the original record, not a new one.
func TestTextualVariantResubmissionReturnsOriginal(t *testing.T) {
	_, h := newTestRouter(t)
	first := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	variant := postPlacement(t, h, `{
	  "nodes": [{"lab\u0065ls": {}, "memory": 128, "cpu": 200, "name": "n"}],
	  "resour\u0063es": {"memory": 64, "cpu": 100},
	  "selector": {},
	  "priority": 1, "queue": "q", "name": "job", "namespac\u0065": "ns"
	}`)
	if variant.Code != http.StatusOK {
		t.Fatalf("variant = %d, want 200: %s", variant.Code, variant.Body.String())
	}
	if variant.Body.String() != first.Body.String() {
		t.Fatalf("variant returned a different record:\n%s\n%s", first.Body.String(), variant.Body.String())
	}
}
