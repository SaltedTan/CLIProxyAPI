package handlers

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaBootstrapExecutor is the bootstrap stream stub registered as a Claude executor.
type quotaBootstrapExecutor struct{ *bootstrapStreamExecutor }

func (*quotaBootstrapExecutor) Identifier() string { return "claude" }

// TestStreamBootstrapRetryKeepsTheRequestAdmission pins that a stream bootstrap
// retry is the same client request: admission was decided when the request was
// first admitted, so a limit reached by its own first attempt neither turns the
// upstream failure into a 429 nor counts the request as blocked.
func TestStreamBootstrapRetryKeepsTheRequestAdmission(t *testing.T) {
	const key = "fixture-client-key-1"
	const authID = "auth-claude-bootstrap"
	const model = "claude-bootstrap-model"

	tracker := clientusage.NewTracker()
	tracker.SetLimits(map[string]float64{key: 0.15})
	observe := func(utilization string, failed bool) {
		tracker.HandleUsage(context.Background(), usage.Record{
			APIKey: key, Provider: "claude", AuthID: authID, Model: model, RequestedAt: time.Now(), Failed: failed,
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Utilization": {utilization},
				"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)},
			},
		})
	}
	// 0.1 Pro units used before the request: under the 0.15 limit.
	observe("0.1", false)
	observe("0.2", false)

	executor := &quotaBootstrapExecutor{&bootstrapStreamExecutor{stream: func(context.Context, int) (*coreexecutor.StreamResult, error) {
		// The attempt pushes the key past its limit, then fails before any deliverable byte.
		observe("0.4", true)
		chunks := make(chan coreexecutor.StreamChunk, 2)
		chunks <- coreexecutor.StreamChunk{Payload: []byte("drop")}
		chunks <- coreexecutor.StreamChunk{Err: &coreauth.Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"}}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetAdmissionPolicy(tracker)
	manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
	auth := &coreauth.Auth{ID: authID, Provider: "claude", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1}}, manager)
	h.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(_ context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
		return pluginapi.StreamChunkInterceptResponse{Body: cloneBytes(req.Body), DropChunk: string(req.Body) == "drop"}
	}})
	g := &gin.Context{}
	g.Set("userApiKey", key)
	ctx := context.WithValue(context.Background(), "gin", g)

	_, _, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	msg := <-errs
	if msg == nil {
		t.Fatal("the failed stream reported no error")
	}
	if msg.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d (%v), want the upstream 500 after %d upstream calls", msg.StatusCode, msg.Error, executor.Calls())
	}
	if calls := executor.Calls(); calls != 2 {
		t.Fatalf("upstream calls = %d, want the bootstrap retry to proceed as the same admitted request", calls)
	}
	row := tracker.Snapshot(clientusage.SnapshotOptions{}).Keys[0]
	if row.Totals.Blocked != 0 {
		t.Fatalf("blocked = %d for a request that reached upstream (failed = %d)", row.Totals.Blocked, row.Totals.Failed)
	}
}
