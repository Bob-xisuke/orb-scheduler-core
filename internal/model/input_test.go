package model

import (
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
	data, err := CanonicalInput(sampleInput())
	if err != nil {
		t.Fatalf("CanonicalInput: %v", err)
	}
	want := `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
		`"resources":{"cpu":100,"memory":64},"selector":{"zone":"cn"},` +
		`"nodes":[{"name":"n1","cpu":200,"memory":128,"labels":{"zone":"cn"}},` +
		`{"name":"n2","cpu":100,"memory":64,"labels":{}}]}`
	if string(data) != want {
		t.Fatalf("canonical form changed:\n got: %s\nwant: %s", data, want)
	}
}

// DecodeInput is the read half of the stored format: it must reproduce the
// request fields of any record CanonicalInput wrote, regardless of member
// order in the stored bytes, and must not invent scheduling results.
func TestDecodeInputRoundTrip(t *testing.T) {
	data, err := CanonicalInput(sampleInput())
	if err != nil {
		t.Fatalf("CanonicalInput: %v", err)
	}
	got, err := DecodeInput(data)
	if err != nil {
		t.Fatalf("DecodeInput: %v", err)
	}
	if same, err := SameInput(sampleInput(), got); err != nil || !same {
		t.Fatalf("round trip changed content: same=%v err=%v", same, err)
	}
	if got.Status != "" || got.Node != nil || got.Reason != nil {
		t.Fatalf("DecodeInput filled scheduling results: %+v", got)
	}

	// Member order in the stored bytes is not significant on decode.
	scrambled := `{"nodes":[{"labels":{"zone":"cn"},"memory":128,"cpu":200,"name":"n1"},` +
		`{"labels":{},"memory":64,"cpu":100,"name":"n2"}],` +
		`"selector":{"zone":"cn"},"resources":{"memory":64,"cpu":100},` +
		`"priority":1,"queue":"q","name":"job","namespace":"ns"}`
	got, err = DecodeInput([]byte(scrambled))
	if err != nil {
		t.Fatalf("DecodeInput scrambled: %v", err)
	}
	if same, err := SameInput(sampleInput(), got); err != nil || !same {
		t.Fatalf("scrambled decode changed content: same=%v err=%v", same, err)
	}

	if _, err := DecodeInput([]byte(`{`)); err == nil {
		t.Fatalf("DecodeInput accepted malformed JSON")
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
