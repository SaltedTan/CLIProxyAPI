package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// keepAliveProbeWait bounds how long a stub waits for a keepalive byte. The
// keepalive interval is configured in whole seconds, so a stub that must see
// no byte waits past two ticks; one that must see a byte returns at the first.
const keepAliveProbeWait = 2500 * time.Millisecond

// flushRecorder reports the first flush: the first keepalive byte commits the
// response status, after which no 429 can be sent.
type flushRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
}

func (r *flushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

func (r *flushRecorder) wasFlushed() bool {
	select {
	case <-r.flushed:
		return true
	default:
		return false
	}
}

// keepAliveExecutor serves non-streaming calls for one provider.
type keepAliveExecutor struct {
	*bootstrapStreamExecutor
	provider string
	execute  func(context.Context) (coreexecutor.Response, error)
}

func (e *keepAliveExecutor) Identifier() string { return e.provider }

func (e *keepAliveExecutor) Execute(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return e.execute(ctx)
}

// newKeepAliveTracker returns a tracker in which key has used the given share
// of a Pro allowance on a Claude credential, against a limit of 0.15.
func newKeepAliveTracker(key, authID, model, utilization string) *clientusage.Tracker {
	tracker := clientusage.NewTracker()
	tracker.SetLimits(map[string]float64{key: 0.15})
	for _, value := range []string{"0", utilization} {
		tracker.HandleUsage(context.Background(), usage.Record{
			APIKey: key, Provider: "claude", AuthID: authID, Model: model, RequestedAt: time.Now(),
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Utilization": {value},
				"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)},
			},
		})
	}
	return tracker
}

func registerKeepAliveAuth(t *testing.T, manager *coreauth.Manager, authID, provider, model string) {
	t.Helper()
	auth := &coreauth.Auth{ID: authID, Provider: provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
}

// newKeepAliveRequest builds a client request the way the protocol handlers
// do: a gin context on a flushing writer and the handler's request context.
func newKeepAliveRequest(h *BaseAPIHandler, key string) (*flushRecorder, *gin.Context, context.Context, func()) {
	rec := newFlushRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	c.Set("userApiKey", key)
	ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
	return rec, c, ctx, func() { cancel() }
}

func keepAliveHandler(manager *coreauth.Manager) *BaseAPIHandler {
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{NonStreamKeepAliveInterval: 1}, manager)
}

// TestNonStreamKeepAliveWaitsForAdmission pins that no keepalive byte is
// written while a request can still be refused by admission: a slow plugin
// before admission must not let a tick commit HTTP 200 for a refused key.
func TestNonStreamKeepAliveWaitsForAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "fixture-client-key-1"
	const authID = "auth-claude-keepalive"
	const model = "claude-keepalive-model"
	tracker := newKeepAliveTracker(key, authID, model, "0.2")
	executor := &keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "claude", execute: func(context.Context) (coreexecutor.Response, error) {
		t.Error("a refused request reached upstream")
		return coreexecutor.Response{}, nil
	}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetAdmissionPolicy(tracker)
	manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
	registerKeepAliveAuth(t, manager, authID, "claude", model)
	h := keepAliveHandler(manager)
	rec, c, ctx, cancel := newKeepAliveRequest(h, key)
	defer cancel()
	h.SetPluginHost(&handlerInterceptorTestHost{interceptRequestBeforeAuth: func(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
		// Runs before admission; a keepalive tick must not commit the response meanwhile.
		select {
		case <-rec.flushed:
		case <-time.After(keepAliveProbeWait):
		}
		return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers)}
	}})

	stop := h.StartNonStreamingKeepAlive(c, ctx)
	_, _, errMsg := h.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
	stop()
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("errMsg = %v, want the 429 refusal", errMsg)
	}
	if rec.wasFlushed() {
		t.Fatal("a keepalive byte was written before admission decided the request")
	}
	// The protocol handler would now write the refusal; gin keeps the status
	// until the first byte, so flush the header the way a body write would.
	c.Writer.WriteHeader(errMsg.StatusCode)
	c.Writer.WriteHeaderNow()
	if got := rec.Result().StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("committed status = %d, want 429", got)
	}
	if row := tracker.Snapshot(clientusage.SnapshotOptions{}).Keys[0]; row.Totals.Blocked != 1 {
		t.Fatalf("blocked = %d, want 1", row.Totals.Blocked)
	}
}

// TestNonStreamKeepAliveFlowsOnceUpstreamIsCommitted pins that the keepalive
// still runs while an admitted request, or one no policy can refuse, waits on
// upstream.
func TestNonStreamKeepAliveFlowsOnceUpstreamIsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "fixture-client-key-1"
	for _, tc := range []struct {
		name   string
		policy bool
	}{{"admitted", true}, {"no policy", false}} {
		t.Run(tc.name, func(t *testing.T) {
			authID := "auth-claude-keepalive-" + strings.ReplaceAll(tc.name, " ", "-")
			model := "claude-keepalive-flow-" + strings.ReplaceAll(tc.name, " ", "-")
			manager := coreauth.NewManager(nil, nil, nil)
			if tc.policy {
				manager.SetAdmissionPolicy(newKeepAliveTracker(key, authID, model, "0.1"))
			}
			manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
			h := keepAliveHandler(manager)
			rec, c, ctx, cancel := newKeepAliveRequest(h, key)
			defer cancel()
			manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "claude", execute: func(context.Context) (coreexecutor.Response, error) {
				// Upstream is slow; the client must see a keepalive byte meanwhile.
				select {
				case <-rec.flushed:
				case <-time.After(keepAliveProbeWait):
				}
				return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			}})
			registerKeepAliveAuth(t, manager, authID, "claude", model)

			stop := h.StartNonStreamingKeepAlive(c, ctx)
			_, _, errMsg := h.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
			stop()
			if errMsg != nil {
				t.Fatalf("ExecuteWithAuthManager: %v", errMsg.Error)
			}
			if !rec.wasFlushed() {
				t.Fatal("no keepalive byte reached the client while upstream was pending")
			}
		})
	}
}

// TestNestedModelCallDoesNotCommitTheOuterKeepAlive pins that a model call a
// plugin makes before the client request is admitted is a request of its own:
// its upstream attempt must not release the client request's keepalive.
func TestNestedModelCallDoesNotCommitTheOuterKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "fixture-client-key-1"
	const claudeAuth = "auth-claude-keepalive-nested"
	const claudeModel = "claude-keepalive-nested"
	const geminiAuth = "auth-gemini-keepalive-nested"
	const geminiModel = "gemini-keepalive-nested"
	tracker := newKeepAliveTracker(key, claudeAuth, claudeModel, "0.2")
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetAdmissionPolicy(tracker)
	manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
	manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "claude", execute: func(context.Context) (coreexecutor.Response, error) {
		t.Error("the refused client request reached upstream")
		return coreexecutor.Response{}, nil
	}})
	manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "gemini", execute: func(context.Context) (coreexecutor.Response, error) {
		return coreexecutor.Response{Payload: []byte(`{"nested":true}`)}, nil
	}})
	registerKeepAliveAuth(t, manager, claudeAuth, "claude", claudeModel)
	registerKeepAliveAuth(t, manager, geminiAuth, "gemini", geminiModel)
	h := keepAliveHandler(manager)
	rec, c, ctx, cancel := newKeepAliveRequest(h, key)
	defer cancel()
	nestedOK := false
	h.SetPluginHost(&handlerInterceptorTestHost{interceptRequestBeforeAuth: func(ctx context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
		if req.Model == claudeModel {
			_, errNested := h.ExecuteModel(ctx, ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: geminiModel, Body: []byte(`{"model":"` + geminiModel + `"}`)})
			nestedOK = errNested == nil
			select {
			case <-rec.flushed:
			case <-time.After(keepAliveProbeWait):
			}
		}
		return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers)}
	}})

	stop := h.StartNonStreamingKeepAlive(c, ctx)
	_, _, errMsg := h.ExecuteWithAuthManager(ctx, "openai", claudeModel, []byte(`{"model":"`+claudeModel+`"}`), "")
	stop()
	if !nestedOK {
		t.Fatal("the nested model call did not succeed")
	}
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("errMsg = %v, want the 429 refusal of the client request", errMsg)
	}
	if rec.wasFlushed() {
		t.Fatal("the nested call's upstream attempt released the client request's keepalive")
	}
}

// keepAlivePluginHost routes one model to a plugin executor and serves it.
type keepAlivePluginHost struct {
	*handlerInterceptorTestHost
	pluginModel string
}

func (*keepAlivePluginHost) HasModelRouters() bool { return true }

func (h *keepAlivePluginHost) RouteModel(_ context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	if req.RequestedModel == h.pluginModel {
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: "keepalive-plugin"}, true
	}
	return pluginapi.ModelRouteResponse{}, false
}

func (*keepAlivePluginHost) ExecutePluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (*keepAlivePluginHost) ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"ok":true}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*keepAlivePluginHost) CountPluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

// TestNestedPluginExecutorCallDoesNotCommitTheOuterKeepAlive pins that a
// nested model call served by a plugin executor, which admission never
// refuses, still does not release the client request's keepalive: the client
// request itself may yet be refused.
func TestNestedPluginExecutorCallDoesNotCommitTheOuterKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "fixture-client-key-1"
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			authID := "auth-claude-keepalive-plugin-" + name
			model := "claude-keepalive-plugin-" + name
			pluginModel := "plugin-keepalive-" + name
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetAdmissionPolicy(newKeepAliveTracker(key, authID, model, "0.2"))
			manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
			manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "claude", execute: func(context.Context) (coreexecutor.Response, error) {
				t.Error("the refused client request reached upstream")
				return coreexecutor.Response{}, nil
			}})
			registerKeepAliveAuth(t, manager, authID, "claude", model)
			h := keepAliveHandler(manager)
			rec, c, ctx, cancel := newKeepAliveRequest(h, key)
			defer cancel()
			host := &keepAlivePluginHost{handlerInterceptorTestHost: &handlerInterceptorTestHost{}, pluginModel: pluginModel}
			nestedOK := false
			host.interceptRequestBeforeAuth = func(ctx context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
				if req.Model == model {
					nested := ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: pluginModel, Body: []byte(`{"model":"` + pluginModel + `"}`), Stream: stream}
					if stream {
						result, errNested := h.ExecuteModelStream(ctx, nested)
						nestedOK = errNested == nil
						if errNested == nil {
							for range result.Chunks {
							}
						}
					} else {
						_, errNested := h.ExecuteModel(ctx, nested)
						nestedOK = errNested == nil
					}
					select {
					case <-rec.flushed:
					case <-time.After(keepAliveProbeWait):
					}
				}
				return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers)}
			}
			h.SetPluginHost(host)

			stop := h.StartNonStreamingKeepAlive(c, ctx)
			_, _, errMsg := h.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
			stop()
			if !nestedOK {
				t.Fatal("the nested plugin-executor call did not succeed")
			}
			if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("errMsg = %v, want the 429 refusal of the client request", errMsg)
			}
			if rec.wasFlushed() {
				t.Fatal("the nested plugin-executor call released the client request's keepalive")
			}
		})
	}
}

// TestNonStreamKeepAliveFlowsBeforeAdmissionForKeysWithoutALimit pins that the
// keepalive is held back only for a key admission could refuse: for a key
// without a Claude allowance, a slow before-auth plugin, or a model call it
// makes, gets keepalive bytes like an upstream wait does.
func TestNonStreamKeepAliveFlowsBeforeAdmissionForKeysWithoutALimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limitedKey = "fixture-client-key-1"
	const key = "fixture-client-key-2"
	for _, nested := range []bool{false, true} {
		name := "slow plugin"
		if nested {
			name = "nested call"
		}
		t.Run(name, func(t *testing.T) {
			suffix := strings.ReplaceAll(name, " ", "-")
			claudeAuth := "auth-claude-keepalive-unlimited-" + suffix
			claudeModel := "claude-keepalive-unlimited-" + suffix
			geminiAuth := "auth-gemini-keepalive-unlimited-" + suffix
			geminiModel := "gemini-keepalive-unlimited-" + suffix
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetAdmissionPolicy(newKeepAliveTracker(limitedKey, claudeAuth, claudeModel, "0.2"))
			manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
			manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "claude", execute: func(context.Context) (coreexecutor.Response, error) {
				return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			}})
			manager.RegisterExecutor(&keepAliveExecutor{bootstrapStreamExecutor: &bootstrapStreamExecutor{}, provider: "gemini", execute: func(context.Context) (coreexecutor.Response, error) {
				return coreexecutor.Response{Payload: []byte(`{"nested":true}`)}, nil
			}})
			registerKeepAliveAuth(t, manager, claudeAuth, "claude", claudeModel)
			registerKeepAliveAuth(t, manager, geminiAuth, "gemini", geminiModel)
			h := keepAliveHandler(manager)
			rec, c, ctx, cancel := newKeepAliveRequest(h, key)
			defer cancel()
			flushedBeforeAdmission := false
			h.SetPluginHost(&handlerInterceptorTestHost{interceptRequestBeforeAuth: func(ctx context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
				if req.Model == claudeModel {
					if nested {
						if _, errNested := h.ExecuteModel(ctx, ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: geminiModel, Body: []byte(`{"model":"` + geminiModel + `"}`)}); errNested != nil {
							t.Errorf("nested call: %v", errNested.Error)
						}
					}
					select {
					case <-rec.flushed:
					case <-time.After(keepAliveProbeWait):
					}
					flushedBeforeAdmission = rec.wasFlushed()
				}
				return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers)}
			}})

			stop := h.StartNonStreamingKeepAlive(c, ctx)
			_, _, errMsg := h.ExecuteWithAuthManager(ctx, "openai", claudeModel, []byte(`{"model":"`+claudeModel+`"}`), "")
			stop()
			if errMsg != nil {
				t.Fatalf("ExecuteWithAuthManager: %v", errMsg.Error)
			}
			if !flushedBeforeAdmission {
				t.Fatal("no keepalive byte reached a client whose key admission cannot refuse")
			}
		})
	}
}
