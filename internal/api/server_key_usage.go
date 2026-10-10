package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
)

// keyUsageAccountWait is how long after their lookup started a key usage report waits
// for Claude accounts that have never been read. The 5-hour and Fable figures share
// one wait, which stays under the 3 seconds the example status line script allows a
// request. The lookups continue in the background either way.
const keyUsageAccountWait = 2 * time.Second

// keyUsage serves GET /v1/key/usage to the holder of a client API key: the key's own
// Claude allowance, the Claude 5-hour limit left across all the Claude accounts and
// the Fable allowance left across the accounts that serve Fable, as JSON, as plain
// text with ?format=text, or as one status bar line with ?format=line (&color=1 for
// ANSI colors).
func (s *Server) keyUsage(c *gin.Context) {
	cfg := s.getConfig()
	if cfg != nil && cfg.Home.Enabled {
		// Home tracks client keys and holds the credentials; this node has neither.
		c.JSON(http.StatusNotImplemented, gin.H{"error": "key usage is not available when CLIProxyAPIHome manages this proxy"})
		return
	}
	apiKey := clientKeyFromGin(c)
	var names map[string]string
	if cfg != nil {
		names = cfg.APIKeyNames
	}
	report := keyusage.Build(c.Request.Context(), apiKey, keyusage.KeyName(names, apiKey), clientusage.Default(), s.usagePool, keyUsageAccountWait)
	c.Header("Cache-Control", "no-store")
	switch strings.ToLower(strings.TrimSpace(c.Query("format"))) {
	case "text":
		c.String(http.StatusOK, report.Text())
	case "line":
		c.String(http.StatusOK, report.Line(c.Query("color") == "1" || strings.EqualFold(c.Query("color"), "true"))+"\n")
	default:
		c.JSON(http.StatusOK, report)
	}
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
