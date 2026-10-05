// Package model is the storage-independent definition of the placement
// business: the records that flow through every entry point, the documented
// request-content rules (omitted-field defaults, order-insensitive object
// comparison, order-sensitive array comparison), the canonical JSON encoding
// stored on disk, and the storage contract the business flows program
// against. It opens no database and imports no SQL driver; the SQLite-backed
// implementation and the in-memory test double are two interchangeable
// implementations of Store.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Resources is a workload request or a node capacity. CPU is in millicores and
// memory in MiB; both are non-negative 64-bit integers on the wire.
type Resources struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
}

// Node is one candidate node offered by a placement request.
type Node struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"`
	Memory int64             `json:"memory"`
	Labels map[string]string `json:"labels"`
}

// Placement is one recorded scheduling decision: the request payload plus its
// status. Node and Reason are pointers so a rejected record renders them as
// JSON null.
type Placement struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Queue     string            `json:"queue"`
	Priority  int32             `json:"priority"`
	Resources Resources         `json:"resources"`
	Selector  map[string]string `json:"selector"`
	Nodes     []Node            `json:"nodes"`
	Status    string            `json:"status"`
	Node      *string           `json:"node"`
	Reason    *string           `json:"reason"`
}

// ErrNotFound is the contract error Get returns when no placement matches the
// identity. Every Store implementation returns this sentinel so business code
// classifies a missing record with errors.Is without knowing the backend.
var ErrNotFound = errors.New("placement not found")

// ListFilter narrows List; empty fields are ignored. A node filter never
// matches a rejected record, whose Node is nil.
type ListFilter struct {
	Namespace string
	Queue     string
	Node      string
}

// ---------------------------------------------------------------------------
// Request-content rules — the single definition shared by every entry point:
//   - placement.ParsePlacementInput fills defaults through NormalizeInput,
//   - service.Accept normalizes through NormalizeInput before the trial,
//   - the SQLite store writes EncodeInput, the canonical stored form,
//   - Submit/SameInput compare content through the same canonical form.
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
// ---------------------------------------------------------------------------

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

// EncodeInput returns the canonical JSON bytes of the request half of p: the
// exact bytes a store writes for a new record. It works on a copy so callers
// can encode records they still own without having defaults filled into their
// own structs, and it never reorders the nodes. The result is stable across
// builds — field set, JSON names and key order are pinned by a regression
// test — so pre-refactor database files stay readable and comparable.
func EncodeInput(p *Placement) ([]byte, error) {
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
	return json.Marshal(in)
}

// SameInput reports whether the request halves of two placements are equal
// under the documented comparison: omitted selector/labels are treated as
// empty objects, object member order is ignored and array order is kept.
// Scheduling results on either record do not affect the outcome. Neither
// argument is modified.
func SameInput(a, b *Placement) (bool, error) {
	ab, err := EncodeInput(a)
	if err != nil {
		return false, err
	}
	bb, err := EncodeInput(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

// DecodeRecord rebuilds a stored record from its canonical request-half JSON
// and the separately stored scheduling columns. It is shared by every Store
// implementation so reading a row always yields the same shape regardless of
// the member order used when it was written.
func DecodeRecord(inputJSON, status string, node, reason *string) (*Placement, error) {
	var in inputPayload
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return nil, fmt.Errorf("decode stored placement: %w", err)
	}
	return &Placement{
		Namespace: in.Namespace,
		Name:      in.Name,
		Queue:     in.Queue,
		Priority:  in.Priority,
		Resources: in.Resources,
		Selector:  in.Selector,
		Nodes:     in.Nodes,
		Status:    status,
		Node:      node,
		Reason:    reason,
	}, nil
}
