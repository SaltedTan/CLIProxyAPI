package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetRoutingObservability reports the live routing strategy, session affinity state,
// counters, and recent local credential selections.
func (h *Handler) GetRoutingObservability(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	c.JSON(http.StatusOK, h.authManager.RoutingObservability())
}
