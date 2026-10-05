package service

import (
	"context"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Store is the persistence contract Accept and Query rely on. The production
// implementation is *store.Store over SQLite; tests can substitute an
// in-memory double (see internal/service/fakestore) that controls the
// returned records and errors without creating a database file. Either way
// the business flow — defaults, trial scheduling, idempotency and conflict
// rules, query validation — stays in this package and is never re-implemented
// by the storage side.
type Store interface {
	// Submit stores p the first time its (namespace, name) identity is seen
	// and reports created; on a repeated identity it returns the previously
	// stored record with created false and changes nothing.
	Submit(ctx context.Context, p *store.Placement) (stored *store.Placement, created bool, err error)
	// Get fetches one record by identity, returning store.ErrNotFound when
	// the identity is unknown.
	Get(ctx context.Context, namespace, name string) (*store.Placement, error)
	// List returns the committed records matching f, sorted by namespace
	// then name in UTF-8 byte order; a node filter never matches a rejected
	// record.
	List(ctx context.Context, f store.ListFilter) ([]*store.Placement, error)
}

// *store.Store is the production Store implementation.
var _ Store = (*store.Store)(nil)
