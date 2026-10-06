package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// This file is the end-to-end pin of the record-immutability contract over
// the real HTTP surface (SQLite-backed router from newTestRouter): the
// accepted status matrix 201/200/409/400/404/503, byte-for-byte record
// stability across retries/conflicts, the GET filter/sort/empty-array
// semantics, and — via the shared placement.ParsePlacementInput plus
// service.Accept/Query entries — proof that mutating the Go objects a direct
// caller holds cannot change an HTTP response or the committed record.
//
// Every case states its concrete input and deterministic expectation in its
// body and assertions. None of these tests adds or changes a production
// behavior.

// immutRejectedBody asks for 2000 millicores against a 1000-millicore node,
// so the deterministic decision is rejected/null/no_eligible_node. Selector
// and labels are omitted, exercising default rendering in the 201 body.
const immutRejectedBody = `{
  "namespace": "team-a",
  "name": "job-rej",
  "queue": "default",
  "priority": -1,
  "resources": {"cpu": 2000, "memory": 256},
  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512}]
}`

// First acceptance of a placed request returns 201 with the full record; an
// identical retry and a textual-variant retry return 200 with the exact
// original bytes; changed content (including a node-array reorder that keeps
// the same scheduling decision) returns 409 and leaves the original row.
func TestImmutHTTPAcceptedLifecycleMatrix(t *testing.T) {
	_, h := newTestRouter(t)

	// --- First submission: 201, full record, defaults rendered. ---
	first := postPlacement(t, h, validBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201: %s", first.Code, first.Body.String())
	}
	got := decodeBody(t, first)
	if got["namespace"] != "team-a" || got["name"] != "job-1" || got["queue"] != "default" ||
		got["priority"].(float64) != 10 {
		t.Fatalf("ordinary input fields not retained: %s", first.Body.String())
	}
	res := got["resources"].(map[string]any)
	if res["cpu"].(float64) != 500 || res["memory"].(float64) != 256 {
		t.Fatalf("resources not retained: %s", first.Body.String())
	}
	if sel := got["selector"].(map[string]any); len(sel) != 1 || sel["zone"] != "cn" {
		t.Fatalf("selector not retained: %s", first.Body.String())
	}
	nodes := got["nodes"].([]any)
	if len(nodes) != 4 {
		t.Fatalf("nodes = %d, want 4 in submitted order", len(nodes))
	}
	order := []string{
		nodes[0].(map[string]any)["name"].(string),
		nodes[1].(map[string]any)["name"].(string),
		nodes[2].(map[string]any)["name"].(string),
		nodes[3].(map[string]any)["name"].(string),
	}
	wantOrder := []string{"node-b", "node-a", "node-c", "node-d"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Fatalf("node array order = %v, want %v", order, wantOrder)
	}
	if labels := nodes[1].(map[string]any)["labels"].(map[string]any); len(labels) != 1 || labels["zone"] != "cn" {
		t.Fatalf("node labels not retained: %s", first.Body.String())
	}
	if got["status"] != "placed" || got["node"] != "node-a" || got["reason"] != nil {
		t.Fatalf("scheduling result wrong: %s", first.Body.String())
	}
	snapshot := first.Body.String()

	// --- Identical retry: 200, byte-identical original. ---
	retry := postPlacement(t, h, validBody)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	if retry.Body.String() != snapshot {
		t.Fatalf("identical retry changed the record:\n%s\n%s", retry.Body.String(), snapshot)
	}

	// --- Textual variant (shuffled members, explicit {} defaults): still 200
	// with the original bytes; object member order is not content. ---
	variant := postPlacement(t, h, `{
	  "nodes": [
	    {"labels": {"zone": "cn", "ssd": "true"}, "cpu": 1000, "name": "node-b", "memory": 512},
	    {"labels": {"zone": "cn"}, "cpu": 1000, "name": "node-a", "memory": 512},
	    {"labels": {"zone": "cn"}, "cpu": 100, "name": "node-c", "memory": 128},
	    {"labels": {"zone": "us"}, "cpu": 1000, "name": "node-d", "memory": 512}
	  ],
	  "selector": {"zone": "cn"},
	  "resources": {"memory": 256, "cpu": 500},
	  "priority": 10, "queue": "default", "name": "job-1", "namespace": "team-a"
	}`)
	if variant.Code != http.StatusOK {
		t.Fatalf("textual variant status = %d, want 200: %s", variant.Code, variant.Body.String())
	}
	if variant.Body.String() != snapshot {
		t.Fatalf("textual variant returned a different record:\n%s\n%s", variant.Body.String(), snapshot)
	}

	// --- Content conflict: ordinary field change -> 409, original intact. ---
	conflict := postPlacement(t, h, `{
	  "namespace": "team-a", "name": "job-1", "queue": "default", "priority": 11,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn"},
	  "nodes": [
	    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn", "ssd": "true"}},
	    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}},
	    {"name": "node-c", "cpu": 100, "memory": 128, "labels": {"zone": "cn"}},
	    {"name": "node-d", "cpu": 1000, "memory": 512, "labels": {"zone": "us"}}
	  ]
	}`)
	expectErrorCode(t, conflict, http.StatusConflict, "PlacementConflictError")

	// --- Array-order conflict: same decision (node-a wins either way) but
	// different content -> 409; the stored node order must be the original. ---
	reordered := postPlacement(t, h, `{
	  "namespace": "team-a", "name": "job-1", "queue": "default", "priority": 10,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn"},
	  "nodes": [
	    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}},
	    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn", "ssd": "true"}},
	    {"name": "node-c", "cpu": 100, "memory": 128, "labels": {"zone": "cn"}},
	    {"name": "node-d", "cpu": 1000, "memory": 512, "labels": {"zone": "us"}}
	  ]
	}`)
	expectErrorCode(t, reordered, http.StatusConflict, "PlacementConflictError")

	// Original survives in both GET forms; exactly one record exists.
	single := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if single.Code != http.StatusOK || single.Body.String() != snapshot {
		t.Fatalf("single GET after conflicts: %d %s", single.Code, single.Body.String())
	}
	listRec := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	items := decodeBody(t, listRec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("conflicts changed stored record count: %d", len(items))
	}
}

// A rejected record obeys the same lifecycle: 201 with node=null and the
// reason, 200 for an identical retry (spelled with explicit defaults), 409 for
// a placeable resubmission — the new scheduling outcome never replaces stored
// content — and the original rejection reads back unchanged.
func TestImmutHTTPRejectedLifecycleMatrix(t *testing.T) {
	_, h := newTestRouter(t)

	first := postPlacement(t, h, immutRejectedBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201: %s", first.Code, first.Body.String())
	}
	got := decodeBody(t, first)
	if got["status"] != "rejected" || got["node"] != nil || got["reason"] != "no_eligible_node" {
		t.Fatalf("want rejected/null/no_eligible_node: %s", first.Body.String())
	}
	if sel, ok := got["selector"].(map[string]any); !ok || len(sel) != 0 {
		t.Fatalf("omitted selector must render as {}: %s", first.Body.String())
	}
	if labels := got["nodes"].([]any)[0].(map[string]any)["labels"].(map[string]any); len(labels) != 0 {
		t.Fatalf("omitted labels must render as {}: %s", first.Body.String())
	}
	snapshot := first.Body.String()

	// Identical content with defaults spelled out: 200, original bytes.
	retry := postPlacement(t, h, `{
	  "namespace": "team-a", "name": "job-rej", "queue": "default", "priority": -1,
	  "resources": {"cpu": 2000, "memory": 256}, "selector": {},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512, "labels": {}}]
	}`)
	if retry.Code != http.StatusOK || retry.Body.String() != snapshot {
		t.Fatalf("retry = %d, want 200 with original bytes:\n%s\n%s",
			retry.Code, retry.Body.String(), snapshot)
	}

	// Same identity, now placeable: conflict, not a fresh decision.
	placeable := postPlacement(t, h, `{
	  "namespace": "team-a", "name": "job-rej", "queue": "default", "priority": -1,
	  "resources": {"cpu": 100, "memory": 64}, "selector": {},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512, "labels": {}}]
	}`)
	expectErrorCode(t, placeable, http.StatusConflict, "PlacementConflictError")

	single := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-rej", "")
	if single.Code != http.StatusOK || single.Body.String() != snapshot {
		t.Fatalf("rejection changed after conflict: %d %s", single.Code, single.Body.String())
	}

	// A node filter must not surface the rejected record; an unfiltered list
	// still includes it.
	byNode := doRequest(t, h, http.MethodGet, "/v1/placements?node=node-a", "")
	if len(decodeBody(t, byNode)["items"].([]any)) != 0 {
		t.Fatalf("node filter must not match a rejected record: %s", byNode.Body.String())
	}
	all := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a", "")
	if len(decodeBody(t, all)["items"].([]any)) != 1 {
		t.Fatalf("rejected record missing from unfiltered list: %s", all.Body.String())
	}
}

// Representative invalid inputs stay 400 with InvalidPlacementInputError, the
// single-top-level-error shape, and store nothing. The exhaustive parse-level
// matrix lives in internal/api placement_test.go / internal/placement; these
// cases pin the HTTP boundary for the immutability suite.
func TestImmutHTTPInvalidInputMatrix(t *testing.T) {
	_, h := newTestRouter(t)
	cases := map[string]string{
		"malformed json":   `{"namespace":`,
		"trailing data":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]} {}`,
		"unknown field":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"bogus":1}`,
		"duplicate field":  `{"namespace":"ns","namespace":"x","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing required": `{"name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"wrong type":       `{"namespace":"ns","name":"j","queue":"q","priority":"1","resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"out of range":     `{"namespace":"ns","name":"j","queue":"q","priority":2147483648,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"null selector":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":null,"nodes":[]}`,
		"duplicate nodes":  `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1},{"name":"n","cpu":2,"memory":2}]}`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			expectErrorCode(t, postPlacement(t, h, body),
				http.StatusBadRequest, "InvalidPlacementInputError")
		})
	}
	if rec := doRequest(t, h, http.MethodGet, "/v1/placements", ""); rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	} else if items := decodeBody(t, rec)["items"].([]any); len(items) != 0 {
		t.Fatalf("invalid submissions stored %d records", len(items))
	}
}

// Single-record GET for an unknown identity stays 404 with the published error
// shape and stores nothing visible.
func TestImmutHTTPMissingIdentityMatrix(t *testing.T) {
	_, h := newTestRouter(t)
	postPlacement(t, h, validBody)

	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", "")
	expectErrorCode(t, rec, http.StatusNotFound, "PlacementNotFoundError")

	// The one real record is unaffected by the not-found lookup.
	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if got.Code != http.StatusOK || decodeBody(t, got)["priority"].(float64) != 10 {
		t.Fatalf("existing record changed after not-found lookup: %s", got.Body.String())
	}
}

// With the database closed, both POST and GET map the persistence failure to
// 503 storage_unavailable; messages leak no SQL, stack or file paths.
func TestImmutHTTPStorageUnavailableMatrix(t *testing.T) {
	st, h := newTestRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	post := postPlacement(t, h, validBody)
	expectErrorCode(t, post, http.StatusServiceUnavailable, "storage_unavailable")
	if bytes.Contains(post.Body.Bytes(), []byte("SQLITE")) || bytes.Contains(post.Body.Bytes(), []byte(".go")) {
		t.Fatalf("503 body leaks internals: %s", post.Body.String())
	}

	get := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	expectErrorCode(t, get, http.StatusServiceUnavailable, "storage_unavailable")
}

// GET keeps the established list semantics alongside immutability:
// intersection filters, UTF-8 byte ordering by namespace then name, node
// filter limited to placed records, and a literal non-null empty array when
// nothing matches.
func TestImmutHTTPQueryFilterSortEmptyMatrix(t *testing.T) {
	_, h := newTestRouter(t)
	submit := func(ns, name, queue, node string, rejected bool) {
		t.Helper()
		cpuReq, nodeCPU := int64(1), int64(10)
		if rejected {
			cpuReq, nodeCPU = 2000, 10
		}
		body := map[string]any{
			"namespace": ns, "name": name, "queue": queue, "priority": 0,
			"resources": map[string]any{"cpu": cpuReq, "memory": 1},
			"nodes":     []any{map[string]any{"name": node, "cpu": nodeCPU, "memory": 10, "labels": map[string]any{}}},
		}
		data, _ := json.Marshal(body)
		if rec := postPlacement(t, h, string(data)); rec.Code != http.StatusCreated {
			t.Fatalf("create %s/%s: %d %s", ns, name, rec.Code, rec.Body.String())
		}
	}
	// Byte order: a < z < ä. The a/n0 row is rejected; its offered node is
	// host-2 but its scheduled node column is NULL.
	submit("z", "n2", "q1", "host-1", false)
	submit("a", "n2", "q1", "host-1", false)
	submit("ä", "n1", "q2", "host-2", false)
	submit("a", "n0", "q2", "host-2", true)

	all := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	var order []string
	for _, item := range decodeBody(t, all)["items"].([]any) {
		m := item.(map[string]any)
		order = append(order, m["namespace"].(string)+"/"+m["name"].(string))
	}
	want := []string{"a/n0", "a/n2", "z/n2", "ä/n1"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("sort order = %v, want %v", order, want)
	}

	// namespace+queue intersect to the rejected row (node condition absent).
	nsQueue := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a&queue=q2", "")
	if items := decodeBody(t, nsQueue)["items"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["name"] != "n0" {
		t.Fatalf("namespace+queue intersection mismatch: %s", nsQueue.Body.String())
	}

	// node filter matches the scheduled node, never a rejected NULL row, even
	// though the rejected request offered host-2.
	byNode := doRequest(t, h, http.MethodGet, "/v1/placements?node=host-2", "")
	if items := decodeBody(t, byNode)["items"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["namespace"] != "ä" {
		t.Fatalf("node filter must return only the placed host-2 row: %s", byNode.Body.String())
	}

	// All three conditions: the only namespace=a/queue=q2 row is rejected and
	// fails the node condition, so the intersection is empty.
	triple := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a&queue=q2&node=host-2", "")
	if items := decodeBody(t, triple)["items"].([]any); len(items) != 0 {
		t.Fatalf("triple intersection must be empty: %s", triple.Body.String())
	}

	// No match: HTTP body carries an actual empty array, not null/omitted.
	none := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if none.Code != http.StatusOK || none.Body.String() != `{"items":[]}` {
		t.Fatalf("empty result = %q, want %q", none.Body.String(), `{"items":[]}`)
	}
}

// Mutations of the Go objects a direct caller of placement.ParsePlacementInput
// / service.Accept / service.Query holds are invisible over HTTP: the parsed
// input, the accepted record (whose SQLite created path shares reference
// fields with that input — see internal/service/immutability_test.go), and a
// queried record can all be edited in place without changing any later HTTP
// response or the committed record.
func TestImmutDirectObjectMutationsInvisibleOverHTTP(t *testing.T) {
	t.Run("placed", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)
		ctx := t.Context()

		p, err := placement.ParsePlacementInput([]byte(validBody))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		rec, outcome, err := svc.Accept(ctx, p)
		if err != nil || outcome != service.Created {
			t.Fatalf("direct accept: outcome=%d err=%v", outcome, err)
		}
		// Baseline HTTP view before any in-memory tampering.
		snapshot := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
		if snapshot.Code != http.StatusOK {
			t.Fatalf("snapshot GET: %d", snapshot.Code)
		}

		// Poison both objects the direct caller owns: the parsed input and
		// the accepted record, including map entries, array elements and the
		// scheduling pointer target.
		p.Priority = 42
		p.Selector["zone"] = "zz"
		p.Nodes[0].Name = "zz-node"
		p.Nodes[0].Labels["ssd"] = "no"
		*p.Node = "zz-choice"
		rec.Priority = 777
		if rec.Selector != nil {
			rec.Selector["zone"] = "qq"
		}
		rec.Nodes[1].Name = "qq-node"

		// HTTP GET is unchanged...
		got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
		if got.Body.String() != snapshot.Body.String() {
			t.Fatalf("direct-object edits leaked into GET:\n%s\n%s", got.Body.String(), snapshot.Body.String())
		}
		// ...an HTTP retry returns the stored original with 200...
		retry := postPlacement(t, h, validBody)
		if retry.Code != http.StatusOK || retry.Body.String() != snapshot.Body.String() {
			t.Fatalf("retry after object tampering: %d %s", retry.Code, retry.Body.String())
		}
		// ...and a fresh direct Query decodes the committed content.
		values, err := url.ParseQuery("namespace=team-a&name=job-1")
		if err != nil {
			t.Fatalf("parse values: %v", err)
		}
		queried, err := svc.Query(ctx, values)
		if err != nil {
			t.Fatalf("direct query: %v", err)
		}
		expectSameRecordAsHTTP(t, queried.Record, snapshot.Body.String())

		// A queried object is itself disposable: poison it, read again.
		queried.Record.Priority = 555
		queried.Record.Nodes[0].Name = "again-node"
		after := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
		if after.Body.String() != snapshot.Body.String() {
			t.Fatalf("queried-object edits leaked into GET:\n%s\n%s", after.Body.String(), snapshot.Body.String())
		}
	})

	t.Run("rejected", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)
		ctx := t.Context()

		p, err := placement.ParsePlacementInput([]byte(immutRejectedBody))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		rec, outcome, err := svc.Accept(ctx, p)
		if err != nil || outcome != service.Created || rec.Status != service.StatusRejected {
			t.Fatalf("direct accept: outcome=%d status=%q err=%v", outcome, rec.Status, err)
		}
		snapshot := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-rej", "")
		if snapshot.Code != http.StatusOK {
			t.Fatalf("snapshot GET: %d", snapshot.Code)
		}

		// Edit the input, the record, the reason pointer, and even overwrite
		// the decision fields on the returned struct.
		p.Priority = 9
		*p.Reason = "zz-reason"
		rec.Status = service.StatusPlaced
		rec.Node = p.Node // stays nil for a rejection; assigning explicitly
		*rec.Reason = "qq-reason"
		rec.Resources = store.Resources{CPU: 1, Memory: 1}

		got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-rej", "")
		if got.Body.String() != snapshot.Body.String() {
			t.Fatalf("rejected-object edits leaked into GET:\n%s\n%s", got.Body.String(), snapshot.Body.String())
		}
		retry := postPlacement(t, h, immutRejectedBody)
		if retry.Code != http.StatusOK || retry.Body.String() != snapshot.Body.String() {
			t.Fatalf("rejected retry: %d %s", retry.Code, retry.Body.String())
		}
	})
}
