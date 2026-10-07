package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Transaction visibility over HTTP. The router runs on one Store; a second,
// independently opened handle on the same SQLite file performs writes whose
// transaction boundaries the test controls. This observes the three moments
// — before commit, after commit, after rollback — through the real GET
// endpoints, so every expectation here is a SQLite fact, not a stand-in's.

// listNames decodes a 200 list response and returns the identities it carries.
func listNames(t *testing.T, recBody []byte) []string {
	t.Helper()
	var body struct {
		Items []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recBody, &body); err != nil {
		t.Fatalf("decode list %q: %v", recBody, err)
	}
	names := []string{}
	for _, it := range body.Items {
		names = append(names, it.Namespace+"/"+it.Name)
	}
	return names
}

func TestVisibilityAcrossCommitAndRollbackOverHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "visibility.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := NewRouter(st)

	// A committed seed record through the normal POST path; its 201 body is
	// the reference for "existing records stay unchanged".
	seed := postPlacement(t, h, validBody)
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed post: %d %s", seed.Code, seed.Body.String())
	}
	seedBody := seed.Body.String()

	// The independent writer: its own connection to the same file (the sqlite
	// driver is already registered via internal/store). The router's Store
	// never sees this handle's uncommitted state.
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open independent writer: %v", err)
	}
	t.Cleanup(func() { writer.Close() })

	pendingInput := &model.Placement{
		Namespace: "team-b", Name: "job-pending", Queue: "default", Priority: 3,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []model.Node{
			{Name: "node-z", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
		},
	}
	payload, err := model.CanonicalInput(pendingInput)
	if err != nil {
		t.Fatalf("canonical input: %v", err)
	}
	insert := func(tx *sql.Tx, p *model.Placement, payload string) {
		t.Helper()
		if _, err := tx.Exec(
			"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
			p.Namespace, p.Name, payload, "placed", "node-z", nil,
		); err != nil {
			t.Fatalf("insert %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	getSingle := func(ns, name string) *httptest.ResponseRecorder {
		t.Helper()
		return doRequest(t, h, http.MethodGet, "/v1/placements?namespace="+ns+"&name="+name, "")
	}
	getList := func() *httptest.ResponseRecorder {
		t.Helper()
		return doRequest(t, h, http.MethodGet, "/v1/placements", "")
	}

	// Moment 1 — INSERT done, transaction still open. The single-record query
	// answers 404 PlacementNotFoundError and the list omits the identity,
	// exactly as if the write had never started.
	tx, err := writer.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	insert(tx, pendingInput, string(payload))

	expectErrorCode(t, getSingle("team-b", "job-pending"), http.StatusNotFound, "PlacementNotFoundError")
	if rec := getList(); rec.Code != http.StatusOK ||
		fmt.Sprint(listNames(t, rec.Body.Bytes())) != "[team-a/job-1]" {
		t.Fatalf("before commit: list = %d %s", rec.Code, rec.Body.String())
	}

	// Moment 2 — committed. Both query forms now return the record with the
	// full original input and the scheduling columns as written.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	single := getSingle("team-b", "job-pending")
	if single.Code != http.StatusOK {
		t.Fatalf("after commit: single = %d %s", single.Code, single.Body.String())
	}
	body := decodeBody(t, single)
	if body["queue"] != "default" || body["priority"].(float64) != 3 ||
		body["status"] != "placed" || body["node"] != "node-z" || body["reason"] != nil {
		t.Fatalf("after commit: record incomplete: %s", single.Body.String())
	}
	nodes := body["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["name"] != "node-z" {
		t.Fatalf("after commit: candidate array not preserved: %s", single.Body.String())
	}
	if rec := getList(); rec.Code != http.StatusOK ||
		fmt.Sprint(listNames(t, rec.Body.Bytes())) != "[team-a/job-1 team-b/job-pending]" {
		t.Fatalf("after commit: list = %d %s", rec.Code, rec.Body.String())
	}

	// Moment 3 — a rolled-back insert never becomes visible: the single query
	// keeps answering the same 404, the list keeps excluding the identity,
	// and every committed record is byte-for-byte what it was.
	rolledInput := &model.Placement{
		Namespace: "team-b", Name: "job-rolled-back", Queue: "default", Priority: 4,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{},
	}
	rolledPayload, err := model.CanonicalInput(rolledInput)
	if err != nil {
		t.Fatalf("canonical input: %v", err)
	}
	tx2, err := writer.Begin()
	if err != nil {
		t.Fatalf("begin rollback tx: %v", err)
	}
	insert(tx2, rolledInput, string(rolledPayload))
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	expectErrorCode(t, getSingle("team-b", "job-rolled-back"), http.StatusNotFound, "PlacementNotFoundError")
	if rec := getList(); rec.Code != http.StatusOK ||
		fmt.Sprint(listNames(t, rec.Body.Bytes())) != "[team-a/job-1 team-b/job-pending]" {
		t.Fatalf("after rollback: list = %d %s", rec.Code, rec.Body.String())
	}
	if got := getSingle("team-a", "job-1"); got.Code != http.StatusOK || got.Body.String() != seedBody {
		t.Fatalf("committed seed changed across uncommitted/rolled-back writes:\nseed: %s\n got: %s", seedBody, got.Body.String())
	}
}
