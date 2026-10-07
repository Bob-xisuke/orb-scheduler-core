package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

// This suite drives the full HTTP surface against the in-memory
// fakestore double: no SQLite file is opened, yet acceptance, retry,
// conflict, query and health behavior all run through the real router,
// handlers and service. The SQLite-backed cases in placement_test.go and
// router_test.go remain the regression for persistence, unique keys and
// transactions.

func newFakeRouter(t *testing.T) (*fakestore.Store, http.Handler) {
	t.Helper()
	fake := &fakestore.Store{}
	return fake, NewRouter(fake)
}

// place submits one placement whose single node is eligible; cpu only
// needs to fit the requested resources. A cpu above the node capacity
// yields the stored rejection used by the node-filter cases.
func place(t *testing.T, h http.Handler, ns, name, queue, node string, cpu int64) {
	t.Helper()
	body := fmt.Sprintf(`{
	  "namespace": %q, "name": %q, "queue": %q, "priority": 0,
	  "resources": {"cpu": %d, "memory": 1},
	  "nodes": [{"name": %q, "cpu": 1000, "memory": 512, "labels": {}}]
	}`, ns, name, queue, cpu, node)
	if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
		t.Fatalf("create %s/%s: %d %s", ns, name, rec.Code, rec.Body.String())
	}
}

func TestFakeCreateReturns201AndRetryReturns200WithOriginal(t *testing.T) {
	_, h := newFakeRouter(t)
	first := postPlacement(t, h, validBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201: %s", first.Code, first.Body.String())
	}
	second := postPlacement(t, h, validBody)
	if second.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("retry returned a different record:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

func TestFakeConflictReturns409AndKeepsOriginal(t *testing.T) {
	_, h := newFakeRouter(t)
	postPlacement(t, h, validBody)
	different := `{
	  "namespace": "team-a", "name": "job-1", "queue": "default", "priority": 11,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn"}, "nodes": []
	}`
	expectErrorCode(t, postPlacement(t, h, different), http.StatusConflict, "PlacementConflictError")

	// The conflicting write must not have replaced the stored record.
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get after conflict = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["priority"].(float64) != 10 {
		t.Fatalf("original record changed by conflict: %s", rec.Body.String())
	}
}

func TestFakeGetSingleHitAndMiss(t *testing.T) {
	fake, h := newFakeRouter(t)
	postPlacement(t, h, validBody)

	hit := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if hit.Code != http.StatusOK {
		t.Fatalf("hit status = %d: %s", hit.Code, hit.Body.String())
	}
	if got := decodeBody(t, hit); got["name"] != "job-1" {
		t.Fatalf("unexpected record: %s", hit.Body.String())
	}
	if len(fake.Gets) != 1 {
		t.Fatalf("gets = %d, want 1", len(fake.Gets))
	}

	miss := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", "")
	expectErrorCode(t, miss, http.StatusNotFound, "PlacementNotFoundError")
}

func TestFakeListFiltersSortsAndExcludesRejectedByNode(t *testing.T) {
	fake, h := newFakeRouter(t)
	// UTF-8 byte order: 'a' < 'z' < 'ä'; the rejected record carries a
	// nil node and must never satisfy a node filter.
	place(t, h, "z", "n2", "q1", "host-1", 1)
	place(t, h, "a", "n1", "q2", "host-2", 1)
	place(t, h, "a", "n0", "q2", "host-2", 2000) // rejected: no eligible node

	all := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	var order []string
	for _, item := range decodeBody(t, all)["items"].([]any) {
		m := item.(map[string]any)
		order = append(order, m["namespace"].(string)+"/"+m["name"].(string))
	}
	want := []string{"a/n0", "a/n1", "z/n2"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}

	byNamespace := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a", "")
	if len(decodeBody(t, byNamespace)["items"].([]any)) != 2 {
		t.Fatalf("namespace filter: %s", byNamespace.Body.String())
	}
	// Intersection of queue and node keeps only the placed match.
	both := doRequest(t, h, http.MethodGet, "/v1/placements?queue=q2&node=host-2", "")
	if len(decodeBody(t, both)["items"].([]any)) != 1 {
		t.Fatalf("queue+node intersection must skip the rejection: %s", both.Body.String())
	}
	// A node filter never matches the rejected record even on its own.
	byNode := doRequest(t, h, http.MethodGet, "/v1/placements?node=host-2", "")
	if len(decodeBody(t, byNode)["items"].([]any)) != 1 {
		t.Fatalf("node filter must exclude the rejection: %s", byNode.Body.String())
	}
	none := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if got := decodeBody(t, none)["items"].([]any); len(got) != 0 {
		t.Fatalf("want empty items, got %v", got)
	}
	if len(fake.Lists) != 5 {
		t.Fatalf("lists = %d, want 5", len(fake.Lists))
	}
}

func TestFakeEmptyListIsEmptyArray(t *testing.T) {
	_, h := newFakeRouter(t)
	rec := doRequest(t, h, http.MethodGet, "/v1/placements", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"items":[]}` {
		t.Fatalf("empty list body = %s, want {\"items\":[]}", got)
	}
}

func TestFakeInvalidRequestNeverReachesStorage(t *testing.T) {
	fake, h := newFakeRouter(t)

	// Malformed body: 400 before the service, so Submit is never called.
	expectErrorCode(t, postPlacement(t, h, `not json`),
		http.StatusBadRequest, "InvalidPlacementInputError")
	// Malformed query string: 400 before Get or List.
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?name=x&queue=q", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?unknown=1", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")

	if len(fake.Submits) != 0 || len(fake.Gets) != 0 || len(fake.Lists) != 0 {
		t.Fatalf("invalid requests reached storage: submits=%d gets=%d lists=%d",
			len(fake.Submits), len(fake.Gets), len(fake.Lists))
	}
}

func TestFakeStorageFailuresReturn503WithoutLeakingInternals(t *testing.T) {
	root := errors.New("boom disk on fire")

	// Submit failure.
	fake, h := newFakeRouter(t)
	fake.SubmitErr = root
	rec := postPlacement(t, h, validBody)
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsInternal(body) {
		t.Fatalf("submit 503 leaks internals: %s", body)
	}

	// Single-read failure.
	fake, h = newFakeRouter(t)
	fake.GetErr = root
	rec = doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsInternal(body) {
		t.Fatalf("get 503 leaks internals: %s", body)
	}

	// List failure.
	fake, h = newFakeRouter(t)
	fake.ListErr = root
	rec = doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a", "")
	expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
	if body := rec.Body.String(); containsInternal(body) {
		t.Fatalf("list 503 leaks internals: %s", body)
	}
}

func TestFakeHealthzFollowsPingAndNeverBlocksPlacements(t *testing.T) {
	fake, h := newFakeRouter(t)

	ok := doRequest(t, h, http.MethodGet, "/healthz", "")
	if ok.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200: %s", ok.Code, ok.Body.String())
	}
	if got := ok.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz body = %s", got)
	}
	if fake.Pings != 1 {
		t.Fatalf("pings = %d, want 1", fake.Pings)
	}

	// A failing probe only moves healthz to 503; it is not consulted on
	// the placement path, so submissions are accepted unchanged.
	fake.PingErr = errors.New("probe boom")
	down := doRequest(t, h, http.MethodGet, "/healthz", "")
	expectErrorCode(t, down, http.StatusServiceUnavailable, "storage_unavailable")

	pingsBefore := fake.Pings
	created := postPlacement(t, h, validBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("placement must not be blocked by a failed probe: %d %s",
			created.Code, created.Body.String())
	}
	if fake.Pings != pingsBefore {
		t.Fatalf("placement path probed storage: pings %d -> %d", pingsBefore, fake.Pings)
	}
}

func TestFakeUnknownRouteReturns404(t *testing.T) {
	_, h := newFakeRouter(t)
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/nodes", ""),
		http.StatusNotFound, "route_not_found")
}

// containsInternal reports whether a 503 body exposes the injected root
// error text, a source file, or a driver name.
func containsInternal(body string) bool {
	for _, leak := range []string{"boom disk on fire", "probe boom", ".go", "sqlite", "SQLITE"} {
		if strings.Contains(body, leak) {
			return true
		}
	}
	return false
}
