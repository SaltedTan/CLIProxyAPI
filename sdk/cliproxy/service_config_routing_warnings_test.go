package cliproxy

import (
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func routingWarnings(hook *logtest.Hook) []string {
	var out []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.HasPrefix(entry.Message, "routing.") {
			out = append(out, entry.Message)
		}
	}
	return out
}

func TestLogRoutingConfigWarningsNamesUnknownSettings(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()

	logRoutingConfigWarnings(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:           "quota-awrae",
		SessionAffinityTTL: "90",
	}})

	warnings := routingWarnings(hook)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want one for the strategy and one for the TTL", warnings)
	}
	if !strings.Contains(warnings[0], `"quota-awrae"`) || !strings.Contains(warnings[0], "round-robin instead") {
		t.Fatalf("strategy warning = %q, want it to name the value and the fallback", warnings[0])
	}
	if !strings.Contains(warnings[1], `"90"`) || !strings.Contains(warnings[1], "1h instead") {
		t.Fatalf("TTL warning = %q, want it to name the value and the fallback", warnings[1])
	}
}

func TestLogRoutingConfigWarningsQuietForValidSettings(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()

	for _, routing := range []internalconfig.RoutingConfig{
		{},
		{Strategy: "round-robin"},
		{Strategy: " RR "},
		{Strategy: "Quota-Aware", SessionAffinity: true, SessionAffinityTTL: "30m"},
		{Strategy: "fill-first", SessionAffinityTTL: "500ms"},
	} {
		logRoutingConfigWarnings(&internalconfig.Config{Routing: routing})
	}
	logRoutingConfigWarnings(nil)

	if warnings := routingWarnings(hook); len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
}

func TestNormalizedRoutingRuntimeStateFallsBackForUnknownSettings(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:           "quota-awrae",
		SessionAffinityTTL: "-5m",
	}})
	if state.strategy != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", state.strategy)
	}
	if state.sessionAffinityTTL != time.Hour {
		t.Fatalf("session affinity TTL = %v, want the 1h default", state.sessionAffinityTTL)
	}

	state = normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{SessionAffinityTTL: "500ms"}})
	if state.sessionAffinityTTL != time.Second {
		t.Fatalf("session affinity TTL = %v, want sub-second values raised to 1s", state.sessionAffinityTTL)
	}
}
