package api

import (
	"fmt"
	"net/http"
	"testing"
)

// End-to-end boundary traces over the public HTTP surface: each test walks a
// legal request through parsing, default filling, trial scheduling, storage
// and re-query, then checks that the published statuses and error codes are
// unchanged and that no client-side or in-flight mutation can rewrite a
// committed record. The in-memory object relationships behind these
// responses are pinned in internal/service/accept_boundary*_test.go; see
// docs/record-boundary.md for the source walkthrough.

// A placed record: 201 on first submission, 200 with the original record on
// an identical retry, 409 on conflicting content, and the queried record
// never changes — including after the client edits its own copy of an
// earlier response.
func TestBoundaryTracePlacedRecordLifecycle(t *testing.T) {
	_, h := newTestRouter(t)

	// First submission: 201 with the full record, defaults rendered.
	first := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201: %s", first.Code, first.Body.String())
	}
	if got := decodeBody(t, first); got["status"] != "placed" || got["node"] != "n1" ||
		got["selector"] == nil || got["reason"] != nil {
		t.Fatalf("first record incomplete: %s", first.Body.String())
	}

	// Identical retry: 200 with byte-identical body.
	retry := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	if retry.Body.String() != first.Body.String() {
		t.Fatalf("identical retry returned a different record:\n%s\n%s",
			first.Body.String(), retry.Body.String())
	}

	// Conflicting content: 409 PlacementConflictError.
	conflict := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 2,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	expectErrorCode(t, conflict, http.StatusConflict, "PlacementConflictError")

	// The stored record is still the original.
	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	if rec := decodeBody(t, got); rec["priority"].(float64) != 1 || rec["node"] != "n1" {
		t.Fatalf("conflict rewrote the record: %s", got.Body.String())
	}

	// Editing a previously returned response body client-side has no effect:
	// the next query still serves the committed record.
	forged := decodeBody(t, first)
	forged["node"] = "forged-node"
	if rec := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")); rec["node"] != "n1" {
		t.Fatalf("record changed after a client-side edit: %v", rec["node"])
	}
}

// A rejected record is stored and queried like any other: 201 with the
// rejection, 200 on identical retry, 409 on a placeable retry, and a node
// filter never matches it.
func TestBoundaryTraceRejectedRecordLifecycle(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128, "labels": {}}]
	}`
	first := postPlacement(t, h, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201: %s", first.Code, first.Body.String())
	}
	if got := decodeBody(t, first); got["status"] != "rejected" || got["node"] != nil ||
		got["reason"] != "no_eligible_node" {
		t.Fatalf("want a stored rejection: %s", first.Body.String())
	}

	retry := postPlacement(t, h, body)
	if retry.Code != http.StatusOK || retry.Body.String() != first.Body.String() {
		t.Fatalf("identical retry of a rejection = %d: %s", retry.Code, retry.Body.String())
	}

	// A placeable submission on the same identity conflicts and changes
	// nothing.
	placeable := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128, "labels": {}}]
	}`)
	expectErrorCode(t, placeable, http.StatusConflict, "PlacementConflictError")
	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if rec := decodeBody(t, got); rec["status"] != "rejected" || rec["reason"] != "no_eligible_node" {
		t.Fatalf("conflict rewrote the rejection: %s", got.Body.String())
	}

	// The rejected record appears in unfiltered lists but never under a node
	// filter.
	if items := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns", ""))["items"].([]any); len(items) != 1 {
		t.Fatalf("rejected record missing from the list: %v", items)
	}
	if items := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?node=n1", ""))["items"].([]any); len(items) != 0 {
		t.Fatalf("node filter matched a rejected record: %v", items)
	}
}

// Object member order never matters for a retry, but the candidate node
// array order is content: reordering it conflicts even when the scheduling
// result would be identical.
func TestBoundaryTraceMemberOrderRetryVsNodeOrderConflict(t *testing.T) {
	_, h := newTestRouter(t)
	first := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10},
	    {"name": "n2", "cpu": 10, "memory": 10}
	  ]
	}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}

	// Same content with members reordered, escaped member names and defaults
	// spelled out: 200 with the original record.
	reordered := postPlacement(t, h, `{
	  "nodes": [{"labels": {}, "memory": 10, "cpu": 10, "name": "n1"},
	            {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}],
	  "resources": {"memory": 1, "cpu": 1},
	  "selector": {},
	  "priority": 1, "queue": "q", "name": "job", "namespace": "ns"
	}`)
	if reordered.Code != http.StatusOK {
		t.Fatalf("member-reordered retry = %d, want 200: %s", reordered.Code, reordered.Body.String())
	}
	if reordered.Body.String() != first.Body.String() {
		t.Fatalf("member-reordered retry returned a different record:\n%s\n%s",
			first.Body.String(), reordered.Body.String())
	}

	// Same members, node array swapped: 409, and the stored order is the
	// original one.
	swapped := postPlacement(t, h, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`)
	expectErrorCode(t, swapped, http.StatusConflict, "PlacementConflictError")
	got := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", ""))
	nodes := got["nodes"].([]any)
	if len(nodes) != 2 || nodes[0].(map[string]any)["name"] != "n1" || nodes[1].(map[string]any)["name"] != "n2" {
		t.Fatalf("stored node order changed: %v", nodes)
	}
}

// The published error codes are unchanged: 400 InvalidPlacementInputError
// for bad bodies and bad query parameters, 404 PlacementNotFoundError for an
// unknown identity, and invalid input leaves nothing behind.
func TestBoundaryErrorCodesUnchanged(t *testing.T) {
	_, h := newTestRouter(t)

	expectErrorCode(t, postPlacement(t, h, `{"namespace":"ns"}`),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, postPlacement(t, h, `not json`),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?bogus=1", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=missing", ""),
		http.StatusNotFound, "PlacementNotFoundError")

	// The rejected requests stored nothing.
	if items := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements", ""))["items"].([]any); len(items) != 0 {
		t.Fatalf("invalid requests left records behind: %v", items)
	}
}

// With the store closed, both endpoints answer 503 storage_unavailable in
// the published error shape.
func TestBoundaryStorageUnavailableReturns503(t *testing.T) {
	st, h := newTestRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	expectErrorCode(t, postPlacement(t, h, validBody),
		http.StatusServiceUnavailable, "storage_unavailable")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", ""),
		http.StatusServiceUnavailable, "storage_unavailable")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements", ""),
		http.StatusServiceUnavailable, "storage_unavailable")
}

// Query semantics are preserved: filters intersect, results sort by
// namespace then name in UTF-8 byte order, and no match is an empty array.
func TestBoundaryQuerySemanticsPreserved(t *testing.T) {
	_, h := newTestRouter(t)
	submit := func(ns, name, queue, nodeName string, cpu int64) {
		t.Helper()
		body := fmt.Sprintf(`{
		  "namespace": %q, "name": %q, "queue": %q, "priority": 0,
		  "resources": {"cpu": 1, "memory": 1},
		  "nodes": [{"name": %q, "cpu": %d, "memory": 10, "labels": {}}]
		}`, ns, name, queue, nodeName, cpu)
		if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
			t.Fatalf("create %s/%s: %d %s", ns, name, rec.Code, rec.Body.String())
		}
	}
	submit("b", "j2", "q1", "host-1", 10)
	submit("a", "j2", "q1", "host-1", 10)
	submit("a", "j1", "q2", "host-2", 10)
	submit("a", "j0", "q2", "host-3", 0) // rejected: cpu 0 < requested 1

	// Sorted by namespace then name, rejected records included.
	all := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements", ""))["items"].([]any)
	want := []string{"a/j0", "a/j1", "a/j2", "b/j2"}
	if len(all) != len(want) {
		t.Fatalf("list all = %d items, want %d", len(all), len(want))
	}
	for i, item := range all {
		m := item.(map[string]any)
		if got := m["namespace"].(string) + "/" + m["name"].(string); got != want[i] {
			t.Fatalf("order[%d] = %s, want %s", i, got, want[i])
		}
	}

	// Filters intersect.
	byQueue := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a&queue=q1", ""))["items"].([]any)
	if len(byQueue) != 1 || byQueue[0].(map[string]any)["name"] != "j2" {
		t.Fatalf("intersection filter: %v", byQueue)
	}

	// A node filter never matches the rejected record.
	byNode := decodeBody(t, doRequest(t, h, http.MethodGet, "/v1/placements?node=host-1", ""))["items"].([]any)
	if len(byNode) != 2 {
		t.Fatalf("node filter = %d items, want 2 placed records", len(byNode))
	}

	// No match is an empty array, never null.
	empty := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if got := decodeBody(t, empty)["items"].([]any); len(got) != 0 {
		t.Fatalf("want empty items, got %v", got)
	}
}
