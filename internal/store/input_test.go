package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

// placementFromJSON decodes a placement the same way stored records are
// decoded, so tests can build inputs from raw JSON text with member order and
// escape spellings intact — things a Go literal cannot express.
func placementFromJSON(t *testing.T, text string) *Placement {
	t.Helper()
	var p Placement
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("unmarshal placement: %v", err)
	}
	return &p
}

func mustSameInput(t *testing.T, a, b *Placement) bool {
	t.Helper()
	same, err := SameInput(a, b)
	if err != nil {
		t.Fatalf("SameInput: %v", err)
	}
	return same
}

// Defaults apply to omitted fields only: present maps and slices keep their
// identity and content, and normalization never reorders the node array.
func TestNormalizeInputFillsOnlyOmittedFields(t *testing.T) {
	selector := map[string]string{"zone": "cn"}
	labels := map[string]string{"ssd": "true"}
	nodes := []Node{
		{Name: "n2", CPU: 1, Memory: 1, Labels: labels},
		{Name: "n1", CPU: 1, Memory: 1}, // labels omitted
	}
	p := &Placement{Selector: selector, Nodes: nodes}

	NormalizeInput(p)

	if reflect.ValueOf(p.Selector).Pointer() != reflect.ValueOf(selector).Pointer() {
		t.Fatalf("present selector was replaced: %v", p.Selector)
	}
	if reflect.ValueOf(p.Nodes[0].Labels).Pointer() != reflect.ValueOf(labels).Pointer() {
		t.Fatalf("present labels were replaced: %v", p.Nodes[0].Labels)
	}
	if p.Nodes[1].Labels == nil || len(p.Nodes[1].Labels) != 0 {
		t.Fatalf("omitted labels = %v, want empty non-nil object", p.Nodes[1].Labels)
	}
	if p.Nodes[0].Name != "n2" || p.Nodes[1].Name != "n1" {
		t.Fatalf("node order changed: %+v", p.Nodes)
	}

	bare := &Placement{}
	NormalizeInput(bare)
	if bare.Selector == nil || len(bare.Selector) != 0 {
		t.Fatalf("omitted selector = %v, want empty non-nil object", bare.Selector)
	}
	if bare.Nodes == nil || len(bare.Nodes) != 0 {
		t.Fatalf("omitted nodes = %v, want empty non-nil array", bare.Nodes)
	}

	// Idempotent: a second pass changes nothing.
	again := &Placement{}
	NormalizeInput(again)
	NormalizeInput(again)
	if again.Selector == nil || again.Nodes == nil {
		t.Fatalf("second pass lost defaults: %+v", again)
	}
}

// The comparison rules in one table: omitted defaults equal explicit empty
// objects, object member order and escape spellings are ignored, node array
// order and priority are significant, and the scheduling outcome is not part
// of the content.
func TestSameInputContentRules(t *testing.T) {
	base := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
	  "resources": {"cpu": 100, "memory": 64},
	  "selector": {"zone": "cn", "tier": "edge"},
	  "nodes": [
	    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}},
	    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}}
	  ]
	}`

	cases := []struct {
		name  string
		other string
		want  bool
	}{
		{
			name: "omitted defaults equal explicit empty objects",
			other: `{
			  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
			  "resources": {"cpu": 100, "memory": 64},
			  "selector": {"zone": "cn", "tier": "edge"},
			  "nodes": [
			    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}},
			    {"name": "n2", "cpu": 200, "memory": 128}
			  ]
			}`,
			want: true,
		},
		{
			name: "object member order ignored",
			other: `{
			  "nodes": [
			    {"labels": {"zone": "cn"}, "memory": 128, "cpu": 200, "name": "n1"},
			    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}}
			  ],
			  "selector": {"tier": "edge", "zone": "cn"},
			  "resources": {"memory": 64, "cpu": 100},
			  "priority": 3, "queue": "q", "name": "job", "namespace": "ns"
			}`,
			want: true,
		},
		{
			name: "equivalent escape spellings ignored",
			other: `{
			  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
			  "resources": {"cpu": 100, "memory": 64},
			  "selector": {"zon\u0065": "cn", "tier": "edge"},
			  "nodes": [
			    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}},
			    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}}
			  ]
			}`,
			want: true,
		},
		{
			name: "node array order is significant",
			other: `{
			  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
			  "resources": {"cpu": 100, "memory": 64},
			  "selector": {"zone": "cn", "tier": "edge"},
			  "nodes": [
			    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}},
			    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}}
			  ]
			}`,
			want: false,
		},
		{
			name: "priority is content",
			other: `{
			  "namespace": "ns", "name": "job", "queue": "q", "priority": 4,
			  "resources": {"cpu": 100, "memory": 64},
			  "selector": {"zone": "cn", "tier": "edge"},
			  "nodes": [
			    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}},
			    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}}
			  ]
			}`,
			want: false,
		},
		{
			name: "selector value is content",
			other: `{
			  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
			  "resources": {"cpu": 100, "memory": 64},
			  "selector": {"zone": "us", "tier": "edge"},
			  "nodes": [
			    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}},
			    {"name": "n2", "cpu": 200, "memory": 128, "labels": {}}
			  ]
			}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := placementFromJSON(t, base)
			b := placementFromJSON(t, tc.other)
			if got := mustSameInput(t, a, b); got != tc.want {
				t.Fatalf("SameInput = %v, want %v", got, tc.want)
			}
			// The comparison is symmetric.
			if got := mustSameInput(t, b, a); got != tc.want {
				t.Fatalf("SameInput (reversed) = %v, want %v", got, tc.want)
			}
		})
	}
}

// Status, node and reason are the scheduling outcome, not submitted content:
// two records that differ only there still carry the same content.
func TestSameInputIgnoresSchedulingOutcome(t *testing.T) {
	a := placementFromJSON(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	b := placementFromJSON(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	a.Status, b.Status = "placed", "rejected"
	node := "n1"
	reason := "no_eligible_node"
	a.Node, b.Reason = &node, &reason

	if !mustSameInput(t, a, b) {
		t.Fatalf("scheduling outcome leaked into content comparison")
	}
}

// A record decoded from storage (defaults materialized) compares equal to a
// freshly parsed submission that omitted them — the read path and the write
// path share one definition.
func TestSameInputAcrossStoredAndFreshForms(t *testing.T) {
	fresh := &Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: Resources{CPU: 100, Memory: 64},
		Nodes:     []Node{{Name: "n", CPU: 200, Memory: 128}},
	}
	stored := placementFromJSON(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 100, "memory": 64},
	  "selector": {},
	  "nodes": [{"name": "n", "cpu": 200, "memory": 128, "labels": {}}]
	}`)
	if !mustSameInput(t, fresh, stored) {
		t.Fatalf("fresh submission does not match its stored form")
	}
}
