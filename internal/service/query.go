package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

// Query business errors, both recognizable with errors.Is.
var (
	// ErrInvalidPlacementQuery is returned when the parameter set is not one
	// of the documented combinations. Validation happens before any storage
	// access, so an invalid query never reaches the store.
	ErrInvalidPlacementQuery = errors.New("invalid placement query")
	// ErrPlacementNotFound is returned by the single-record form when the
	// (namespace, name) identity is unknown.
	ErrPlacementNotFound = errors.New("placement not found")
)

// QueryResult carries the outcome of Query. Exactly one field is non-nil on
// success: Record for the single-record form, Items for the list form. On
// failure both are nil. Items is never nil on a successful list, even when
// nothing matched.
type QueryResult struct {
	Record *model.Placement
	Items  []*model.Placement
}

// Query is the transport-independent read entry point. values holds the
// already-decoded query parameters with duplicates preserved; it is only
// read, never modified, and its values are matched exactly as given — no
// trimming and no further decoding.
//
// Only the four documented keys are accepted. With name, namespace is the
// only other allowed key and the result is the single stored record or
// ErrPlacementNotFound. Without name, namespace, queue and node intersect as
// filters, omitted conditions are ignored, and a nil or empty set lists
// everything. Lists are ordered by namespace then name in UTF-8 byte order;
// a node filter never matches a rejected record. Unknown keys, repeated or
// empty values and illegal combinations yield ErrInvalidPlacementQuery, and
// any persistence failure yields ErrStorageUnavailable.
func (s *Service) Query(ctx context.Context, values url.Values) (QueryResult, error) {
	params, err := validateQuery(values)
	if err != nil {
		return QueryResult{}, err
	}

	if name, hasName := params["name"]; hasName {
		rec, err := s.st.Get(ctx, params["namespace"], name)
		if errors.Is(err, model.ErrNotFound) {
			return QueryResult{}, fmt.Errorf("query placement: %w", ErrPlacementNotFound)
		}
		if err != nil {
			return QueryResult{}, queryStorageError(err)
		}
		return QueryResult{Record: rec}, nil
	}

	recs, err := s.st.List(ctx, model.ListFilter{
		Namespace: params["namespace"],
		Queue:     params["queue"],
		Node:      params["node"],
	})
	if err != nil {
		return QueryResult{}, queryStorageError(err)
	}
	if recs == nil {
		recs = []*model.Placement{}
	}
	return QueryResult{Items: recs}, nil
}

var allowedQueryKeys = map[string]bool{
	"namespace": true,
	"name":      true,
	"queue":     true,
	"node":      true,
}

// validateQuery accepts only the four documented keys, each exactly once with
// a non-empty value, and only the documented combinations: name requires
// namespace and forbids queue and node.
func validateQuery(values url.Values) (map[string]string, error) {
	params := make(map[string]string, len(values))
	for key, vals := range values {
		if !allowedQueryKeys[key] || len(vals) != 1 || vals[0] == "" {
			return nil, fmt.Errorf("query placement: %w", ErrInvalidPlacementQuery)
		}
		params[key] = vals[0]
	}
	if _, hasName := params["name"]; hasName {
		_, hasNamespace := params["namespace"]
		_, hasQueue := params["queue"]
		_, hasNode := params["node"]
		if !hasNamespace || hasQueue || hasNode {
			return nil, fmt.Errorf("query placement: %w", ErrInvalidPlacementQuery)
		}
	}
	return params, nil
}

func queryStorageError(err error) error {
	return fmt.Errorf("query placements: %w: %w", ErrStorageUnavailable, err)
}
