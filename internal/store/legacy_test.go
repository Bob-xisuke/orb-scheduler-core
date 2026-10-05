package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// A row written by a pre-refactor build — same canonical fields, members in
// any order — must still decode and take part in retry comparison.
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

	fresh := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
	}
	same, err := SameInput(fresh, got)
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
	if same, err := SameInput(stored, got); err != nil || !same {
		t.Fatalf("stored record changed by retry: same=%v err=%v", same, err)
	}
	recs, err := st.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
}

// New writes must keep the established stored format: input_json holds
// exactly the shared canonical encoding of the request half, so files written
// before and after the refactor are interchangeable.
func TestSubmitWritesCanonicalEncoding(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "format.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	p := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes:     []Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}}},
		Status:    "placed",
	}
	if _, _, err := st.Submit(context.Background(), p); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var inputJSON string
	if err := st.db.QueryRow(
		"SELECT input_json FROM placements WHERE namespace = ? AND name = ?", "ns", "job",
	).Scan(&inputJSON); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	want, err := model.CanonicalInput(p)
	if err != nil {
		t.Fatalf("canonical input: %v", err)
	}
	if inputJSON != string(want) {
		t.Fatalf("stored encoding changed:\n got: %s\nwant: %s", inputJSON, want)
	}
}
