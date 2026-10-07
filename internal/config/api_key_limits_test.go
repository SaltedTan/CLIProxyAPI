package config

import (
	"os"
	"path/filepath"
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

func TestDuplicateClientKeyEntriesAreReportedMasked(t *testing.T) {
	const key = "fixture-client-key-1"
	documents := map[string]string{
		"v8 limits":     "access:\n  api-key-limits:\n    " + key + ": 1\n    " + key + ": 2\n",
		"legacy limits": "api-key-limits:\n  " + key + ": 1\n  " + key + ": 2\n",
		"v8 names":      "access:\n  api-key-names:\n    " + key + ": a\n    " + key + ": b\n",
	}
	for label, data := range documents {
		_, errParse := ParseConfigBytes([]byte(data))
		if errParse == nil {
			t.Fatalf("%s: a duplicated entry must be rejected", label)
		}
		if strings.Contains(errParse.Error(), key) {
			t.Fatalf("%s: error echoes the raw key: %v", label, errParse)
		}
		if !strings.Contains(errParse.Error(), "fixt...ey-1") {
			t.Fatalf("%s: error does not name the masked entry: %v", label, errParse)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if errWrite := os.WriteFile(path, []byte(data), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		_, errLoad := LoadConfig(path)
		if errLoad == nil || strings.Contains(errLoad.Error(), key) {
			t.Fatalf("%s: load error = %v, want a masked rejection", label, errLoad)
		}
	}
}
