package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// websocketQuotaRefusingPolicy refuses every Claude credential with one allowance error.
type websocketQuotaRefusingPolicy struct {
	refusal *coreauth.ClientQuotaError
}

func (p *websocketQuotaRefusingPolicy) Admit(_ context.Context, auth *coreauth.Auth) error {
	if auth != nil && auth.Provider == "claude" {
		return p.refusal
	}
	return nil
}

// websocketQuotaExecutor counts upstream calls; a refused request must make none.
type websocketQuotaExecutor struct {
	mu    sync.Mutex
	calls int
}

func (e *websocketQuotaExecutor) Identifier() string { return "claude" }

func (e *websocketQuotaExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketQuotaExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-quota","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketQuotaExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketQuotaExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketQuotaExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketQuotaExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func websocketQuotaRefusal(resetIn time.Duration) *coreauth.ClientQuotaError {
	return &coreauth.ClientQuotaError{
		Code:    coreauth.ErrorCodeClientKeyLimitReached,
		Message: "client API key Claude allowance reached: 1.52 of 1.5 Pro units used this week; resets in 1h30m",
		ResetIn: resetIn,
	}
}

// TestShouldExposeResponsesUpstreamErrorForClientQuotaRefusal pins that the
// proxy's own allowance refusal is exposed with its Retry-After, unlike an
// upstream 429 that only means the credential should rotate.
func TestShouldExposeResponsesUpstreamErrorForClientQuotaRefusal(t *testing.T) {
	errMsg := handlers.ExecutionErrorMessage(websocketQuotaRefusal(time.Hour))
	if !shouldExposeResponsesUpstreamError(errMsg) {
		t.Fatal("a local allowance refusal must reach the client instead of looking like an upstream 429")
	}
	payload, errBuild := buildResponsesWebsocketErrorPayload(errMsg)
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	assertWebsocketQuotaErrorPayload(t, payload, "3600")
}

// TestResponsesWebsocketClientQuotaRefusalCarriesRetryAfter drives a refused
// response.create over a live socket: the client must read the 429 error event
// with Retry-After before the socket closes, and no upstream call may happen.
func TestResponsesWebsocketClientQuotaRefusalCarriesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelName := "claude-websocket-allowance-model"
	executor := &websocketQuotaExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetAdmissionPolicy(&websocketQuotaRefusingPolicy{refusal: websocketQuotaRefusal(90 * time.Minute)})
	auth := &coreauth.Auth{ID: "auth-claude-allowance", Provider: "claude", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	request := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","id":"msg-1"}]}`, modelName)
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
		t.Fatalf("write response.create: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("the allowance refusal was hidden behind a bare close: %v", errRead)
	}
	assertWebsocketQuotaErrorPayload(t, payload, "5400")
	// The refusal is terminal for this socket; a reconnect after Retry-After starts a new turn.
	if _, extra, errExtra := conn.ReadMessage(); errExtra == nil {
		t.Fatalf("received a frame after the terminal refusal: %s", extra)
	}
	if calls := executor.callCount(); calls != 0 {
		t.Fatalf("upstream was called %d times for a refused request", calls)
	}
}

func assertWebsocketQuotaErrorPayload(t *testing.T, payload []byte, retryAfter string) {
	t.Helper()
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("type = %q, want %q: %s", got, wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "headers.Retry-After").String(); got != retryAfter {
		t.Fatalf("headers.Retry-After = %q, want %q: %s", got, retryAfter, payload)
	}
	if got := gjson.GetBytes(payload, "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "error.code").String(); got != "rate_limit_exceeded" {
		t.Fatalf("error.code = %q, want rate_limit_exceeded: %s", got, payload)
	}
	if !strings.Contains(gjson.GetBytes(payload, "error.message").String(), "allowance reached") {
		t.Fatalf("error.message does not carry the refusal: %s", payload)
	}
}
