package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// A row written by a pre-refactor build — same canonical fields, members in
// any order, selector/labels possibly omitted — must still decode in full,
// take part in retry comparison, and never be rewritten. No schema migration
// is involved: the same CREATE TABLE IF NOT EXISTS and the same input_json
// bytes open and read the old file.
func TestLegacyStoredRowStillReadsAndCompares(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Member order scrambled relative to the canonical encoding; semantically
	// the same content as the fresh submission below.
	legacyJSON := `{"nodes":[{"labels":{"zone":"cn"},"memory":128,"cpu":200,"name":"n1"}],` +
		`"selector":{"zone":"cn"},"resources":{"memory":64,"cpu":100},` +
		`"priority":1,"queue":"q","name":"job","namespace":"ns"}`
	if _, err := st.db.Exec(
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		"ns", "job", legacyJSON, "placed", "n1", nil,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	ctx := context.Background()
	got, err := st.Get(ctx, "ns", "job")
	if err != nil {
		t.Fatalf("get legacy record: %v", err)
	}
	if got.Status != "placed" || got.Node == nil || *got.Node != "n1" || got.Queue != "q" {
		t.Fatalf("legacy record decoded wrong: %+v", got)
	}
	if got.Selector["zone"] != "cn" || len(got.Nodes) != 1 || got.Nodes[0].Labels["zone"] != "cn" {
		t.Fatalf("legacy request half decoded wrong: %+v", got)
	}

	fresh := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
	}
	same, err := model.SameInput(fresh, got)
	if err != nil || !same {
		t.Fatalf("legacy record no longer compares equal: same=%v err=%v", same, err)
	}

	// A retry against the legacy identity must hit the unique identity and
	// return the stored record, never insert a second row.
	stored, created, err := st.Submit(ctx, fresh)
	if err != nil {
		t.Fatalf("submit retry: %v", err)
	}
	if created {
		t.Fatalf("retry against legacy identity inserted a new row")
	}
	if same, err := model.SameInput(stored, got); err != nil || !same {
		t.Fatalf("stored record changed by retry: same=%v err=%v", same, err)
	}
	recs, err := st.List(ctx, model.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}

	// A different-content retry conflicts through the unchanged row: the
	// stored input_json bytes stay exactly the legacy bytes.
	different := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 2,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
	}
	again, createdAgain, err := st.Submit(ctx, different)
	if err != nil || createdAgain {
		t.Fatalf("conflicting retry: created=%v err=%v", createdAgain, err)
	}
	if same, err := model.SameInput(again, got); err != nil || !same {
		t.Fatalf("conflict must hand back the legacy record: same=%v err=%v", same, err)
	}
	var raw string
	if err := st.db.QueryRow(
		"SELECT input_json FROM placements WHERE namespace = ? AND name = ?", "ns", "job",
	).Scan(&raw); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if raw != legacyJSON {
		t.Fatalf("legacy bytes rewritten:\n got: %s\nwant: %s", raw, legacyJSON)
	}
}

// An even older row that omitted selector and labels entirely decodes as if
// the defaults were present and compares equal to an explicitly-defaulted
// submission.
func TestLegacyRowWithoutOptionalFields(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "legacy-min.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	legacyJSON := `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
		`"resources":{"cpu":100,"memory":64},` +
		`"nodes":[{"name":"n1","cpu":200,"memory":128}]}`
	if _, err := st.db.Exec(
		"INSERT INTO placements (namespace, name, input_json, status, node, reason) VALUES (?, ?, ?, ?, ?, ?)",
		"ns", "job", legacyJSON, "rejected", nil, "no_eligible_node",
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	got, err := st.Get(context.Background(), "ns", "job")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	explicit := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	same, err := model.SameInput(explicit, got)
	if err != nil || !same {
		t.Fatalf("row missing optional fields must compare equal to defaulted form: same=%v err=%v", same, err)
	}
	if got.Node != nil || got.Reason == nil || *got.Reason != "no_eligible_node" {
		t.Fatalf("rejected columns decoded wrong: %+v", got)
	}
}

// New writes keep the canonical request encoding this release has always put
// on disk: the model package owns those bytes, and the store must not change
// their shape.
func TestNewWriteUsesCanonicalEncoding(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "canonical.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	p := &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []model.Node{
			{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
			{Name: "n2", CPU: 100, Memory: 64, Labels: map[string]string{}},
		},
		Status: "placed",
	}
	if _, created, err := st.Submit(context.Background(), p); err != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, err)
	}
	var raw string
	if err := st.db.QueryRow(
		"SELECT input_json FROM placements WHERE namespace = ? AND name = ?", "ns", "job",
	).Scan(&raw); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	want, err := model.EncodeInput(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if raw != string(want) {
		t.Fatalf("stored encoding drifted from model:\n got: %s\nwant: %s", raw, want)
	}
}
