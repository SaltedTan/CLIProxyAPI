package cliproxy

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// shutdownRefreshStore keeps the last saved refresh token per auth and, like the
// database-backed stores, refuses to write under a cancelled context.
type shutdownRefreshStore struct {
	mu    sync.Mutex
	saved map[string]string
}

func (s *shutdownRefreshStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }

func (s *shutdownRefreshStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if errCtx := ctx.Err(); errCtx != nil {
		return "", errCtx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = make(map[string]string)
	}
	token, _ := auth.Metadata["refresh_token"].(string)
	s.saved[auth.ID] = token
	return auth.ID, nil
}

func (s *shutdownRefreshStore) Delete(context.Context, string) error { return nil }

func (s *shutdownRefreshStore) refreshToken(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[id]
}

// blockingRotationExecutor blocks Refresh until released, then rotates the
// refresh token unless its context was cancelled, like an HTTP token exchange.
type blockingRotationExecutor struct {
	syncTestExecutor
	started chan struct{}
	release chan struct{}
}

func (e *blockingRotationExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	close(e.started)
	<-e.release
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "rotated-access"
	updated.Metadata["refresh_token"] = "rotated-refresh"
	return updated, nil
}

// drainWaitHook signals once the manager logs that it waits for authID's refresh.
type drainWaitHook struct {
	authID  string
	once    sync.Once
	waiting chan struct{}
}

func (h *drainWaitHook) Levels() []log.Level { return []log.Level{log.InfoLevel} }

func (h *drainWaitHook) Fire(entry *log.Entry) error {
	if strings.HasPrefix(entry.Message, "waiting for") && strings.Contains(entry.Message, h.authID) {
		h.once.Do(func() { close(h.waiting) })
	}
	return nil
}

func TestServiceShutdownWaitsForInFlightRefreshToPersist(t *testing.T) {
	const authID = "codex-shutdown-refresh.json"
	hook := &drainWaitHook{authID: authID, waiting: make(chan struct{})}
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldLevel := logger.GetLevel()
	logger.AddHook(hook)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		logger.SetLevel(oldLevel)
		logger.ReplaceHooks(oldHooks)
	})

	store := &shutdownRefreshStore{}
	executor := &blockingRotationExecutor{started: make(chan struct{}), release: make(chan struct{})}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh"},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	// A management force refresh whose HTTP request goes away during shutdown.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	refreshDone := make(chan error, 1)
	go func() {
		_, errRefresh := manager.ForceRefreshAuth(reqCtx, authID)
		refreshDone <- errRefresh
	}()
	<-executor.started

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- service.Shutdown(context.Background()) }()

	select {
	case errShutdown := <-shutdownDone:
		close(executor.release)
		<-refreshDone
		t.Fatalf("Shutdown() returned %v while a token refresh was still in flight", errShutdown)
	case <-hook.waiting:
	}
	cancelReq()
	close(executor.release)

	if errShutdown := <-shutdownDone; errShutdown != nil {
		t.Fatalf("Shutdown() error = %v", errShutdown)
	}
	if got := store.refreshToken(authID); got != "rotated-refresh" {
		t.Fatalf("refresh token persisted by the time Shutdown returned = %q, want %q", got, "rotated-refresh")
	}
	if errRefresh := <-refreshDone; errRefresh != nil {
		t.Fatalf("ForceRefreshAuth() error = %v, want nil", errRefresh)
	}
}
