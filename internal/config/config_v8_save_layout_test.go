package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIsV8ConfigLayout(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		v8        bool
	}{
		{"legacy", "request-retry: 3\ncodex: {response-steering: true}\n", false},
		{"legacy common roots", "routing: {strategy: fill-first}\nplugins: {configs: {example: {enabled: true}}}\nquota-exceeded: {switch-project: true}\nclient: {codex: {enable-apply-patch: true}}\napi-keys: [client-key]\n", false},
		{"legacy optimize alias", "codex: {optimize-multi-agent-v2: true}\n", false},
		{"declared v8", "config-version: 8\nrequest-retry: 3\n", true},
		{"historical v8", "oauth: {providers: {codex: {response-steering: true}}}\n", true},
		{"empty v8", "server: {}\n", true},
		{"latest v8", "upstream: {xai: {inject-x-search: false}}\n", true},
		{"grouped credentials", "api-keys: {}\n", true},
		{"partial v8 retry", "request-retry: 3\nrouting: {retry: {max-retry-credentials: 2}}\n", true},
		{"empty v8 retry", "routing: {retry: {}}\n", true},
		{"historical client", "providers: {codex: {optimize-multi-agent-v2: true}}\n", true},
		{"latest client", "client: {codex: {optimize-multi-agent-v2: false}}\n", true},
		{"legacy client key names", "api-keys: [client-key]\napi-key-names: {client-key: Laptop}\n", false},
		{"v8 client key names", "access: {api-key-names: {client-key: Laptop}}\n", true},
		{"legacy client key limits", "api-keys: [client-key]\napi-key-limits: {client-key: 1.5}\n", false},
		{"v8 client key limits", "access: {api-key-limits: {client-key: 1.5}}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.raw), &doc); err != nil {
				t.Fatal(err)
			}
			if got := IsV8ConfigLayout(doc.Content[0]); got != tc.v8 {
				t.Fatalf("v8 layout = %v, want %v", got, tc.v8)
			}
		})
	}
}

func TestV0SaveUpgradesHistoricalV8Layout(t *testing.T) {
	for _, version := range []string{"", "config-version: 8\n"} {
		t.Run(version, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			raw := version + "# Preserve provider settings\noauth: {providers: {codex: {response-steering: true, header-defaults: {user-agent: oauth-agent}}, xai: {inject-x-search: true}}}\n"
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Port = 8318
			cfg.Home.Enabled = true
			if err = SaveConfigPreserveComments(file, cfg, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(data); err != nil {
				t.Fatalf("v0 saved legacy/mixed fields: %v", err)
			}
			var doc yaml.Node
			if err = yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			root := doc.Content[0]
			if yamlPath(root, "upstream.codex.response-steering") == nil || yamlPath(root, "upstream.xai.inject-x-search") == nil || yamlPath(root, "server.port").Value != "8318" {
				t.Fatal("v0 save did not restore canonical provider fields and updated port")
			}
			if !cfg.ForAPIKey().Codex.ResponseSteering || !cfg.ForAPIKey().XAI.InjectXSearch || cfg.ForAPIKey().CodexHeaderDefaults.UserAgent != "" || !cfg.Home.Enabled {
				t.Fatal("v0 save changed shared/OAuth scope or runtime-only state")
			}
			if !strings.Contains(string(data), "# Preserve provider settings") {
				t.Fatal("v0 save lost provider comments")
			}
		})
	}
}

func TestAPIKeyNamesLoadAndValidate(t *testing.T) {
	want := map[string]string{"sk-laptop": "MacBook", "sk-desktop": "Desktop"}
	v8 := "config-version: 8\naccess:\n  api-keys: [sk-laptop, sk-desktop]\n  api-key-names:\n    sk-laptop: MacBook\n    sk-desktop: Desktop\napi-keys:\n  codex: []\n"
	for _, tc := range []struct {
		name, raw string
		// rejection is the expected ValidateV8Config error substring; empty means valid.
		rejection string
	}{
		{"v8", v8, ""},
		{"legacy", "api-keys: [sk-laptop, sk-desktop]\napi-key-names:\n  sk-laptop: MacBook\n  sk-desktop: Desktop\n", "legacy field"},
		{"v8 wins over legacy", "api-key-names: {sk-laptop: Stale}\naccess:\n  api-keys: [sk-laptop, sk-desktop]\n  api-key-names: {sk-laptop: MacBook, sk-desktop: Desktop}\n", "use access.api-key-names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.APIKeyNames, want) || len(cfg.APIKeys) != 2 {
				t.Fatalf("APIKeyNames = %#v, APIKeys = %#v", cfg.APIKeyNames, cfg.APIKeys)
			}
			errValidate := ValidateV8Config([]byte(tc.raw))
			if tc.rejection == "" && errValidate != nil {
				t.Fatalf("ValidateV8Config() error = %v", errValidate)
			}
			if tc.rejection != "" && (errValidate == nil || !strings.Contains(errValidate.Error(), tc.rejection)) {
				t.Fatalf("ValidateV8Config() error = %v, want %q", errValidate, tc.rejection)
			}
		})
	}
}

func TestAPIKeyNamesSaveKeepsAccessLayout(t *testing.T) {
	raw := "config-version: 8\naccess:\n  api-keys: [sk-laptop, sk-desktop]\n  # Device names\n  api-key-names:\n    sk-laptop: MacBook\n    sk-desktop: Desktop\napi-keys:\n  codex: []\n"
	for _, tc := range []struct {
		name    string
		migrate []bool
	}{
		{"v8 save", []bool{true}},
		{"v0 save", []bool{false}},
		{"default save", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			cfg.APIKeys = append(cfg.APIKeys, "sk-phone")
			cfg.APIKeyNames["sk-phone"] = "Phone"
			delete(cfg.APIKeyNames, "sk-desktop")
			if err = SaveConfigPreserveComments(file, cfg, tc.migrate...); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(data); err != nil {
				t.Fatalf("saved config is not valid v8: %v\n%s", err, data)
			}
			var doc yaml.Node
			if err = yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			root := doc.Content[0]
			if yamlPath(root, "api-key-names") != nil {
				t.Fatalf("api-key-names moved to the root:\n%s", data)
			}
			names := yamlPath(root, "access.api-key-names")
			if names == nil || yamlPath(names, "sk-laptop").Value != "MacBook" || yamlPath(names, "sk-phone").Value != "Phone" || yamlPath(names, "sk-desktop") != nil {
				t.Fatalf("access.api-key-names not saved correctly:\n%s", data)
			}
			if !strings.Contains(string(data), "# Device names") {
				t.Fatalf("save lost api-key-names comment:\n%s", data)
			}
			reloaded, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.APIKeyNames, cfg.APIKeyNames) {
				t.Fatalf("reloaded APIKeyNames = %#v, want %#v", reloaded.APIKeyNames, cfg.APIKeyNames)
			}

			cfg.APIKeyNames = nil
			if err = SaveConfigPreserveComments(file, cfg, tc.migrate...); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			doc = yaml.Node{}
			if err = yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if yamlPath(doc.Content[0], "access.api-key-names") != nil || yamlPath(doc.Content[0], "api-key-names") != nil {
				t.Fatalf("cleared api-key-names persisted:\n%s", data)
			}
			if keys := yamlPath(doc.Content[0], "access.api-keys"); keys == nil || len(keys.Content) != 3 {
				t.Fatalf("access.api-keys changed:\n%s", data)
			}
		})
	}
}

func TestAPIKeyNamesAddedToV8FileWithoutNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("config-version: 8\naccess:\n  api-keys: [sk-laptop]\napi-keys:\n  codex: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKeyNames = map[string]string{"sk-laptop": "MacBook"}
	if err = SaveConfigPreserveComments(file, cfg, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", err, data)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if names := yamlPath(doc.Content[0], "access.api-key-names"); names == nil || yamlPath(names, "sk-laptop").Value != "MacBook" {
		t.Fatalf("new api-key-names not saved under access:\n%s", data)
	}
}

func TestAPIKeyNamesLegacySaveAndMigration(t *testing.T) {
	raw := "api-keys:\n  - sk-laptop\napi-key-names:\n  sk-laptop: MacBook\n"
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(file, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if names := yamlPath(doc.Content[0], "api-key-names"); names == nil || yamlPath(names, "sk-laptop").Value != "MacBook" || yamlPath(doc.Content[0], "access") != nil {
		t.Fatalf("legacy save changed api-key-names layout:\n%s", data)
	}
	if err = SaveConfigPreserveComments(file, cfg, true); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("migrated config is not valid v8: %v\n%s", err, data)
	}
	doc = yaml.Node{}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if names := yamlPath(doc.Content[0], "access.api-key-names"); names == nil || yamlPath(names, "sk-laptop").Value != "MacBook" {
		t.Fatalf("migration did not move api-key-names under access:\n%s", data)
	}
}

func TestAPIKeyLimitsLoadAndValidate(t *testing.T) {
	want := map[string]float64{"fixture-key-laptop": 1.5, "3f9a1c2b7d4e5f60": 0.25}
	v8 := "config-version: 8\naccess:\n  api-keys: [fixture-key-laptop]\n  api-key-limits:\n    fixture-key-laptop: 1.5\n    \"3f9a1c2b7d4e5f60\": 0.25\napi-keys:\n  codex: []\n"
	for _, tc := range []struct {
		name, raw string
		// rejection is the expected ValidateV8Config error substring; empty means valid.
		rejection string
	}{
		{"v8", v8, ""},
		{"legacy", "api-keys: [fixture-key-laptop]\napi-key-limits:\n  fixture-key-laptop: 1.5\n  \"3f9a1c2b7d4e5f60\": 0.25\n", "legacy field"},
		{"v8 wins over legacy", "api-key-limits: {fixture-key-laptop: 9}\naccess:\n  api-keys: [fixture-key-laptop]\n  api-key-limits: {fixture-key-laptop: 1.5, \"3f9a1c2b7d4e5f60\": 0.25}\n", "use access.api-key-limits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.APIKeyLimits, want) || len(cfg.APIKeys) != 1 {
				t.Fatalf("APIKeyLimits = %#v, APIKeys = %#v", cfg.APIKeyLimits, cfg.APIKeys)
			}
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err = os.WriteFile(file, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.APIKeyLimits, want) {
				t.Fatalf("loaded APIKeyLimits = %#v, want %#v", loaded.APIKeyLimits, want)
			}
			errValidate := ValidateV8Config([]byte(tc.raw))
			if tc.rejection == "" && errValidate != nil {
				t.Fatalf("ValidateV8Config() error = %v", errValidate)
			}
			if tc.rejection != "" && (errValidate == nil || !strings.Contains(errValidate.Error(), tc.rejection)) {
				t.Fatalf("ValidateV8Config() error = %v, want %q", errValidate, tc.rejection)
			}
		})
	}
}

func TestAPIKeyLimitsNormalizeKeys(t *testing.T) {
	raw := "config-version: 8\naccess:\n  api-key-limits:\n    \"  fixture-key-laptop \": 1.5\n    \"   \": 2\n    fixture-key-zero: 0\napi-keys:\n  codex: []\n"
	want := map[string]float64{"fixture-key-laptop": 1.5, "fixture-key-zero": 0}
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.APIKeyLimits, want) {
		t.Fatalf("ParseConfigBytes APIKeyLimits = %#v, want %#v", cfg.APIKeyLimits, want)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.APIKeyLimits, want) {
		t.Fatalf("LoadConfig APIKeyLimits = %#v, want %#v", loaded.APIKeyLimits, want)
	}
	if err = ValidateV8Config([]byte(raw)); err != nil {
		t.Fatalf("ValidateV8Config() error = %v", err)
	}
	// An empty map after normalization becomes nil so the field is omitted on save.
	empty, err := ParseConfigBytes([]byte("access:\n  api-key-limits:\n    \"  \": 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.APIKeyLimits != nil {
		t.Fatalf("APIKeyLimits = %#v, want nil", empty.APIKeyLimits)
	}
	// The already trimmed spelling wins over a padded duplicate.
	dup := normalizeAPIKeyLimitKeys(map[string]float64{" fixture-key-dup": 1, "fixture-key-dup": 2, "fixture-key-dup ": 3})
	if !reflect.DeepEqual(dup, map[string]float64{"fixture-key-dup": 2}) {
		t.Fatalf("normalizeAPIKeyLimitKeys() = %#v", dup)
	}
}

func TestAPIKeyLimitsRejectInvalidValues(t *testing.T) {
	const key = "fixture-key-laptop-secret"
	for _, tc := range []struct {
		name, value string
	}{
		{"negative", "-1"},
		{"negative fraction", "-0.5"},
		{"nan", ".nan"},
		{"positive infinity", ".inf"},
		{"negative infinity", "-.inf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, layout := range []struct {
				name, raw string
			}{
				{"v8", "config-version: 8\naccess:\n  api-key-limits:\n    " + key + ": " + tc.value + "\n"},
				{"legacy", "api-key-limits:\n  " + key + ": " + tc.value + "\n"},
			} {
				t.Run(layout.name, func(t *testing.T) {
					file := filepath.Join(t.TempDir(), "config.yaml")
					if err := os.WriteFile(file, []byte(layout.raw), 0600); err != nil {
						t.Fatal(err)
					}
					_, errLoad := LoadConfig(file)
					if errLoad == nil || !strings.Contains(errLoad.Error(), "api-key-limits") {
						t.Fatalf("LoadConfig() error = %v, want api-key-limits rejection", errLoad)
					}
					if strings.Contains(errLoad.Error(), key) {
						t.Fatalf("LoadConfig() error leaks the raw key: %v", errLoad)
					}
					if _, errOptional := LoadConfigOptional(file, true); errOptional == nil {
						t.Fatal("LoadConfigOptional() accepted an invalid allowance")
					}
					if layout.name != "v8" {
						return
					}
					errValidate := ValidateV8Config([]byte(layout.raw))
					if errValidate == nil || !strings.Contains(errValidate.Error(), "api-key-limits") {
						t.Fatalf("ValidateV8Config() error = %v, want api-key-limits rejection", errValidate)
					}
					if strings.Contains(errValidate.Error(), key) {
						t.Fatalf("ValidateV8Config() error leaks the raw key: %v", errValidate)
					}
					// ParseConfigBytes keeps the value: the management handler reports
					// invalid allowances through ValidateV8Config as 400 instead of 422.
					if _, errParse := ParseConfigBytes([]byte(layout.raw)); errParse != nil {
						t.Fatalf("ParseConfigBytes() error = %v", errParse)
					}
				})
			}
		})
	}
	if err := ValidateV8Config([]byte("config-version: 8\naccess:\n  api-key-limits:\n    fixture-key-laptop: 0\n    fixture-key-desktop: 10\n")); err != nil {
		t.Fatalf("ValidateV8Config() rejected valid allowances: %v", err)
	}
}

func TestAPIKeyLimitsSaveKeepsAccessLayout(t *testing.T) {
	raw := "config-version: 8\naccess:\n  api-keys: [fixture-key-laptop, fixture-key-desktop]\n  # Weekly allowances\n  api-key-limits:\n    fixture-key-laptop: 1.5\n    fixture-key-desktop: 2\napi-keys:\n  codex: []\n"
	for _, tc := range []struct {
		name    string
		migrate []bool
	}{
		{"v8 save", []bool{true}},
		{"v0 save", []bool{false}},
		{"default save", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			cfg.APIKeyLimits["fixture-key-phone"] = 0.5
			delete(cfg.APIKeyLimits, "fixture-key-desktop")
			if err = SaveConfigPreserveComments(file, cfg, tc.migrate...); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(data); err != nil {
				t.Fatalf("saved config is not valid v8: %v\n%s", err, data)
			}
			var doc yaml.Node
			if err = yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			root := doc.Content[0]
			if yamlPath(root, "api-key-limits") != nil {
				t.Fatalf("api-key-limits moved to the root:\n%s", data)
			}
			limits := yamlPath(root, "access.api-key-limits")
			if limits == nil || yamlPath(limits, "fixture-key-laptop").Value != "1.5" || yamlPath(limits, "fixture-key-phone").Value != "0.5" || yamlPath(limits, "fixture-key-desktop") != nil {
				t.Fatalf("access.api-key-limits not saved correctly:\n%s", data)
			}
			if !strings.Contains(string(data), "# Weekly allowances") {
				t.Fatalf("save lost api-key-limits comment:\n%s", data)
			}
			reloaded, err := LoadConfig(file)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.APIKeyLimits, cfg.APIKeyLimits) {
				t.Fatalf("reloaded APIKeyLimits = %#v, want %#v", reloaded.APIKeyLimits, cfg.APIKeyLimits)
			}

			cfg.APIKeyLimits = nil
			if err = SaveConfigPreserveComments(file, cfg, tc.migrate...); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			doc = yaml.Node{}
			if err = yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if yamlPath(doc.Content[0], "access.api-key-limits") != nil || yamlPath(doc.Content[0], "api-key-limits") != nil {
				t.Fatalf("cleared api-key-limits persisted:\n%s", data)
			}
			if keys := yamlPath(doc.Content[0], "access.api-keys"); keys == nil || len(keys.Content) != 2 {
				t.Fatalf("access.api-keys changed:\n%s", data)
			}
		})
	}
}

func TestAPIKeyLimitsAddedToV8FileWithoutLimits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("config-version: 8\naccess:\n  api-keys: [fixture-key-laptop]\napi-keys:\n  codex: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKeyLimits = map[string]float64{"fixture-key-laptop": 1.5}
	if err = SaveConfigPreserveComments(file, cfg, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", err, data)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if limits := yamlPath(doc.Content[0], "access.api-key-limits"); limits == nil || yamlPath(limits, "fixture-key-laptop").Value != "1.5" {
		t.Fatalf("new api-key-limits not saved under access:\n%s", data)
	}
}

func TestAPIKeyLimitsLegacySaveAndMigration(t *testing.T) {
	raw := "api-keys:\n  - fixture-key-laptop\napi-key-limits:\n  fixture-key-laptop: 1.5\n"
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(file, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if limits := yamlPath(doc.Content[0], "api-key-limits"); limits == nil || yamlPath(limits, "fixture-key-laptop").Value != "1.5" || yamlPath(doc.Content[0], "access") != nil {
		t.Fatalf("legacy save changed api-key-limits layout:\n%s", data)
	}
	if err = SaveConfigPreserveComments(file, cfg, true); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("migrated config is not valid v8: %v\n%s", err, data)
	}
	doc = yaml.Node{}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if limits := yamlPath(doc.Content[0], "access.api-key-limits"); limits == nil || yamlPath(limits, "fixture-key-laptop").Value != "1.5" {
		t.Fatalf("migration did not move api-key-limits under access:\n%s", data)
	}
}
