package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func newConfigV8KeysRouter(t *testing.T, raw string) (*Handler, *gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	router.DELETE("/v8/management/config/*path", h.ConfigV8)
	return h, router, path
}

func TestConfigV8RefusesRemovingTheLastClientKey(t *testing.T) {
	const raw = "config-version: 8\naccess:\n  api-keys: [fixture-key-laptop]\n"
	for _, tc := range []struct {
		name   string
		method string
		body   string
	}{
		{name: "replace with empty list", method: http.MethodPut, body: `[]`},
		{name: "replace with blank key", method: http.MethodPut, body: `["  "]`},
		{name: "delete the list", method: http.MethodDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, router, path := newConfigV8KeysRouter(t, raw)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, "/v8/management/config/access/api-keys", strings.NewReader(tc.body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"last_client_key"`) {
				t.Fatalf("status=%d body=%s, want 400 last_client_key", w.Code, w.Body.String())
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != raw {
				t.Fatalf("config file changed to %q (err %v), want it untouched", data, err)
			}
			if !reflect.DeepEqual(h.cfg.APIKeys, []string{"fixture-key-laptop"}) {
				t.Fatalf("handler keys = %q, want the previous key", h.cfg.APIKeys)
			}
		})
	}
}

func TestConfigV8AllowsClientKeyChangesThatKeepAKey(t *testing.T) {
	h, router, _ := newConfigV8KeysRouter(t, "config-version: 8\naccess:\n  api-keys: [fixture-key-laptop]\n")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v8/management/config/access/api-keys", strings.NewReader(`["fixture-key-desktop"]`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(h.cfg.APIKeys, []string{"fixture-key-desktop"}) {
		t.Fatalf("handler keys = %q, want the replacement key", h.cfg.APIKeys)
	}
}

func TestConfigV8SavesWithoutClientKeysWhenNoneWereSet(t *testing.T) {
	_, router, _ := newConfigV8KeysRouter(t, "config-version: 8\nrouting:\n  strategy: round-robin\n")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v8/management/config/routing/strategy", strings.NewReader(`"fill-first"`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}
