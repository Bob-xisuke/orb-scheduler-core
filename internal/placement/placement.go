// Package placement owns the acceptance flow for placement submissions. It
// sits between the HTTP interface and the store: given an already validated
// placement request it computes the scheduling outcome, registers the record
// and reports how the submission was accepted. It has no dependency on HTTP
// request or response objects, so it can be verified on its own.
package placement

import (
	"context"
	"fmt"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Scheduling outcomes stored on every record.
const (
	statusPlaced   = "placed"
	statusRejected = "rejected"
	reasonNoNode   = "no_eligible_node"
)

// Outcome reports how a submission was accepted.
type Outcome int

const (
	// Created means the (namespace, name) identity was new and the record
	// was stored.
	Created Outcome = iota
	// Duplicate means identical content was already stored; the stored
	// record is returned and nothing is written.
	Duplicate
	// Conflict means the identity exists with different content; nothing is
	// written and the pre-existing record is returned unchanged.
	Conflict
)

// String renders the outcome for logs and tests.
func (o Outcome) String() string {
	switch o {
	case Created:
		return "created"
	case Duplicate:
		return "duplicate"
	case Conflict:
		return "conflict"
	}
	return "unknown"
}

// Service is the acceptance entry point for placement submissions.
type Service struct {
	st *store.Store
}

// NewService builds the acceptance entry point on top of st.
func NewService(st *store.Store) *Service {
	return &Service{st: st}
}

// Accept runs the trial placement for p — which must already be validated —
// and registers the result. It returns the stored record and how the
// submission was accepted:
//
//   - Created: first submission for the identity; the record was stored.
//   - Duplicate: same identity and same content; the previously stored
//     record is returned and nothing is written.
//   - Conflict: same identity but different content; the pre-existing record
//     is returned unchanged and nothing is written.
//
// A non-nil error means the storage layer is unavailable; no outcome is
// meaningful in that case.
func (s *Service) Accept(ctx context.Context, p *store.Placement) (*store.Placement, Outcome, error) {
	p.Status, p.Node, p.Reason = decide(p)

	stored, created, err := s.st.Submit(ctx, p)
	if err != nil {
		return nil, Created, fmt.Errorf("submit placement: %w", err)
	}
	if created {
		return stored, Created, nil
	}

	same, err := store.SameInput(p, stored)
	if err != nil {
		return nil, Created, fmt.Errorf("compare placement: %w", err)
	}
	if !same {
		return stored, Conflict, nil
	}
	return stored, Duplicate, nil
}

// decide performs the trial placement: among nodes whose labels contain every
// selector pair and whose capacity fits the request, it picks the name that is
// smallest in UTF-8 byte order. Capacity is only probed and never deducted.
func decide(p *store.Placement) (status string, node *string, reason *string) {
	var chosen string
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if !labelsSatisfy(n.Labels, p.Selector) {
			continue
		}
		if n.CPU < p.Resources.CPU || n.Memory < p.Resources.Memory {
			continue
		}
		if chosen == "" || n.Name < chosen {
			chosen = n.Name
		}
	}
	if chosen != "" {
		n := chosen
		return statusPlaced, &n, nil
	}
	r := reasonNoNode
	return statusRejected, nil, &r
}

func labelsSatisfy(labels, selector map[string]string) bool {
	for key, want := range selector {
		got, ok := labels[key]
		if !ok || got != want {
			return false
		}
	}
	return true
}
