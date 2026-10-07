package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestClientUsageEndpoints(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-max",
		Provider: "claude",
		Label:    "Personal",
		Metadata: map[string]any{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"},
	}); errRegister != nil {
		t.Fatalf("register claude auth: %v", errRegister)
	}

	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.APIKeys = []string{"laptop-key-0001", "phone-key-0002"}
	cfg.APIKeyNames = map[string]string{"laptop-key-0001": "Laptop"}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.clientUsage = clientusage.NewTracker()

	resetAt := strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)
	publish := func(apiKey, traceID, utilization string) {
		h.clientUsage.HandleUsage(context.Background(), coreusage.Record{
			TraceID:     traceID,
			RequestedAt: time.Now(),
			Provider:    "claude",
			Model:       "claude-sonnet-4-5",
			APIKey:      apiKey,
			AuthID:      "claude-max",
			Detail:      coreusage.Detail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Utilization": []string{utilization},
				"Anthropic-Ratelimit-Unified-7d-Reset":       []string{resetAt},
			},
		})
	}
	publish("laptop-key-0001", "t1", "0.40")
	publish("laptop-key-0001", "t2", "0.45")

	get := func() clientusage.Snapshot {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/clients", nil)
		h.GetClientUsage(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
		}
		var snapshot clientusage.Snapshot
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &snapshot); errDecode != nil {
			t.Fatalf("decode: %v", errDecode)
		}
		return snapshot
	}
	del := func(query string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/v8/management/observability/usage/clients"+query, nil)
		h.DeleteClientUsage(ctx)
		return rec.Code
	}

	snapshot := get()
	if len(snapshot.Keys) != 2 {
		t.Fatalf("keys = %+v, want both configured keys", snapshot.Keys)
	}
	laptop := snapshot.Keys[0]
	if laptop.Name != "Laptop" || laptop.Key != "lapt...0001" || laptop.Totals.Requests != 2 || laptop.Totals.Tokens.Total != 240 {
		t.Fatalf("laptop = %+v", laptop)
	}
	if laptop.Claude == nil || len(laptop.Claude.Credentials) != 1 {
		t.Fatalf("laptop claude usage = %+v", laptop.Claude)
	}
	credential := laptop.Claude.Credentials[0]
	if credential.Label != "Personal" || credential.Plan != clientusage.PlanMax5x || credential.PlanProUnits != 2 {
		t.Fatalf("credential = %+v", credential)
	}
	// 5% of a Max 5x weekly limit is 0.1 Pro units.
	if credential.CurrentFraction != 0.05 || laptop.Claude.CurrentProUnits != 0.1 {
		t.Fatalf("claude usage = %+v", laptop.Claude)
	}
	if phone := snapshot.Keys[1]; phone.Name != "" || phone.Totals.Requests != 0 || !phone.Configured {
		t.Fatalf("phone = %+v", phone)
	}
	body, _ := json.Marshal(snapshot)
	for _, secret := range cfg.APIKeys {
		if strings.Contains(string(body), secret) {
			t.Fatalf("response leaks raw key %q", secret)
		}
	}

	if code := del("?id=unknown"); code != http.StatusNotFound {
		t.Fatalf("DELETE unknown id status = %d, want 404", code)
	}
	if code := del("?id=" + laptop.ID); code != http.StatusOK {
		t.Fatalf("DELETE id status = %d, want 200", code)
	}
	if got := get().Keys[0]; got.Totals.Requests != 0 || got.Claude != nil {
		t.Fatalf("laptop after reset = %+v", got)
	}
	if code := del(""); code != http.StatusBadRequest {
		t.Fatalf("DELETE without id or all status = %d, want 400", code)
	}
	if code := del("?all=true"); code != http.StatusOK {
		t.Fatalf("DELETE all status = %d, want 200", code)
	}
}
