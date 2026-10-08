package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
)

// keyUsageFableWait is how long a key usage report waits for Fable accounts that
// have never been read. Their lookups continue in the background either way.
const keyUsageFableWait = 3 * time.Second

// keyUsage serves GET /v1/key/usage to the holder of a client API key: the key's own
// Claude allowance and the Fable allowance left across the accounts, as JSON, or as
// plain text with ?format=text.
func (s *Server) keyUsage(c *gin.Context) {
	apiKey := clientKeyFromGin(c)
	var names map[string]string
	if cfg := s.getConfig(); cfg != nil {
		names = cfg.APIKeyNames
	}
	report := keyusage.Build(c.Request.Context(), apiKey, keyusage.KeyName(names, apiKey), clientusage.Default(), s.fablePool, keyUsageFableWait)
	c.Header("Cache-Control", "no-store")
	if strings.EqualFold(strings.TrimSpace(c.Query("format")), "text") {
		c.String(http.StatusOK, report.Text())
		return
	}
	c.JSON(http.StatusOK, report)
}

// clientKeyFromGin returns the client API key the access middleware authenticated,
// or "" when the request carries none.
func clientKeyFromGin(c *gin.Context) string {
	value, ok := c.Get("userApiKey")
	if !ok {
		return ""
	}
	apiKey, _ := value.(string)
	return strings.TrimSpace(apiKey)
}
