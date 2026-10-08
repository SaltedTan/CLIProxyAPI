package cliproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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
		if _, ok := newRoutingSelector(state, nil).(*coreauth.QuotaAwareSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.QuotaAwareSelector", newRoutingSelector(state, nil))
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
	selector, ok := newRoutingSelector(state, nil).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state, nil))
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

// resolveClientUsageCredentials makes the client usage tracker resolve Claude credentials
// from auths, as the service resolves them from its auth manager, until the test ends.
func resolveClientUsageCredentials(t *testing.T, auths ...*coreauth.Auth) {
	t.Helper()
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		byID[auth.ID] = auth
	}
	tracker := clientusage.Default()
	tracker.SetCredentialResolver(func(authID string) (clientusage.CredentialInfo, bool) {
		auth, ok := byID[authID]
		if !ok {
			return clientusage.CredentialInfo{}, false
		}
		return clientusage.CredentialInfoFromAuth(auth), true
	})
	t.Cleanup(func() { tracker.SetCredentialResolver(nil) })
}

// claudeAccountAuth returns an active Claude credential of its own account.
func claudeAccountAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{
		"organization_uuid": "org-" + id,
		"email":             id + "@example.com",
	}}
}

// Credentials without a quota snapshot, as after a restart, are ranked from the weekly
// readings the client usage tracker kept for them.
func TestQuotaAwareRoutingSelectorReadsClientUsageWeeklyQuota(t *testing.T) {
	now := time.Now()
	// "a" sorts first, so round-robin over credentials without data would pick it.
	auths := []*coreauth.Auth{claudeAccountAuth("qa-tracker-a"), claudeAccountAuth("qa-tracker-b")}
	resolveClientUsageCredentials(t, auths...)
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
	usageCache := keyusage.NewUsageCache(func() []*coreauth.Auth { return nil }, nil)
	selector := newRoutingSelector(normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "quota-aware"},
	}), usageCache)
	picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", cliproxyexecutor.Options{}, auths)
	if errPick != nil || picked == nil || picked.ID != "qa-tracker-b" {
		pickedID := ""
		if picked != nil {
			pickedID = picked.ID
		}
		t.Fatalf("Pick() = %q, %v; want qa-tracker-b", pickedID, errPick)
	}
}

// Quota-aware routing reads the tracker's saved readings first and the usage cache's
// endpoint readings second, so the saved reading wins a tie.
func TestQuotaAwareRoutingSourcesTrackerThenUsageCache(t *testing.T) {
	now := time.Now()
	auth := claudeAccountAuth("qa-sources-a")
	resolveClientUsageCredentials(t, auth)
	clientusage.Default().HandleUsage(context.Background(), usage.Record{
		Provider:    "claude",
		AuthID:      "qa-sources-a",
		RequestedAt: now,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.3"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       []string{strconv.FormatInt(now.Add(50*time.Hour).Unix(), 10)},
		},
	})

	if sources := quotaSources(nil); len(sources) != 1 {
		t.Fatalf("sources without a usage cache = %d, want the tracker only", len(sources))
	}
	sources := quotaSources(keyusage.NewUsageCache(func() []*coreauth.Auth { return nil }, nil))
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want the tracker and the usage cache", len(sources))
	}
	if reading, ok := sources[0](auth); !ok || reading.Source != "last-known" || reading.Weekly == nil || reading.Weekly.Used != 0.3 {
		t.Fatalf("first source reading = %+v (%v), want the tracker's", reading, ok)
	}
	// The usage cache has not read the account, unlike the tracker.
	if reading, ok := sources[1](auth); ok {
		t.Fatalf("second source reading = %+v, want the usage cache's (none)", reading)
	}
}

// The service's usage cache lists the service's credentials and runs only while the
// current config routes with quota-aware.
func TestBuilderBindsTheClaudeUsageCacheToTheService(t *testing.T) {
	cfg := &internalconfig.Config{AuthDir: t.TempDir(), Routing: internalconfig.RoutingConfig{Strategy: "quota-aware"}}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if service.claudeUsage == nil || !service.quotaAwareActive() {
		t.Fatalf("claudeUsage = %v, quotaAwareActive = %v", service.claudeUsage, service.quotaAwareActive())
	}

	const authID = "qa-builder-claude"
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "claude-fable-5-1"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	// The token has expired, so the summary starts no lookup.
	expired := &coreauth.Auth{ID: authID, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{
		"access_token": "expired-token",
		"expired":      time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	if _, errRegister := service.coreManager.Register(coreauth.WithSkipPersist(context.Background()), expired); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if summary := keyusage.NewFablePool(service.claudeUsage).Summary(context.Background(), 0); !summary.Partial {
		t.Fatalf("summary = %+v, want the service's unread Claude account counted", summary)
	}

	roundRobin := *cfg
	roundRobin.Routing.Strategy = "round-robin"
	service.cfgMu.Lock()
	service.cfg = &roundRobin
	service.cfgMu.Unlock()
	if service.quotaAwareActive() {
		t.Fatal("quotaAwareActive() = true under round-robin")
	}
}
