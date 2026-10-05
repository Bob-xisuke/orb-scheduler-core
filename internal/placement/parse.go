// Package placement parses raw placement request bodies into the input
// object accepted by service.Accept. It is a standalone entry point: it has
// no dependency on the HTTP transport, opens no database, and performs no
// scheduling or writes. The HTTP API and direct callers share exactly these
// strict rules.
package placement

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// ErrInvalidPlacementInput is the single sentinel returned for every parse
// failure: malformed JSON, a non-object root, trailing data, unknown or
// duplicate member names, missing required fields, explicit nulls, type or
// range errors, and duplicate node names. Callers classify failures with
// errors.Is; a failed parse never yields a partial input object.
var ErrInvalidPlacementInput = errors.New("invalid placement input")

// ---------------------------------------------------------------------------
// Strict request parsing.
//
// readStrictObject/readObjectMembers below are the single definition of the
// object-member rules shared by the top-level request body and every nested
// object (resources, node, selector, labels):
//   - the value is exactly one JSON object,
//   - no unknown or duplicate member names (member names are compared after
//     escape decoding, so a name also collides with its escaped spellings),
//   - each value is captured raw for the field-specific rules that follow.
//
// ParsePlacementInput adds the root-level rules on top of that shared walk:
// no trailing data after the root object, every required member present, each
// field's declared JSON type and range (null is never accepted where a value
// is required), and unique node names. Defaults for omitted selector/labels
// are not redefined here: the parsed object is finished with the single
// shared definition, store.NormalizeInput.
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

// ParsePlacementInput decodes one placement request body. On success it
// returns the complete input object, ready for service.Accept: every input
// field keeps its submitted value, the node array keeps its order, omitted
// selector and node labels are filled as empty objects by the shared
// store.NormalizeInput definition, and an empty nodes array stays an empty
// array. On any rule violation it returns nil and an error matching
// errors.Is(err, ErrInvalidPlacementInput).
func ParsePlacementInput(body []byte) (*store.Placement, error) {
	dec := json.NewDecoder(bytes.NewReader(body))

	fields, err := readStrictObject(dec, topLevelFields)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidPlacementInput // trailing data
	}

	for _, required := range []string{"namespace", "name", "queue", "priority", "resources", "nodes"} {
		if _, ok := fields[required]; !ok {
			return nil, ErrInvalidPlacementInput
		}
	}

	p := &store.Placement{}
	if p.Namespace, err = parseTrimmedString(fields["namespace"]); err != nil {
		return nil, err
	}
	if p.Name, err = parseTrimmedString(fields["name"]); err != nil {
		return nil, err
	}
	if p.Queue, err = parseTrimmedString(fields["queue"]); err != nil {
		return nil, err
	}
	priority, err := parseBoundedInt(fields["priority"], 32)
	if err != nil {
		return nil, err
	}
	p.Priority = int32(priority)
	if p.Resources, err = parseResources(fields["resources"]); err != nil {
		return nil, err
	}
	if p.Nodes, err = parseNodes(fields["nodes"]); err != nil {
		return nil, err
	}
	if raw, ok := fields["selector"]; ok {
		if p.Selector, err = parseStringMap(raw); err != nil {
			return nil, err
		}
	}
	store.NormalizeInput(p)
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
		// Omitted labels stay nil here; the shared store.NormalizeInput call
		// at the end of ParsePlacementInput fills the documented default.
		// An explicit null is still rejected inside parseStringMap.
		var labels map[string]string
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

// readStrictObject consumes exactly one JSON object from dec and returns its
// members with their values still raw. Together with readObjectMembers it is
// the single definition of the member rules: member names are restricted to
// allowed (nil allows any names), and unknown and duplicate names are
// rejected before the duplicate value can overwrite anything.
func readStrictObject(dec *json.Decoder, allowed map[string]bool) (map[string]json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrInvalidPlacementInput
	}
	return readObjectMembers(dec, allowed)
}

// parseStrictObject applies the shared member rules to a raw nested object.
func parseStrictObject(raw json.RawMessage, allowed map[string]bool) (map[string]json.RawMessage, error) {
	if isNull(raw) {
		return nil, ErrInvalidPlacementInput
	}
	return readStrictObject(json.NewDecoder(bytes.NewReader(raw)), allowed)
}

// parseStringMap decodes an object with arbitrary string keys and string
// values; null or non-string values are rejected.
func parseStringMap(raw json.RawMessage) (map[string]string, error) {
	raws, err := parseStrictObject(raw, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
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
