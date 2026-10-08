package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/keyusage"
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
	if report.Fable.Available {
		t.Fatalf("fable = %+v, want unavailable without Claude accounts", report.Fable)
	}

	text := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/key/usage?format=text", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	server.engine.ServeHTTP(text, request)
	if !strings.HasPrefix(text.Header().Get("Content-Type"), "text/plain") || !strings.Contains(text.Body.String(), "Claude allowance: 100% left, 0.00 of 2.50 Pro units used") {
		t.Fatalf("text report = %q", text.Body.String())
	}

	line := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/key/usage?format=line", nil)
	request.Header.Set("Authorization", "Bearer test-key")
	server.engine.ServeHTTP(line, request)
	if got, want := line.Body.String(), "Claude 100% left · 7d window starts on next use │ Fable: n/a\n"; got != want {
		t.Fatalf("line report = %q, want %q", got, want)
	}
}
