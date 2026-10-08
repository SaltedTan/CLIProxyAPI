package cliproxy

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestQuotaAwareRoutingSelector(t *testing.T) {
	for _, raw := range []string{"quota-aware", " Quota-Aware ", "quotaaware", "qa", "reset-priority"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: raw},
		})
		if state.strategy != "quota-aware" {
			t.Fatalf("strategy(%q) = %q, want quota-aware", raw, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.QuotaAwareSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.QuotaAwareSelector", newRoutingSelector(state))
		}
	}
}

func TestQuotaAwareRoutingSelectorWrappedBySessionAffinity(t *testing.T) {
	subagents := false
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:                 "quota-aware",
			SessionAffinity:          true,
			SessionAffinityTTL:       "30m",
			SessionAffinitySubagents: &subagents,
		},
	})
	selector, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
	defer selector.Stop()

	// "a" sorts first, so only quota-aware ranking (not the round-robin fallback) picks "b",
	// whose weekly quota resets much sooner.
	now := time.Now()
	weeklyAuth := func(id string, resetIn time.Duration) *coreauth.Auth {
		return &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "20",
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(resetIn).Unix(), 10),
			},
		}}
	}
	auths := []*coreauth.Auth{weeklyAuth("a", 100*time.Hour), weeklyAuth("b", 2*time.Hour)}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"wrapped"}}}
	picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("Pick() = %v, %v; want b", picked, errPick)
	}
}

// Credentials without a quota snapshot, as after a restart, are ranked from the weekly
// readings the client usage tracker kept for them.
func TestQuotaAwareRoutingSelectorReadsClientUsageWeeklyQuota(t *testing.T) {
	now := time.Now()
	for id, reading := range map[string]struct {
		used    string
		resetIn time.Duration
	}{
		"qa-tracker-a": {used: "0.9", resetIn: 100 * time.Hour},
		"qa-tracker-b": {used: "0.1", resetIn: 50 * time.Hour},
	} {
		clientusage.Default().HandleUsage(context.Background(), usage.Record{
			Provider:    "claude",
			AuthID:      id,
			RequestedAt: now,
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Utilization": []string{reading.used},
				"Anthropic-Ratelimit-Unified-7d-Reset":       []string{strconv.FormatInt(now.Add(reading.resetIn).Unix(), 10)},
			},
		})
	}
	selector := newRoutingSelector(normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "quota-aware"},
	}))
	// "a" sorts first, so round-robin over credentials without data would pick it.
	auths := []*coreauth.Auth{
		{ID: "qa-tracker-a", Provider: "claude", Status: coreauth.StatusActive},
		{ID: "qa-tracker-b", Provider: "claude", Status: coreauth.StatusActive},
	}
	picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", cliproxyexecutor.Options{}, auths)
	if errPick != nil || picked == nil || picked.ID != "qa-tracker-b" {
		pickedID := ""
		if picked != nil {
			pickedID = picked.ID
		}
		t.Fatalf("Pick() = %q, %v; want qa-tracker-b", pickedID, errPick)
	}
}
