package cliproxy

import (
	"context"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A token refresh rewrites the auth file and the watcher reloads it. The reload must keep
// the quota snapshot, so quota-aware routing still skips a credential near its 5h cap.
func TestPrepareCoreAuthForModelRegistrationKeepsQuotaAwareRanking(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	svc := &Service{cfg: &config.Config{}, coreManager: manager}

	// The selector clock is wall time; every reset is hours away from any boundary.
	now := time.Now()
	claudeAuth := func(id, token string) *coreauth.Auth {
		return &coreauth.Auth{
			ID:       id,
			Provider: "claude",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":              "claude",
				"access_token":      "access-" + token,
				"refresh_token":     "refresh-" + token,
				"account_uuid":      "acc-" + id,
				"organization_uuid": "org-" + id,
				"email":             id + "@example.com",
			},
		}
	}
	snapshot := func(fiveHourUsed, weeklyUsed string) coreauth.QuotaState {
		return coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": fiveHourUsed,
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": weeklyUsed,
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10),
		}}
	}
	// Ranked on weekly pace alone, the saturated credential would win (more quota left).
	saturated := claudeAuth("claude-saturated", "1")
	saturated.Quota = snapshot("0.95", "0.10")
	other := claudeAuth("claude-other", "1")
	other.Quota = snapshot("0.10", "0.50")
	for _, auth := range []*coreauth.Auth{saturated, other} {
		if svc.prepareCoreAuthForModelRegistration(ctx, auth) == nil {
			t.Fatalf("register %s failed", auth.ID)
		}
	}

	reloaded := claudeAuth("claude-saturated", "2")
	if svc.prepareCoreAuthForModelRegistration(ctx, reloaded) == nil {
		t.Fatal("reload of claude-saturated failed")
	}
	current, ok := manager.GetByID("claude-saturated")
	if !ok || current.Metadata["access_token"] != "access-2" {
		t.Fatalf("reloaded auth = %+v, want the new access token", current)
	}
	if !current.Quota.ObservedAt.Equal(now) {
		t.Fatalf("quota ObservedAt = %v, want %v", current.Quota.ObservedAt, now)
	}

	selector := coreauth.NewQuotaAwareSelector(nil)
	for i := 0; i < 3; i++ {
		picked, errPick := selector.Pick(ctx, "claude", "claude-sonnet-4-5", cliproxyexecutor.Options{}, manager.List())
		if errPick != nil {
			t.Fatalf("Pick() error = %v", errPick)
		}
		if picked.ID != "claude-other" {
			t.Fatalf("pick #%d = %s, want claude-other (claude-saturated is near its 5h cap)", i, picked.ID)
		}
	}
}
