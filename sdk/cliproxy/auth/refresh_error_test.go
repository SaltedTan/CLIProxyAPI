package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// refreshErrorRecordTestExecutor fails every refresh with err until err is cleared.
type refreshErrorRecordTestExecutor struct {
	schedulerProviderTestExecutor
	mu  sync.Mutex
	err error
}

func (e *refreshErrorRecordTestExecutor) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *refreshErrorRecordTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	auth.Metadata["access_token"] = "refreshed-access-token"
	auth.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return auth, nil
}

func registerRefreshErrorTestAuth(t *testing.T, refreshErr error) (*Manager, *refreshErrorRecordTestExecutor, string) {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &refreshErrorRecordTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"}}
	executor.setErr(refreshErr)
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "refresh-error-record",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":        "refresh@example.com",
			"access_token": "valid-access-token",
			"expired":      time.Now().Add(48 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return manager, executor, auth.ID
}

func TestManager_RefreshFailureRecordsRefreshErrorUntilRefreshSucceeds(t *testing.T) {
	ctx := context.Background()
	manager, executor, id := registerRefreshErrorTestAuth(t, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"})

	before := time.Now()
	if _, errRefresh := manager.ForceRefreshAuth(ctx, id); errRefresh == nil {
		t.Fatal("expected refresh to fail")
	}
	failed, _ := manager.GetByID(id)
	if failed.RefreshError == nil || failed.RefreshError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("RefreshError = %+v, want HTTP 503", failed.RefreshError)
	}
	if failed.RefreshErrorAt.Before(before) {
		t.Fatalf("RefreshErrorAt = %v, want at or after %v", failed.RefreshErrorAt, before)
	}
	if failed.RefreshTokenRejected() {
		t.Fatal("a 503 refresh failure must not count as a rejected refresh token")
	}
	if failed.Status != StatusActive || failed.Unavailable || failed.StatusMessage != "" {
		t.Fatalf("valid access token must keep the credential active, got status=%s unavailable=%v message=%q", failed.Status, failed.Unavailable, failed.StatusMessage)
	}
	if failed.NextRefreshAfter.IsZero() {
		t.Fatal("expected a scheduled refresh retry")
	}

	// Request results replace LastError but never the refresh failure.
	manager.MarkResult(ctx, Result{AuthID: id, Provider: "codex", Model: "gpt-test", Error: &Error{HTTPStatus: http.StatusInternalServerError, Message: "request failed"}})
	manager.MarkResult(ctx, Result{AuthID: id, Provider: "codex", Model: "gpt-test", Success: true})
	afterRequests, _ := manager.GetByID(id)
	if afterRequests.RefreshError == nil || afterRequests.RefreshError.Message != failed.RefreshError.Message || !afterRequests.RefreshErrorAt.Equal(failed.RefreshErrorAt) {
		t.Fatalf("request results changed RefreshError: before=%+v at %v, after=%+v at %v", failed.RefreshError, failed.RefreshErrorAt, afterRequests.RefreshError, afterRequests.RefreshErrorAt)
	}

	executor.setErr(nil)
	if _, errRefresh := manager.ForceRefreshAuth(ctx, id); errRefresh != nil {
		t.Fatalf("refresh: %v", errRefresh)
	}
	refreshed, _ := manager.GetByID(id)
	if refreshed.RefreshError != nil || !refreshed.RefreshErrorAt.IsZero() {
		t.Fatalf("successful refresh must clear the refresh failure, got %+v at %v", refreshed.RefreshError, refreshed.RefreshErrorAt)
	}
}

func TestManager_RefreshFailureClassifiesRejectedRefreshToken(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     string
		rejected bool
	}{
		{name: "invalid grant", err: &Error{HTTPStatus: http.StatusBadRequest, Message: `{"error":"invalid_grant"}`}, code: "invalid_grant", rejected: true},
		{name: "unauthorized", err: errors.New("token refresh failed with status 401: unauthorized"), code: "unauthorized", rejected: true},
		{name: "server error", err: &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}},
		{name: "rate limited", err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}},
		{name: "network", err: errors.New("dial tcp: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, id := registerRefreshErrorTestAuth(t, tc.err)
			_, _ = manager.ForceRefreshAuth(context.Background(), id)
			failed, _ := manager.GetByID(id)
			if failed.RefreshError == nil {
				t.Fatal("expected RefreshError to be recorded")
			}
			if failed.RefreshError.Code != tc.code {
				t.Fatalf("RefreshError.Code = %q, want %q", failed.RefreshError.Code, tc.code)
			}
			if got := failed.RefreshTokenRejected(); got != tc.rejected {
				t.Fatalf("RefreshTokenRejected() = %v, want %v", got, tc.rejected)
			}
		})
	}
}

// upstreamBodyRefreshError mimics an error whose text is a raw upstream body but
// which offers a log-safe diagnostic.
type upstreamBodyRefreshError struct{}

func (upstreamBodyRefreshError) Error() string {
	return `{"error":"server_error","debug":"secret-body-value"}`
}
func (upstreamBodyRefreshError) StatusCode() int { return http.StatusBadGateway }
func (upstreamBodyRefreshError) LogDiagnostic() string {
	return "Home refresh upstream response: status=502"
}

func TestManager_RefreshFailureRecordsOperatorSafeMessage(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   string
		secret []string
	}{
		{
			name:   "log diagnostic",
			err:    upstreamBodyRefreshError{},
			want:   "Home refresh upstream response: status=502",
			secret: []string{"secret-body-value"},
		},
		{
			name:   "credentials and cookies",
			err:    errors.New(`token refresh failed with status 503: "refresh_token":"rt-secret-value" Authorization: Bearer bearer-secret-value Cookie: session=cookie-secret-value`),
			want:   "token refresh failed with status 503",
			secret: []string{"rt-secret-value", "bearer-secret-value", "cookie-secret-value"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, id := registerRefreshErrorTestAuth(t, tc.err)
			_, _ = manager.ForceRefreshAuth(context.Background(), id)
			failed, _ := manager.GetByID(id)
			if failed.RefreshError == nil || !strings.Contains(failed.RefreshError.Message, tc.want) {
				t.Fatalf("RefreshError = %+v, want message containing %q", failed.RefreshError, tc.want)
			}
			for _, secret := range tc.secret {
				if strings.Contains(failed.RefreshError.Message, secret) {
					t.Fatalf("RefreshError.Message leaks %q: %s", secret, failed.RefreshError.Message)
				}
			}
		})
	}
}

func TestAuth_RefreshErrorIsRuntimeOnlyAndDeepCopied(t *testing.T) {
	auth := &Auth{
		ID:             "refresh-error-clone",
		Provider:       "codex",
		RefreshError:   &Error{Code: "invalid_grant", Message: "refresh token revoked", HTTPStatus: http.StatusBadRequest},
		RefreshErrorAt: time.Now(),
	}
	clone := auth.Clone()
	if clone.RefreshError == auth.RefreshError {
		t.Fatal("Clone must not share the RefreshError pointer")
	}
	if *clone.RefreshError != *auth.RefreshError || !clone.RefreshErrorAt.Equal(auth.RefreshErrorAt) {
		t.Fatalf("Clone changed the refresh failure: %+v at %v", clone.RefreshError, clone.RefreshErrorAt)
	}
	data, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	if strings.Contains(string(data), "refresh token revoked") {
		t.Fatalf("refresh failure must stay out of serialized auth: %s", data)
	}
}

func TestMergePreparedAuthClearsRefreshErrorOnlyAfterATokenExchange(t *testing.T) {
	refreshedAt := time.Now().Add(-time.Hour)
	failed := &Auth{
		ID:              "prepared-refresh-error",
		Provider:        "meta",
		LastRefreshedAt: refreshedAt,
		RefreshError:    &Error{Code: "invalid_grant", Message: "mint rejected", HTTPStatus: http.StatusBadRequest},
		RefreshErrorAt:  refreshedAt.Add(time.Minute),
	}

	// A preparation that only adds metadata keeps the refresh failure.
	metadataOnly := failed.Clone()
	metadataOnly.Metadata = map[string]any{"base_url": "https://example.invalid"}
	if merged := MergePreparedAuth(failed, failed.Clone(), metadataOnly); merged.RefreshError == nil {
		t.Fatal("a preparation without a token exchange must keep RefreshError")
	}

	// A preparation that minted new credentials stamps LastRefreshedAt and clears it.
	minted := failed.Clone()
	minted.LastRefreshedAt = time.Now()
	merged := MergePreparedAuth(failed, failed.Clone(), minted)
	if merged.RefreshError != nil || !merged.RefreshErrorAt.IsZero() || merged.RefreshTokenRejected() {
		t.Fatalf("a successful token exchange must clear the refresh failure, got %+v at %v", merged.RefreshError, merged.RefreshErrorAt)
	}
}
