package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func doRequest(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeBody(t, recorder)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %q has no error object", recorder.Body.String())
	}
	code, ok := errObj["code"].(string)
	if !ok {
		t.Fatalf("body %q has no error code", recorder.Body.String())
	}
	return code
}

const twoNodePlacement = `{
	"namespace": "team-a",
	"name": "job-1",
	"queue": "batch",
	"priority": 5,
	"resources": {"cpu": 500, "memory": 256},
	"selector": {"zone": "east"},
	"nodes": [
		{"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "east"}},
		{"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "east"}}
	]
}`

func TestCreatePlacementPicksSmallestEligibleNodeName(t *testing.T) {
	router := newTestRouter(t)
	recorder := doRequest(router, http.MethodPost, "/v1/placements", twoNodePlacement)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["status"] != "placed" {
		t.Fatalf("status = %v, want placed", body["status"])
	}
	if body["node"] != "node-a" {
		t.Fatalf("node = %v, want node-a (byte order minimum)", body["node"])
	}
	if body["reason"] != nil {
		t.Fatalf("reason = %v, want null", body["reason"])
	}
}

func TestCreatePlacementRejectsWhenNoNodeFits(t *testing.T) {
	router := newTestRouter(t)
	body := `{
		"namespace": "team-a", "name": "job-big", "queue": "batch", "priority": 0,
		"resources": {"cpu": 500, "memory": 256},
		"nodes": [
			{"name": "node-a", "cpu": 100, "memory": 512},
			{"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "west"}}
		],
		"selector": {"zone": "east"}
	}`
	recorder := doRequest(router, http.MethodPost, "/v1/placements", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	got := decodeBody(t, recorder)
	if got["status"] != "rejected" {
		t.Fatalf("status = %v, want rejected", got["status"])
	}
	if got["node"] != nil {
		t.Fatalf("node = %v, want null", got["node"])
	}
	if got["reason"] != "no_eligible_node" {
		t.Fatalf("reason = %v, want no_eligible_node", got["reason"])
	}
}

func TestResubmitSameContentReturnsOriginalRecord(t *testing.T) {
	router := newTestRouter(t)
	if rec := doRequest(router, http.MethodPost, "/v1/placements", twoNodePlacement); rec.Code != http.StatusCreated {
		t.Fatalf("first submit = %d (%s)", rec.Code, rec.Body.String())
	}
	// Object member order differs, defaults omitted vs explicit: still the same request.
	reordered := `{
		"nodes": [
			{"labels": {"zone": "east"}, "memory": 512, "cpu": 1000, "name": "node-b"},
			{"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "east"}}
		],
		"selector": {"zone": "east"},
		"resources": {"memory": 256, "cpu": 500},
		"priority": 5, "queue": "batch", "name": "job-1", "namespace": "team-a"
	}`
	recorder := doRequest(router, http.MethodPost, "/v1/placements", reordered)
	if recorder.Code != http.StatusOK {
		t.Fatalf("resubmit = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := decodeBody(t, recorder); got["node"] != "node-a" || got["status"] != "placed" {
		t.Fatalf("resubmit returned %v, want the original record", got)
	}
}

func TestResubmitDifferentContentConflicts(t *testing.T) {
	router := newTestRouter(t)
	if rec := doRequest(router, http.MethodPost, "/v1/placements", twoNodePlacement); rec.Code != http.StatusCreated {
		t.Fatalf("first submit = %d (%s)", rec.Code, rec.Body.String())
	}
	changed := strings.Replace(twoNodePlacement, `"priority": 5`, `"priority": 6`, 1)
	recorder := doRequest(router, http.MethodPost, "/v1/placements", changed)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if code := errorCode(t, recorder); code != "PlacementConflictError" {
		t.Fatalf("code = %q, want PlacementConflictError", code)
	}
}

func TestInvalidSubmissionsReturn400AndSaveNothing(t *testing.T) {
	cases := map[string]string{
		"not json":              `{`,
		"empty body":            ``,
		"trailing data":         `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]} extra`,
		"unknown field":         `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"zone":"x"}`,
		"missing namespace":     `{"name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing nodes":         `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1}}`,
		"empty name":            `{"namespace":"a","name":"","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"padded queue":          `{"namespace":"a","name":"b","queue":" q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"float priority":        `{"namespace":"a","name":"b","queue":"q","priority":1.5,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"string priority":       `{"namespace":"a","name":"b","queue":"q","priority":"1","resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"priority out of int32": `{"namespace":"a","name":"b","queue":"q","priority":2147483648,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"zero cpu":              `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":0,"memory":1},"nodes":[]}`,
		"negative memory":       `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":-1},"nodes":[]}`,
		"cpu beyond int64":      `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":9223372036854775808,"memory":1},"nodes":[]}`,
		"resources not object":  `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":[1],"nodes":[]}`,
		"resources unknown key": `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1,"gpu":1},"nodes":[]}`,
		"selector not object":   `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":"x","nodes":[]}`,
		"selector null":         `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":null,"nodes":[]}`,
		"selector non-string":   `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":{"k":1},"nodes":[]}`,
		"nodes not array":       `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":{}}`,
		"node missing cpu":      `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","memory":1}]}`,
		"node negative cpu":     `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":-1,"memory":1}]}`,
		"node padded name":      `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n ","cpu":1,"memory":1}]}`,
		"node unknown field":    `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"arch":"x"}]}`,
		"duplicate node names":  `{"namespace":"a","name":"b","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1},{"name":"n","cpu":2,"memory":2}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := newTestRouter(t)
			recorder := doRequest(router, http.MethodPost, "/v1/placements", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if code := errorCode(t, recorder); code != "InvalidPlacementInputError" {
				t.Fatalf("code = %q, want InvalidPlacementInputError", code)
			}
			list := doRequest(router, http.MethodGet, "/v1/placements", "")
			if got := list.Body.String(); got != `{"items":[]}` {
				t.Fatalf("invalid submission was saved: %s", got)
			}
		})
	}
}

func TestGetPlacementByNameAndNamespace(t *testing.T) {
	router := newTestRouter(t)
	doRequest(router, http.MethodPost, "/v1/placements", twoNodePlacement)

	recorder := doRequest(router, http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := decodeBody(t, recorder); got["name"] != "job-1" || got["status"] != "placed" {
		t.Fatalf("unexpected record %v", got)
	}

	missing := doRequest(router, http.MethodGet, "/v1/placements?namespace=team-a&name=nope", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", missing.Code, http.StatusNotFound)
	}
	if code := errorCode(t, missing); code != "PlacementNotFoundError" {
		t.Fatalf("code = %q, want PlacementNotFoundError", code)
	}
}

func TestListPlacementsFiltersAndSorts(t *testing.T) {
	router := newTestRouter(t)
	submit := func(namespace, name, queue string, nodeNames ...string) {
		t.Helper()
		nodes := ""
		for i, n := range nodeNames {
			if i > 0 {
				nodes += ","
			}
			nodes += fmt.Sprintf(`{"name":%q,"cpu":1000,"memory":512}`, n)
		}
		body := fmt.Sprintf(`{"namespace":%q,"name":%q,"queue":%q,"priority":1,"resources":{"cpu":1,"memory":1},"nodes":[%s]}`,
			namespace, name, queue, nodes)
		if rec := doRequest(router, http.MethodPost, "/v1/placements", body); rec.Code != http.StatusCreated {
			t.Fatalf("submit %s/%s = %d (%s)", namespace, name, rec.Code, rec.Body.String())
		}
	}
	submit("team-b", "job-1", "batch", "node-x")
	submit("team-a", "job-2", "stream", "node-y")
	submit("team-a", "job-1", "batch", "node-x")

	// No filters: everything, sorted by namespace then name in byte order.
	recorder := doRequest(router, http.MethodGet, "/v1/placements", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var keys []string
	for _, item := range list.Items {
		keys = append(keys, item["namespace"].(string)+"/"+item["name"].(string))
	}
	want := []string{"team-a/job-1", "team-a/job-2", "team-b/job-1"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", keys, want)
	}

	// Intersection of filters.
	byQueue := doRequest(router, http.MethodGet, "/v1/placements?queue=stream", "")
	var streamList struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(byQueue.Body.Bytes(), &streamList); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(streamList.Items) != 1 || streamList.Items[0]["name"] != "job-2" {
		t.Fatalf("queue filter = %v", streamList.Items)
	}

	byNode := doRequest(router, http.MethodGet, "/v1/placements?node=node-x", "")
	var nodeList struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(byNode.Body.Bytes(), &nodeList); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodeList.Items) != 2 {
		t.Fatalf("node filter matched %d items, want 2", len(nodeList.Items))
	}

	combined := doRequest(router, http.MethodGet, "/v1/placements?namespace=team-a&queue=batch&node=node-x", "")
	var combinedList struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(combined.Body.Bytes(), &combinedList); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(combinedList.Items) != 1 || combinedList.Items[0]["name"] != "job-1" {
		t.Fatalf("combined filter = %v", combinedList.Items)
	}

	empty := doRequest(router, http.MethodGet, "/v1/placements?namespace=nobody", "")
	if got := empty.Body.String(); got != `{"items":[]}` {
		t.Fatalf("empty list = %s", got)
	}
}

func TestQueryParameterViolationsReturn400(t *testing.T) {
	router := newTestRouter(t)
	paths := []string{
		"/v1/placements?unknown=1",
		"/v1/placements?namespace=a&namespace=b",
		"/v1/placements?namespace=",
		"/v1/placements?queue",
		"/v1/placements?name=job-1",
		"/v1/placements?name=job-1&queue=batch",
		"/v1/placements?name=job-1&namespace=a&node=n",
	}
	for _, path := range paths {
		recorder := doRequest(router, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want %d (%s)", path, recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
		if code := errorCode(t, recorder); code != "InvalidPlacementInputError" {
			t.Fatalf("GET %s code = %q, want InvalidPlacementInputError", path, code)
		}
	}
}

func TestPlacementsSurviveStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if rec := doRequest(NewRouter(st), http.MethodPost, "/v1/placements", twoNodePlacement); rec.Code != http.StatusCreated {
		t.Fatalf("submit = %d (%s)", rec.Code, rec.Body.String())
	}
	st.Close()

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	recorder := doRequest(NewRouter(reopened), http.MethodGet, "/v1/placements?namespace=team-a&name=job-1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := decodeBody(t, recorder); got["node"] != "node-a" {
		t.Fatalf("record after reopen = %v", got)
	}
}

func TestConcurrentResubmitsStoreExactlyOneRecord(t *testing.T) {
	router := newTestRouter(t)
	const workers = 16
	codes := make(chan int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- doRequest(router, http.MethodPost, "/v1/placements", twoNodePlacement).Code
		}()
	}
	wg.Wait()
	close(codes)

	created := 0
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1", created)
	}
	list := doRequest(router, http.MethodGet, "/v1/placements", "")
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("stored %d records, want 1", len(got.Items))
	}
}
