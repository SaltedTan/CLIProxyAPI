package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const rotationRefreshProvider = "rotation-refresh"

// ctxCheckingAuthStore refuses to write under a cancelled context, like the
// Postgres, git and object stores do.
type ctxCheckingAuthStore struct {
	*memoryAuthTestStore
}

func newCtxCheckingAuthStore() ctxCheckingAuthStore {
	return ctxCheckingAuthStore{memoryAuthTestStore: newMemoryAuthTestStore()}
}

func (s ctxCheckingAuthStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if errCtx := ctx.Err(); errCtx != nil {
		return "", errCtx
	}
	return s.memoryAuthTestStore.Save(ctx, auth)
}

func (s ctxCheckingAuthStore) savedRefreshToken(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if saved := s.auths[id]; saved != nil {
		return authRefreshToken(saved)
	}
	return ""
}

// rotatingRefreshExecutor rotates both tokens on Refresh. For blockID it first
// signals started and waits for release. Like an HTTP client, it reports a
// cancelled context as an error even though the provider has already rotated
// the refresh token by then.
type rotatingRefreshExecutor struct {
	countingRefreshExecutor
	blockID     string
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once

	mu    sync.Mutex
	calls map[string]int
}

func newRotatingRefreshExecutor(blockID string) *rotatingRefreshExecutor {
	return &rotatingRefreshExecutor{
		countingRefreshExecutor: countingRefreshExecutor{id: rotationRefreshProvider},
		blockID:                 blockID,
		started:                 make(chan struct{}),
		release:                 make(chan struct{}),
		calls:                   make(map[string]int),
	}
}

func (e *rotatingRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.calls[auth.ID]++
	e.mu.Unlock()
	if auth.ID == e.blockID {
		e.startedOnce.Do(func() { close(e.started) })
		<-e.release
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = rotatedAccessToken(auth.ID)
	updated.Metadata["refresh_token"] = rotatedRefreshToken(auth.ID)
	updated.Metadata["expires_at"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	return updated, nil
}

func (e *rotatingRefreshExecutor) refreshCallsFor(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
}

func rotatedAccessToken(id string) string  { return "rotated-access-" + id }
func rotatedRefreshToken(id string) string { return "rotated-refresh-" + id }

func newRotationManager(t *testing.T, blockID string) (*Manager, ctxCheckingAuthStore, *rotatingRefreshExecutor) {
	t.Helper()
	store := newCtxCheckingAuthStore()
	executor := newRotatingRefreshExecutor(blockID)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	return manager, store, executor
}

// registerRotationAuth registers an OAuth auth without persisting it, so the store
// only sees writes made by a refresh. A due auth has an expired access token.
func registerRotationAuth(t *testing.T, manager *Manager, id string, due bool) {
	t.Helper()
	now := time.Now()
	expiresAt := now.Add(2 * time.Hour)
	if due {
		expiresAt = now.Add(-time.Minute)
	}
	auth := &Auth{
		ID:              id,
		Provider:        rotationRefreshProvider,
		Status:          StatusActive,
		LastRefreshedAt: now,
		Metadata: map[string]any{
			"access_token":             "old-access-" + id,
			"refresh_token":            "old-refresh-" + id,
			"expires_at":               expiresAt.Format(time.RFC3339),
			"refresh_interval_seconds": 3600,
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register %s: %v", id, errRegister)
	}
}

func assertRotatedTokensKept(t *testing.T, manager *Manager, store ctxCheckingAuthStore, id string) {
	t.Helper()
	if got, want := store.savedRefreshToken(id), rotatedRefreshToken(id); got != want {
		t.Fatalf("persisted refresh token = %q, want the rotated %q", got, want)
	}
	current, ok := manager.GetByID(id)
	if !ok || current == nil {
		t.Fatalf("auth %s missing from the manager", id)
	}
	if got, want := authRefreshToken(current), rotatedRefreshToken(id); got != want {
		t.Fatalf("manager refresh token = %q, want the rotated %q", got, want)
	}
	if got, want := authAccessToken(current), rotatedAccessToken(id); got != want {
		t.Fatalf("manager access token = %q, want the rotated %q", got, want)
	}
}

// Stopping the auto-refresh loop (as Shutdown does) must not abort a token
// exchange that is already running: the provider has rotated the refresh token,
// so dropping the result costs the account a re-login.
func TestStopAutoRefreshKeepsInFlightRefreshAndPersistsRotatedTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "auto-refresh-in-flight"
		manager, store, executor := newRotationManager(t, id)
		registerRotationAuth(t, manager, id, true)

		ctx, cancelCtx := context.WithCancel(context.Background())
		defer cancelCtx()
		manager.StartAutoRefresh(ctx, time.Minute)
		<-executor.started

		manager.StopAutoRefresh()
		cancelCtx()
		close(executor.release)
		synctest.Wait()

		assertRotatedTokensKept(t, manager, store, id)
	})
}

// A request-path refresh (401 recovery, management force refresh) must finish
// and persist even when the client goes away mid-exchange.
func TestRefreshForRequestPersistsRotatedTokensAfterCallerCancels(t *testing.T) {
	const id = "request-in-flight"
	manager, store, executor := newRotationManager(t, id)
	registerRotationAuth(t, manager, id, true)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	type result struct {
		auth *Auth
		err  error
	}
	done := make(chan result, 1)
	go func() {
		refreshed, errRefresh := manager.refreshAuthForRequest(reqCtx, id, "")
		done <- result{auth: refreshed, err: errRefresh}
	}()
	<-executor.started
	cancelReq()
	close(executor.release)
	res := <-done

	assertRotatedTokensKept(t, manager, store, id)
	if res.err != nil {
		t.Fatalf("refreshAuthForRequest() error = %v, want nil", res.err)
	}
	if got, want := authRefreshToken(res.auth), rotatedRefreshToken(id); got != want {
		t.Fatalf("returned refresh token = %q, want %q", got, want)
	}
}

// staleTokenRefreshExecutor is a non-Claude provider whose upstream calls all
// answer 401. Its Refresh blocks until released, then rotates the tokens unless
// its context was cancelled.
type staleTokenRefreshExecutor struct {
	bootstrapErr bool // ExecuteStream fails in the first chunk instead of the call
	started      chan struct{}
	release      chan struct{}
	startedOnce  sync.Once

	upstreamCalls atomic.Int32
	refreshCalls  atomic.Int32
}

func (*staleTokenRefreshExecutor) Identifier() string { return "codex" }

func staleTokenError() error {
	return &Error{HTTPStatus: http.StatusUnauthorized, Message: "Your authentication token has been invalidated."}
}

func (e *staleTokenRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.upstreamCalls.Add(1)
	return cliproxyexecutor.Response{}, staleTokenError()
}

func (e *staleTokenRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.upstreamCalls.Add(1)
	return cliproxyexecutor.Response{}, staleTokenError()
}

func (e *staleTokenRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.upstreamCalls.Add(1)
	if !e.bootstrapErr {
		return nil, staleTokenError()
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Err: staleTokenError()}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *staleTokenRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	e.startedOnce.Do(func() { close(e.started) })
	<-e.release
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = rotatedAccessToken(auth.ID)
	updated.Metadata["refresh_token"] = rotatedRefreshToken(auth.ID)
	return updated, nil
}

func (*staleTokenRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// A client that disconnects while its 401 triggers a refresh must still get the
// rotated tokens persisted, but no retry upstream and no cooldown or error
// recorded from the stale 401 against the freshly refreshed credential.
func TestUnauthorizedRefreshForCancelledRequestKeepsTokensWithoutRetryOrCooldown(t *testing.T) {
	stream := func(ctx context.Context, manager *Manager, model string) error {
		_, errStream := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		return errStream
	}
	tests := []struct {
		name         string
		bootstrapErr bool
		run          func(context.Context, *Manager, string) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "count tokens",
			run: func(ctx context.Context, manager *Manager, model string) error {
				_, errCount := manager.ExecuteCount(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errCount
			},
		},
		{name: "stream", run: stream},
		{name: "stream bootstrap", bootstrapErr: true, run: stream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &staleTokenRefreshExecutor{
				bootstrapErr: tt.bootstrapErr,
				started:      make(chan struct{}),
				release:      make(chan struct{}),
			}
			store := newCtxCheckingAuthStore()
			manager := NewManager(store, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(executor)
			model := "codex-cancel-model-" + uuid.NewString()
			auth := &Auth{
				ID:       "codex-cancel-auth-" + uuid.NewString(),
				Provider: "codex",
				Metadata: map[string]any{"access_token": "stale-access", "refresh_token": "old-refresh"},
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tt.run(ctx, manager, model) }()
			<-executor.started
			cancel()
			close(executor.release)
			errRun := <-done

			if !errors.Is(errRun, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", errRun)
			}
			assertRotatedTokensKept(t, manager, store, auth.ID)
			if got := executor.refreshCalls.Load(); got != 1 {
				t.Fatalf("Refresh calls = %d, want 1", got)
			}
			if got := executor.upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no retry for a cancelled request)", got)
			}
			requireClaudeCancellationNeutral(t, manager, auth.ID, model)
			current, _ := manager.GetByID(auth.ID)
			if current.LastError != nil {
				t.Fatalf("auth LastError = %+v, want none from the stale 401", current.LastError)
			}
			if state := current.ModelStates[model]; state != nil && state.LastError != nil {
				t.Fatalf("model LastError = %+v, want none from the stale 401", state.LastError)
			}
		})
	}
}
