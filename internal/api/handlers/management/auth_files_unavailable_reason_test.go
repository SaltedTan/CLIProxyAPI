package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	reasonTestCoolingModel = "gpt-5-cooling"
	reasonTestOtherModel   = "gpt-5-other"
)

// TestListAuthFiles_UnavailableReason builds each state through the manager the
// way requests do, then checks the listed reason against the selector: "models"
// must leave other models selectable, "auth" and "cooldown" must not.
func TestListAuthFiles_UnavailableReason(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	hour := time.Hour
	coolModel := func(credentialScope bool) func(*testing.T, *coreauth.Manager, string) {
		return func(t *testing.T, manager *coreauth.Manager, id string) {
			manager.MarkResult(context.Background(), coreauth.Result{
				AuthID: id, Provider: "codex", Model: reasonTestCoolingModel,
				Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "upstream request failed"},
				RetryAfter: &hour, CredentialScope: credentialScope,
			})
		}
	}
	tests := []struct {
		name           string
		tokenLifetime  time.Duration
		setup          func(*testing.T, *coreauth.Manager, string)
		wantReason     string
		wantSelectable bool
	}{
		{
			name:           "available",
			tokenLifetime:  48 * time.Hour,
			setup:          func(*testing.T, *coreauth.Manager, string) {},
			wantSelectable: true,
		},
		{
			name:           "model cooldown only",
			tokenLifetime:  48 * time.Hour,
			setup:          coolModel(false),
			wantReason:     authFileUnavailableModels,
			wantSelectable: true,
		},
		{
			name:          "expired token with model cooldown",
			tokenLifetime: -time.Minute,
			setup:         coolModel(false),
			wantReason:    authFileUnavailableAuth,
		},
		{
			name:          "valid token with model cooldown and transient refresh failure",
			tokenLifetime: 48 * time.Hour,
			setup: func(t *testing.T, manager *coreauth.Manager, id string) {
				coolModel(false)(t, manager, id)
				if _, errRefresh := manager.ForceRefreshAuth(context.Background(), id); errRefresh == nil {
					t.Fatal("ForceRefreshAuth() succeeded, want the transient failure")
				}
			},
			wantReason:     authFileUnavailableModels,
			wantSelectable: true,
		},
		{
			name:          "terminal unauthorized",
			tokenLifetime: 48 * time.Hour,
			setup: func(t *testing.T, manager *coreauth.Manager, id string) {
				coolModel(false)(t, manager, id)
				auth, _ := manager.GetByID(id)
				auth.Unavailable = true
				auth.Status = coreauth.StatusError
				auth.StatusMessage = "unauthorized"
				auth.NextRetryAfter = time.Time{}
				auth.NextRefreshAfter = time.Time{}
				auth.LastError = &coreauth.Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatalf("update auth: %v", errUpdate)
				}
			},
			wantReason: authFileUnavailableAuth,
		},
		{
			name:          "credential-wide quota",
			tokenLifetime: 48 * time.Hour,
			setup:         coolModel(true),
			wantReason:    authFileUnavailableCooldown,
		},
		{
			name:          "credential cooldown without model states",
			tokenLifetime: 48 * time.Hour,
			setup: func(t *testing.T, manager *coreauth.Manager, id string) {
				auth, _ := manager.GetByID(id)
				auth.Unavailable = true
				auth.Status = coreauth.StatusError
				auth.StatusMessage = "upstream request failed"
				auth.NextRetryAfter = time.Now().Add(time.Hour)
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatalf("update auth: %v", errUpdate)
				}
			},
			wantReason: authFileUnavailableCooldown,
		},
		{
			name:          "disabled",
			tokenLifetime: 48 * time.Hour,
			setup: func(t *testing.T, manager *coreauth.Manager, id string) {
				coolModel(false)(t, manager, id)
				auth, _ := manager.GetByID(id)
				auth.Disabled = true
				auth.Status = coreauth.StatusDisabled
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatalf("update auth: %v", errUpdate)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(&refreshFailureExecutor{err: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary refresh outage"}})
			id := "reason.json"
			path := filepath.Join(cfg.AuthDir, id)
			if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
				t.Fatalf("write auth file: %v", errWrite)
			}
			registerAuthForLookupTest(t, manager, &coreauth.Auth{
				ID: id, FileName: id, Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"path": path},
				Metadata: map[string]any{
					"type":         "codex",
					"access_token": "access-token",
					"expired":      time.Now().Add(tc.tokenLifetime).Format(time.RFC3339),
				},
			})
			tc.setup(t, manager, id)

			entry := listUnavailableReasonEntry(t, NewHandlerWithoutConfigFilePath(cfg, manager), id)
			reason, hasReason := entry["unavailable_reason"]
			if tc.wantReason == "" {
				if hasReason {
					t.Fatalf("unavailable_reason = %v, want absent (unavailable=%v)", reason, entry["unavailable"])
				}
			} else {
				if entry["unavailable"] != true {
					t.Fatalf("unavailable = %v, want true", entry["unavailable"])
				}
				if reason != tc.wantReason {
					t.Fatalf("unavailable_reason = %v, want %q", reason, tc.wantReason)
				}
			}

			auth, _ := manager.GetByID(id)
			selected, errPick := (&coreauth.FillFirstSelector{}).Pick(context.Background(), "codex", reasonTestOtherModel, cliproxyexecutor.Options{}, []*coreauth.Auth{auth})
			if selectable := errPick == nil && selected != nil; selectable != tc.wantSelectable {
				t.Fatalf("selector picks it for %s = %v (err %v), want %v", reasonTestOtherModel, selectable, errPick, tc.wantSelectable)
			}
		})
	}
}

func listUnavailableReasonEntry(t *testing.T, h *Handler, name string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials?name="+url.QueryEscape(name), nil)
	h.ListAuthFiles(ctx)
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
