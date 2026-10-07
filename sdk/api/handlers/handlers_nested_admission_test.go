package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// nestedAdmissionPolicy refuses Claude when refuse is set and counts its calls.
type nestedAdmissionPolicy struct {
	mu     sync.Mutex
	calls  int
	refuse bool
}

func (p *nestedAdmissionPolicy) Admit(_ context.Context, auth *coreauth.Auth) error {
	if auth == nil || auth.Provider != "claude" {
		return nil
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.refuse {
		return &coreauth.ClientQuotaError{Code: coreauth.ErrorCodeClientKeyLimitReached, Message: "client API key Claude allowance reached"}
	}
	return nil
}

func (p *nestedAdmissionPolicy) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newNestedAdmissionHandler(t *testing.T, policy *nestedAdmissionPolicy) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetAdmissionPolicy(policy)
	for _, provider := range []string{"codex", "claude"} {
		manager.RegisterExecutor(&interceptorCaptureExecutor{provider: provider})
		auth := &coreauth.Auth{ID: "nested-admission-" + provider, Provider: provider, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "nested-admission-" + provider}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
}

// TestNestedModelExecutionsAreAdmittedOnTheirOwn pins that a plugin's nested
// model call is its own request: the outer request's admission (and its
// upstream attempt) does not decide for it, so a refused nested Claude call
// gets the 429 refusal, not a credential-selection error.
func TestNestedModelExecutionsAreAdmittedOnTheirOwn(t *testing.T) {
	policy := &nestedAdmissionPolicy{refuse: true}
	h := newNestedAdmissionHandler(t, policy)
	nestedStatus := 0
	h.SetPluginHost(&handlerInterceptorTestHost{interceptResponse: func(ctx context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
		if req.Model == "nested-admission-codex" {
			_, errMsg := h.ExecuteModel(ctx, ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: "nested-admission-claude", Body: []byte(`{"model":"nested-admission-claude"}`)})
			if errMsg != nil {
				nestedStatus = errMsg.StatusCode
			}
		}
		return pluginapi.ResponseInterceptResponse{Body: req.Body}
	}})
	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "openai", "nested-admission-codex", []byte(`{"model":"nested-admission-codex"}`), ""); errMsg != nil {
		t.Fatalf("outer request failed: %v", errMsg.Error)
	}
	if nestedStatus != http.StatusTooManyRequests {
		t.Fatalf("nested refusal status = %d, want 429", nestedStatus)
	}
	if calls := policy.callCount(); calls != 1 {
		t.Fatalf("policy consulted %d times for the nested Claude call, want 1", calls)
	}
}

// TestConcurrentNestedModelExecutionsDoNotShareAdmission runs two nested model
// calls concurrently from one interceptor; each must be admitted on its own
// without touching a shared cache (the race detector guards the latter).
func TestConcurrentNestedModelExecutionsDoNotShareAdmission(t *testing.T) {
	policy := &nestedAdmissionPolicy{}
	h := newNestedAdmissionHandler(t, policy)
	h.SetPluginHost(&handlerInterceptorTestHost{interceptResponse: func(ctx context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
		if req.Model == "nested-admission-codex" {
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, errMsg := h.ExecuteModel(ctx, ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: "nested-admission-claude", Body: []byte(`{"model":"nested-admission-claude"}`)}); errMsg != nil {
						t.Errorf("nested call failed: %v", errMsg.Error)
					}
				}()
			}
			wg.Wait()
		}
		return pluginapi.ResponseInterceptResponse{Body: req.Body}
	}})
	if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "openai", "nested-admission-codex", []byte(`{"model":"nested-admission-codex"}`), ""); errMsg != nil {
		t.Fatalf("outer request failed: %v", errMsg.Error)
	}
	if calls := policy.callCount(); calls != 4 {
		t.Fatalf("policy consulted %d times, want once per nested call", calls)
	}
}
