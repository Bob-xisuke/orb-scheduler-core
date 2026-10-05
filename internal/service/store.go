package service

import "github.com/Bob-xisuke/orb-scheduler-core/internal/model"

// Store is the persistence contract Accept and Query rely on. It is the
// storage-independent contract defined in the model package: the production
// implementation is the SQLite-backed store, while tests substitute an
// in-memory double (see internal/service/fakestore) that controls the
// returned records and errors without creating a database file or loading a
// SQL driver. Either way the business flow — defaults, trial scheduling,
// idempotency and conflict rules, query validation — stays in this package
// and is never re-implemented by the storage side.
type Store = model.Store
