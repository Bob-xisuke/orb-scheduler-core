package placement_test

import (
	"reflect"
	"testing"
)

// Text variants of one legal request: the business content must parse to the
// same Placement however the bytes spell it. Member order is insignificant at
// every level, JSON escape spellings of member names decode to the same name,
// and selector/labels written explicitly as {} equal omitted ones.
func TestParseTextVariantsYieldSameContent(t *testing.T) {
	canonical := `{
  "namespace": "team-a",
  "name": "job-1",
  "queue": "default",
  "priority": 10,
  "resources": {"cpu": 500, "memory": 256},
  "selector": {"zone": "cn"},
  "nodes": [
    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}}
  ]
}`

	variants := map[string]string{
		// Every object's members in a different order.
		"reordered": `{
  "nodes": [{"labels": {"zone": "cn"}, "memory": 512, "cpu": 1000, "name": "node-a"}],
  "selector": {"zone": "cn"},
  "resources": {"memory": 256, "cpu": 500},
  "priority": 10, "queue": "default", "name": "job-1", "namespace": "team-a"
}`,
		// Fixed member names and map keys written with equivalent \uXXXX
		// escape spellings; order scrambled at every level. They decode to
		// exactly the same unescaped names.
		"escaped names": "{\n" +
			"  \"nodes\": [{\"labels\": {\"\\u007aone\": \"\\u0063n\"}, \"memory\": 512, \"cpu\": 1000, \"\\u006eame\": \"node-a\"}],\n" +
			"  \"\\u0073elector\": {\"\\u007aone\": \"cn\"},\n" +
			"  \"resources\": {\"\\u006demory\": 256, \"cpu\": 500},\n" +
			"  \"priority\": 10, \"queue\": \"default\", \"\\u006eame\": \"job-1\", \"\\u006eamespace\": \"team-a\"\n" +
			"}",
		// Defaults written explicitly as empty objects, members reordered.
		"explicit empty defaults": `{
  "nodes": [{"labels": {}, "memory": 0, "cpu": 0, "name": "n"}],
  "selector": {},
  "resources": {"memory": 1, "cpu": 1},
  "priority": 0, "queue": "q", "name": "job", "namespace": "ns"
}`,
		// Same content with selector/labels omitted entirely.
		"omitted defaults": `{
  "namespace": "ns", "name": "job", "queue": "q", "priority": 0,
  "resources": {"cpu": 1, "memory": 1},
  "nodes": [{"name": "n", "cpu": 0, "memory": 0}]
}`,
	}

	want := parseOK(t, canonical)
	if got := parseOK(t, variants["reordered"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered variant parsed to %+v, want %+v", got, want)
	}
	if got := parseOK(t, variants["escaped names"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("escaped-name variant parsed to %+v, want %+v", got, want)
	}

	// The two default forms must agree: omitted == explicit {}.
	explicit := parseOK(t, variants["explicit empty defaults"])
	omitted := parseOK(t, variants["omitted defaults"])
	if !reflect.DeepEqual(explicit, omitted) {
		t.Fatalf("explicit defaults %+v != omitted defaults %+v", explicit, omitted)
	}
}

// A repeated member is a parse error at every object level, including when the
// two spellings differ only by JSON escaping. The later value must never win.
func TestParseRejectsRepeatedMembersEverywhere(t *testing.T) {
	cases := map[string]string{
		"duplicate top-level field plain":          `{"namespace":"ns","namespace":"x","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[]}`,
		"duplicate top-level field escaped first":  "{\"\\u006eamespace\":\"ns\",\"namespace\":\"x\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[]}",
		"duplicate top-level field escaped second": "{\"namespace\":\"ns\",\"\\u006eamespace\":\"x\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[]}",
		"duplicate resource plain":                 `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"cpu":2,"memory":1},"nodes":[]}`,
		"duplicate resource escaped":               "{\"namespace\":\"ns\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"\\u0063pu\":1,\"cpu\":2,\"memory\":1},\"nodes\":[]}",
		"duplicate node field plain":               `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","name":"m","cpu":1,"memory":1}]}`,
		"duplicate node field escaped":             "{\"namespace\":\"ns\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[{\"\\u006eame\":\"n\",\"name\":\"m\",\"cpu\":1,\"memory\":1}]}",
		"duplicate selector key plain":             `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[],"selector":{"zone":"a","zone":"b"}}`,
		"duplicate selector key escaped":           "{\"namespace\":\"ns\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[],\"selector\":{\"\\u007aone\":\"a\",\"zone\":\"b\"}}",
		"duplicate label key plain":                `{"namespace":"ns","name":"j","queue":"q","priority":1,"resources":{"cpu":1,"memory":1},"nodes":[{"name":"n","cpu":1,"memory":1,"labels":{"k":"1","k":"2"}}]}`,
		"duplicate label key escaped":              "{\"namespace\":\"ns\",\"name\":\"j\",\"queue\":\"q\",\"priority\":1,\"resources\":{\"cpu\":1,\"memory\":1},\"nodes\":[{\"name\":\"n\",\"cpu\":1,\"memory\":1,\"labels\":{\"\\u006b\":\"1\",\"k\":\"2\"}}]}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			parseErr(t, body)
		})
	}
}
