package cliproxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type stubAccessProvider struct{}

func (stubAccessProvider) Identifier() string { return "stub" }

func (stubAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, sdkaccess.NewNotHandledError()
}

func clientAuthWarnings(hook *logtest.Hook) []string {
	var out []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.HasPrefix(entry.Message, "no client API keys are configured") {
			out = append(out, entry.Message)
		}
	}
	return out
}

func TestWarnIfClientAuthOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		providers []sdkaccess.Provider
		wantWhere string
	}{
		{name: "all interfaces", host: "", wantWhere: "all interfaces, port 8317"},
		{name: "lan address", host: "192.168.1.20", wantWhere: "192.168.1.20:8317"},
		{name: "ipv6 any", host: "::", wantWhere: "[::]:8317"},
		{name: "loopback ipv4", host: "127.0.0.1"},
		{name: "loopback name", host: "localhost"},
		{name: "loopback ipv6", host: "::1"},
		{name: "provider registered", host: "", providers: []sdkaccess.Provider{stubAccessProvider{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			defer hook.Reset()

			manager := sdkaccess.NewManager()
			manager.SetProviders(tc.providers)
			service := &Service{cfg: &internalconfig.Config{}, accessManager: manager}
			service.cfg.Host = tc.host
			service.cfg.Port = 8317
			service.warnIfClientAuthOff()

			warnings := clientAuthWarnings(hook)
			if tc.wantWhere == "" {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %q, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "on "+tc.wantWhere+" accept") {
				t.Fatalf("warnings = %q, want one naming %q", warnings, tc.wantWhere)
			}
		})
	}
}
