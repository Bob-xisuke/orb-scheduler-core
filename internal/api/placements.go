package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// Error codes published by the placements API.
const (
	codeInvalidInput       = "InvalidPlacementInputError"
	codeNotFound           = "PlacementNotFoundError"
	codeConflict           = "PlacementConflictError"
	codeStorageUnavailable = "storage_unavailable"
)

const (
	jsonContentType       = "application/json; charset=utf-8"
	maxPlacementBodyBytes = 8 << 20
	reasonNoEligibleNode  = "no_eligible_node"
	statusPlaced          = "placed"
	statusRejected        = "rejected"
)

// inputError describes a rejected placement payload or query string. The message is
// always a static, client-safe sentence: no SQL, stack traces, or file paths.
type inputError struct{ message string }

func (e *inputError) Error() string { return e.message }

func invalid(message string) *inputError { return &inputError{message: message} }

type nodeSpec struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"`
	Memory int64             `json:"memory"`
	Labels map[string]string `json:"labels"`
}

type resourceRequest struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
}

// placementRequest is the canonical form of a submission: defaults are filled in
// (empty selector/labels, non-nil nodes slice) so two requests marshal to identical
// JSON exactly when they are semantically equal.
type placementRequest struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Queue     string            `json:"queue"`
	Priority  int32             `json:"priority"`
	Resources resourceRequest   `json:"resources"`
	Selector  map[string]string `json:"selector"`
	Nodes     []nodeSpec        `json:"nodes"`
}

// placementRecord is a request plus the scheduling decision, as stored and returned.
type placementRecord struct {
	placementRequest
	Status string  `json:"status"`
	Node   *string `json:"node"`
	Reason *string `json:"reason"`
}

type placementsHandler struct {
	st *store.Store
}

func (h *placementsHandler) create(c *gin.Context) {
	req, err := decodePlacementRequest(c.Request.Body)
	if err != nil {
		writeInputError(c, err)
		return
	}
	record := newPlacementRecord(req)
	requestJSON, err := json.Marshal(req)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidInput, "request body could not be encoded")
		return
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidInput, "request body could not be encoded")
		return
	}
	storedRequest, storedRecord, created, err := h.st.SavePlacement(req.Namespace, req.Name, req.Queue, record.Node, requestJSON, recordJSON)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, "database is not available")
		return
	}
	switch {
	case created:
		c.Data(http.StatusCreated, jsonContentType, recordJSON)
	case bytes.Equal(storedRequest, requestJSON):
		c.Data(http.StatusOK, jsonContentType, storedRecord)
	default:
		writeError(c, http.StatusConflict, codeConflict, "a different placement already exists for this namespace and name")
	}
}

func (h *placementsHandler) get(c *gin.Context) {
	filters, err := parsePlacementQuery(c.Request.URL.Query())
	if err != nil {
		writeInputError(c, err)
		return
	}
	if filters.name != nil {
		record, found, err := h.st.GetPlacement(*filters.namespace, *filters.name)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, "database is not available")
			return
		}
		if !found {
			writeError(c, http.StatusNotFound, codeNotFound, "placement not found")
			return
		}
		c.Data(http.StatusOK, jsonContentType, record)
		return
	}
	records, err := h.st.ListPlacements(filters.namespace, filters.queue, filters.node)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, "database is not available")
		return
	}
	var buf bytes.Buffer
	buf.WriteString(`{"items":[`)
	for i, record := range records {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(record)
	}
	buf.WriteString(`]}`)
	c.Data(http.StatusOK, jsonContentType, buf.Bytes())
}

// decodePlacementRequest parses and validates a submission. Any violation — malformed
// JSON, trailing data, unknown or missing fields, type or range errors, duplicate node
// names — yields an *inputError and nothing is saved.
func decodePlacementRequest(body io.Reader) (*placementRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxPlacementBodyBytes+1))
	if err != nil {
		return nil, invalid("request body could not be read")
	}
	if len(raw) > maxPlacementBodyBytes {
		return nil, invalid("request body is too large")
	}
	var fields struct {
		Namespace json.RawMessage `json:"namespace"`
		Name      json.RawMessage `json:"name"`
		Queue     json.RawMessage `json:"queue"`
		Priority  json.RawMessage `json:"priority"`
		Resources json.RawMessage `json:"resources"`
		Selector  json.RawMessage `json:"selector"`
		Nodes     json.RawMessage `json:"nodes"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fields); err != nil {
		return nil, invalid("request body must be a single JSON object")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, invalid("request body must not contain trailing data")
	}

	req := &placementRequest{}
	if req.Namespace, err = requiredName(fields.Namespace, "namespace"); err != nil {
		return nil, err
	}
	if req.Name, err = requiredName(fields.Name, "name"); err != nil {
		return nil, err
	}
	if req.Queue, err = requiredName(fields.Queue, "queue"); err != nil {
		return nil, err
	}
	priority, err := requiredInt(fields.Priority, "priority", math.MinInt32, math.MaxInt32)
	if err != nil {
		return nil, err
	}
	req.Priority = int32(priority)
	if req.Resources, err = parseResources(fields.Resources); err != nil {
		return nil, err
	}
	if req.Selector, err = optionalStringMap(fields.Selector, "selector"); err != nil {
		return nil, err
	}
	if req.Nodes, err = parseNodes(fields.Nodes); err != nil {
		return nil, err
	}
	return req, nil
}

// requiredName validates the rule shared by namespace, name, queue, and node names:
// a non-empty string without leading or trailing whitespace.
func requiredName(raw json.RawMessage, field string) (string, error) {
	if raw == nil {
		return "", invalid(field + " is required")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalid(field + " must be a string")
	}
	if value == "" || strings.TrimSpace(value) != value {
		return "", invalid(field + " must be non-empty without leading or trailing whitespace")
	}
	return value, nil
}

// requiredInt validates a JSON integer within [min, max]. Strings, floats, and values
// outside the int64 range are rejected.
func requiredInt(raw json.RawMessage, field string, min, max int64) (int64, error) {
	if raw == nil {
		return 0, invalid(field + " is required")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return 0, invalid(field + " must be an integer")
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, invalid(field + " must be an integer")
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, invalid(field + " must be an integer")
	}
	if parsed < min || parsed > max {
		return 0, invalid(field + " is out of range")
	}
	return parsed, nil
}

func parseResources(raw json.RawMessage) (resourceRequest, error) {
	var out resourceRequest
	if raw == nil {
		return out, invalid("resources is required")
	}
	var fields struct {
		CPU    json.RawMessage `json:"cpu"`
		Memory json.RawMessage `json:"memory"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fields); err != nil {
		return out, invalid("resources must be an object")
	}
	var err error
	if out.CPU, err = requiredInt(fields.CPU, "resources.cpu", 1, math.MaxInt64); err != nil {
		return out, err
	}
	if out.Memory, err = requiredInt(fields.Memory, "resources.memory", 1, math.MaxInt64); err != nil {
		return out, err
	}
	return out, nil
}

// optionalStringMap validates a string-to-string object; an omitted field defaults to
// an empty object while an explicit null or non-string value is rejected.
func optionalStringMap(raw json.RawMessage, field string) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, invalid(field + " must be an object with string values")
	}
	return values, nil
}

func parseNodes(raw json.RawMessage) ([]nodeSpec, error) {
	if raw == nil {
		return nil, invalid("nodes is required")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, invalid("nodes must be an array")
	}
	nodes := make([]nodeSpec, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		var fields struct {
			Name   json.RawMessage `json:"name"`
			CPU    json.RawMessage `json:"cpu"`
			Memory json.RawMessage `json:"memory"`
			Labels json.RawMessage `json:"labels"`
		}
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fields); err != nil {
			return nil, invalid("each node must be an object")
		}
		name, err := requiredName(fields.Name, "node name")
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, invalid("node names must be unique")
		}
		seen[name] = struct{}{}
		cpu, err := requiredInt(fields.CPU, "node cpu", 0, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		memory, err := requiredInt(fields.Memory, "node memory", 0, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		labels, err := optionalStringMap(fields.Labels, "node labels")
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, nodeSpec{Name: name, CPU: cpu, Memory: memory, Labels: labels})
	}
	return nodes, nil
}

// newPlacementRecord runs the trial placement: among nodes whose labels cover the
// selector and whose capacity fits the request, the node with the smallest name in
// UTF-8 byte order wins. Capacity is never deducted.
func newPlacementRecord(req *placementRequest) *placementRecord {
	record := &placementRecord{placementRequest: *req}
	var best *nodeSpec
	for i := range req.Nodes {
		node := &req.Nodes[i]
		if node.CPU < req.Resources.CPU || node.Memory < req.Resources.Memory {
			continue
		}
		if !labelsCover(node.Labels, req.Selector) {
			continue
		}
		if best == nil || node.Name < best.Name {
			best = node
		}
	}
	if best == nil {
		record.Status = statusRejected
		reason := reasonNoEligibleNode
		record.Reason = &reason
		return record
	}
	record.Status = statusPlaced
	name := best.Name
	record.Node = &name
	return record
}

func labelsCover(labels, selector map[string]string) bool {
	for key, want := range selector {
		got, ok := labels[key]
		if !ok || got != want {
			return false
		}
	}
	return true
}

type placementQuery struct {
	namespace *string
	name      *string
	queue     *string
	node      *string
}

// parsePlacementQuery validates the query string: only namespace, name, queue, and
// node are known; each may appear at most once with a non-empty value; name may only
// be combined with namespace, and requires it.
func parsePlacementQuery(values url.Values) (*placementQuery, error) {
	query := &placementQuery{}
	for key, list := range values {
		if len(list) != 1 {
			return nil, invalid("query parameter " + key + " must appear exactly once")
		}
		value := list[0]
		if value == "" {
			return nil, invalid("query parameter " + key + " must not be empty")
		}
		switch key {
		case "namespace":
			query.namespace = &value
		case "name":
			query.name = &value
		case "queue":
			query.queue = &value
		case "node":
			query.node = &value
		default:
			return nil, invalid("unknown query parameter " + key)
		}
	}
	if query.name != nil && (query.namespace == nil || query.queue != nil || query.node != nil) {
		return nil, invalid("name requires namespace and cannot be combined with queue or node")
	}
	return query, nil
}

func writeInputError(c *gin.Context, err error) {
	message := "invalid placement input"
	var ie *inputError
	if errors.As(err, &ie) {
		message = ie.message
	}
	writeError(c, http.StatusBadRequest, codeInvalidInput, message)
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
