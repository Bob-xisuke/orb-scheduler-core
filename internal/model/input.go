package model

import (
	"bytes"
	"encoding/json"
)

// This file is the single definition of what counts as the request content
// of a placement and how omitted fields are defaulted. Three entry points
// rely on it and must not re-implement any part of it:
//   - placement.ParsePlacementInput fills defaults through NormalizeInput,
//   - service.Accept normalizes through NormalizeInput before scheduling,
//   - Submit/SameInput compare and store content through CanonicalInput.
//
// The rules:
//   - an omitted (nil) selector, node labels map, or nodes array is
//     equivalent to its explicit empty form ({}, {}, []),
//   - JSON object member order and escape spellings never matter: comparison
//     happens on the re-encoded canonical form, whose object keys are sorted
//     by encoding/json,
//   - the nodes array order is significant: reordering it is different
//     content even when the scheduling result would be identical,
//   - status, node and reason are scheduling results, not request content,
//     and never participate in the comparison.

// inputPayload is the canonical JSON representation of the request half of a
// record. It omits Status/Node/Reason so two submissions can be compared for
// content equality byte-for-byte; encoding/json sorts object keys, while
// array order is preserved. Its field set and JSON names are part of the
// stored format: existing database files hold exactly this encoding.
type inputPayload struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Queue     string            `json:"queue"`
	Priority  int32             `json:"priority"`
	Resources Resources         `json:"resources"`
	Selector  map[string]string `json:"selector"`
	Nodes     []Node            `json:"nodes"`
}

// NormalizeInput fills the documented defaults in place so every entry point
// behaves the same regardless of how the request was assembled: an omitted
// selector or node labels map becomes an empty object, and an omitted nodes
// array becomes an empty array. It only touches nil fields — explicit values,
// including explicit empty objects, are kept as submitted — and it never
// reorders the candidate array, whose order is significant for content
// comparison.
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
// empty objects, object member order is ignored and array order is kept.
// Scheduling results on either record do not affect the outcome.
func SameInput(a, b *Placement) (bool, error) {
	ab, err := CanonicalInput(a)
	if err != nil {
		return false, err
	}
	bb, err := CanonicalInput(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

// CanonicalInput encodes the request half of p in its canonical form. It
// works on a copy so callers can compare records they still own without
// having defaults filled into their structs. The encoding is the stored
// format: stores persist exactly these bytes, so the field set, JSON names
// and key order must never change.
func CanonicalInput(p *Placement) ([]byte, error) {
	cp := *p
	if p.Nodes != nil {
		cp.Nodes = make([]Node, len(p.Nodes))
		copy(cp.Nodes, p.Nodes)
	}
	NormalizeInput(&cp)
	in := inputPayload{
		Namespace: cp.Namespace,
		Name:      cp.Name,
		Queue:     cp.Queue,
		Priority:  cp.Priority,
		Resources: cp.Resources,
		Selector:  cp.Selector,
		Nodes:     cp.Nodes,
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// DecodeInput decodes the request half of a stored record from its canonical
// encoding, as written by CanonicalInput. The scheduling results are not part
// of that encoding; the returned placement carries only the request fields
// and leaves Status, Node and Reason to the caller.
func DecodeInput(data []byte) (*Placement, error) {
	var in inputPayload
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, err
	}
	return &Placement{
		Namespace: in.Namespace,
		Name:      in.Name,
		Queue:     in.Queue,
		Priority:  in.Priority,
		Resources: in.Resources,
		Selector:  in.Selector,
		Nodes:     in.Nodes,
	}, nil
}
