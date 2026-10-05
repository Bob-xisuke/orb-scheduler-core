package model_test

import (
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

func sampleInput() *model.Placement {
	return &model.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []model.Node{
			{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{"zone": "cn"}},
			{Name: "n2", CPU: 100, Memory: 64, Labels: map[string]string{}},
		},
	}
}

func TestNormalizeInputFillsOnlyOmittedFields(t *testing.T) {
	p := &model.Placement{
		Nodes: []model.Node{{Name: "n1"}, {Name: "n2", Labels: map[string]string{"k": "v"}}},
	}
	model.NormalizeInput(p)
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
	q := &model.Placement{Selector: explicit, Nodes: []model.Node{}}
	model.NormalizeInput(q)
	if len(q.Nodes) != 0 || q.Nodes == nil {
		t.Fatalf("explicit empty nodes array changed: %v", q.Nodes)
	}

	// Nil nodes array becomes an empty array, never nil.
	r := &model.Placement{}
	model.NormalizeInput(r)
	if r.Nodes == nil || len(r.Nodes) != 0 {
		t.Fatalf("omitted nodes = %v, want empty non-nil array", r.Nodes)
	}

	// Normalizing twice changes nothing.
	model.NormalizeInput(p)
	if p.Selector == nil || p.Nodes[0].Labels == nil {
		t.Fatalf("second normalize mutated filled defaults: %+v", p)
	}
}

// NormalizeInput fills defaults but must not reorder the candidate array:
// its order is significant request content.
func TestNormalizeInputDoesNotReorderNodes(t *testing.T) {
	p := &model.Placement{
		Nodes: []model.Node{
			{Name: "z", CPU: 1, Memory: 1},
			{Name: "a", CPU: 1, Memory: 1},
			{Name: "m", CPU: 1, Memory: 1},
		},
	}
	model.NormalizeInput(p)
	got := [3]string{p.Nodes[0].Name, p.Nodes[1].Name, p.Nodes[2].Name}
	want := [3]string{"z", "a", "m"}
	if got != want {
		t.Fatalf("nodes reordered by normalization: got %v want %v", got, want)
	}
}

// The canonical encoding is the stored format: existing database files hold
// exactly these bytes, so the field set, JSON names and key order are pinned
// by this test. It lives in the storage-independent package because new
// writes through any Store implementation must keep producing this format.
func TestEncodeInputIsStable(t *testing.T) {
	data, err := model.EncodeInput(sampleInput())
	if err != nil {
		t.Fatalf("EncodeInput: %v", err)
	}
	want := `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
		`"resources":{"cpu":100,"memory":64},"selector":{"zone":"cn"},` +
		`"nodes":[{"name":"n1","cpu":200,"memory":128,"labels":{"zone":"cn"}},` +
		`{"name":"n2","cpu":100,"memory":64,"labels":{}}]}`
	if string(data) != want {
		t.Fatalf("canonical form changed:\n got: %s\nwant: %s", data, want)
	}
}

// EncodeInput fills defaults on a copy: the canonical bytes of an omitted
// selector/labels form equal the explicit-empty form, but the caller's
// struct is left untouched.
func TestEncodeInputDefaultsWithoutMutatingInput(t *testing.T) {
	omitted := sampleInput()
	omitted.Selector = nil
	omitted.Nodes[0].Labels = nil

	data, err := model.EncodeInput(omitted)
	if err != nil {
		t.Fatalf("EncodeInput: %v", err)
	}
	if string(data) != `{"namespace":"ns","name":"job","queue":"q","priority":1,`+
		`"resources":{"cpu":100,"memory":64},"selector":{},`+
		`"nodes":[{"name":"n1","cpu":200,"memory":128,"labels":{}},`+
		`{"name":"n2","cpu":100,"memory":64,"labels":{}}]}` {
		t.Fatalf("defaults not rendered in canonical bytes: %s", data)
	}
	if omitted.Selector != nil || omitted.Nodes[0].Labels != nil {
		t.Fatalf("EncodeInput mutated its argument: %+v", omitted)
	}
}

// Object member order and Unicode escape spellings normalize away when a
// legacy byte sequence is decoded back into a record: re-encoding it yields
// the canonical bytes.
func TestDecodeRecordAcceptsLegacyMemberOrder(t *testing.T) {
	// Members scrambled relative to the canonical encoding; semantically the
	// same content as sampleInput's first node.
	legacyJSON := `{"nodes":[{"labels":{"zone":"cn"},"memory":128,"cpu":200,"name":"n1"}],` +
		`"selector":{"zone":"cn"},"resources":{"memory":64,"cpu":100},` +
		`"priority":1,"queue":"q","name":"job","namespace":"ns"}`
	node := "n1"
	got, err := model.DecodeRecord(legacyJSON, "placed", &node, nil)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	same, err := model.SameInput(got, sampleInputSecondNodeDropped())
	if err != nil || !same {
		t.Fatalf("legacy bytes no longer compare equal: same=%v err=%v", same, err)
	}
	reencoded, err := model.EncodeInput(got)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	want := `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
		`"resources":{"cpu":100,"memory":64},"selector":{"zone":"cn"},` +
		`"nodes":[{"name":"n1","cpu":200,"memory":128,"labels":{"zone":"cn"}}]}`
	if string(reencoded) != want {
		t.Fatalf("re-encoded legacy record:\n got: %s\nwant: %s", reencoded, want)
	}
	if got.Status != "placed" || got.Node == nil || *got.Node != "n1" || got.Reason != nil {
		t.Fatalf("scheduling columns not restored: %+v", got)
	}

	// A NULL node/reason decode as nil pointers, so rejected records render
	// null on the wire.
	rejected, err := model.DecodeRecord(`{"namespace":"ns","name":"j","queue":"q","priority":0,`+
		`"resources":{"cpu":1,"memory":1},"selector":{},"nodes":[]}`, "rejected", nil, reasonPtr())
	if err != nil {
		t.Fatalf("decode rejected: %v", err)
	}
	if rejected.Node != nil || rejected.Reason == nil || *rejected.Reason != "no_eligible_node" {
		t.Fatalf("null columns decoded wrong: %+v", rejected)
	}
}

func reasonPtr() *string {
	r := "no_eligible_node"
	return &r
}

func sampleInputSecondNodeDropped() *model.Placement {
	p := sampleInput()
	p.Nodes = p.Nodes[:1]
	return p
}

func TestSameInputRules(t *testing.T) {
	base := sampleInput()

	t.Run("omitted defaults equal explicit empty", func(t *testing.T) {
		explicit := sampleInput()
		explicit.Selector = map[string]string{}
		omitted := sampleInput()
		omitted.Selector = nil
		omitted.Nodes[1].Labels = nil
		same, err := model.SameInput(omitted, explicit)
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
		reordered.Nodes = []model.Node{reordered.Nodes[1], reordered.Nodes[0]}
		same, err := model.SameInput(base, reordered)
		if err != nil || same {
			t.Fatalf("same=%v err=%v, want different for reordered nodes", same, err)
		}
	})

	t.Run("priority is content", func(t *testing.T) {
		changed := sampleInput()
		changed.Priority++
		same, err := model.SameInput(base, changed)
		if err != nil || same {
			t.Fatalf("same=%v err=%v, want different for changed priority", same, err)
		}
	})

	t.Run("scheduling results are not content", func(t *testing.T) {
		decided := sampleInput()
		node, reason := "n1", "no_eligible_node"
		decided.Status, decided.Node, decided.Reason = "rejected", &node, &reason
		same, err := model.SameInput(base, decided)
		if err != nil || !same {
			t.Fatalf("same=%v err=%v, want equal when only status/node/reason differ", same, err)
		}
	})
}
