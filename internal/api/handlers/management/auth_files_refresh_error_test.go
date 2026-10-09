package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// refreshFailureExecutor fails every refresh with err until err is cleared.
type refreshFailureExecutor struct {
	mu  sync.Mutex
	err error
}

func (e *refreshFailureExecutor) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *refreshFailureExecutor) Identifier() string { return "codex" }

func (e *refreshFailureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
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

func (e *refreshFailureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshFailureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *refreshFailureExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshFailureExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type refreshFailureFixture struct {
	manager  *coreauth.Manager
	executor *refreshFailureExecutor
	handler  *Handler
	id       string
	expiry   time.Time
}

// newRefreshFailureFixture registers a file-backed codex credential whose access
// token is still valid; mutate adjusts it before registration.
func newRefreshFailureFixture(t *testing.T, refreshErr error, mutate func(*coreauth.Auth)) *refreshFailureFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	id := "codex-refresh.json"
	path := filepath.Join(authDir, id)
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	expiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &refreshFailureExecutor{}
	executor.setErr(refreshErr)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:         id,
		FileName:   id,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": path},
		Metadata: map[string]any{
			"type":         "codex",
			"email":        "refresh@example.com",
			"access_token": "valid-access-token",
			"expired":      expiry.Format(time.RFC3339),
		},
	}
	if mutate != nil {
		mutate(auth)
	}
	registerAuthForLookupTest(t, manager, auth)
	cfg := &config.Config{AuthDir: authDir}
	return &refreshFailureFixture{
		manager:  manager,
		executor: executor,
		handler:  NewHandlerWithoutConfigFilePath(cfg, manager),
		id:       id,
		expiry:   expiry,
	}
}

func (f *refreshFailureFixture) refresh(t *testing.T, wantErr bool) {
	t.Helper()
	_, errRefresh := f.manager.ForceRefreshAuth(context.Background(), f.id)
	if (errRefresh != nil) != wantErr {
		t.Fatalf("ForceRefreshAuth() error = %v, wantErr %v", errRefresh, wantErr)
	}
}

func (f *refreshFailureFixture) listEntry(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials?name="+url.QueryEscape(f.id), nil)
	f.handler.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode list: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files = %+v, want one entry", payload.Files)
	}
	return payload.Files[0]
}

func refreshErrorField(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	refreshErr, ok := entry["refresh_error"].(map[string]any)
	if !ok {
		t.Fatalf("refresh_error missing from entry: %+v", entry)
	}
	return refreshErr
}

func TestListAuthFiles_TransientRefreshFailureShowsRefreshErrorOnly(t *testing.T) {
	refreshErr := &coreauth.Error{
		HTTPStatus: http.StatusServiceUnavailable,
		Message:    `token refresh failed after 3 attempts: status 503; Cookie: session=cookie-secret-value; "refresh_token":"rt-secret-value"; Authorization: Bearer bearer-secret-value`,
	}
	f := newRefreshFailureFixture(t, refreshErr, nil)
	f.refresh(t, true)

	entry := f.listEntry(t)
	if entry["status"] != string(coreauth.StatusActive) || entry["status_message"] != "" {
		t.Fatalf("transient refresh failure changed status: status=%v status_message=%q", entry["status"], entry["status_message"])
	}
	got := refreshErrorField(t, entry)
	if got["http_status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("refresh_error.http_status = %v, want 503", got["http_status"])
	}
	if _, hasCode := got["code"]; hasCode {
		t.Fatalf("refresh_error.code = %v, want absent for a transient failure", got["code"])
	}
	message, _ := got["message"].(string)
	if !strings.Contains(message, "status 503") {
		t.Fatalf("refresh_error.message = %q, want the failure diagnostic", message)
	}
	for _, secret := range []string{"cookie-secret-value", "rt-secret-value", "bearer-secret-value"} {
		if strings.Contains(message, secret) {
			t.Fatalf("refresh_error.message leaks %q: %s", secret, message)
		}
	}
	if at, _ := got["at"].(string); at == "" {
		t.Fatalf("refresh_error.at missing: %+v", got)
	}
	if next, _ := entry["next_refresh_after"].(string); next == "" {
		t.Fatalf("next_refresh_after missing: %+v", entry)
	}
}

func TestListAuthFiles_RejectedRefreshTokenSetsStatusMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{name: "invalid grant", err: &coreauth.Error{HTTPStatus: http.StatusBadRequest, Message: `token refresh failed with status 400: {"error":"invalid_grant"}`}, code: "invalid_grant"},
		{name: "unauthorized", err: errors.New("token refresh failed with status 401: unauthorized"), code: "unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefreshFailureFixture(t, tc.err, nil)
			f.refresh(t, true)

			entry := f.listEntry(t)
			want := "refresh token rejected; sign in again before the access token expires at " + f.expiry.Format(time.RFC3339)
			if entry["status"] != string(coreauth.StatusActive) || entry["status_message"] != want {
				t.Fatalf("status=%v status_message=%q, want active with %q", entry["status"], entry["status_message"], want)
			}
			if got := refreshErrorField(t, entry); got["code"] != tc.code {
				t.Fatalf("refresh_error.code = %v, want %q", got["code"], tc.code)
			}
			// The listing must not write the message back to the credential.
			if auth, _ := f.manager.GetByID(f.id); auth.StatusMessage != "" {
				t.Fatalf("auth StatusMessage = %q, want empty", auth.StatusMessage)
			}
		})
	}
}

func TestListAuthFiles_RefreshErrorSurvivesRequestsUntilRefreshSucceeds(t *testing.T) {
	f := newRefreshFailureFixture(t, &coreauth.Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}, nil)
	f.refresh(t, true)

	ctx := context.Background()
	f.manager.MarkResult(ctx, coreauth.Result{AuthID: f.id, Provider: "codex", Model: "gpt-test", Error: &coreauth.Error{HTTPStatus: http.StatusInternalServerError, Message: "request failed"}})
	f.manager.MarkResult(ctx, coreauth.Result{AuthID: f.id, Provider: "codex", Model: "gpt-test", Success: true})
	if got := refreshErrorField(t, f.listEntry(t)); got["http_status"] != float64(http.StatusBadGateway) {
		t.Fatalf("request results changed refresh_error: %+v", got)
	}

	f.executor.setErr(nil)
	f.refresh(t, false)
	entry := f.listEntry(t)
	if _, has := entry["refresh_error"]; has {
		t.Fatalf("successful refresh must clear refresh_error: %+v", entry["refresh_error"])
	}
	if _, has := entry["next_refresh_after"]; has {
		t.Fatalf("next_refresh_after must only accompany refresh_error: %+v", entry["next_refresh_after"])
	}
}

func TestListAuthFiles_RejectedRefreshTokenKeepsExistingStatusMessage(t *testing.T) {
	unauthorized := errors.New("token refresh failed with status 401: unauthorized")
	cases := []struct {
		name       string
		mutate     func(*coreauth.Auth)
		wantStatus coreauth.Status
		wantMsg    string
	}{
		{
			name: "disabled",
			mutate: func(auth *coreauth.Auth) {
				auth.Disabled = true
				auth.Status = coreauth.StatusDisabled
				auth.StatusMessage = "disabled via management API"
			},
			wantStatus: coreauth.StatusDisabled,
			wantMsg:    "disabled via management API",
		},
		{
			name: "error",
			mutate: func(auth *coreauth.Auth) {
				auth.Status = coreauth.StatusError
				auth.StatusMessage = "quota_exceeded"
				auth.Unavailable = true
				auth.NextRetryAfter = time.Now().Add(time.Hour)
			},
			wantStatus: coreauth.StatusError,
			wantMsg:    "quota_exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefreshFailureFixture(t, unauthorized, tc.mutate)
			f.refresh(t, true)

			entry := f.listEntry(t)
			if entry["status"] != string(tc.wantStatus) || entry["status_message"] != tc.wantMsg {
				t.Fatalf("status=%v status_message=%q, want %s with %q", entry["status"], entry["status_message"], tc.wantStatus, tc.wantMsg)
			}
			refreshErrorField(t, entry)
		})
	}
}
