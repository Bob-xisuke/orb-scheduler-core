package model

import "context"

// Store is the persistence contract the business flows rely on. The
// production implementation is the SQLite-backed store; tests can substitute
// an in-memory double that controls the returned records and errors without
// creating a database file or registering a SQL driver. Either way the
// business flow — defaults, trial scheduling, idempotency and conflict
// rules, query validation — stays above this contract and is never
// re-implemented by a Store.
type Store interface {
	// Submit stores p the first time its (namespace, name) identity is seen
	// and reports created; on a repeated identity it returns the previously
	// stored record with created false and changes nothing. A single
	// transaction and a unique key guarantee one identity is stored exactly
	// once even under concurrent submissions.
	Submit(ctx context.Context, p *Placement) (stored *Placement, created bool, err error)
	// Get fetches one record by identity, returning ErrNotFound when the
	// identity is unknown. Only committed content is returned.
	Get(ctx context.Context, namespace, name string) (*Placement, error)
	// List returns the committed records matching f, sorted by namespace
	// then name in UTF-8 byte order; a node filter never matches a rejected
	// record.
	List(ctx context.Context, f ListFilter) ([]*Placement, error)
}
