package cliproxy

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

	// "a" sorts first, so only quota-aware ranking (not the round-robin fallback) picks "b".
	now := time.Now()
	auths := []*coreauth.Auth{
		{ID: "a", Provider: "codex", Status: coreauth.StatusActive},
		{ID: "b", Provider: "codex", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "20",
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
			},
		}},
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"wrapped"}}}
	picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("Pick() = %v, %v; want b", picked, errPick)
	}
}
