// Package fakestore provides an in-memory implementation of the storage
// contract defined by package service. It lets business-logic tests run the
// real accept and query flows without creating a SQLite file: stored records
// are controlled by the test, every call is recorded, and each method can be
// switched to fail with a chosen error. It carries no scheduling, defaulting
// or comparison rules of its own — those stay in package service.
package fakestore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// SubmitCall records one Submit invocation.
type SubmitCall struct {
	// Record is a deep copy of the placement as the service submitted it,
	// after its own normalization and trial scheduling.
	Record *model.Placement
}

// GetCall records one Get invocation.
type GetCall struct {
	Namespace string
	Name      string
}

// Store is an in-memory storage double. The zero value is ready to use and
// safe for concurrent use.
//
// Its default behavior mirrors the storage guarantees the service relies on:
//   - Submit keeps the first record stored for a (namespace, name) identity
//     and answers later submissions of that identity with the kept record
//     and created false;
//   - Get returns model.ErrNotFound for an unknown identity;
//   - List intersects the non-empty filter fields (a node filter never
//     matches a rejected record) and sorts by namespace then name in UTF-8
//     byte order.
//
// Setting PingErr, SubmitErr, GetErr or ListErr makes the matching method
// fail with that error instead of touching the in-memory records. Records
// can be preloaded with Seed, which is not counted as a call. Every Ping,
// Submit, Get and List invocation is appended to Pings, Submits, Gets and
// Lists.
type Store struct {
	mu      sync.Mutex
	records map[identity]*model.Placement
	order   []identity

	PingErr   error
	SubmitErr error
	GetErr    error
	ListErr   error

	Pings   int
	Submits []SubmitCall
	Gets    []GetCall
	Lists   []model.ListFilter
}

type identity struct {
	namespace string
	name      string
}

// Seed preloads records as if they had been registered earlier, without
// recording calls. The first record for an identity wins, matching Submit.
func (s *Store) Seed(records ...*model.Placement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range records {
		id := identity{rec.Namespace, rec.Name}
		if s.records == nil {
			s.records = map[identity]*model.Placement{}
		}
		if _, ok := s.records[id]; ok {
			continue
		}
		s.records[id] = clone(rec)
		s.order = append(s.order, id)
	}
}

// Ping implements the service storage contract: the in-memory double is
// always healthy unless PingErr is set; it never inspects the records.
func (s *Store) Ping(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pings++
	return s.PingErr
}

// Submit implements the service storage contract: it stores a copy of p the
// first time its identity is seen and otherwise returns the kept record.
func (s *Store) Submit(_ context.Context, p *model.Placement) (*model.Placement, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Submits = append(s.Submits, SubmitCall{Record: clone(p)})
	if s.SubmitErr != nil {
		return nil, false, s.SubmitErr
	}
	id := identity{p.Namespace, p.Name}
	if existing, ok := s.records[id]; ok {
		return clone(existing), false, nil
	}
	if s.records == nil {
		s.records = map[identity]*model.Placement{}
	}
	s.records[id] = clone(p)
	s.order = append(s.order, id)
	return clone(p), true, nil
}

// Get implements the service storage contract: it returns a copy of the
// stored record or model.ErrNotFound for an unknown identity.
func (s *Store) Get(_ context.Context, namespace, name string) (*model.Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Gets = append(s.Gets, GetCall{Namespace: namespace, Name: name})
	if s.GetErr != nil {
		return nil, s.GetErr
	}
	rec, ok := s.records[identity{namespace, name}]
	if !ok {
		return nil, model.ErrNotFound
	}
	return clone(rec), nil
}

// List implements the service storage contract: it intersects the non-empty
// filter fields and returns copies sorted by namespace then name in UTF-8
// byte order. A node filter never matches a rejected record, whose Node is
// nil.
func (s *Store) List(_ context.Context, f model.ListFilter) ([]*model.Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Lists = append(s.Lists, f)
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	var out []*model.Placement
	for _, id := range s.order {
		rec := s.records[id]
		if f.Namespace != "" && rec.Namespace != f.Namespace {
			continue
		}
		if f.Queue != "" && rec.Queue != f.Queue {
			continue
		}
		if f.Node != "" && (rec.Node == nil || *rec.Node != f.Node) {
			continue
		}
		out = append(out, clone(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// clone deep-copies a record through its JSON form so the double never
// aliases a caller's or a stored struct. Placement only holds JSON-safe
// values, so encoding cannot fail.
func clone(p *model.Placement) *model.Placement {
	data, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("fakestore: encode placement: %v", err))
	}
	var out model.Placement
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("fakestore: decode placement: %v", err))
	}
	return &out
}
