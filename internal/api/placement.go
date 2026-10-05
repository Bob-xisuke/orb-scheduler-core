package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Error codes published by the placements API.
const (
	codeInvalidInput = "InvalidPlacementInputError"
	codeNotFound     = "PlacementNotFoundError"
	codeConflict     = "PlacementConflictError"
	codeStorageDown  = "storage_unavailable"
)

var errInvalidPlacement = errors.New("invalid placement input")

type placementHandler struct {
	svc *service.Service
}

func writeAPIError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// createPlacement handles POST /v1/placements. The transport layer only
// parses and validates the request and maps the business acceptance outcome
// to its HTTP status; scheduling and idempotency/conflict decisions belong to
// service.Accept.
func (h *placementHandler) createPlacement(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "invalid placement input")
		return
	}
	p, err := parsePlacement(body)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "invalid placement input")
		return
	}

	stored, outcome, err := h.svc.Accept(c.Request.Context(), p)
	if err != nil {
		writeAPIError(c, http.StatusServiceUnavailable, codeStorageDown, "database is not available")
		return
	}
	switch outcome {
	case service.Created:
		c.JSON(http.StatusCreated, stored)
	case service.Identical:
		c.JSON(http.StatusOK, stored)
	case service.Conflict:
		writeAPIError(c, http.StatusConflict, codeConflict,
			"a placement with this namespace and name already exists with different content")
	}
}

// listPlacements handles GET /v1/placements, both the single-record form
// (namespace+name) and the filtered-list form. The transport layer only
// decodes the raw query string and maps the business result to its HTTP
// status; all parameter rules and filtering belong to service.Query.
func (h *placementHandler) listPlacements(c *gin.Context) {
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "invalid placement query")
		return
	}

	result, err := h.svc.Query(c.Request.Context(), values)
	if errors.Is(err, service.ErrInvalidPlacementQuery) {
		writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "invalid placement query")
		return
	}
	if errors.Is(err, service.ErrPlacementNotFound) {
		writeAPIError(c, http.StatusNotFound, codeNotFound, "placement not found")
		return
	}
	if err != nil {
		writeAPIError(c, http.StatusServiceUnavailable, codeStorageDown, "database is not available")
		return
	}
	if result.Record != nil {
		c.JSON(http.StatusOK, result.Record)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": result.Items})
}

// ---------------------------------------------------------------------------
// Strict request parsing.
//
// The token walk below enforces, before anything is stored:
//   - the body is one JSON object with exactly the documented members,
//   - no unknown or duplicate member names at any level,
//   - no trailing data after the root object,
//   - every value has the declared JSON type and range (null is never accepted
//     where a value is required; omitted selector/labels default to {}),
//   - node names are unique.
// ---------------------------------------------------------------------------

var topLevelFields = map[string]bool{
	"namespace": true,
	"name":      true,
	"queue":     true,
	"priority":  true,
	"resources": true,
	"selector":  true,
	"nodes":     true,
}

var resourceFields = map[string]bool{"cpu": true, "memory": true}

var nodeFields = map[string]bool{
	"name":   true,
	"cpu":    true,
	"memory": true,
	"labels": true,
}

func parsePlacement(body []byte) (*store.Placement, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, errInvalidPlacement
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errInvalidPlacement
	}

	p := &store.Placement{}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, errInvalidPlacement
		}
		key, ok := keyTok.(string)
		if !ok || !topLevelFields[key] || seen[key] {
			return nil, errInvalidPlacement
		}
		seen[key] = true

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errInvalidPlacement
		}

		switch key {
		case "namespace", "name", "queue":
			s, err := parseTrimmedString(raw)
			if err != nil {
				return nil, err
			}
			switch key {
			case "namespace":
				p.Namespace = s
			case "name":
				p.Name = s
			case "queue":
				p.Queue = s
			}
		case "priority":
			n, err := parseBoundedInt(raw, 32)
			if err != nil {
				return nil, err
			}
			p.Priority = int32(n)
		case "resources":
			r, err := parseResources(raw)
			if err != nil {
				return nil, err
			}
			p.Resources = r
		case "selector":
			m, err := parseStringMap(raw)
			if err != nil {
				return nil, err
			}
			p.Selector = m
		case "nodes":
			nodes, err := parseNodes(raw)
			if err != nil {
				return nil, err
			}
			p.Nodes = nodes
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, errInvalidPlacement // closing '}'
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errInvalidPlacement // trailing data
	}

	for _, required := range []string{"namespace", "name", "queue", "priority", "resources", "nodes"} {
		if !seen[required] {
			return nil, errInvalidPlacement
		}
	}
	if p.Selector == nil {
		p.Selector = map[string]string{}
	}
	return p, nil
}

func parseResources(raw json.RawMessage) (store.Resources, error) {
	var r store.Resources
	obj, err := parseStrictObject(raw, resourceFields)
	if err != nil {
		return r, err
	}
	cpuRaw, ok := obj["cpu"]
	if !ok {
		return r, errInvalidPlacement
	}
	memoryRaw, ok := obj["memory"]
	if !ok {
		return r, errInvalidPlacement
	}
	cpu, err := parseBoundedInt(cpuRaw, 64)
	if err != nil || cpu <= 0 {
		return r, errInvalidPlacement
	}
	memory, err := parseBoundedInt(memoryRaw, 64)
	if err != nil || memory <= 0 {
		return r, errInvalidPlacement
	}
	return store.Resources{CPU: cpu, Memory: memory}, nil
}

func parseNodes(raw json.RawMessage) ([]store.Node, error) {
	if isNull(raw) {
		return nil, errInvalidPlacement
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, errInvalidPlacement
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errInvalidPlacement
	}

	nodes := []store.Node{}
	names := map[string]bool{}
	for dec.More() {
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return nil, errInvalidPlacement
		}
		obj, err := parseStrictObject(elem, nodeFields)
		if err != nil {
			return nil, err
		}
		nameRaw, ok := obj["name"]
		if !ok {
			return nil, errInvalidPlacement
		}
		cpuRaw, ok := obj["cpu"]
		if !ok {
			return nil, errInvalidPlacement
		}
		memoryRaw, ok := obj["memory"]
		if !ok {
			return nil, errInvalidPlacement
		}
		name, err := parseTrimmedString(nameRaw)
		if err != nil {
			return nil, err
		}
		cpu, err := parseBoundedInt(cpuRaw, 64)
		if err != nil || cpu < 0 {
			return nil, errInvalidPlacement
		}
		memory, err := parseBoundedInt(memoryRaw, 64)
		if err != nil || memory < 0 {
			return nil, errInvalidPlacement
		}
		labels := map[string]string{}
		if labelRaw, ok := obj["labels"]; ok {
			labels, err = parseStringMap(labelRaw)
			if err != nil {
				return nil, err
			}
		}
		if names[name] {
			return nil, errInvalidPlacement
		}
		names[name] = true
		nodes = append(nodes, store.Node{Name: name, CPU: cpu, Memory: memory, Labels: labels})
	}
	if _, err := dec.Token(); err != nil {
		return nil, errInvalidPlacement // closing ']'
	}
	return nodes, nil
}

// parseStrictObject decodes an object whose member names are restricted to
// allowed. Both unknown and duplicate names are rejected.
func parseStrictObject(raw json.RawMessage, allowed map[string]bool) (map[string]json.RawMessage, error) {
	if isNull(raw) {
		return nil, errInvalidPlacement
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, errInvalidPlacement
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errInvalidPlacement
	}
	return readObjectMembers(dec, allowed)
}

// parseStringMap decodes an object with arbitrary string keys and string
// values; null or non-string values are rejected.
func parseStringMap(raw json.RawMessage) (map[string]string, error) {
	if isNull(raw) {
		return nil, errInvalidPlacement
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, errInvalidPlacement
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errInvalidPlacement
	}

	out := map[string]string{}
	raws, err := readObjectMembers(dec, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range raws {
		s, err := parseJSONString(value)
		if err != nil {
			return nil, err
		}
		out[key] = s
	}
	return out, nil
}

func readObjectMembers(dec *json.Decoder, allowed map[string]bool) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, errInvalidPlacement
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errInvalidPlacement
		}
		if allowed != nil && !allowed[key] {
			return nil, errInvalidPlacement
		}
		if _, dup := out[key]; dup {
			return nil, errInvalidPlacement
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errInvalidPlacement
		}
		out[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, errInvalidPlacement // closing '}'
	}
	return out, nil
}

// parseTrimmedString accepts a JSON string that is non-empty and has no
// leading or trailing Unicode whitespace.
func parseTrimmedString(raw json.RawMessage) (string, error) {
	s, err := parseJSONString(raw)
	if err != nil {
		return "", err
	}
	if s == "" || s != strings.TrimSpace(s) {
		return "", errInvalidPlacement
	}
	return s, nil
}

func parseJSONString(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", errInvalidPlacement
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errInvalidPlacement
	}
	return s, nil
}

// parseBoundedInt accepts an integral JSON number without fraction or exponent
// that fits a signed integer of the given bit size (32 or 64).
func parseBoundedInt(raw json.RawMessage, bitSize int) (int64, error) {
	if isNull(raw) {
		return 0, errInvalidPlacement
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return 0, errInvalidPlacement
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, errInvalidPlacement
	}
	n, err := strconv.ParseInt(number.String(), 10, bitSize)
	if err != nil {
		return 0, errInvalidPlacement
	}
	return n, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
