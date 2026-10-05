package store

import "github.com/Bob-xisuke/orb-scheduler-core/internal/model"

// The record, node, resource and filter types used to be defined in this
// package. They now live in the storage-independent model package so that
// parsing, trial placement and idempotency depend only on the business
// definition and the Store contract, never on the SQLite driver. These
// aliases keep call sites that build records through the store package
// working unchanged: store.Placement, store.Node, store.Resources and
// store.ListFilter are the very same types as their model counterparts.
type (
	Placement  = model.Placement
	Node       = model.Node
	Resources  = model.Resources
	ListFilter = model.ListFilter
)

// ErrNotFound is re-exported from the model so existing callers keep
// classifying a missing identity with store.ErrNotFound.
var ErrNotFound = model.ErrNotFound

// NormalizeInput re-exports the single shared defaulting rule.
func NormalizeInput(p *Placement) { model.NormalizeInput(p) }

// SameInput re-exports the single shared content-comparison rule.
func SameInput(a, b *Placement) (bool, error) { return model.SameInput(a, b) }
