package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// The valid request used across the parse tests. Node order is significant
// and must survive parsing untouched.
const validInput = `{
  "namespace": "team-a",
  "name": "job-1",
  "queue": "default",
  "priority": 10,
  "resources": {"cpu": 500, "memory": 256},
  "selector": {"zone": "cn"},
  "nodes": [
    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn", "ssd": "true"}},
    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}}
  ]
}`

func TestParsePlacementInputPreservesValuesAndNodeOrder(t *testing.T) {
	p, err := ParsePlacementInput([]byte(validInput))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := &store.Placement{
		Namespace: "team-a",
		Name:      "job-1",
		Queue:     "default",
		Priority:  10,
		Resources: store.Resources{CPU: 500, Memory: 256},
		Selector:  map[string]string{"zone": "cn"},
		Nodes: []store.Node{
			{Name: "node-b", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn", "ssd": "true"}},
			{Name: "node-a", CPU: 1000, Memory: 512, Labels: map[string]string{"zone": "cn"}},
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("parsed = %+v, want %+v", p, want)
	}
	if p.Nodes[0].Name != "node-b" || p.Nodes[1].Name != "node-a" {
		t.Fatalf("node order not preserved: %+v", p.Nodes)
	}
}

func TestParsePlacementInputFillsDefaults(t *testing.T) {
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [{"name": "n", "cpu": 0, "memory": 0}]
	}`
	p, err := ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Selector == nil || len(p.Selector) != 0 {
		t.Fatalf("omitted selector must default to an empty object, got %#v", p.Selector)
	}
	if p.Nodes[0].Labels == nil || len(p.Nodes[0].Labels) != 0 {
		t.Fatalf("omitted labels must default to an empty object, got %#v", p.Nodes[0].Labels)
	}

	empty := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1}, "nodes": []
	}`
	q, err := ParsePlacementInput([]byte(empty))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Nodes == nil || len(q.Nodes) != 0 {
		t.Fatalf("empty nodes must stay an empty array, got %#v", q.Nodes)
	}
}

func TestParsePlacementInputKeepsLabelStringsVerbatim(t *testing.T) {
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
	  "resources": {"cpu": 1, "memory": 1},
	  "selector": {"": "", " k ": " v "},
	  "nodes": [{"name": "n", "cpu": 1, "memory": 1, "labels": {"":""}}]
	}`
	p, err := ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{"": "", " k ": " v "}
	if !reflect.DeepEqual(p.Selector, want) {
		t.Fatalf("selector = %#v, want %#v (empty and whitespace-padded label strings are kept verbatim)", p.Selector, want)
	}
	if !reflect.DeepEqual(p.Nodes[0].Labels, map[string]string{"": ""}) {
		t.Fatalf("labels = %#v, want empty key and value preserved", p.Nodes[0].Labels)
	}
}

func TestParsePlacementInputBoundaryValues(t *testing.T) {
	body := `{
	  "namespace": "ns", "name": "job", "queue": "q",
	  "priority": -2147483648,
	  "resources": {"cpu": 9223372036854775807, "memory": 1},
	  "nodes": [{"name": "n", "cpu": 0, "memory": 0}]
	}`
	p, err := ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("min int32 priority / max int64 resource / zero capacity rejected: %v", err)
	}
	if p.Priority != -2147483648 || p.Resources.CPU != 9223372036854775807 {
		t.Fatalf("boundary values not preserved: %+v", p)
	}

	max := `{
	  "namespace": "ns", "name": "job", "queue": "q",
	  "priority": 2147483647,
	  "resources": {"cpu": 1, "memory": 9223372036854775807},
	  "nodes": []
	}`
	q, err := ParsePlacementInput([]byte(max))
	if err != nil {
		t.Fatalf("max int32 priority rejected: %v", err)
	}
	if q.Priority != 2147483647 || q.Resources.Memory != 9223372036854775807 {
		t.Fatalf("boundary values not preserved: %+v", q)
	}
}

func TestParsePlacementInputRejectsInvalidBodies(t *testing.T) {
	valid := func() string {
		return `{"namespace":"ns","name":"job","queue":"q","priority":1,` +
			`"resources":{"cpu":100,"memory":64},` +
			`"nodes":[{"name":"n1","cpu":100,"memory":64,"labels":{}}]}`
	}
	cases := map[string]string{
		"empty body":                 ``,
		"invalid JSON":               `not json`,
		"unterminated object":        `{"namespace":"ns"`,
		"array root":                 `[]`,
		"null root":                  `null`,
		"number root":                `123`,
		"string root":                `"hello"`,
		"trailing object":            valid() + ` {"x":1}`,
		"trailing garbage":           valid() + `tail`,
		"unknown top-level field":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[],"unknown":true}`,
		"duplicate top-level field":  `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[],"namespace":"ns2"}`,
		"escaped duplicate field":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[],"n\u0061me":"x"}`,
		"missing namespace":          `{"name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"missing name":               `{"namespace":"ns","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"missing queue":              `{"namespace":"ns","name":"job","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"missing priority":           `{"namespace":"ns","name":"job","queue":"q","resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"missing resources":          `{"namespace":"ns","name":"job","queue":"q","priority":1,"nodes":[]}`,
		"missing nodes":              `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64}}`,
		"missing resource memory":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100},"nodes":[]}`,
		"unknown resource field":     `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64,"extra":1},"nodes":[]}`,
		"duplicate resource field":   `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"cpu":200,"memory":64},"nodes":[]}`,
		"unknown node field":         `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":100,"memory":64,"x":1}]}`,
		"duplicate node field":       `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":100,"memory":64,"name":"n2"}]}`,
		"duplicate selector key":     `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"selector":{"a":"1","a":"2"},"nodes":[]}`,
		"escaped duplicate label":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":1,"memory":1,"labels":{"a":"1","a":"2"}}]}`,
		"empty namespace":            `{"namespace":"","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"leading space namespace":    `{"namespace":" ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"trailing space name":        `{"namespace":"ns","name":"job ","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"unicode whitespace queue":   `{"namespace":"ns","name":"job","queue":" q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"fractional priority":        `{"namespace":"ns","name":"job","queue":"q","priority":1.5,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"exponential priority":       `{"namespace":"ns","name":"job","queue":"q","priority":1e3,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"string priority":            `{"namespace":"ns","name":"job","queue":"q","priority":"1","resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"null priority":              `{"namespace":"ns","name":"job","queue":"q","priority":null,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"int32 overflow priority":    `{"namespace":"ns","name":"job","queue":"q","priority":2147483648,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"int32 underflow priority":   `{"namespace":"ns","name":"job","queue":"q","priority":-2147483649,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"zero resource cpu":          `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":0,"memory":64},"nodes":[]}`,
		"negative resource memory":   `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":-1},"nodes":[]}`,
		"int64 overflow resource":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":9223372036854775808,"memory":1},"nodes":[]}`,
		"fractional resource cpu":    `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":10.5,"memory":64},"nodes":[]}`,
		"null resources":             `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":null,"nodes":[]}`,
		"nodes not array":            `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":{}}`,
		"null nodes":                 `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":null}`,
		"node not object":            `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":["n1"]}`,
		"negative node cpu":          `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":-1,"memory":64}]}`,
		"duplicate node names":       `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":100,"memory":64},{"name":"n1","cpu":100,"memory":64}]}`,
		"empty node name":            `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"","cpu":100,"memory":64}]}`,
		"numeric node name":          `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":123,"cpu":100,"memory":64}]}`,
		"null node labels":           `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":100,"memory":64,"labels":null}]}`,
		"null selector":              `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"selector":null,"nodes":[]}`,
		"non-string selector value":  `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"selector":{"k":1},"nodes":[]}`,
		"null name":                  `{"namespace":"ns","name":null,"queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"numeric namespace":          `{"namespace":7,"name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[]}`,
		"string node cpu":            `{"namespace":"ns","name":"job","queue":"q","priority":1,"resources":{"cpu":100,"memory":64},"nodes":[{"name":"n1","cpu":"100","memory":64}]}`,
	}
	for name, body := range cases {
		p, err := ParsePlacementInput([]byte(body))
		if err == nil {
			t.Errorf("%s: got nil error, parsed %+v", name, p)
			continue
		}
		if !errors.Is(err, ErrInvalidPlacementInput) {
			t.Errorf("%s: error %v does not match ErrInvalidPlacementInput", name, err)
		}
		if p != nil {
			t.Errorf("%s: expected nil placement on failure, got %+v", name, p)
		}
	}
}

func TestParsePlacementInputErrorMessage(t *testing.T) {
	_, err := ParsePlacementInput([]byte(`{}`))
	if err == nil || err.Error() != "invalid placement input" {
		t.Fatalf("error = %v, want %q", err, "invalid placement input")
	}
}

// The parsed input is the exact object Accept consumes: no router, no extra
// transformation between the two entry points.
func TestParsePlacementInputFeedsAcceptDirectly(t *testing.T) {
	_, svc := newService(t)
	p, err := ParsePlacementInput([]byte(validInput))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if outcome != Created {
		t.Fatalf("outcome = %d, want Created", outcome)
	}
	if rec.Status != StatusPlaced || rec.Node == nil || *rec.Node != "node-a" {
		t.Fatalf("decision not applied to parsed input: %+v", rec)
	}
}
