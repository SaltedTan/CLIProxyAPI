package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type obsoleteRefreshFailureExecutor struct {
	schedulerProviderTestExecutor
	started chan struct{}
	release chan struct{}
	err     error
}

func (e *obsoleteRefreshFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	close(e.started)
	<-e.release
	return nil, e.err
}

// A refresh that fails or is canceled after a replace committed other credentials describes
// the replaced credentials. The replacement keeps its refresh error, status and refresh
// schedule.
func TestManagerRefreshAuthObsoleteFailureLeavesReplacementUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: &Error{HTTPStatus: http.StatusUnauthorized, Message: "refresh token revoked"}},
		{name: "transient", err: &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}},
		{name: "canceled", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			executor := &obsoleteRefreshFailureExecutor{
				schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "antigravity"},
				started:                       make(chan struct{}),
				release:                       make(chan struct{}),
				err:                           tc.err,
			}
			manager.RegisterExecutor(executor)
			auth := &Auth{
				ID:       "obsolete-refresh-failure-" + tc.name,
				Provider: "antigravity",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token":  "access-token-a",
					"refresh_token": "refresh-token-a",
					"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
				},
			}
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			refreshDone := make(chan struct{})
			go func() {
				manager.refreshAuth(ctx, auth.ID)
				close(refreshDone)
			}()
			<-executor.started

			replacement := currentForLineage(t, manager, auth.ID)
			replacement.Metadata["access_token"] = "access-token-b"
			replacement.Metadata["refresh_token"] = "refresh-token-b"
			replacement.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
			if _, errUpdate := manager.Update(ctx, replacement); errUpdate != nil {
				t.Fatalf("Update() error = %v", errUpdate)
			}
			before := currentForLineage(t, manager, auth.ID)

			close(executor.release)
			<-refreshDone

			current := currentForLineage(t, manager, auth.ID)
			if got := authAccessToken(current); got != "access-token-b" {
				t.Fatalf("access_token = %q, want access-token-b", got)
			}
			if current.RefreshError != nil || !current.RefreshErrorAt.IsZero() {
				t.Fatalf("RefreshError = %+v at %v, want none", current.RefreshError, current.RefreshErrorAt)
			}
			if current.LastError != nil || current.Unavailable || current.Status != StatusActive || current.StatusMessage != "" {
				t.Fatalf("state = (%+v, unavailable %v, %s, %q), want the active replacement", current.LastError, current.Unavailable, current.Status, current.StatusMessage)
			}
			if !current.NextRefreshAfter.Equal(before.NextRefreshAfter) || current.RefreshFailures != before.RefreshFailures {
				t.Fatalf("refresh schedule = (%v, %d), want (%v, %d)", current.NextRefreshAfter, current.RefreshFailures, before.NextRefreshAfter, before.RefreshFailures)
			}
			if current.Generation != before.Generation {
				t.Fatalf("generation = %d, want %d: the obsolete outcome mutated the replacement", current.Generation, before.Generation)
			}
		})
	}
}
