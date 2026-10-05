// Package service implements placement acceptance: the trial scheduling
// decision together with idempotent, conflict-aware registration. Accept is
// the single business entry point for a parsed placement request; it has no
// dependency on the HTTP transport.
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Scheduling outcomes recorded on every placement.
const (
	StatusPlaced   = "placed"
	StatusRejected = "rejected"
	ReasonNoNode   = "no_eligible_node"
)

// Outcome classifies what Accept did with a submission.
type Outcome int

const (
	// Created means the (namespace, name) identity was registered for the
	// first time by this call.
	Created Outcome = iota
	// Identical means the identity already existed with the same request
	// content; the returned record is the original one.
	Identical
	// Conflict means the identity already existed with different content;
	// the returned record is the untouched original.
	Conflict
)

// ErrStorageUnavailable is the sentinel wrapped for every persistence failure.
var ErrStorageUnavailable = errors.New("storage unavailable")

// Store is the persistence contract Accept and Query rely on. *store.Store
// satisfies it for production; tests may substitute a controlled fake without
// opening a SQLite file. The contract an implementation must keep:
//   - Submit stores p the first time its (namespace, name) identity is seen
//     and reports created; on a repeated identity it returns the previously
//     stored record unchanged and reports not-created,
//   - Get returns the stored record or an error matching store.ErrNotFound
//     when the identity is unknown,
//   - List returns committed records matching the filter, ordered by
//     namespace then name in UTF-8 byte order; a Node condition never
//     matches a rejected record.
// Content normalization, comparison and all scheduling rules stay in this
// package — an implementation never re-applies them.
type Store interface {
	Submit(ctx context.Context, p *store.Placement) (stored *store.Placement, created bool, err error)
	Get(ctx context.Context, namespace, name string) (*store.Placement, error)
	List(ctx context.Context, f store.ListFilter) ([]*store.Placement, error)
}

// Service holds the collaborators needed to accept placements.
type Service struct {
	st Store
}

// New builds the acceptance service over a store. A *store.Store is the
// production implementation; any Store substitute drops in unchanged.
func New(st Store) *Service {
	return &Service{st: st}
}

// Accept runs the trial placement and registers its result under the
// (namespace, name) identity. The first valid submission for an identity is
// stored and reported as Created; a later submission carrying the same request
// content is reported as Identical with the original record; one carrying
// different content is reported as Conflict and never changes the stored
// record. Scheduling results alone do not count as equal content. Input
// validation is expected to have happened before this call.
func (s *Service) Accept(ctx context.Context, p *store.Placement) (*store.Placement, Outcome, error) {
	// Fill omitted-field defaults through the single shared definition so
	// the trial, the stored record and the content comparison all see the
	// same input. The candidate array's order is left untouched.
	store.NormalizeInput(p)
	p.Status, p.Node, p.Reason = schedule(p)

	stored, created, err := s.st.Submit(ctx, p)
	if err != nil {
		return nil, Created, storageError(err)
	}
	if created {
		return stored, Created, nil
	}

	same, err := store.SameInput(p, stored)
	if err != nil {
		return nil, Created, storageError(err)
	}
	if !same {
		return stored, Conflict, nil
	}
	return stored, Identical, nil
}

func storageError(err error) error {
	return fmt.Errorf("accept placement: %w: %w", ErrStorageUnavailable, err)
}
