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
// Every object in the request — the root object, resources, each node, and
// the selector/labels maps — obeys exactly one shared structural rule,
// defined once in readMembers:
//   - a member value is decoded once and handed to the object's own
//     field handler for its business-specific type and range checks,
//   - an unknown member name is rejected,
//   - a repeated member name is rejected even when the two spellings differ
//     only by JSON escaping (e.g. "name" vs "n\u0061me"), so a duplicate is
//     never silently overwritten by the later value.
//
// readStrictObject adds the one remaining shared requirement: the value must
// be an object (never null, an array or a scalar). Per-field business limits
// (non-empty untrimmed identifiers, bounded integers, positivity) live only
// in the individual field handlers.
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

// memberHandler validates one decoded object member and applies its business
// rule. A nil allowed map means arbitrary member names are accepted.
type memberHandler func(key string, value json.RawMessage) error

// readStrictObject enforces the shared object rule on a standalone JSON
// value: it must be an object, its member names must be in allowed (when
// allowed is non-nil), no member may repeat, and every member is passed to
// handle for its field-specific checks. It returns the set of member names
// the object actually carried, for the caller's required-field checks.
func readStrictObject(raw json.RawMessage, allowed map[string]bool, handle memberHandler) (map[string]bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidPlacementInput
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrInvalidPlacementInput
	}
	return readMembers(dec, allowed, handle)
}

// readMembers walks the members of the object dec is positioned inside and
// applies the single shared structural rule. json.Decoder delivers member
// names already unescaped, so comparison is done on the resulting name and
// escape-only spellings collapse to one key. It returns the set of member
// names seen. This is the only place in the parser where unknown- and
// duplicate-member rejection is defined.
func readMembers(dec *json.Decoder, allowed map[string]bool, handle memberHandler) (map[string]bool, error) {
	seen := map[string]bool{}
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
		if seen[key] {
			return nil, ErrInvalidPlacementInput
		}
		seen[key] = true

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, ErrInvalidPlacementInput
		}
		if handle != nil {
			if err := handle(key, value); err != nil {
				return nil, err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidPlacementInput // closing '}'
	}
	return seen, nil
}

// ParsePlacementInput decodes one placement request body. On success it
// returns the complete input object, ready for service.Accept: every input
// field keeps its submitted value, the node array keeps its order, omitted
// selector and node labels are filled as empty objects, and an empty nodes
// array stays an empty array. On any rule violation it returns nil and an
// error matching errors.Is(err, ErrInvalidPlacementInput).
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
	seen, err := readMembers(dec, topLevelFields, func(key string, raw json.RawMessage) error {
		switch key {
		case "namespace":
			s, err := parseTrimmedString(raw)
			if err != nil {
				return err
			}
			p.Namespace = s
		case "name":
			s, err := parseTrimmedString(raw)
			if err != nil {
				return err
			}
			p.Name = s
		case "queue":
			s, err := parseTrimmedString(raw)
			if err != nil {
				return err
			}
			p.Queue = s
		case "priority":
			n, err := parseBoundedInt(raw, 32)
			if err != nil {
				return err
			}
			p.Priority = int32(n)
		case "resources":
			r, err := parseResources(raw)
			if err != nil {
				return err
			}
			p.Resources = r
		case "selector":
			m, err := parseStringMap(raw)
			if err != nil {
				return err
			}
			p.Selector = m
		case "nodes":
			nodes, err := parseNodes(raw)
			if err != nil {
				return err
			}
			p.Nodes = nodes
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	present, err := readStrictObject(raw, resourceFields, func(key string, value json.RawMessage) error {
		switch key {
		case "cpu":
			n, err := parseBoundedInt(value, 64)
			if err != nil || n <= 0 {
				return ErrInvalidPlacementInput
			}
			r.CPU = n
		case "memory":
			n, err := parseBoundedInt(value, 64)
			if err != nil || n <= 0 {
				return ErrInvalidPlacementInput
			}
			r.Memory = n
		}
		return nil
	})
	if err != nil {
		return store.Resources{}, err
	}
	if !present["cpu"] || !present["memory"] {
		return store.Resources{}, ErrInvalidPlacementInput
	}
	return r, nil
}

func parseNodes(raw json.RawMessage) ([]store.Node, error) {
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
		node, err := parseNode(elem)
		if err != nil {
			return nil, err
		}
		if names[node.Name] {
			return nil, ErrInvalidPlacementInput
		}
		names[node.Name] = true
		nodes = append(nodes, node)
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidPlacementInput // closing ']'
	}
	return nodes, nil
}

func parseNode(raw json.RawMessage) (store.Node, error) {
	var n store.Node
	present, err := readStrictObject(raw, nodeFields, func(key string, value json.RawMessage) error {
		switch key {
		case "name":
			s, err := parseTrimmedString(value)
			if err != nil {
				return err
			}
			n.Name = s
		case "cpu":
			v, err := parseBoundedInt(value, 64)
			if err != nil || v < 0 {
				return ErrInvalidPlacementInput
			}
			n.CPU = v
		case "memory":
			v, err := parseBoundedInt(value, 64)
			if err != nil || v < 0 {
				return ErrInvalidPlacementInput
			}
			n.Memory = v
		case "labels":
			m, err := parseStringMap(value)
			if err != nil {
				return err
			}
			n.Labels = m
		}
		return nil
	})
	if err != nil {
		return store.Node{}, err
	}
	for _, required := range []string{"name", "cpu", "memory"} {
		if !present[required] {
			return store.Node{}, ErrInvalidPlacementInput
		}
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	return n, nil
}

// parseStringMap decodes an object with arbitrary string keys and string
// values; null or non-string values are rejected.
func parseStringMap(raw json.RawMessage) (map[string]string, error) {
	out := map[string]string{}
	if _, err := readStrictObject(raw, nil, func(key string, value json.RawMessage) error {
		s, err := parseJSONString(value)
		if err != nil {
			return err
		}
		out[key] = s
		return nil
	}); err != nil {
		return nil, err
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
