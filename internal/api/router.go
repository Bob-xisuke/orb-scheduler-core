package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// NewRouter wires the public HTTP surface: health and the placements API. Every
// entry keeps the error shape described in README.md.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	h := &placementHandler{svc: service.New(st)}
	router.POST("/v1/placements", h.createPlacement)
	router.GET("/v1/placements", h.listPlacements)

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
