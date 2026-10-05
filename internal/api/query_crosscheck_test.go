package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
)

// queryViaService runs the independent business entry point with the same raw
// query string the HTTP handler receives.
func queryViaService(t *testing.T, svc *service.Service, rawQuery string) *service.QueryResult {
	t.Helper()
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("parse %q: %v", rawQuery, err)
	}
	res, err := svc.Query(t.Context(), values)
	if err != nil {
		t.Fatalf("query %q: %v", rawQuery, err)
	}
	return res
}

// listBody builds a placement request whose single candidate node is nodeName.
// rejected forces the request to exceed the candidate capacity.
func listBody(ns, name, queue, nodeName string, rejected bool) string {
	cpu, mem := int64(1), int64(1)
	if rejected {
		cpu, mem = 2000, 2000
	}
	return fmt.Sprintf(`{
	  "namespace": %q, "name": %q, "queue": %q, "priority": 0,
	  "resources": {"cpu": %d, "memory": %d},
	  "nodes": [{"name": %q, "cpu": 10, "memory": 10, "labels": {}}]
	}`, ns, name, queue, cpu, mem, nodeName)
}

// A single-record GET and service.Query over the same identity must return the
// very same record, including all input fields and the scheduling decision.
func TestCrossCheckSingleRecordHTTPAndServiceAgree(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	postPlacement(t, h, validBody)

	raw := "namespace=team-a&name=job-1"
	httpRec := doRequest(t, h, http.MethodGet, "/v1/placements?"+raw, "")
	if httpRec.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", httpRec.Code, httpRec.Body.String())
	}

	res := queryViaService(t, svc, raw)
	if res.Record == nil || res.Items != nil {
		t.Fatalf("service result shape = %+v, want Record only", res)
	}
	expectSameRecordAsHTTP(t, res.Record, httpRec.Body.String())
}

// List results (order, intersection filters, empty result) must be identical
// whether reached over HTTP or through the independent entry point.
func TestCrossCheckListHTTPAndServiceAgree(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)
	submit := func(body string) {
		t.Helper()
		if rec := postPlacement(t, h, body); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
	}
	submit(listBody("z", "n2", "q1", "host-1", false))
	submit(listBody("a", "n2", "q1", "host-1", false))
	submit(listBody("a", "n1", "q2", "host-2", false))
	// A rejected record: host-x was offered as a candidate but the scheduled
	// node is NULL, so node=host-x must not return it.
	submit(listBody("ns", "rejected", "q", "host-x", true))

	for _, raw := range []string{
		"",
		"queue=q2",
		"namespace=a",
		"node=host-2&queue=q2",
		"queue=nope",
		"node=host-x", // rejected record excluded even though host-x was a candidate
	} {
		t.Run(raw, func(t *testing.T) {
			target := "/v1/placements"
			if raw != "" {
				target += "?" + raw
			}
			httpRec := doRequest(t, h, http.MethodGet, target, "")
			if httpRec.Code != http.StatusOK {
				t.Fatalf("http = %d: %s", httpRec.Code, httpRec.Body.String())
			}
			var httpEnvelope struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(httpRec.Body.Bytes(), &httpEnvelope); err != nil {
				t.Fatalf("decode http: %v", err)
			}

			res := queryViaService(t, svc, raw)
			if res.Record != nil || res.Items == nil {
				t.Fatalf("service shape = %+v, want Items non-nil", res)
			}
			if len(res.Items) != len(httpEnvelope.Items) {
				t.Fatalf("counts differ for %q: http=%d service=%d",
					raw, len(httpEnvelope.Items), len(res.Items))
			}
			for i, rec := range res.Items {
				// Both channels serve the same JSON; compare decoded values so
				// struct-vs-map key ordering is irrelevant.
				if direct := recordAsMap(t, rec); !reflect.DeepEqual(direct, httpEnvelope.Items[i]) {
					t.Fatalf("item %d differs for %q:\nservice: %v\nhttp:    %v",
						i, raw, direct, httpEnvelope.Items[i])
				}
			}

			// The empty case is a present, empty array in both channels.
			if raw == "queue=nope" && (len(res.Items) != 0 || len(httpEnvelope.Items) != 0) {
				t.Fatalf("expected empty result for queue=nope")
			}
		})
	}
}

// NotFound and the validation error classes must match between channels.
func TestCrossCheckQueryErrorsHTTPAndServiceAgree(t *testing.T) {
	st, h := newTestRouter(t)
	svc := service.New(st)

	// Missing identity: service returns ErrPlacementNotFound; HTTP 404.
	values := url.Values{"namespace": {"team-a"}, "name": {"missing"}}
	if _, err := svc.Query(t.Context(), values); !errors.Is(err, service.ErrPlacementNotFound) {
		t.Fatalf("service err = %v, want ErrPlacementNotFound", err)
	}
	expectErrorCode(t,
		doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team-a&name=missing", ""),
		http.StatusNotFound, "PlacementNotFoundError")

	for _, raw := range []string{
		"name=job-1",
		"namespace=a&name=job-1&queue=q",
		"namespace=a&name=job-1&node=n",
		"unknown=1",
		"queue=",
		"namespace=a&namespace=b",
	} {
		parsed, perr := url.ParseQuery(raw)
		if perr != nil {
			t.Fatalf("parse %q: %v", raw, perr)
		}
		if _, err := svc.Query(t.Context(), parsed); !errors.Is(err, service.ErrInvalidPlacementQuery) {
			t.Fatalf("service err for %q = %v, want ErrInvalidPlacementQuery", raw, err)
		}
		expectErrorCode(t,
			doRequest(t, h, http.MethodGet, "/v1/placements?"+raw, ""),
			http.StatusBadRequest, "InvalidPlacementInputError")
	}

	// A malformed query string cannot be handed to the service as url.Values;
	// the transport answers 400 itself.
	expectErrorCode(t,
		doRequest(t, h, http.MethodGet, "/v1/placements?%zz", ""),
		http.StatusBadRequest, "InvalidPlacementInputError")
}

// Storage failures on both query forms map to the published 503 shape and do
// not leak SQL, stack frames or paths.
func TestGetStorageFailureReturns503(t *testing.T) {
	st, h := newTestRouter(t)
	postPlacement(t, h, validBody)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, target := range []string{
		"/v1/placements?namespace=team-a&name=job-1",
		"/v1/placements",
		"/v1/placements?queue=default",
	} {
		rec := doRequest(t, h, http.MethodGet, target, "")
		expectErrorCode(t, rec, http.StatusServiceUnavailable, "storage_unavailable")
		if bytes.Contains(rec.Body.Bytes(), []byte("SQLITE")) ||
			bytes.Contains(rec.Body.Bytes(), []byte(".go")) ||
			bytes.Contains(rec.Body.Bytes(), []byte("/")) {
			t.Fatalf("503 body leaks internals for %s: %s", target, rec.Body.String())
		}
	}
}

// Values decoded from the wire are matched literally: exact percent-decoding
// still hits, while padded whitespace is a legitimate decoded value that
// simply matches nothing rather than being trimmed or re-decoded.
func TestGetMatchesDecodedValuesWithoutTrimming(t *testing.T) {
	_, h := newTestRouter(t)
	postPlacement(t, h, validBody)

	// %2D -> '-': a once-decoded value matches the stored identity exactly.
	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=team%2Da&name=job%2D1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("decoded exact match = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec); got["name"] != "job-1" {
		t.Fatalf("unexpected record: %s", rec.Body.String())
	}

	// "%20default%20" decodes to " default "; it must not be trimmed into a
	// hit, and it is a legal value so the answer is an empty 200 list.
	none := doRequest(t, h, http.MethodGet, "/v1/placements?queue=%20default%20", "")
	if none.Code != http.StatusOK {
		t.Fatalf("padded value status = %d: %s", none.Code, none.Body.String())
	}
	if items := decodeBody(t, none)["items"].([]any); len(items) != 0 {
		t.Fatalf("padded value must not be trimmed into a match: %s", none.Body.String())
	}
}

// Every GET error branch keeps the single error-object envelope.
func TestQueryErrorResponseShape(t *testing.T) {
	_, h := newTestRouter(t)
	cases := []struct {
		target string
		status int
		code   string
	}{
		{"/v1/placements?namespace=a&name=missing", http.StatusNotFound, "PlacementNotFoundError"},
		{"/v1/placements?bogus=1", http.StatusBadRequest, "InvalidPlacementInputError"},
		{"/v1/placements?%zz", http.StatusBadRequest, "InvalidPlacementInputError"},
	}
	for _, tc := range cases {
		expectErrorCode(t, doRequest(t, h, http.MethodGet, tc.target, ""), tc.status, tc.code)
	}
}

// A rejected record written through POST is read back over the single-record
// GET unchanged (node null, the documented reason) and excluded from node
// filtering in the list form.
func TestGetRejectedRecordComplete(t *testing.T) {
	_, h := newTestRouter(t)
	postPlacement(t, h, listBody("ns", "rej", "q", "host-x", true))

	rec := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=rej", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if got["status"] != "rejected" || got["node"] != nil || got["reason"] != "no_eligible_node" {
		t.Fatalf("rejected fields not retained: %s", rec.Body.String())
	}

	byNode := doRequest(t, h, http.MethodGet, "/v1/placements?node=host-x", "")
	if items := decodeBody(t, byNode)["items"].([]any); len(items) != 0 {
		t.Fatalf("node filter must not match the rejected record: %s", byNode.Body.String())
	}
}
