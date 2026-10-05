package store

import (
	"context"
	"path/filepath"
	"testing"
)

func sampleInput() *Placement {
	return &Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  1,
		Resources: Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []Node{
			{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
			{Name: "n2", CPU: 100, Memory: 64, Labels: map[string]string{}},
		},
	}
}

func TestNormalizeInputFillsOnlyOmittedFields(t *testing.T) {
	p := &Placement{
		Nodes: []Node{{Name: "n1"}, {Name: "n2", Labels: map[string]string{"k": "v"}}},
	}
	NormalizeInput(p)
	if p.Selector == nil || len(p.Selector) != 0 {
		t.Fatalf("omitted selector = %v, want empty non-nil object", p.Selector)
	}
	if p.Nodes[0].Labels == nil || len(p.Nodes[0].Labels) != 0 {
		t.Fatalf("omitted labels = %v, want empty non-nil object", p.Nodes[0].Labels)
	}
	if got := p.Nodes[1].Labels["k"]; got != "v" {
		t.Fatalf("explicit labels must be kept, got %v", p.Nodes[1].Labels)
	}

	// Explicit empty objects stay the very maps the caller submitted.
	explicit := map[string]string{}
	q := &Placement{Selector: explicit, Nodes: []Node{}}
	NormalizeInput(q)
	if len(q.Nodes) != 0 || q.Nodes == nil {
		t.Fatalf("explicit empty nodes array changed: %v", q.Nodes)
	}

	// Nil nodes array becomes an empty array, never nil.
	r := &Placement{}
	NormalizeInput(r)
	if r.Nodes == nil || len(r.Nodes) != 0 {
		t.Fatalf("omitted nodes = %v, want empty non-nil array", r.Nodes)
	}

	// Normalizing twice changes nothing.
	NormalizeInput(p)
	if p.Selector == nil || p.Nodes[0].Labels == nil {
		t.Fatalf("second normalize mutated filled defaults: %+v", p)
	}
}

// The canonical encoding is the stored format: existing database files hold
// exactly these bytes, so the field set, JSON names and key order are pinned
// by this test.
func TestCanonicalInputIsStable(t *testing.T) {
	data, err := canonicalInput(sampleInput())
	if err != nil {
		t.Fatalf("canonicalInput: %v", err)
	}
	want := `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
		`"resources":{"cpu":100,"memory":64},"selector":{"zone":"cn"},` +
		`"nodes":[{"name":"n1","cpu":200,"memory":128,"labels":{"zone":"cn"}},` +
		`{"name":"n2","cpu":100,"memory":64,"labels":{}}]}`
	if string(data) != want {
		t.Fatalf("canonical form changed:\n got: %s\nwant: %s", data, want)
	}
}

func TestSameInputRules(t *testing.T) {
	base := sampleInput()

	t.Run("omitted defaults equal explicit empty", func(t *testing.T) {
		explicit := sampleInput()
		explicit.Selector = map[string]string{}
		omitted := sampleInput()
		omitted.Selector = nil
		omitted.Nodes[1].Labels = nil
		same, err := SameInput(omitted, explicit)
		if err != nil || !same {
			t.Fatalf("same=%v err=%v, want equal", same, err)
		}
		// Comparison must not fill defaults into the caller's structs.
		if omitted.Selector != nil || omitted.Nodes[1].Labels != nil {
			t.Fatalf("SameInput mutated its argument: %+v", omitted)
		}
	})

	t.Run("node array order is content", func(t *testing.T) {
		reordered := sampleInput()
		reordered.Nodes = []Node{reordered.Nodes[1], reordered.Nodes[0]}
		same, err := SameInput(base, reordered)
		if err != nil || same {
			t.Fatalf("same=%v err=%v, want different for reordered nodes", same, err)
		}
	})

	t.Run("priority is content", func(t *testing.T) {
		changed := sampleInput()
		changed.Priority++
		same, err := SameInput(base, changed)
		if err != nil || same {
			t.Fatalf("same=%v err=%v, want different for changed priority", same, err)
		}
	})

	t.Run("scheduling results are not content", func(t *testing.T) {
		decided := sampleInput()
		node, reason := "n1", "no_eligible_node"
		decided.Status, decided.Node, decided.Reason = "rejected", &node, &reason
		same, err := SameInput(base, decided)
		if err != nil || !same {
			t.Fatalf("same=%v err=%v, want equal when only status/node/reason differ", same, err)
		}
	})
}

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
