package placement_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// parseOK asserts the body parses and returns the input object.
func parseOK(t *testing.T, body string) *store.Placement {
	t.Helper()
	p, err := placement.ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("ParsePlacementInput(%s) = %v, want success", body, err)
	}
	if p == nil {
		t.Fatalf("ParsePlacementInput(%s) returned nil placement with nil error", body)
	}
	return p
}

// parseErr asserts the body is rejected with the shared sentinel and no
// partial input object.
func parseErr(t *testing.T, body string) {
	t.Helper()
	p, err := placement.ParsePlacementInput([]byte(body))
	if !errors.Is(err, placement.ErrInvalidPlacementInput) {
		t.Fatalf("ParsePlacementInput(%s) error = %v, want ErrInvalidPlacementInput", body, err)
	}
	if p != nil {
		t.Fatalf("ParsePlacementInput(%s) returned partial input %+v on error", body, p)
	}
}

func TestParsePreservesAllFieldsAndNodeOrder(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "team-a",
	  "name": "job-1",
	  "queue": "default",
	  "priority": -10,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn", "": ""},
	  "nodes": [
	    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}},
	    {"name": "node-a", "cpu": 0, "memory": 0, "labels": {"": ""}}
	  ]
	}`)

	want := &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "default",
		Priority:  -10,
		Resources: store.Resources{CPU: 500, Memory: 256},
		Selector:  map[string]string{"zone": "cn", "": ""},
		Nodes: []store.Node{
			{Name: "node-b", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn"}},
			{Name: "node-a", CPU: 0, Memory: 0, Labels: map[string]string{"": ""}},
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("parsed = %+v, want %+v", p, want)
	}
}

func TestParseFillsDefaults(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [{"name": "n", "cpu": 0, "memory": 0}]
	}`)
	if p.Selector == nil || len(p.Selector) != 0 {
		t.Fatalf("omitted selector = %v, want empty non-nil object", p.Selector)
	}
	if p.Nodes[0].Labels == nil || len(p.Nodes[0].Labels) != 0 {
		t.Fatalf("omitted labels = %v, want empty non-nil object", p.Nodes[0].Labels)
	}
}

func TestParseEmptyNodesStaysEmptyArray(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1}, "nodes": []
	}`)
	if p.Nodes == nil || len(p.Nodes) != 0 {
		t.Fatalf("nodes = %v, want empty non-nil array", p.Nodes)
	}
}

func TestParseAllowsSurroundingWhitespace(t *testing.T) {
	p := parseOK(t, "  \n{\"namespace\":\"ns\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[]}\t\n ")
	if p.Namespace != "ns" || p.Name != "j" {
		t.Fatalf("parsed = %+v", p)
	}
}

func TestParseDoesNotTrimIdentifiers(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "a b", "name": "job", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1}, "nodes": []
	}`)
	if p.Namespace != "a b" {
		t.Fatalf("namespace = %q, interior whitespace must be preserved", p.Namespace)
	}
}

func TestParseIntegerBoundaries(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "ns", "name": "job", "queue": "q",
	  "priority": -2147483648,
	  "resources": {"cpu": 9223372036854775807, "memory": 1},
	  "nodes": []
	}`)
	if p.Priority != math.MinInt32 {
		t.Fatalf("priority = %d, want %d", p.Priority, int64(math.MinInt32))
	}
	if p.Resources.CPU != math.MaxInt64 {
		t.Fatalf("cpu = %d, want %d", p.Resources.CPU, int64(math.MaxInt64))
	}

	p = parseOK(t, `{
	  "namespace": "ns", "name": "job", "queue": "q",
	  "priority": 2147483647,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": []
	}`)
	if p.Priority != math.MaxInt32 {
		t.Fatalf("priority = %d, want %d", p.Priority, int64(math.MaxInt32))
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		// Malformed JSON and wrong root shape.
		"empty body":       ``,
		"malformed json":   `{"namespace":`,
		"root array":       `[]`,
		"root string":      `"x"`,
		"root number":      `5`,
		"root null":        `null`,
		"trailing object":  `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]} {}`,
		"trailing garbage": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]} x`,

		// Unknown and duplicate members.
		"unknown top-level field":   `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"extra":1}`,
		"unknown resource field":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1,"gpu":1},"nodes":[]}`,
		"unknown node field":        `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"zone":"a"}]}`,
		"duplicate top-level field": `{"namespace":"ns","namespace":"x","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"duplicate escaped field":   `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"n\u0061me":"z"}`, // "n\u0061me" unescapes to "name"
		"duplicate resource field":  `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"cpu":2,"memory":1},"nodes":[]}`,
		"duplicate node field":      `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","name":"m","cpu":1,"memory":1}]}`,
		"duplicate selector key":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"selector":{"a":"1","a":"2"}}`,
		"duplicate label key":       `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":{"k":"1","k":"2"}}]}`,

		// Missing required fields.
		"missing namespace":       `{"name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing name":            `{"namespace":"ns","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing queue":           `{"namespace":"ns","name":"j","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing priority":        `{"namespace":"ns","name":"j","queue":"q","resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"missing resources":       `{"namespace":"ns","name":"j","queue":"q","priority":1,"nodes":[]}`,
		"missing nodes":           `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1}}`,
		"missing resource cpu":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"memory":1},"nodes":[]}`,
		"missing resource memory": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1},"nodes":[]}`,
		"missing node name":       `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"cpu":1,"memory":1}]}`,
		"missing node cpu":        `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","memory":1}]}`,
		"missing node memory":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1}]}`,

		// Explicit nulls.
		"null namespace": `{"namespace":null,"name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"null priority":  `{"namespace":"ns","name":"j","queue":"q","priority":null,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"null resources": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":null,"nodes":[]}`,
		"null selector":  `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"selector":null}`,
		"null nodes":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":null}`,
		"null labels":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":null}]}`,
		"null label val": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":{"k":null}}]}`,

		// Type errors.
		"priority string":    `{"namespace":"ns","name":"j","queue":"q","priority":"1","resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"namespace number":   `{"namespace":1,"name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"resources array":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":[],"nodes":[]}`,
		"nodes object":       `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":{}}`,
		"node not object":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[1]}`,
		"node name number":   `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":1,"cpu":1,"memory":1}]}`,
		"selector value num": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"selector":{"k":1}}`,
		"label value bool":   `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":{"k":true}}]}`,

		// Identifier rules: empty or edge-whitespace strings.
		"empty namespace":         `{"namespace":"","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"leading space name":      `{"namespace":"ns","name":" j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"trailing space queue":    `{"namespace":"ns","name":"j","queue":"q ","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"unicode whitespace name": `{"namespace":"ns","name":"\u00a0j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"empty node name":         `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"","cpu":1,"memory":1}]}`,
		"tab-padded node name":    `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"\tn","cpu":1,"memory":1}]}`,

		// Integer rules: decimal literals only, in range.
		"priority fraction":     `{"namespace":"ns","name":"j","queue":"q","priority":1.5,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"priority trailing .0":  `{"namespace":"ns","name":"j","queue":"q","priority":1.0,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"priority exponent":     `{"namespace":"ns","name":"j","queue":"q","priority":1e2,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"priority overflow":     `{"namespace":"ns","name":"j","queue":"q","priority":2147483648,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"priority underflow":    `{"namespace":"ns","name":"j","queue":"q","priority":-2147483649,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"resource cpu zero":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":0,"memory":1},"nodes":[]}`,
		"resource cpu negative": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":-1,"memory":1},"nodes":[]}`,
		"resource mem zero":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":0},"nodes":[]}`,
		"resource overflow":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":9223372036854775808,"memory":1},"nodes":[]}`,
		"resource fraction":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1.5,"memory":1},"nodes":[]}`,
		"node cpu negative":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":-1,"memory":1}]}`,
		"node memory negative":  `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":-1}]}`,
		"node cpu overflow":     `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":9223372036854775808,"memory":1}]}`,

		// Duplicate node names.
		"duplicate node names": `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1},{"name":"n","cpu":2,"memory":2}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			parseErr(t, body)
		})
	}
}

// A successful parse must be usable as-is by the business entry point: the
// object carries every field Accept reads, with defaults already filled.
func TestParseOutputReadyForAccept(t *testing.T) {
	p := parseOK(t, `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 3,
	  "resources": {"cpu": 100, "memory": 64},
	  "nodes": [{"name": "n1", "cpu": 200, "memory": 128}]
	}`)
	if p.Status != "" || p.Node != nil || p.Reason != nil {
		t.Fatalf("parse must not schedule: %+v", p)
	}
	same, err := store.SameInput(p, &store.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  3,
		Resources: store.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []store.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	})
	if err != nil || !same {
		t.Fatalf("parsed input does not match its explicitly defaulted form: same=%v err=%v", same, err)
	}
}
