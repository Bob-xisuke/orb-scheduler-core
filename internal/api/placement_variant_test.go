package api

import (
	"net/http"
	"testing"
)

// Equivalent textual forms of one request — omitted defaults, reordered
// object members, explicit empty objects, and \uXXXX-escaped member names —
// must register once (201) and every later equivalent form must return 200
// with the exact original record.
func TestHTTPTextVariantsRegisterOnce(t *testing.T) {
	_, h := newTestRouter(t)

	minimal := `{
  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
  "resources": {"memory": 64, "cpu": 100},
  "nodes": [{"name": "n", "cpu": 200, "memory": 128}]
}`
	explicitReordered := `{
  "resources": {"cpu": 100, "memory": 64},
  "priority": 1, "queue": "q", "name": "job", "namespace": "ns",
  "selector": {},
  "nodes": [{"labels": {}, "memory": 128, "cpu": 200, "name": "n"}]
}`
	escaped := "{\n" +
		"  \"resources\": {\"\\u0063pu\": 100, \"memory\": 64},\n" +
		"  \"\\u0070riority\": 1, \"queue\": \"q\", \"\\u006eame\": \"job\", \"\\u006eamespace\": \"ns\",\n" +
		"  \"\\u0073elector\": {},\n" +
		"  \"nodes\": [{\"\\u006cabels\": {}, \"memory\": 128, \"cpu\": 200, \"\\u006eame\": \"n\"}]\n" +
		"}"

	first := postPlacement(t, h, minimal)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201: %s", first.Code, first.Body.String())
	}
	for name, body := range map[string]string{"explicit reordered": explicitReordered, "escaped names": escaped} {
		rec := postPlacement(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", name, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != first.Body.String() {
			t.Fatalf("%s returned a different record:\n%s\n%s", name, rec.Body.String(), first.Body.String())
		}
	}
}

// Changing only the node array order is a conflict through the HTTP entry,
// even when the second body spells member names with escapes; the stored
// record keeps its original order.
func TestHTTPNodeReorderConflictKeepsOriginal(t *testing.T) {
	_, h := newTestRouter(t)

	body := `{
  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
  "resources": {"cpu": 1, "memory": 1},
  "nodes": [
    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
  ]
}`
	reordered := "{\n" +
		"  \"\\u006eamespace\": \"ns\", \"\\u006eame\": \"job\", \"queue\": \"q\", \"priority\": 1,\n" +
		"  \"resources\": {\"cpu\": 1, \"memory\": 1},\n" +
		"  \"nodes\": [\n" +
		"    {\"\\u006eame\": \"n2\", \"cpu\": 10, \"memory\": 10, \"labels\": {}},\n" +
		"    {\"name\": \"n1\", \"cpu\": 10, \"memory\": 10, \"labels\": {}}\n" +
		"  ]\n" +
		"}"

	first := postPlacement(t, h, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	expectErrorCode(t, postPlacement(t, h, reordered), http.StatusConflict, "PlacementConflictError")

	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", got.Code, got.Body.String())
	}
	nodes := decodeBody(t, got)["nodes"].([]any)
	if len(nodes) != 2 ||
		nodes[0].(map[string]any)["name"] != "n1" || nodes[1].(map[string]any)["name"] != "n2" {
		t.Fatalf("original node order changed: %s", got.Body.String())
	}
}

// Repeated members are rejected through the HTTP entry: 400
// InvalidPlacementInputError, no record is created, and a duplicate carrying
// a different later value can neither create a new identity nor overwrite an
// existing record.
func TestHTTPRepeatedMembersRejectedWithoutWrites(t *testing.T) {
	_, h := newTestRouter(t)

	// Seed one valid record that the later duplicate attempts to overwrite.
	seed := `{
  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
  "resources": {"cpu": 1, "memory": 1}, "nodes": []
}`
	if rec := postPlacement(t, h, seed); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", rec.Code, rec.Body.String())
	}

	duplicates := map[string]string{
		"duplicate top-level plain":  `{"namespace":"ns","namespace":"other","name":"job","queue":"q","priority":2,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"duplicate top-level escape": "{\"\\u006eamespace\":\"ns\",\"namespace\":\"other\",\"name\":\"job\",\"queue\":\"q\",\"priority\":2,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[]}",
		"duplicate priority":         `{"namespace":"ns","name":"job","queue":"q","priority":1,"priority":2,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"duplicate resource field":   `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":1,"cpu":2,"memory":1},"nodes":[]}`,
		"duplicate node field":       `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","name":"m","cpu":1,"memory":1}]}`,
		"duplicate selector key":     `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"selector":{"a":"1","a":"2"},"nodes":[]}`,
		"duplicate label key":        `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":{"k":"1","k":"2"}}]}`,
	}
	for name, body := range duplicates {
		t.Run(name, func(t *testing.T) {
			rec := postPlacement(t, h, body)
			expectErrorCode(t, rec, http.StatusBadRequest, "InvalidPlacementInputError")
		})
	}

	// The later value must not win: the original record is untouched and the
	// alternative identity from the duplicate spelling was never created.
	got := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=ns&name=job", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get original = %d: %s", got.Code, got.Body.String())
	}
	if priority := decodeBody(t, got)["priority"].(float64); priority != 1 {
		t.Fatalf("original record overwritten by duplicate value: %s", got.Body.String())
	}
	missing := doRequest(t, h, http.MethodGet, "/v1/placements?namespace=other&name=job", "")
	expectErrorCode(t, missing, http.StatusNotFound, "PlacementNotFoundError")
}
