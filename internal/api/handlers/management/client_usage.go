package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
)

func (h *Handler) clientUsageTracker() *clientusage.Tracker {
	if h.clientUsage != nil {
		return h.clientUsage
	}
	return clientusage.Default()
}

// GetClientUsage reports usage per client API key (access.api-keys), including the
// Claude subscription usage attributed to each key in Claude Pro units.
func (h *Handler) GetClientUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	var opts clientusage.SnapshotOptions
	h.mu.Lock()
	if h.cfg != nil {
		opts.APIKeys = append([]string(nil), h.cfg.APIKeys...)
		if len(h.cfg.APIKeyNames) > 0 {
			opts.APIKeyNames = make(map[string]string, len(h.cfg.APIKeyNames))
			for apiKey, name := range h.cfg.APIKeyNames {
				opts.APIKeyNames[apiKey] = name
			}
		}
	}
	manager := h.authManager
	h.mu.Unlock()
	if manager != nil {
		opts.Credential = func(authID string) (clientusage.CredentialInfo, bool) {
			auth, ok := manager.GetByID(authID)
			if !ok || auth == nil {
				return clientusage.CredentialInfo{}, false
			}
			return clientusage.CredentialInfoFromAuth(auth), true
		}
	}

	c.JSON(http.StatusOK, h.clientUsageTracker().Snapshot(opts))
}

// DeleteClientUsage resets the usage of one client key (?id=<key id>) or of every key
// (?all=true).
func (h *Handler) DeleteClientUsage(c *gin.Context) {
	h.resetClientUsage(c, func(tracker *clientusage.Tracker, id string) bool { return tracker.Reset(id) })
}

// ResetClientUsageWindow ends the current Claude allowance window of one client key
// (?id=<key id>) or of every key (?all=true), so the key's current usage is zero and
// its next Claude request opens a fresh window. Totals and daily history are kept.
func (h *Handler) ResetClientUsageWindow(c *gin.Context) {
	h.resetClientUsage(c, func(tracker *clientusage.Tracker, id string) bool { return tracker.ResetWindow(id) })
}

func (h *Handler) resetClientUsage(c *gin.Context, reset func(tracker *clientusage.Tracker, id string) bool) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	id := strings.TrimSpace(c.Query("id"))
	if id == "" && !strings.EqualFold(strings.TrimSpace(c.Query("all")), "true") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id or all=true is required"})
		return
	}
	if !reset(h.clientUsageTracker(), id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "client usage not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
