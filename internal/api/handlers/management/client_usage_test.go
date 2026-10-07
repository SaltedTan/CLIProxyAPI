package management

import (
	"context"
	"encoding/json"
	"errors"
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
	// Admission prices usage with the tracker's resolver, as the service wires it.
	h.clientUsage.SetCredentialResolver(func(authID string) (clientusage.CredentialInfo, bool) {
		auth, ok := manager.GetByID(authID)
		if !ok || auth == nil {
			return clientusage.CredentialInfo{}, false
		}
		return clientusage.CredentialInfoFromAuth(auth), true
	})

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
	// The laptop spent 0.1 Pro units; a 0.05 limit is reached. The tablet key is
	// not configured and has no usage, so it is listed only because of its limit.
	h.clientUsage.SetLimits(map[string]float64{"laptop-key-0001": 0.05, clientusage.KeyID("tablet-key-0003"): 0.5})

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
	if !snapshot.ClaudeLimitsSupported {
		t.Fatal("claude_limits_supported must be true")
	}
	if len(snapshot.Keys) != 3 {
		t.Fatalf("keys = %+v, want both configured keys and the limit-only key", snapshot.Keys)
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
	if laptop.Claude.LimitProUnits == nil || *laptop.Claude.LimitProUnits != 0.05 || !laptop.Claude.LimitReached || laptop.Claude.RemainingProUnits == nil || *laptop.Claude.RemainingProUnits != 0 || laptop.Claude.LimitResetsAt == nil {
		t.Fatalf("laptop limit fields = %+v", laptop.Claude)
	}
	if phone := snapshot.Keys[1]; phone.Name != "" || phone.Totals.Requests != 0 || !phone.Configured || phone.Claude != nil {
		t.Fatalf("phone = %+v", phone)
	}
	tablet := snapshot.Keys[2]
	if tablet.ID != clientusage.KeyID("tablet-key-0003") || tablet.Configured || tablet.Key != "" || tablet.Claude == nil || *tablet.Claude.LimitProUnits != 0.5 || tablet.Claude.LimitReached || len(tablet.Claude.Credentials) != 0 {
		t.Fatalf("limit-only tablet = %+v claude = %+v", tablet, tablet.Claude)
	}

	// A refused request shows up as blocked without touching requests or failed.
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("userApiKey", "laptop-key-0001")
	var quota *coreauth.ClientQuotaError
	if errAdmit := h.clientUsage.Admit(context.WithValue(context.Background(), "gin", ginCtx), &coreauth.Auth{ID: "claude-max", Provider: "claude"}); !errors.As(errAdmit, &quota) {
		t.Fatalf("admit error = %v, want a client quota error", errAdmit)
	}
	if strings.Contains(quota.Error(), "laptop-key-0001") {
		t.Fatalf("refusal message leaks the key: %s", quota.Error())
	}
	snapshot = get()
	laptop = snapshot.Keys[0]
	if laptop.Totals.Blocked != 1 || laptop.Totals.Requests != 2 || laptop.Totals.Failed != 0 || len(laptop.Daily) != 1 || laptop.Daily[0].Blocked != 1 {
		t.Fatalf("laptop after refusal = %+v daily = %+v", laptop.Totals, laptop.Daily)
	}
	body, _ := json.Marshal(snapshot)
	for _, secret := range append(cfg.APIKeys, "tablet-key-0003") {
		if strings.Contains(string(body), secret) {
			t.Fatalf("response leaks raw key %q", secret)
		}
	}
	if !strings.Contains(string(body), `"blocked":1`) || !strings.Contains(string(body), `"claude_limits_supported":true`) {
		t.Fatalf("response = %s", body)
	}

	if code := del("?id=unknown"); code != http.StatusNotFound {
		t.Fatalf("DELETE unknown id status = %d, want 404", code)
	}
	if code := del("?id=" + laptop.ID); code != http.StatusOK {
		t.Fatalf("DELETE id status = %d, want 200", code)
	}
	// Resetting clears usage and blocked counts; the limit itself stays configured.
	if got := get().Keys[0]; got.Totals.Requests != 0 || got.Totals.Blocked != 0 || got.Claude == nil || got.Claude.LimitReached || len(got.Claude.Credentials) != 0 || *got.Claude.LimitProUnits != 0.05 {
		t.Fatalf("laptop after reset = %+v claude = %+v", got, got.Claude)
	}
	if errAdmit := h.clientUsage.Admit(context.WithValue(context.Background(), "gin", ginCtx), &coreauth.Auth{ID: "claude-max", Provider: "claude"}); errAdmit != nil {
		t.Fatalf("a reset key must be re-admitted: %v", errAdmit)
	}
	if code := del(""); code != http.StatusBadRequest {
		t.Fatalf("DELETE without id or all status = %d, want 400", code)
	}
	if code := del("?all=true"); code != http.StatusOK {
		t.Fatalf("DELETE all status = %d, want 200", code)
	}
}
