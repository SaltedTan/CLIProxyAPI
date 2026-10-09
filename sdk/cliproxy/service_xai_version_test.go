package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A config reload that changes proxy-url reaches the Grok CLI version updater: the lookup
// stalled on the startup proxy is canceled and the next one uses the reloaded proxy.
func TestServiceConfigReloadRebindsXAIVersionProxy(t *testing.T) {
	restoreVersion := helps.SetXAIClientVersionForTest(helps.DefaultXAIFallbackClientVersion)
	defer restoreVersion()

	stalledReached := make(chan struct{}, 1)
	stalledCanceled := make(chan struct{}, 1)
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledReached <- struct{}{}
		<-r.Context().Done()
		stalledCanceled <- struct{}{}
	}))
	defer stalled.Close()
	reloaded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.63"}`))
	}))
	defer reloaded.Close()

	restoreURL := helps.OverrideXAINPMRegistryURLForTest("http://registry.npm.invalid/@xai-official/grok/latest")
	defer restoreURL()
	notified := make(chan struct{}, 4)
	restoreHook := helps.SetXAIVersionRefreshedHookForTest(notified)
	defer restoreHook()
	wait := func(signal <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	startup := &config.Config{}
	startup.ProxyURL = stalled.URL
	s := &Service{cfg: startup}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor.StartXAIVersionUpdater(ctx, startup.ProxyURL)
	wait(stalledReached, "lookup through the startup proxy")

	next := &config.Config{}
	next.ProxyURL = reloaded.URL
	if commit := s.commitConfigUpdate(next); commit.cfg == nil {
		t.Fatal("config update rejected")
	}
	wait(stalledCanceled, "cancellation of the stalled lookup")
	for helps.GetXAIClientVersion() != "1.0.63" {
		wait(notified, "lookup through the reloaded proxy")
	}
}
