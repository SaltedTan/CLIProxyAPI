package auth

import (
	"context"
	"net/http"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// staleResultQuotaExecutor answers with quota headers once released.
type staleResultQuotaExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (*staleResultQuotaExecutor) Identifier() string { return "codex" }

func (e *staleResultQuotaExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	close(e.started)
	<-e.release
	setQuotaAttemptIsolationHeaders(ctx)
	return cliproxyexecutor.Response{}, nil
}

func (*staleResultQuotaExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*staleResultQuotaExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*staleResultQuotaExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*staleResultQuotaExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

// A response that arrives after the credential version changed still reports the quota of
// the account while the quota lineage matches (the manager's own token refresh). After a
// replace with another account its headers describe the old account and are dropped.
func TestMarkResultKeepsQuotaObservationOfTheSameAccountAcrossCredentialVersions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rotate      func(*Manager, *Auth) error
		wantPercent string
	}{
		{
			name: "same account refresh",
			rotate: func(manager *Manager, base *Auth) error {
				_, errUpdate := manager.UpdateRefreshedAuth(context.Background(), base, withTokens(base, "2"))
				return errUpdate
			},
			wantPercent: "91",
		},
		{
			name: "another account replace",
			rotate: func(manager *Manager, base *Auth) error {
				_, errUpdate := manager.Update(context.Background(), withTokens(base, "other"))
				return errUpdate
			},
			wantPercent: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
			executor := &staleResultQuotaExecutor{started: make(chan struct{}), release: make(chan struct{})}
			manager.RegisterExecutor(executor)
			id := "stale-result-quota-" + tc.name
			model := "gpt-stale-result-quota"
			registered, errRegister := manager.Register(context.Background(), &Auth{
				ID:       id,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"type": "codex", "access_token": "access-1", "refresh_token": "refresh-1"},
			})
			if errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })

			done := make(chan error, 1)
			go func() {
				ctx := internallogging.WithResponseHeadersHolder(context.Background())
				_, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				done <- errExecute
			}()
			<-executor.started
			if errRotate := tc.rotate(manager, registered); errRotate != nil {
				t.Fatalf("rotate credential: %v", errRotate)
			}
			rotated := currentForLineage(t, manager, id)
			if rotated.CredentialVersion <= registered.CredentialVersion {
				t.Fatalf("credential version = %d, want above %d", rotated.CredentialVersion, registered.CredentialVersion)
			}
			close(executor.release)
			if errExecute := <-done; errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}

			current := currentForLineage(t, manager, id)
			if got := current.Quota.Signals["X-Codex-Primary-Used-Percent"]; got != tc.wantPercent {
				t.Fatalf("quota observation = %q, want %q; quota=%#v", got, tc.wantPercent, current.Quota)
			}
		})
	}
}
