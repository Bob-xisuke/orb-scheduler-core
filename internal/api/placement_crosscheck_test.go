package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// acceptViaService parses a request body with the shared strict parser and
// then submits it through the independent business entry point.
func acceptViaService(t *testing.T, svc *service.Service, body string) (*store.Placement, service.Outcome) {
	t.Helper()
	p, err := placement.ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	rec, outcome, err := svc.Accept(t.Context(), p)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return rec, outcome
}

func recordAsMap(t *testing.T, rec *store.Placement) map[string]any {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	return m
}

func expectSameRecordAsHTTP(t *testing.T, rec *store.Placement, httpBody string) {
	t.Helper()
	var viaHTTP map[string]any
	if err := json.Unmarshal([]byte(httpBody), &viaHTTP); err != nil {
		t.Fatalf("decode http body %q: %v", httpBody, err)
	}
	if direct := recordAsMap(t, rec); !reflect.DeepEqual(direct, viaHTTP) {
		t.Fatalf("records differ between service entry point and HTTP response:\nservice: %v\nhttp:    %v", direct, viaHTTP)
	}
}

// expectOutcomeHTTP checks that a business acceptance outcome maps to the
// documented HTTP status for the same submission.
func expectOutcomeHTTP(t *testing.T, outcome service.Outcome, status int) {
	t.Helper()
	want := map[service.Outcome]int{
		service.Created:   http.StatusCreated,
		service.Identical: http.StatusOK,
		service.Conflict:  http.StatusConflict,
	}[outcome]
	if status != want {
		t.Fatalf("outcome %d maps to HTTP %d, got %d", outcome, want, status)
	}
}

// The same legal input must yield one record and matching acceptance results
// regardless of which channel handles the first and second submission.
func TestCrossCheckServiceAndHTTPAgreeOnCreateAndRetry(t *testing.T) {
	t.Run("direct entry point first", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)

		directRec, directOutcome := acceptViaService(t, svc, validBody)
		if directOutcome != service.Created {
			t.Fatalf("first outcome = %d, want Created", directOutcome)
		}
		httpRetry := postPlacement(t, h, validBody)
		if httpRetry.Code != http.StatusOK {
			t.Fatalf("http retry = %d, want 200: %s", httpRetry.Code, httpRetry.Body.String())
		}
		expectOutcomeHTTP(t, service.Identical, httpRetry.Code)
		expectSameRecordAsHTTP(t, directRec, httpRetry.Body.String())
	})

	t.Run("http first", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)

		httpFirst := postPlacement(t, h, validBody)
		if httpFirst.Code != http.StatusCreated {
			t.Fatalf("http first = %d, want 201: %s", httpFirst.Code, httpFirst.Body.String())
		}
		directRec, directOutcome := acceptViaService(t, svc, validBody)
		if directOutcome != service.Identical {
			t.Fatalf("second outcome = %d, want Identical", directOutcome)
		}
		expectOutcomeHTTP(t, directOutcome, http.StatusOK)
		expectSameRecordAsHTTP(t, directRec, httpFirst.Body.String())
	})
}

// Defaults filled on the wire (omitted selector/labels) must be recognized as
// the same content regardless of which channel sees which submission.
func TestCrossCheckDefaultedRetryAcrossChannels(t *testing.T) {
	minimal := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"memory": 64, "cpu": 100},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
	}`
	withDefaults := `{
	  "resources": {"cpu": 100, "memory": 64},
	  "priority": 1, "queue": "q", "name": "job", "namespace": "ns",
	  "selector": {},
	  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}]
	}`

	t.Run("http minimal then direct defaults", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)
		first := postPlacement(t, h, minimal)
		if first.Code != http.StatusCreated {
			t.Fatalf("first = %d: %s", first.Code, first.Body.String())
		}
		rec, outcome := acceptViaService(t, svc, withDefaults)
		if outcome != service.Identical {
			t.Fatalf("outcome = %d, want Identical", outcome)
		}
		expectSameRecordAsHTTP(t, rec, first.Body.String())
	})

	t.Run("direct minimal then http defaults", func(t *testing.T) {
		st, h := newTestRouter(t)
		svc := service.New(st)
		rec, outcome := acceptViaService(t, svc, minimal)
		if outcome != service.Created {
			t.Fatalf("outcome = %d, want Created", outcome)
		}
		second := postPlacement(t, h, withDefaults)
		if second.Code != http.StatusOK {
			t.Fatalf("http retry = %d, want 200: %s", second.Code, second.Body.String())
		}
		expectSameRecordAsHTTP(t, rec, second.Body.String())
	})
}

// Reordering only the node array produces the same scheduling result but is a
// content conflict through both channels, and the original survives in GET.
func TestCrossCheckNodeReorderConflictAcrossChannels(t *testing.T) {
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

	st, h := newTestRouter(t)
	svc := service.New(st)

	first := postPlacement(t, h, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	conflictRec, outcome := acceptViaService(t, svc, reordered)
	if outcome != service.Conflict {
		t.Fatalf("outcome = %d, want Conflict", outcome)
	}
	expectOutcomeHTTP(t, outcome, http.StatusConflict)

	// The independent entry point hands back the untouched original.
	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	expectSameRecordAsHTTP(t, conflictRec, got.Body.String())
	if nodes := decodeBody(t, got)["nodes"].([]any); len(nodes) != 2 ||
		nodes[0].(map[string]any)["name"] != "n1" || nodes[1].(map[string]any)["name"] != "n2" {
		t.Fatalf("original node order changed: %s", got.Body.String())
	}

	// The HTTP channel must reach the same verdict for the same reordering.
	httpConflict := postPlacement(t, h, reordered)
	expectErrorCode(t, httpConflict, http.StatusConflict, "PlacementConflictError")
}

// A rejected decision produced through the independent entry point is stored
// and reads back through HTTP unchanged, including null node and the reason.
func TestCrossCheckRejectedRecordQueryableOverHTTP(t *testing.T) {
	body := `{
	  "namespace": "team-a", "name": "job-2", "queue": "default", "priority": -1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512}]
	}`
	st, h := newTestRouter(t)
	svc := service.New(st)

	rec, outcome := acceptViaService(t, svc, body)
	if outcome != service.Created {
		t.Fatalf("outcome = %d, want Created", outcome)
	}
	if rec.Status != "rejected" || rec.Node != nil || rec.Reason == nil || *rec.Reason != "no_eligible_node" {
		t.Fatalf("direct decision = %+v", rec)
	}

	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-2", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	expectSameRecordAsHTTP(t, rec, got.Body.String())

	// Retrying the same rejected input over HTTP is the idempotent 200 case,
	// not a fresh rejection decision.
	retry := postPlacement(t, h, body)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	expectSameRecordAsHTTP(t, rec, retry.Body.String())
}
