package auth

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// admissionTestPolicy refuses the providers listed in refuse and records every call.
type admissionTestPolicy struct {
	mu     sync.Mutex
	calls  []string
	refuse map[string]*ClientQuotaError
}

func (p *admissionTestPolicy) Admit(_ context.Context, auth *Auth) error {
	p.mu.Lock()
	p.calls = append(p.calls, auth.ID)
	p.mu.Unlock()
	if quota, ok := p.refuse[auth.Provider]; ok {
		return quota
	}
	return nil
}

func (p *admissionTestPolicy) callIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// admissionTestExecutor serves one provider, records the credentials it was called
// with, and fails every call with executeErr when set.
type admissionTestExecutor struct {
	identifier string
	executeErr error
	mu         sync.Mutex
	executeIDs []string
	streamIDs  []string
	countIDs   []string
}

func (e *admissionTestExecutor) Identifier() string { return e.identifier }

func (e *admissionTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeIDs = append(e.executeIDs, auth.ID)
	e.mu.Unlock()
	if e.executeErr != nil {
		return cliproxyexecutor.Response{}, e.executeErr
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *admissionTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamIDs = append(e.streamIDs, auth.ID)
	e.mu.Unlock()
	if e.executeErr != nil {
		return nil, e.executeErr
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*admissionTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *admissionTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.countIDs = append(e.countIDs, auth.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (*admissionTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *admissionTestExecutor) ids(kind string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch kind {
	case "execute":
		return append([]string(nil), e.executeIDs...)
	case "stream":
		return append([]string(nil), e.streamIDs...)
	default:
		return append([]string(nil), e.countIDs...)
	}
}

func registerAdmissionAuth(t *testing.T, manager *Manager, id, provider, model string) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	}); errRegister != nil {
		t.Fatalf("register %s: %v", id, errRegister)
	}
}

func claudeQuotaRefusal() *ClientQuotaError {
	return &ClientQuotaError{Code: ErrorCodeClientKeyLimitReached, Message: "client API key Claude allowance reached", ResetIn: 90 * time.Second}
}

func assertAuthUntouched(t *testing.T, manager *Manager, id string) {
	t.Helper()
	auth, ok := manager.GetByID(id)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", id)
	}
	if auth.Unavailable || auth.LastError != nil || auth.Quota.Exceeded || !auth.NextRetryAfter.IsZero() || auth.Status != StatusActive {
		t.Fatalf("refusal changed credential state: unavailable=%v lastError=%v quota=%+v nextRetry=%v status=%v", auth.Unavailable, auth.LastError, auth.Quota, auth.NextRetryAfter, auth.Status)
	}
	if len(auth.ModelStates) != 0 {
		t.Fatalf("refusal recorded model state: %+v", auth.ModelStates)
	}
}

func TestAdmissionRefusedClaudeFallsBackToAnotherProvider(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			model := "admission-fallback-" + kind
			claude := &admissionTestExecutor{identifier: "claude"}
			codex := &admissionTestExecutor{identifier: "codex"}
			manager.RegisterExecutor(claude)
			manager.RegisterExecutor(codex)
			registerAdmissionAuth(t, manager, "admission-claude-1-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "admission-claude-2-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "admission-codex-"+kind, "codex", model)
			policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}
			manager.SetAdmissionPolicy(policy)

			var errExecute error
			if kind == "execute" {
				_, errExecute = manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			} else {
				var result *cliproxyexecutor.StreamResult
				result, errExecute = manager.ExecuteStream(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if result != nil {
					for range result.Chunks {
					}
				}
			}
			if errExecute != nil {
				t.Fatalf("execution error = %v, want the other provider to serve", errExecute)
			}
			if got := claude.ids(kind); len(got) != 0 {
				t.Fatalf("refused provider was called: %v", got)
			}
			if got := codex.ids(kind); len(got) != 1 || got[0] != "admission-codex-"+kind {
				t.Fatalf("other provider calls = %v, want one", got)
			}
			// The policy is consulted once per provider, not once per credential.
			calls := policy.callIDs()
			providers := map[string]int{}
			for _, id := range calls {
				auth, _ := manager.GetByID(id)
				providers[auth.Provider]++
			}
			if len(calls) != 2 || providers["claude"] != 1 || providers["codex"] != 1 {
				t.Fatalf("policy calls = %v, want one per provider", calls)
			}
			assertAuthUntouched(t, manager, "admission-claude-1-"+kind)
			assertAuthUntouched(t, manager, "admission-claude-2-"+kind)
		})
	}
}

func TestAdmissionRefusedEverywhereReturnsClientQuotaError(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			// A 429 would normally start request-retry rounds; a refusal must not.
			manager.SetRetryConfig(3, 30*time.Second, 0)
			model := "admission-refused-" + kind
			claude := &admissionTestExecutor{identifier: "claude"}
			manager.RegisterExecutor(claude)
			registerAdmissionAuth(t, manager, "admission-only-1-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "admission-only-2-"+kind, "claude", model)
			policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}
			manager.SetAdmissionPolicy(policy)

			var errExecute error
			if kind == "execute" {
				_, errExecute = manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			} else {
				var result *cliproxyexecutor.StreamResult
				result, errExecute = manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if result != nil {
					t.Fatalf("stream result = %+v, want nil so the handler writes a plain error", result)
				}
			}
			var quota *ClientQuotaError
			if !errors.As(errExecute, &quota) || quota == nil {
				t.Fatalf("execution error = %T %v, want *ClientQuotaError", errExecute, errExecute)
			}
			if quota.StatusCode() != http.StatusTooManyRequests || quota.Code != ErrorCodeClientKeyLimitReached {
				t.Fatalf("quota error = %+v", quota)
			}
			if retryAfter := quota.RetryAfter(); retryAfter == nil || *retryAfter != 90*time.Second {
				t.Fatalf("retry after = %v, want 90s", retryAfter)
			}
			if got := SafeResponseHeaders(errExecute).Get("Retry-After"); got != "90" {
				t.Fatalf("SafeResponseHeaders Retry-After = %q, want 90", got)
			}
			if statusCodeFromError(errExecute) != http.StatusTooManyRequests || !isRequestStopError(errExecute) {
				t.Fatal("refusal must be a 429 request-stop error")
			}
			if got := claude.ids(kind); len(got) != 0 {
				t.Fatalf("executor was called despite the refusal: %v", got)
			}
			if calls := policy.callIDs(); len(calls) != 1 {
				t.Fatalf("policy calls = %v, want exactly one (no retry rounds, one per provider)", calls)
			}
			assertAuthUntouched(t, manager, "admission-only-1-"+kind)
			assertAuthUntouched(t, manager, "admission-only-2-"+kind)
		})
	}
}

func TestAdmissionUpstreamFailureWinsOverRefusalAndIsEvaluatedOncePerRequest(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	// Two retry rounds for the failing provider; the refused provider is evaluated once.
	manager.SetRetryConfig(2, 0, 0)
	model := "admission-upstream-failure"
	claude := &admissionTestExecutor{identifier: "claude"}
	codex := &admissionTestExecutor{identifier: "codex", executeErr: &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"}}
	manager.RegisterExecutor(claude)
	manager.RegisterExecutor(codex)
	registerAdmissionAuth(t, manager, "admission-mixed-claude", "claude", model)
	registerAdmissionAuth(t, manager, "admission-mixed-codex", "codex", model)
	policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}
	manager.SetAdmissionPolicy(policy)

	_, errExecute := manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var quota *ClientQuotaError
	if errors.As(errExecute, &quota) {
		t.Fatalf("error = %v, want the upstream failure once an upstream attempt happened", errExecute)
	}
	if statusCodeFromError(errExecute) != http.StatusInternalServerError {
		t.Fatalf("error = %v, want the upstream 500", errExecute)
	}
	if got := codex.ids("execute"); len(got) != 3 {
		t.Fatalf("failing provider calls = %v, want the initial call plus two retry rounds", got)
	}
	if got := claude.ids("execute"); len(got) != 0 {
		t.Fatalf("refused provider was called: %v", got)
	}
	calls := policy.callIDs()
	claudeCalls := 0
	for _, id := range calls {
		if id == "admission-mixed-claude" {
			claudeCalls++
		}
	}
	if claudeCalls != 1 {
		t.Fatalf("policy evaluated the refused provider %d times across retry rounds, want once: %v", claudeCalls, calls)
	}
	assertAuthUntouched(t, manager, "admission-mixed-claude")
}

func TestAdmissionIsNotConsultedForCountTokens(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	model := "admission-count"
	claude := &admissionTestExecutor{identifier: "claude"}
	manager.RegisterExecutor(claude)
	registerAdmissionAuth(t, manager, "admission-count-claude", "claude", model)
	policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}
	manager.SetAdmissionPolicy(policy)

	if _, errCount := manager.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errCount != nil {
		t.Fatalf("ExecuteCount() error = %v", errCount)
	}
	if got := claude.ids("count"); len(got) != 1 {
		t.Fatalf("count calls = %v, want one", got)
	}
	if calls := policy.callIDs(); len(calls) != 0 {
		t.Fatalf("policy consulted for count-tokens: %v", calls)
	}
}

func TestAdmissionIsNotConsultedWithoutPolicyOrInHomeMode(t *testing.T) {
	t.Run("no policy", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		model := "admission-none"
		claude := &admissionTestExecutor{identifier: "claude"}
		manager.RegisterExecutor(claude)
		registerAdmissionAuth(t, manager, "admission-none-claude", "claude", model)
		if manager.AdmissionPolicy() != nil {
			t.Fatal("new manager must have no admission policy")
		}
		if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		manager.SetAdmissionPolicy(&admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}})
		manager.SetAdmissionPolicy(nil)
		if manager.AdmissionPolicy() != nil {
			t.Fatal("SetAdmissionPolicy(nil) must clear the policy")
		}
		if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
			t.Fatalf("Execute() after clearing the policy error = %v", errExecute)
		}
	})
	t.Run("home mode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		manager := NewManager(nil, nil, nil)
		manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
		registry := executionregistry.New()
		manager.PublishHomeDispatch(homeExecutionDispatcher{}, registry, 1)
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("initial")}
		close(chunks)
		manager.RegisterExecutor(&homeExecutionStreamExecutor{chunks: chunks})
		policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"home-execution": claudeQuotaRefusal()}}
		manager.SetAdmissionPolicy(policy)

		result, errExecute := manager.ExecuteStream(ctx, []string{"home-execution"}, cliproxyexecutor.Request{Model: "test"}, cliproxyexecutor.Options{Stream: true})
		if errExecute != nil {
			t.Fatalf("ExecuteStream() error = %v", errExecute)
		}
		for range result.Chunks {
		}
		if calls := policy.callIDs(); len(calls) != 0 {
			t.Fatalf("policy consulted in Home mode: %v", calls)
		}
		if errDrain := registry.Drain(context.Background()); errDrain != nil {
			t.Fatalf("Drain() error = %v", errDrain)
		}
	})
}

func TestClientQuotaErrorHeadersAndRetryAfter(t *testing.T) {
	cases := []struct {
		name    string
		resetIn time.Duration
		header  string
		retry   time.Duration
	}{
		{name: "unknown reset defaults to a minute", resetIn: 0, header: "60", retry: time.Minute},
		{name: "sub-second rounds up to one", resetIn: 200 * time.Millisecond, header: "1", retry: time.Second},
		{name: "fractional seconds round up", resetIn: 1500 * time.Millisecond, header: "2", retry: 1500 * time.Millisecond},
		{name: "whole seconds", resetIn: 2*24*time.Hour + 3*time.Hour, header: "183600", retry: 2*24*time.Hour + 3*time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quota := &ClientQuotaError{Code: ErrorCodeClientKeyLimitReached, Message: "allowance reached", ResetIn: tc.resetIn}
			headers := quota.Headers()
			if headers.Get("Retry-After") != tc.header || headers.Get("Content-Type") != "application/json" {
				t.Fatalf("headers = %v", headers)
			}
			if got := SafeResponseHeaders(quota); got.Get("Retry-After") != tc.header {
				t.Fatalf("SafeResponseHeaders = %v, want Retry-After %s", got, tc.header)
			}
			if retryAfter := quota.RetryAfter(); retryAfter == nil || *retryAfter != tc.retry {
				t.Fatalf("RetryAfter() = %v, want %s", retryAfter, tc.retry)
			}
			if quota.Error() != "allowance reached" || quota.StatusCode() != http.StatusTooManyRequests || !quota.IsRequestStop() {
				t.Fatalf("quota = %+v", quota)
			}
			if retryAfter := retryAfterFromError(quota); retryAfter == nil || *retryAfter != tc.retry {
				t.Fatalf("retryAfterFromError = %v", retryAfter)
			}
		})
	}
	var nilQuota *ClientQuotaError
	if nilQuota.Error() != "" || nilQuota.Headers() != nil || nilQuota.RetryAfter() != nil {
		t.Fatal("nil quota error accessors must be safe")
	}
	if (&ClientQuotaError{}).Error() == "" {
		t.Fatal("an empty message must still produce an error text")
	}
}

// recordingPolicy refuses claude and records refusals the conductor returns.
type recordingPolicy struct {
	admissionTestPolicy
	recorded []error
}

func (p *recordingPolicy) RecordRefusal(_ context.Context, refusal error) {
	p.mu.Lock()
	p.recorded = append(p.recorded, refusal)
	p.mu.Unlock()
}

func (p *recordingPolicy) recordedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.recorded)
}

func TestAdmissionRecordsARefusalOnlyWhenItIsReturned(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(3, 0, 0)
			model := "admission-record-" + kind
			claude := &admissionTestExecutor{identifier: "claude"}
			codex := &admissionTestExecutor{identifier: "codex"}
			manager.RegisterExecutor(claude)
			manager.RegisterExecutor(codex)
			registerAdmissionAuth(t, manager, "record-claude-1-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "record-claude-2-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "record-codex-"+kind, "codex", model)
			policy := &recordingPolicy{admissionTestPolicy: admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}}
			manager.SetAdmissionPolicy(policy)

			run := func(providers []string) error {
				if kind == "execute" {
					_, errExecute := manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
					return errExecute
				}
				result, errStream := manager.ExecuteStream(context.Background(), providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if result != nil {
					for range result.Chunks {
					}
				}
				return errStream
			}
			if errRun := run([]string{"claude", "codex"}); errRun != nil {
				t.Fatalf("fallback error = %v", errRun)
			}
			if got := policy.recordedCount(); got != 0 {
				t.Fatalf("recorded %d refusals for a request served by another provider, want 0", got)
			}
			errRun := run([]string{"claude"})
			var quota *ClientQuotaError
			if !errors.As(errRun, &quota) {
				t.Fatalf("error = %v, want the refusal", errRun)
			}
			if got := policy.recordedCount(); got != 1 || policy.recorded[0] != errRun {
				t.Fatalf("recorded = %v (%d), want the returned refusal exactly once", policy.recorded, got)
			}
		})
	}
}

func TestAdmissionRefusalDoesNotHideAnEarlierUpstreamError(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(2, 0, 0)
			model := "admission-upstream-precedence-" + kind
			claude := &admissionTestExecutor{identifier: "claude"}
			codex := &admissionTestExecutor{identifier: "codex", executeErr: &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"}}
			manager.RegisterExecutor(claude)
			manager.RegisterExecutor(codex)
			registerAdmissionAuth(t, manager, "precedence-claude-"+kind, "claude", model)
			// Cooling stays on: after the 500 the codex credential is out of the next
			// retry round, which then meets only the cached Claude refusal.
			registry.GetGlobalRegistry().RegisterClient("precedence-codex-"+kind, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("precedence-codex-" + kind) })
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "precedence-codex-" + kind, Provider: "codex", Status: StatusActive}); errRegister != nil {
				t.Fatal(errRegister)
			}
			policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{"claude": claudeQuotaRefusal()}}
			manager.SetAdmissionPolicy(policy)

			var errRun error
			if kind == "execute" {
				_, errRun = manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			} else {
				var result *cliproxyexecutor.StreamResult
				result, errRun = manager.ExecuteStream(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if result != nil {
					for range result.Chunks {
					}
				}
			}
			var quota *ClientQuotaError
			if errors.As(errRun, &quota) {
				t.Fatalf("error = %v, want the upstream failure from the first round", errRun)
			}
			// The usual exhausted-credentials error, carrying the upstream failure.
			if statusCodeFromError(errRun) != http.StatusServiceUnavailable || !strings.Contains(errRun.Error(), "upstream failed") {
				t.Fatalf("error = %v (status %d), want the cooled-down credential error with the upstream 500", errRun, statusCodeFromError(errRun))
			}
			if got := codex.ids(kind); len(got) != 1 {
				t.Fatalf("codex calls = %v, want one (cooled down afterwards)", got)
			}
			if got := claude.ids(kind); len(got) != 0 {
				t.Fatalf("refused provider was called: %v", got)
			}
		})
	}
}

// TestRequestAdmissionIsSharedAcrossConductorCalls pins the scope a handler puts
// on the context once per client request: a second conductor call for the same
// request (a stream bootstrap retry) reuses the admission decided by the first,
// so the policy is not consulted again and the retry cannot be refused, while a
// new request, with or without a scope, is admitted on its own.
func TestRequestAdmissionIsSharedAcrossConductorCalls(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			model := "admission-scope-" + kind
			claude := &admissionTestExecutor{identifier: "claude", executeErr: &Error{Code: "upstream_error", Message: "upstream failed", HTTPStatus: http.StatusInternalServerError}}
			manager.RegisterExecutor(claude)
			registerAdmissionAuth(t, manager, "admission-scope-claude-"+kind, "claude", model)
			policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{}}
			manager.SetAdmissionPolicy(policy)
			run := func(ctx context.Context) error {
				req := cliproxyexecutor.Request{Model: model}
				if kind == "execute" {
					_, err := manager.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
					return err
				}
				_, err := manager.ExecuteStream(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
				return err
			}
			ctx := WithRequestAdmission(context.Background())
			if err := run(ctx); err == nil {
				t.Fatal("the first call must fail upstream")
			}
			// The key reaches its limit while the first attempt runs.
			policy.refuse["claude"] = claudeQuotaRefusal()
			var quota *ClientQuotaError
			if err := run(ctx); errors.As(err, &quota) {
				t.Fatalf("the retry of an admitted request was refused: %v", err)
			}
			if got := len(policy.callIDs()); got != 1 {
				t.Fatalf("policy consulted %d times, want once for the request", got)
			}
			if got := len(claude.ids(kind)); got != 2 {
				t.Fatalf("upstream calls = %d, want the retry to proceed", got)
			}
			if err := run(WithRequestAdmission(context.Background())); !errors.As(err, &quota) {
				t.Fatalf("a new request must be refused: %v", err)
			}
			// A nested execution derives its context from the request's but is a request of its own.
			if err := run(WithRequestAdmission(ctx)); !errors.As(err, &quota) {
				t.Fatalf("a new scope on a scoped context must be admitted on its own: %v", err)
			}
			if err := run(context.Background()); !errors.As(err, &quota) {
				t.Fatalf("a call without a scope must be refused: %v", err)
			}
		})
	}
}

// TestAdmissionCacheIsSafeForConcurrentCallsOnOneScope runs several conductor
// calls concurrently with one request scope; the shared cache must stay
// consistent (the race detector guards the data).
func TestAdmissionCacheIsSafeForConcurrentCallsOnOneScope(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	const model = "admission-scope-concurrent"
	claude := &admissionTestExecutor{identifier: "claude"}
	manager.RegisterExecutor(claude)
	registerAdmissionAuth(t, manager, "admission-scope-concurrent-claude", "claude", model)
	policy := &admissionTestPolicy{refuse: map[string]*ClientQuotaError{}}
	manager.SetAdmissionPolicy(policy)
	ctx := WithRequestAdmission(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}
	if got := len(claude.ids("execute")); got != 8 {
		t.Fatalf("upstream calls = %d, want 8", got)
	}
}

// gatedAdmissionPolicy parks its first Admit call until released and counts calls.
type gatedAdmissionPolicy struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (p *gatedAdmissionPolicy) Admit(context.Context, *Auth) error {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		close(p.entered)
		<-p.release
	}
	return nil
}

func (p *gatedAdmissionPolicy) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestAdmissionConsultsThePolicyOncePerProviderOnOneScope pins that two calls
// sharing a scope and a provider consult the policy once: the second waits for
// the decision in progress instead of asking again. The final call count is the
// assertion; the yields only give the second call room to reach the cache while
// the first is still inside the policy.
func TestAdmissionConsultsThePolicyOncePerProviderOnOneScope(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	const model = "admission-scope-once"
	claude := &admissionTestExecutor{identifier: "claude"}
	manager.RegisterExecutor(claude)
	registerAdmissionAuth(t, manager, "admission-scope-once-claude", "claude", model)
	policy := &gatedAdmissionPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	manager.SetAdmissionPolicy(policy)
	ctx := WithRequestAdmission(context.Background())
	run := func() error {
		_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		return err
	}
	first := make(chan error, 1)
	go func() { first <- run() }()
	<-policy.entered
	second := make(chan error, 1)
	go func() { second <- run() }()
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	close(policy.release)
	if err := <-first; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if got := policy.callCount(); got != 1 {
		t.Fatalf("policy consulted %d times for one provider on one scope, want 1", got)
	}
	if got := len(claude.ids("execute")); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

// panickingAdmissionPolicy parks its first Admit call until released, then
// panics; later calls admit. It counts every call.
type panickingAdmissionPolicy struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (p *panickingAdmissionPolicy) Admit(context.Context, *Auth) error {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		close(p.entered)
		<-p.release
		panic("admission policy failed")
	}
	return nil
}

func (p *panickingAdmissionPolicy) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// awaitWithin receives from results or fails the test after limit. It bounds a
// liveness check: the fixed code answers at once, the broken code never does.
func awaitWithin(t *testing.T, results <-chan error, limit time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(limit):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// TestAdmissionWaiterStopsWhenItsContextIsCancelled pins that a call waiting for
// another call's decision on the same scope gives up when its own context is
// cancelled, instead of waiting for a policy it does not control.
func TestAdmissionWaiterStopsWhenItsContextIsCancelled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	const model = "admission-scope-cancel"
	claude := &admissionTestExecutor{identifier: "claude"}
	manager.RegisterExecutor(claude)
	registerAdmissionAuth(t, manager, "admission-scope-cancel-claude", "claude", model)
	policy := &gatedAdmissionPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	manager.SetAdmissionPolicy(policy)
	ctx := WithRequestAdmission(context.Background())
	run := func(ctx context.Context) error {
		_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		return err
	}
	first := make(chan error, 1)
	go func() { first <- run(ctx) }()
	<-policy.entered
	waiterCtx, cancel := context.WithCancel(ctx)
	second := make(chan error, 1)
	go func() { second <- run(waiterCtx) }()
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	cancel()
	if err := awaitWithin(t, second, 5*time.Second, "the cancelled waiter"); err == nil {
		t.Fatal("the cancelled waiter was admitted")
	}
	if got := len(claude.ids("execute")); got != 0 {
		t.Fatalf("upstream calls = %d before the first decision, want 0", got)
	}
	close(policy.release)
	if err := <-first; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
}

// TestAdmissionPolicyPanicDoesNotAdmitOrHangWaiters pins that a policy panic
// neither admits the calls waiting for that decision nor leaves them waiting,
// and that the scope recovers: the next call consults the policy again.
func TestAdmissionPolicyPanicDoesNotAdmitOrHangWaiters(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	const model = "admission-scope-panic"
	claude := &admissionTestExecutor{identifier: "claude"}
	manager.RegisterExecutor(claude)
	registerAdmissionAuth(t, manager, "admission-scope-panic-claude", "claude", model)
	policy := &panickingAdmissionPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	manager.SetAdmissionPolicy(policy)
	ctx := WithRequestAdmission(context.Background())
	run := func() error {
		_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		return err
	}
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = run()
	}()
	<-policy.entered
	second := make(chan error, 1)
	go func() { second <- run() }()
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	close(policy.release)
	if recovered := <-panicked; recovered == nil {
		t.Fatal("the policy panic did not reach the first caller")
	}
	if err := awaitWithin(t, second, 5*time.Second, "the waiter of a panicked decision"); err == nil {
		t.Fatal("a waiter was admitted on a decision that was never made")
	}
	if got := len(claude.ids("execute")); got != 0 {
		t.Fatalf("upstream calls = %d after the panic, want 0", got)
	}
	if err := run(); err != nil {
		t.Fatalf("the scope did not recover after the panic: %v", err)
	}
	if got := policy.callCount(); got != 2 {
		t.Fatalf("policy consulted %d times, want the panicked call and one fresh consultation", got)
	}
}

// TestAdmissionAbortStopsThePickLoop pins that a call whose admission decision
// could not be made (the deciding call's policy panicked) returns at once: it
// must not move on to another credential of the same provider, consult the
// policy afresh and reach upstream as if it had been admitted.
func TestAdmissionAbortStopsThePickLoop(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			model := "admission-abort-" + kind
			claude := &admissionTestExecutor{identifier: "claude"}
			manager.RegisterExecutor(claude)
			registerAdmissionAuth(t, manager, "admission-abort-claude-1-"+kind, "claude", model)
			registerAdmissionAuth(t, manager, "admission-abort-claude-2-"+kind, "claude", model)
			policy := &panickingAdmissionPolicy{entered: make(chan struct{}), release: make(chan struct{})}
			manager.SetAdmissionPolicy(policy)
			ctx := WithRequestAdmission(context.Background())
			run := func() error {
				req := cliproxyexecutor.Request{Model: model}
				if kind == "execute" {
					_, err := manager.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
					return err
				}
				_, err := manager.ExecuteStream(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
				return err
			}
			panicked := make(chan any, 1)
			go func() {
				defer func() { panicked <- recover() }()
				_ = run()
			}()
			<-policy.entered
			second := make(chan error, 1)
			go func() { second <- run() }()
			for i := 0; i < 1000; i++ {
				runtime.Gosched()
			}
			close(policy.release)
			if recovered := <-panicked; recovered == nil {
				t.Fatal("the policy panic did not reach the first caller")
			}
			err := awaitWithin(t, second, 5*time.Second, "the waiter of a panicked decision")
			if !errors.Is(err, errAdmissionUndecided) {
				t.Fatalf("waiter error = %v, want errAdmissionUndecided", err)
			}
			if got := len(claude.ids(kind)); got != 0 {
				t.Fatalf("upstream calls = %d for the waiter, want 0", got)
			}
			if got := policy.callCount(); got != 1 {
				t.Fatalf("policy consulted %d times by the waiter's request, want only the panicked call", got)
			}
			if err := run(); err != nil {
				t.Fatalf("a later call on the scope must consult afresh and succeed: %v", err)
			}
			if got := len(claude.ids(kind)); got != 1 {
				t.Fatalf("upstream calls = %d after the fresh consultation, want 1", got)
			}
		})
	}
}
