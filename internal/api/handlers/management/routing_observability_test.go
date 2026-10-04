package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetRoutingObservability(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	request := func(h *Handler) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/routing", nil)
		h.GetRoutingObservability(ctx)
		return rec
	}

	cfg := &config.Config{AuthDir: t.TempDir()}
	if rec := request(NewHandler(cfg, "", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without manager = %d, want 503", rec.Code)
	}

	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.WeightedRoundRobinSelector{},
		TTL:      30 * time.Minute,
	})
	defer selector.Stop()
	manager := coreauth.NewManager(nil, selector, nil)
	rec := request(NewHandler(cfg, "", manager))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ObservedAt      time.Time `json:"observed_at"`
		Mode            string    `json:"mode"`
		Strategy        string    `json:"strategy"`
		SessionAffinity struct {
			Enabled        bool  `json:"enabled"`
			TTLSeconds     int64 `json:"ttl_seconds"`
			ActiveSessions int   `json:"active_sessions"`
		} `json:"session_affinity"`
		Counters map[string]int64 `json:"counters"`
		Recent   []any            `json:"recent"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if payload.ObservedAt.IsZero() || payload.Mode != "local" || payload.Strategy != "weighted-round-robin" {
		t.Fatalf("payload = %+v", payload)
	}
	if !payload.SessionAffinity.Enabled || payload.SessionAffinity.TTLSeconds != 1800 || payload.SessionAffinity.ActiveSessions != 0 {
		t.Fatalf("session affinity = %+v", payload.SessionAffinity)
	}
	if _, ok := payload.Counters["failovers"]; !ok || payload.Recent == nil {
		t.Fatalf("counters/recent missing: %s", rec.Body.String())
	}
}
