package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestKeyUsageReportsTheCallersOwnKey(t *testing.T) {
	server := newTestServerWithConfig(t, &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys:     []string{"test-key", "other-key"},
			APIKeyNames: map[string]string{"test-key": "Laptop", "other-key": "Desktop"},
		},
	})
	clientusage.Default().SetLimits(map[string]float64{"test-key": 2.5, "other-key": 9})
	t.Cleanup(func() { clientusage.Default().SetLimits(nil) })

	unauthenticated := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/key/usage", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/key/usage", nil)
	request.Header.Set("X-Api-Key", "test-key")
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, leak := range []string{"test-key", "other-key", "Desktop", clientusage.KeyID("other-key")} {
		if strings.Contains(body, leak) {
			t.Fatalf("report %s contains %q", body, leak)
		}
	}
	var report keyusage.Report
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &report); errDecode != nil {
		t.Fatal(errDecode)
	}
	if report.Key.ID != clientusage.KeyID("test-key") || report.Key.Name != "Laptop" {
		t.Fatalf("key = %+v", report.Key)
	}
	if !report.Claude.Limited || *report.Claude.LimitProUnits != 2.5 || *report.Claude.RemainingProUnits != 2.5 || report.Claude.WindowResetsAt != nil {
		t.Fatalf("claude = %+v", report.Claude)
	}
	if report.FiveHour.Available || report.Fable.Available {
		t.Fatalf("five_hour = %+v, fable = %+v, want unavailable without Claude accounts", report.FiveHour, report.Fable)
	}
	if !strings.Contains(body, `"five_hour":{"available":false,"capacity_pro_units":0,"remaining_pro_units":0,"partial":false}`) {
		t.Fatalf("report %s has no five_hour object", body)
	}

	text := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/key/usage?format=text", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	server.engine.ServeHTTP(text, request)
	if !strings.HasPrefix(text.Header().Get("Content-Type"), "text/plain") || !strings.Contains(text.Body.String(), "Claude allowance: 100% left, 0.00 of 2.50 Pro units used") || !strings.Contains(text.Body.String(), "Claude 5-hour limit (shared): usage unavailable right now") {
		t.Fatalf("text report = %q", text.Body.String())
	}

	line := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/key/usage?format=line", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	server.engine.ServeHTTP(line, request)
	if got, want := line.Body.String(), "Claude 100% left · 7d window starts on next use │ 5h: n/a │ Fable: n/a\n"; got != want {
		t.Fatalf("line report = %q, want %q", got, want)
	}
}

// The key usage pool reads the usage cache the service shares with quota-aware routing,
// listing its accounts once for the 5-hour and Fable figures.
func TestKeyUsagePoolUsesTheInjectedClaudeUsage(t *testing.T) {
	listed := 0
	cache := keyusage.NewUsageCache(func() []*auth.Auth {
		listed++
		return nil
	}, nil)
	server := newTestServerWithConfig(t, &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}, WithClaudeUsage(cache))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/key/usage", nil)
	request.Header.Set("X-Api-Key", "test-key")
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || listed != 1 {
		t.Fatalf("status = %d, cache listed %d times, want one pool summary from the injected cache", recorder.Code, listed)
	}
}

func TestKeyUsageIsUnavailableUnderHome(t *testing.T) {
	server := newTestServerWithConfig(t, &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}})
	cfg := *server.getConfig()
	cfg.Home.Enabled = true
	server.cfg = &cfg

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/key/usage", nil)
	c.Set("userApiKey", "test-key")
	server.keyUsage(c)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body = %s, want 501", recorder.Code, recorder.Body.String())
	}
}
