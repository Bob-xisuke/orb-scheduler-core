package store

import (
	"bytes"
	"encoding/json"
)

// ---------------------------------------------------------------------------
// Placement input defaults and content equality — the single definition.
//
// This file is the only place that defines how omitted input fields are
// defaulted and how two submissions are compared for content equality:
//   - an omitted selector or node labels map means the empty object, and an
//     omitted node array means the empty array (NormalizeInput),
//   - comparison runs over the canonical JSON of the request half only, so
//     object member order and equivalent escape spellings are ignored while
//     node array order stays significant (canonicalInput, SameInput).
//
// The parser (package placement), the acceptance service (package service)
// and this package's own persistence all go through these functions, so the
// rules exist exactly once. Defaults apply to omitted fields only; rejecting
// explicit nulls is the parser's job and always happens before a placement
// reaches any code here.
// ---------------------------------------------------------------------------

// inputPayload is the canonical JSON representation of the request half of a
// record. It omits Status/Node/Reason so two submissions can be compared for
// content equality byte-for-byte; encoding/json sorts object keys, while array
// order is preserved.
type inputPayload struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Queue     string            `json:"queue"`
	Priority  int32             `json:"priority"`
	Resources Resources         `json:"resources"`
	Selector  map[string]string `json:"selector"`
	Nodes     []Node            `json:"nodes"`
}

// NormalizeInput fills the documented defaults for omitted fields in place:
// an omitted selector or node labels map becomes an empty object and an
// omitted node array becomes an empty array. Fields already present are kept
// exactly as submitted, and the node array is never reordered — its order is
// significant for content comparison. The function is idempotent.
func NormalizeInput(p *Placement) {
	if p.Selector == nil {
		p.Selector = map[string]string{}
	}
	if p.Nodes == nil {
		p.Nodes = []Node{}
	}
	for i := range p.Nodes {
		if p.Nodes[i].Labels == nil {
			p.Nodes[i].Labels = map[string]string{}
		}
	}
}

// SameInput reports whether the request halves of two placements are equal
// under the documented comparison: omitted selector/labels are treated as
// empty objects, object member order is ignored and array order is kept. The
// scheduling outcome (Status/Node/Reason) plays no part in the comparison.
func SameInput(a, b *Placement) (bool, error) {
	ab, err := canonicalInput(a)
	if err != nil {
		return false, err
	}
	bb, err := canonicalInput(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

// canonicalInput encodes the request half of p after applying the shared
// defaults, yielding the byte representation stored and compared.
func canonicalInput(p *Placement) ([]byte, error) {
	NormalizeInput(p)
	in := inputPayload{
		Namespace: p.Namespace,
		Name:      p.Name,
		Queue:     p.Queue,
		Priority:  p.Priority,
		Resources: p.Resources,
		Selector:  p.Selector,
		Nodes:     p.Nodes,
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return data, nil
}
