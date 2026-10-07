package config

import (
	"strings"
	"testing"
)

func TestAPIKeyLimitsValidationMasksEveryKey(t *testing.T) {
	cases := map[string]string{
		"QZ":                 "***",
		"a":                  "***",
		"abcd":               "***",
		"abcdef":             "ab...ef",
		"fixture-client-key": "fixt...-key",
	}
	for key, masked := range cases {
		cfg := &Config{}
		cfg.APIKeyLimits = map[string]float64{key: -1}
		errNormalize := normalizeAPIKeyLimits(cfg)
		if errNormalize == nil {
			t.Fatalf("%q: a negative allowance must be rejected", key)
		}
		message := errNormalize.Error()
		if !strings.Contains(message, "entry "+masked+":") {
			t.Fatalf("%q: message = %q, want the key masked as %q", key, message, masked)
		}
		if len(key) > 2 && strings.Contains(message, key) {
			t.Fatalf("%q: error message echoes the key: %s", key, message)
		}
	}
}
