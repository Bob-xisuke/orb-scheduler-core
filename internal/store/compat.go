package store

import "github.com/Bob-xisuke/orb-scheduler-core/internal/model"

// Compatibility re-exports. The record model, the defaulting and
// content-equality rules, and the storage-contract vocabulary live in
// internal/model so that parsing, scheduling and idempotency judgment — and
// in-memory storage doubles — never depend on this package's SQLite driver.
// Existing callers that refer to these names through package store keep
// working unchanged: the aliases are the very same types and functions.

type (
	// Placement is one stored scheduling decision; see model.Placement.
	Placement = model.Placement
	// Node is one candidate node offered by a placement request.
	Node = model.Node
	// Resources is a workload request or a node capacity.
	Resources = model.Resources
	// ListFilter narrows List; empty fields are ignored. Node never matches
	// a rejected record.
	ListFilter = model.ListFilter
)

// ErrNotFound is returned by Get when no placement matches the identity.
var ErrNotFound = model.ErrNotFound

// NormalizeInput fills the documented omitted-field defaults in place; see
// model.NormalizeInput, the single definition shared by every entry point.
var NormalizeInput = model.NormalizeInput

// SameInput reports whether the request halves of two placements are equal
// under the documented comparison; see model.SameInput.
var SameInput = model.SameInput
