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

// TestDuplicateClientKeyEntriesBehindMergesAndAliasesAreReportedMasked covers
// client key maps reached through YAML merge keys and aliases. yaml.v3 decodes
// every merged or aliased mapping on its own and names a duplicated key in its
// error, so the masked check has to look through them first.
func TestDuplicateClientKeyEntriesBehindMergesAndAliasesAreReportedMasked(t *testing.T) {
	const key = "fixture-client-key-1"
	const masked = "fixt...ey-1"
	cases := map[string]string{
		"v8 limits under a merged mapping":     "config-version: 8\naccess:\n  <<: &settings\n    api-key-limits:\n      " + key + ": 1\n      " + key + ": 2\n",
		"v8 limits behind an alias":            "config-version: 8\nx-limits: &limits\n  " + key + ": 1\n  " + key + ": 2\naccess:\n  api-key-limits: *limits\n",
		"merge inside the v8 limits map":       "config-version: 8\naccess:\n  api-key-limits:\n    <<:\n      " + key + ": 1\n      " + key + ": 2\n",
		"merge sequence inside the names map":  "config-version: 8\naccess:\n  api-key-names:\n    <<: [{" + key + ": a, " + key + ": b}]\n",
		"legacy limits under a merged mapping": "<<: &settings\n  api-key-limits:\n    " + key + ": 1\n    " + key + ": 2\n",
		// Round 4: the generic decoder also decodes anchored copies that merge
		// precedence shadows, and mappings whose key is itself an alias.
		"anchored limits map shadowed by a direct one":             "config-version: 8\nx-shared: &shared\n  api-key-limits:\n    " + key + ": 1\n    " + key + ": 2\naccess:\n  <<: *shared\n  api-key-limits:\n    " + key + ": 3\n",
		"section named through an alias key":                       "config-version: 8\nserver: {host: &section access}\n*section:\n  api-key-limits:\n    " + key + ": 1\n    " + key + ": 2\n",
		"limits anchored elsewhere, used only in a shadowed merge": "config-version: 8\nx-limits: &limits\n  " + key + ": 1\n  " + key + ": 2\naccess:\n  <<:\n    api-key-limits: *limits\n  api-key-limits:\n    " + key + ": 3\n",
		"map name given through an alias key":                      "config-version: 8\nserver: {host: &name api-key-limits}\naccess:\n  *name:\n    " + key + ": 1\n    " + key + ": 2\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, errParse := ParseConfigBytes([]byte(raw))
			if errParse == nil {
				t.Fatal("ParseConfigBytes accepted a duplicated client key entry")
			}
			if strings.Contains(errParse.Error(), key) || !strings.Contains(errParse.Error(), masked) {
				t.Fatalf("ParseConfigBytes error = %q, want the key masked as %q", errParse, masked)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, errLoad := LoadConfig(path)
			if errLoad == nil {
				t.Fatal("LoadConfig accepted a duplicated client key entry")
			}
			if strings.Contains(errLoad.Error(), key) || !strings.Contains(errLoad.Error(), masked) {
				t.Fatalf("LoadConfig error = %q, want the key masked as %q", errLoad, masked)
			}
		})
	}
}

// TestClientKeyMapMergePrecedenceIsNotADuplicate keeps YAML merge semantics: an
// entry set directly overrides the same entry from a merged mapping.
func TestClientKeyMapMergePrecedenceIsNotADuplicate(t *testing.T) {
	const key = "fixture-client-key-1"
	raw := "config-version: 8\naccess:\n  api-key-limits:\n    <<:\n      " + key + ": 1\n    " + key + ": 2\n"
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if got := cfg.APIKeyLimits[key]; got != 2 {
		t.Fatalf("limit = %v, want the directly set 2 to win over the merged 1", got)
	}
}

// TestSaveKeepsANewZeroLimitEntry guards the config saver: a full-key 0 added at
// runtime lifts an id limit, so it must survive the save that otherwise drops
// zero values of new keys.
func TestSaveKeepsANewZeroLimitEntry(t *testing.T) {
	const key = "fixture-client-key-1"
	const id = "3f9a1c2b7d4e5f60"
	cases := map[string]string{
		"into an existing map": "config-version: 8\naccess:\n  api-keys: [" + key + "]\n  api-key-limits:\n    \"" + id + "\": 0.125\napi-keys:\n  codex: []\n",
		"as a new map":         "config-version: 8\naccess:\n  api-keys: [" + key + "]\napi-keys:\n  codex: []\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if cfg.APIKeyLimits == nil {
				cfg.APIKeyLimits = map[string]float64{}
			}
			cfg.APIKeyLimits[key] = 0
			if err = SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatalf("SaveConfigPreserveComments() error = %v", err)
			}
			saved, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			loaded, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatalf("LoadConfig() after save error = %v\n%s", errLoad, saved)
			}
			if value, ok := loaded.APIKeyLimits[key]; !ok || value != 0 {
				t.Fatalf("the new full-key 0 did not survive the save: limits = %v\n%s", loaded.APIKeyLimits, saved)
			}
			if strings.Contains(raw, id) && loaded.APIKeyLimits[id] != 0.125 {
				t.Fatalf("the id limit changed: limits = %v\n%s", loaded.APIKeyLimits, saved)
			}
		})
	}
}

// TestGenericDuplicateKeyErrorsAreMasked is the fallback for a duplicated key the
// structural check cannot attribute to a client key map: the YAML decoder's own
// error must not name it either.
func TestGenericDuplicateKeyErrorsAreMasked(t *testing.T) {
	const key = "fixture-client-key-1"
	raw := "config-version: 8\nx-unrelated:\n  " + key + ": 1\n  " + key + ": 2\n"
	_, errParse := ParseConfigBytes([]byte(raw))
	if errParse == nil {
		t.Fatal("ParseConfigBytes accepted a duplicated key")
	}
	if strings.Contains(errParse.Error(), key) || !strings.Contains(errParse.Error(), "fixt...ey-1") || !strings.Contains(errParse.Error(), "line 4") {
		t.Fatalf("ParseConfigBytes error = %q, want the key masked and the line kept", errParse)
	}
	if errValidate := ValidateV8Config([]byte(raw)); errValidate == nil || strings.Contains(errValidate.Error(), key) {
		t.Fatalf("ValidateV8Config error = %v, want a masked rejection", errValidate)
	}
}

// TestClientKeyDiagnosticsNeverNameAncestorKeys covers a malformed value under a
// client key that itself holds a client key map: the diagnostic names no
// ancestor, whether the map is reached under its own path or, first, through
// the anchor an alias later makes a limits map.
func TestClientKeyDiagnosticsNeverNameAncestorKeys(t *testing.T) {
	const key = "fixture-client-key-1"
	cases := map[string]string{
		"nested under the limits map":            "config-version: 8\naccess:\n  api-key-limits:\n    " + key + ":\n      api-key-limits:\n        x: 1\n        x: 2\n",
		"anchored under an unrelated path first": "config-version: 8\nx: &limits\n  " + key + ":\n    api-key-limits:\n      x: 1\n      x: 2\naccess:\n  api-key-limits: *limits\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte(raw))
			if err == nil {
				t.Fatal("ParseConfigBytes accepted a duplicated entry")
			}
			if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "api-key-limits") || !strings.Contains(err.Error(), "line ") {
				t.Fatalf("ParseConfigBytes error = %q, want the map name and line without the ancestor key", err)
			}
		})
	}
}

// TestNonScalarClientKeyEntriesAreRejectedMasked covers compound YAML keys, which
// the generic decoder rejects by printing the key.
func TestNonScalarClientKeyEntriesAreRejectedMasked(t *testing.T) {
	const key = "fixture-client-key-1"
	cases := map[string]string{
		"compound key in the limits map":   "config-version: 8\naccess:\n  api-key-limits:\n    ? [" + key + "]\n    : 1\n",
		"compound key in an unrelated map": "config-version: 8\nx-unrelated:\n  ? [" + key + "]\n  : 1\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, errParse := ParseConfigBytes([]byte(raw))
			if errParse == nil {
				t.Fatal("ParseConfigBytes accepted a compound key")
			}
			if strings.Contains(errParse.Error(), key) {
				t.Fatalf("ParseConfigBytes error echoes the key: %q", errParse)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, errLoad := LoadConfig(path)
			if errLoad == nil {
				t.Fatal("LoadConfig accepted a compound key")
			}
			if strings.Contains(errLoad.Error(), key) {
				t.Fatalf("LoadConfig error echoes the key: %q", errLoad)
			}
		})
	}
}

// TestAliasKeysAreComparedByTheirResolvedName keeps alias keys usable: an alias
// and a literal naming different keys are distinct entries, while an alias
// resolving to a literal already present is the same entry twice.
func TestAliasKeysAreComparedByTheirResolvedName(t *testing.T) {
	const key = "fixture-client-key-1"
	t.Run("distinct keys", func(t *testing.T) {
		raw := "config-version: 8\naccess:\n  api-keys: [&short_name " + key + ", short_name]\n  api-key-limits:\n    *short_name: 1\n    short_name: 2\n"
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil {
			t.Fatalf("ParseConfigBytes() error = %v", err)
		}
		if cfg.APIKeyLimits[key] != 1 || cfg.APIKeyLimits["short_name"] != 2 {
			t.Fatalf("limits = %v, want the alias and the literal as separate entries", cfg.APIKeyLimits)
		}
	})
	t.Run("same key twice", func(t *testing.T) {
		raw := "config-version: 8\naccess:\n  api-keys: [&short_name " + key + "]\n  api-key-limits:\n    *short_name: 1\n    " + key + ": 2\n"
		_, err := ParseConfigBytes([]byte(raw))
		if err == nil {
			t.Fatal("ParseConfigBytes accepted the same key through an alias and a literal")
		}
		if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "fixt...ey-1") {
			t.Fatalf("ParseConfigBytes error = %q, want the key masked", err)
		}
	})
}

// TestTaggedKeysAndUnknownAnchorsAreMasked covers the decoder and parser errors
// that print a key: an explicitly tagged scalar key whose tag does not fit, and
// an alias key naming an anchor that does not exist.
func TestTaggedKeysAndUnknownAnchorsAreMasked(t *testing.T) {
	const key = "fixture-client-key-1"
	cases := map[string]string{
		"tagged key in the limits map":     "config-version: 8\naccess:\n  api-key-limits:\n    !!int " + key + ": 1\n",
		"tagged key in an unrelated map":   "config-version: 8\nx-unrelated:\n  !!int " + key + ": 1\n",
		"tagged key with a backtick":       "config-version: 8\nx-unrelated:\n  !!int \"fixture`" + key + "\": 1\n",
		"unknown anchor in the limits map": "config-version: 8\naccess:\n  api-key-limits:\n    *" + key + ": 1\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, errParse := ParseConfigBytes([]byte(raw))
			if errParse == nil {
				t.Fatal("ParseConfigBytes accepted the document")
			}
			if strings.Contains(errParse.Error(), key) {
				t.Fatalf("ParseConfigBytes error echoes the key: %q", errParse)
			}
			if errValidate := ValidateV8Config([]byte(raw)); errValidate == nil || strings.Contains(errValidate.Error(), key) {
				t.Fatalf("ValidateV8Config error = %v, want a rejection without the key", errValidate)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, errLoad := LoadConfig(path)
			if errLoad == nil {
				t.Fatal("LoadConfig accepted the document")
			}
			if strings.Contains(errLoad.Error(), key) {
				t.Fatalf("LoadConfig error echoes the key: %q", errLoad)
			}
		})
	}
}

// TestNestedScalarSaveMasksDuplicateClientKeys covers the public nested-scalar
// saver, which decodes the whole file before updating one value.
func TestNestedScalarSaveMasksDuplicateClientKeys(t *testing.T) {
	const key = "fixture-client-key-1"
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver:\n  port: 19090\naccess:\n  api-key-limits:\n    " + key + ": 1\n    " + key + ": 2\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	err := SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"server", "port"}, "19091")
	if err == nil {
		t.Fatal("the nested scalar saver accepted a duplicated client key")
	}
	if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "fixt...ey-1") {
		t.Fatalf("error = %q, want the key masked", err)
	}
}
