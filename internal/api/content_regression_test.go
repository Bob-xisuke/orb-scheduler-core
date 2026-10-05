package api

import (
	"fmt"
	"net/http"
	"testing"
)

// Regression tests for the unified defaulting/content-comparison rules: every
// acceptance verdict (201/200/409/400) is checked together with what the
// query endpoints actually hold afterwards, never by the scheduling result
// alone.

func listBodies(t *testing.T, h http.Handler, target string) []any {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)["items"].([]any)
}

// Equivalent submissions — omitted vs explicit defaults, reordered members,
// escaped member names — produce one record; the query view agrees.
func TestRegressionEquivalentContentRegistersOnce(t *testing.T) {
	_, h := newTestRouter(t)

	first := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"memory": 64, "cpu": 100},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	// The returned record keeps the full input with defaults rendered.
	if got := decodeBody(t, first); got["selector"] == nil || got["status"] != "placed" {
		t.Fatalf("first record incomplete: %s", first.Body.String())
	}

	variant := postPlacement(t, h, `{
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}],
	  "resources": {"cpu": 100, "memory": 64},
	  "selector": {},
	  "priority": 1, "queue": "q", "name": "job", "namespace": "ns"
	}`)
	if variant.Code != http.StatusOK {
		t.Fatalf("variant = %d, want 200: %s", variant.Code, variant.Body.String())
	}
	if variant.Body.String() != first.Body.String() {
		t.Fatalf("equivalent retry returned a different record:\n%s\n%s", first.Body.String(), variant.Body.String())
	}

	// The query view holds exactly that one record.
	single := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if single.Code != http.StatusOK || single.Body.String() != first.Body.String() {
		t.Fatalf("single query = %d: %s", single.Code, single.Body.String())
	}
	if items := listBodies(t, h, "/v1/placements?namespace=ns"); len(items) != 1 {
		t.Fatalf("list holds %d records, want 1", len(items))
	}
}

// Node reorder and priority change are content conflicts even when the
// scheduling result would be identical; the stored record and list survive.
func TestRegressionContentConflictsLeaveStoreUntouched(t *testing.T) {
	_, h := newTestRouter(t)

	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	first := postPlacement(t, h, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}

	reordered := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	expectErrorCode(t, postPlacement(t, h, reordered), http.StatusConflict, "PlacementConflictError")

	reprioritized := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 2,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	expectErrorCode(t, postPlacement(t, h, reprioritized), http.StatusConflict, "PlacementConflictError")

	// Neither conflict added or rewrote anything.
	single := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if single.Code != http.StatusOK || single.Body.String() != first.Body.String() {
		t.Fatalf("record changed after conflicts: %d %s", single.Code, single.Body.String())
	}
	if items := listBodies(t, h, "/v1/placements"); len(items) != 1 {
		t.Fatalf("list holds %d records, want 1", len(items))
	}
}

// A rejected record obeys the same retry/conflict rules; a retry that would
// now schedule differently is still a content conflict, not a new decision.
func TestRegressionRejectedRecordRetryAndConflict(t *testing.T) {
	_, h := newTestRouter(t)

	rejected := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "n", "cpu": 1000, "memory": 512}]
	}`
	first := postPlacement(t, h, rejected)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	if got := decodeBody(t, first); got["status"] != "rejected" || got["node"] != nil || got["reason"] != "no_eligible_node" {
		t.Fatalf("setup: want rejected record, got %s", first.Body.String())
	}

	// Same content, spelled with explicit defaults and reordered members.
	retry := postPlacement(t, h, `{
	  "selector": {}, "priority": 1, "queue": "q", "name": "job", "namespace": "ns",
	  "resources": {"memory": 256, "cpu": 2000},
	  "nodes": [{"labels": {}, "memory": 512, "cpu": 1000, "name": "n"}]
	}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	if retry.Body.String() != first.Body.String() {
		t.Fatalf("retry returned a different record:\n%s\n%s", retry.Body.String(), first.Body.String())
	}

	// A smaller request on the same identity would be placeable — the
	// scheduling outcome must not replace the content comparison.
	placeable := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n", "cpu": 1000, "memory": 512}]
	}`
	expectErrorCode(t, postPlacement(t, h, placeable), http.StatusConflict, "PlacementConflictError")

	single := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if single.Code != http.StatusOK || single.Body.String() != first.Body.String() {
		t.Fatalf("rejected record changed: %d %s", single.Code, single.Body.String())
	}
	if items := listBodies(t, h, "/v1/placements"); len(items) != 1 {
		t.Fatalf("list holds %d records, want 1", len(items))
	}
}

// Strict-validation failures still produce no record: after a mix of 400s the
// list stays empty, and defaults never rescue an explicit null.
func TestRegressionInvalidInputLeavesNoRecord(t *testing.T) {
	_, h := newTestRouter(t)

	template := `{
	  "namespace": "ns", "name": "job-%d", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128, "labels": {}}]
	}`
	valid := func(i int) string { return fmt.Sprintf(template, i) }

	bodies := []string{
		// Explicit nulls where defaults only apply to omitted fields.
		`{"namespace":"ns","name":"j1","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":null,"nodes":[]}`,
		`{"namespace":"ns","name":"j2","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":null}]}`,
		// Unknown and duplicate fields.
		`{"namespace":"ns","name":"j3","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"bogus":1}`,
		`{"namespace":"ns","name":"j4","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"queue":"q2"}`,
		// Trailing data.
		valid(5) + ` {}`,
		// Type, range, required and node-name-uniqueness failures.
		`{"namespace":"ns","name":"j6","queue":"q","priority":"1","resources":{"cpu":1,"memory":1},"nodes":[]}`,
		`{"namespace":"ns","name":"j7","queue":"q","priority":1,"resources":{"cpu":0,"memory":1},"nodes":[]}`,
		`{"name":"j8","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		`{"namespace":"ns","name":"j9","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1},{"name":"n","cpu":2,"memory":2}]}`,
	}
	for _, body := range bodies {
		expectErrorCode(t, postPlacement(t, h, body), http.StatusBadRequest, "InvalidPlacementInputError")
	}
	if items := listBodies(t, h, "/v1/placements"); len(items) != 0 {
		t.Fatalf("invalid submissions stored %d records", len(items))
	}
}
