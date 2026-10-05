// Package model holds the pure business definitions of the placement
// service: the record model every layer shares, the single definition of
// input defaulting and content equality, and the storage-contract vocabulary
// (ErrNotFound, ListFilter). It imports nothing beyond the standard library —
// in particular no SQLite driver and no connection management — so input
// parsing, trial placement and idempotency judgment can depend on it without
// pulling in any storage implementation. The SQLite-backed store in
// internal/store is one consumer of these definitions; in-memory storage
// doubles are another.
package model

import "errors"

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

// Placement is one stored scheduling decision: the request payload plus its
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

// ErrNotFound is returned by a store's Get when no placement matches the
// identity. It is part of the storage contract shared by every Store
// implementation, not of any one storage engine.
var ErrNotFound = errors.New("placement not found")

// ListFilter narrows a store's List; empty fields are ignored. Node never
// matches a rejected record.
type ListFilter struct {
	Namespace string
	Queue     string
	Node      string
}
