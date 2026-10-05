package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// ErrInvalidPlacementInput is the sentinel returned by ParsePlacementInput for
// every request-body violation; callers identify it with errors.Is.
var ErrInvalidPlacementInput = errors.New("invalid placement input")

// ---------------------------------------------------------------------------
// Strict request parsing.
//
// ParsePlacementInput is the transport-independent entry point for the
// documented request semantics: it takes the raw JSON body and returns the
// placement input ready for Accept, without touching routing, scheduling or
// storage. The token walk below enforces, before anything is accepted:
//   - the body is one JSON object with exactly the documented members,
//   - no unknown or duplicate member names at any level (member names that
//     unescape to the same string count as duplicates),
//   - no trailing data after the root object,
//   - every value has the declared JSON type and range (null is never accepted
//     where a value is required; omitted selector/labels default to {}),
//   - node names are unique.
//
// Any violation returns nil together with ErrInvalidPlacementInput; no partial
// input is ever produced.
// ---------------------------------------------------------------------------

var topLevelFields = map[string]bool{
	"namespace": true,
	"name":      true,
	"queue":     true,
	"priority":  true,
	"resources": true,
	"selector":  true,
	"nodes":     true,
}

var resourceFields = map[string]bool{"cpu": true, "memory": true}

var nodeFields = map[string]bool{
	"name":   true,
	"cpu":    true,
	"memory": true,
	"labels": true,
}

// ParsePlacementInput decodes and validates one placement request body. On
// success it returns the input half of a placement — every submitted field
// value and the node array order preserved — ready to hand to Accept. On any
// violation it returns nil and an error matching ErrInvalidPlacementInput.
func ParsePlacementInput(body []byte) (*store.Placement, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrInvalidPlacementInput
	}

	p := &store.Placement{}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, ErrInvalidPlacementInput
		}
		key, ok := keyTok.(string)
		if !ok || !topLevelFields[key] || seen[key] {
			return nil, ErrInvalidPlacementInput
		}
		seen[key] = true

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, ErrInvalidPlacementInput
		}

		switch key {
		case "namespace", "name", "queue":
			s, err := parseTrimmedString(raw)
			if err != nil {
				return nil, err
			}
			switch key {
			case "namespace":
				p.Namespace = s
			case "name":
				p.Name = s
			case "queue":
				p.Queue = s
			}
		case "priority":
			n, err := parseBoundedInt(raw, 32)
			if err != nil {
				return nil, err
			}
			p.Priority = int32(n)
		case "resources":
			r, err := parseResources(raw)
			if err != nil {
				return nil, err
			}
			p.Resources = r
		case "selector":
			m, err := parseStringMap(raw)
			if err != nil {
				return nil, err
			}
			p.Selector = m
		case "nodes":
			nodes, err := parseNodes(raw)
			if err != nil {
				return nil, err
			}
			p.Nodes = nodes
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidPlacementInput // closing '}'
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidPlacementInput // trailing data
	}

	for _, required := range []string{"namespace", "name", "queue", "priority", "resources", "nodes"} {
		if !seen[required] {
			return nil, ErrInvalidPlacementInput
		}
	}
	if p.Selector == nil {
		p.Selector = map[string]string{}
	}
	return p, nil
}

func parseResources(raw json.RawMessage) (store.Resources, error) {
	var r store.Resources
	obj, err := parseStrictObject(raw, resourceFields)
	if err != nil {
		return r, err
	}
	cpuRaw, ok := obj["cpu"]
	if !ok {
		return r, ErrInvalidPlacementInput
	}
	memoryRaw, ok := obj["memory"]
	if !ok {
		return r, ErrInvalidPlacementInput
	}
	cpu, err := parseBoundedInt(cpuRaw, 64)
	if err != nil || cpu <= 0 {
		return r, ErrInvalidPlacementInput
	}
	memory, err := parseBoundedInt(memoryRaw, 64)
	if err != nil || memory <= 0 {
		return r, ErrInvalidPlacementInput
	}
	return store.Resources{CPU: cpu, Memory: memory}, nil
}

func parseNodes(raw json.RawMessage) ([]store.Node, error) {
	if isNull(raw) {
		return nil, ErrInvalidPlacementInput
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, ErrInvalidPlacementInput
	}

	nodes := []store.Node{}
	names := map[string]bool{}
	for dec.More() {
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return nil, ErrInvalidPlacementInput
		}
		obj, err := parseStrictObject(elem, nodeFields)
		if err != nil {
			return nil, err
		}
		nameRaw, ok := obj["name"]
		if !ok {
			return nil, ErrInvalidPlacementInput
		}
		cpuRaw, ok := obj["cpu"]
		if !ok {
			return nil, ErrInvalidPlacementInput
		}
		memoryRaw, ok := obj["memory"]
		if !ok {
			return nil, ErrInvalidPlacementInput
		}
		name, err := parseTrimmedString(nameRaw)
		if err != nil {
			return nil, err
		}
		cpu, err := parseBoundedInt(cpuRaw, 64)
		if err != nil || cpu < 0 {
			return nil, ErrInvalidPlacementInput
		}
		memory, err := parseBoundedInt(memoryRaw, 64)
		if err != nil || memory < 0 {
			return nil, ErrInvalidPlacementInput
		}
		labels := map[string]string{}
		if labelRaw, ok := obj["labels"]; ok {
			labels, err = parseStringMap(labelRaw)
			if err != nil {
				return nil, err
			}
		}
		if names[name] {
			return nil, ErrInvalidPlacementInput
		}
		names[name] = true
		nodes = append(nodes, store.Node{Name: name, CPU: cpu, Memory: memory, Labels: labels})
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidPlacementInput // closing ']'
	}
	return nodes, nil
}

// parseStrictObject decodes an object whose member names are restricted to
// allowed. Both unknown and duplicate names are rejected.
func parseStrictObject(raw json.RawMessage, allowed map[string]bool) (map[string]json.RawMessage, error) {
	if isNull(raw) {
		return nil, ErrInvalidPlacementInput
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrInvalidPlacementInput
	}
	return readObjectMembers(dec, allowed)
}

// parseStringMap decodes an object with arbitrary string keys and string
// values; null or non-string values are rejected.
func parseStringMap(raw json.RawMessage) (map[string]string, error) {
	if isNull(raw) {
		return nil, ErrInvalidPlacementInput
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrInvalidPlacementInput
	}

	out := map[string]string{}
	raws, err := readObjectMembers(dec, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range raws {
		s, err := parseJSONString(value)
		if err != nil {
			return nil, err
		}
		out[key] = s
	}
	return out, nil
}

func readObjectMembers(dec *json.Decoder, allowed map[string]bool) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, ErrInvalidPlacementInput
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, ErrInvalidPlacementInput
		}
		if allowed != nil && !allowed[key] {
			return nil, ErrInvalidPlacementInput
		}
		if _, dup := out[key]; dup {
			return nil, ErrInvalidPlacementInput
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, ErrInvalidPlacementInput
		}
		out[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidPlacementInput // closing '}'
	}
	return out, nil
}

// parseTrimmedString accepts a JSON string that is non-empty and has no
// leading or trailing Unicode whitespace.
func parseTrimmedString(raw json.RawMessage) (string, error) {
	s, err := parseJSONString(raw)
	if err != nil {
		return "", err
	}
	if s == "" || s != strings.TrimSpace(s) {
		return "", ErrInvalidPlacementInput
	}
	return s, nil
}

func parseJSONString(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", ErrInvalidPlacementInput
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ErrInvalidPlacementInput
	}
	return s, nil
}

// parseBoundedInt accepts an integral JSON number without fraction or exponent
// that fits a signed integer of the given bit size (32 or 64).
func parseBoundedInt(raw json.RawMessage, bitSize int) (int64, error) {
	if isNull(raw) {
		return 0, ErrInvalidPlacementInput
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return 0, ErrInvalidPlacementInput
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, ErrInvalidPlacementInput
	}
	n, err := strconv.ParseInt(number.String(), 10, bitSize)
	if err != nil {
		return 0, ErrInvalidPlacementInput
	}
	return n, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
