package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
)

// Error codes published by the placements API.
const (
	codeInvalidInput = "InvalidPlacementInputError"
	codeNotFound     = "PlacementNotFoundError"
	codeConflict     = "PlacementConflictError"
	codeStorageDown  = "storage_unavailable"
)

type placementHandler struct {
	svc *service.Service
}

func writeAPIError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// createPlacement handles POST /v1/placements. The transport layer only
// parses and validates the request and maps the business acceptance outcome
// to its HTTP status; scheduling and idempotency/conflict decisions belong to
// service.Accept. The strict parsing rules live in package placement and are
// shared with direct, non-HTTP callers.
func (h *placementHandler) createPlacement(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "invalid placement input")
		return
	}
	p, err := placement.ParsePlacementInput(body)
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
