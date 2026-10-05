package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// queryViaService runs the independent business query entry point with the
// same decoded parameters the HTTP layer would hand it.
func queryViaService(t *testing.T, svc *service.Service, values url.Values) service.QueryResult {
	t.Helper()
	result, err := svc.Query(t.Context(), values)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return result
}

func itemsAsMap(t *testing.T, items []*store.Placement) map[string]any {
	t.Helper()
	data, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatalf("marshal items: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal items: %v", err)
	}
	return m
}

func expectSameItemsAsHTTP(t *testing.T, items []*store.Placement, httpBody string) {
	t.Helper()
	var viaHTTP map[string]any
	if err := json.Unmarshal([]byte(httpBody), &viaHTTP); err != nil {
		t.Fatalf("decode http body %q: %v", httpBody, err)
	}
	if direct := itemsAsMap(t, items); !reflect.DeepEqual(direct, viaHTTP) {
		t.Fatalf("lists differ between service entry point and HTTP response:\nservice: %v\nhttp:    %v", direct, viaHTTP)
	}
}

// The single-record form returns the same record through both channels.
func TestCrossCheckQuerySingleRecord(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	if rec := postPlacement(t, h, validBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	result := queryViaService(t, svc, url.Values{"namespace": {"team-a"}, "name": {"job-1"}})
	if result.Record == nil || result.Items != nil {
		t.Fatalf("single success must set only Record: %+v", result)
	}

	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	expectSameRecordAsHTTP(t, result.Record, got.Body.String())
}

// Intersection filters produce the same sorted list through both channels.
func TestCrossCheckQueryIntersectionFilter(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	submit := func(ns, name, queue, node string) {
		t.Helper()
		body := `{
		  "namespace": "` + ns + `", "name": "` + name + `", "queue": "` + queue + `", "priority": 0,
		  "resources": {"cpu": 1, "memory": 1},
		  "nodes": [{"name": "` + node + `", "cpu": 10, "memory": 10, "labels": {}}]
		}`
		if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
			t.Fatalf("create %s/%s: %d %s", ns, name, rec.Code, rec.Body.String())
		}
	}
	submit("a", "n1", "q2", "host-2")
	submit("a", "n2", "q1", "host-1")
	submit("z", "n2", "q1", "host-1")

	values := url.Values{"namespace": {"a"}, "queue": {"q2"}, "node": {"host-2"}}
	result := queryViaService(t, svc, values)
	if result.Record != nil || result.Items == nil {
		t.Fatalf("list success must set only Items: %+v", result)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "n1" {
		t.Fatalf("intersection = %+v", result.Items)
	}

	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=a&queue=q2&node=host-2", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	expectSameItemsAsHTTP(t, result.Items, got.Body.String())
}

// A filter matching nothing is a non-nil empty list in both channels.
func TestCrossCheckQueryEmptyResult(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	if rec := postPlacement(t, h, validBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	result := queryViaService(t, svc, url.Values{"queue": {"nope"}})
	if result.Items == nil || len(result.Items) != 0 {
		t.Fatalf("empty match must be a non-nil empty list: %+v", result)
	}

	got := doRequest(t, h, http.MethodGet, "/v1/placements?queue=nope", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	items := decodeBody(t, got)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("http items = %v, want empty", items)
	}
	expectSameItemsAsHTTP(t, result.Items, got.Body.String())
}

// Every business error branch maps to its documented HTTP error shape.
func TestCrossCheckQueryErrorBranches(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	if rec := postPlacement(t, h, validBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid parameter set: ErrInvalidPlacementQuery <-> 400.
	if _, err := svc.Query(t.Context(), url.Values{"name": {"job-1"}}); !errors.Is(err, service.ErrInvalidPlacementQuery) {
		t.Fatalf("service err = %v, want ErrInvalidPlacementQuery", err)
	}
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?name=job-1", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")

	// Missing identity: ErrPlacementNotFound <-> 404.
	if _, err := svc.Query(t.Context(), url.Values{"namespace": {"team-a"}, "name": {"missing"}}); !errors.Is(err, service.ErrPlacementNotFound) {
		t.Fatalf("service err = %v, want ErrPlacementNotFound", err)
	}
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", ""),
		http.StatusNotFound, "PlacementNotFoundError")

	// Storage failure: ErrStorageUnavailable <-> 503.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := svc.Query(t.Context(), url.Values{"namespace": {"team-a"}}); !errors.Is(err, service.ErrStorageUnavailable) {
		t.Fatalf("service err = %v, want ErrStorageUnavailable", err)
	}
	expectErrorCode(t, doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a", ""),
		http.StatusServiceUnavailable, "storage_unavailable")
}
