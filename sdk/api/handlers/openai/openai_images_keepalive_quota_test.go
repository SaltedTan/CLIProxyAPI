package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// imagesProbeWait bounds how long a stub waits for a heartbeat. The streaming
// heartbeat interval is configured in whole seconds, so a stub that must see
// none waits past two ticks; one that must see one returns at the first.
const imagesProbeWait = 2500 * time.Millisecond

// imagesFlushRecorder reports the first flush: the first heartbeat commits the
// response as an SSE stream under HTTP 200.
type imagesFlushRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func newImagesFlushRecorder() *imagesFlushRecorder {
	return &imagesFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
}

func (r *imagesFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

func (r *imagesFlushRecorder) wasFlushed() bool {
	select {
	case <-r.flushed:
		return true
	default:
		return false
	}
}

// slowQuotaPolicy decides only once the client has seen a heartbeat, or after
// the probe wait, and then refuses.
type slowQuotaPolicy struct{ flushed chan struct{} }

func (p *slowQuotaPolicy) Admit(context.Context, *coreauth.Auth) error {
	select {
	case <-p.flushed:
	case <-time.After(imagesProbeWait):
	}
	return websocketQuotaRefusal(time.Hour)
}

// slowImagesExecutor answers once the client has seen a heartbeat.
type slowImagesExecutor struct {
	websocketQuotaExecutor
	flushed chan struct{}
}

func (e *slowImagesExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	select {
	case <-e.flushed:
	case <-time.After(imagesProbeWait):
	}
	return e.websocketQuotaExecutor.ExecuteStream(ctx, auth, req, opts)
}

func newImagesStreamRequest(t *testing.T, executor coreauth.ProviderExecutor, policy coreauth.AdmissionPolicy, model string) (*imagesFlushRecorder, *gin.Context, *OpenAIAPIHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := newImagesFlushRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{}`))
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetAdmissionPolicy(policy)
	manager.SetConfig(&sdkconfig.Config{DisableCooling: true})
	authID := "auth-images-" + model
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "claude", Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1}}, manager)
	return rec, c, NewOpenAIAPIHandler(base)
}

// TestImagesStreamHeartbeatWaitsForAdmission pins that the image stream's
// bootstrap heartbeat does not commit an SSE 200 while the request can still
// be refused: a refusal stays a plain JSON 429 with Retry-After.
func TestImagesStreamHeartbeatWaitsForAdmission(t *testing.T) {
	const model = "claude-images-refused"
	executor := &websocketQuotaExecutor{}
	policy := &slowQuotaPolicy{}
	rec, c, h := newImagesStreamRequest(t, executor, policy, model)
	policy.flushed = rec.flushed

	h.streamImagesFromResponses(c, []byte(`{"model":"`+model+`","stream":true}`), "b64_json", "image_generation")
	if rec.wasFlushed() {
		t.Fatal("a heartbeat was written before admission decided the request")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want a plain JSON refusal", contentType)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After missing from the refusal")
	}
	if calls := executor.callCount(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

// TestImagesStreamHeartbeatFlowsOnceUpstreamIsCommitted pins that the
// heartbeat still runs while an admitted image stream waits on upstream.
func TestImagesStreamHeartbeatFlowsOnceUpstreamIsCommitted(t *testing.T) {
	const model = "claude-images-admitted"
	executor := &slowImagesExecutor{}
	rec, c, h := newImagesStreamRequest(t, executor, nil, model)
	executor.flushed = rec.flushed

	h.streamImagesFromResponses(c, []byte(`{"model":"`+model+`","stream":true}`), "b64_json", "image_generation")
	if !rec.wasFlushed() {
		t.Fatal("no heartbeat reached the client while upstream was pending")
	}
	if calls := executor.callCount(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}
