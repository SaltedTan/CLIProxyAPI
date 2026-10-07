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
