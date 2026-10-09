package watcher

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// reloadWithClientKeys runs one config reload from currentKeys to the file body and
// reports whether it applied, how often the callback ran and the keys left in force.
func reloadWithClientKeys(t *testing.T, currentKeys []string, body string) (bool, int, []string) {
	t.Helper()
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"+body), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	callbacks := 0
	w := &Watcher{
		configPath:     configPath,
		authDir:        authDir,
		lastAuthHashes: make(map[string]string),
		reloadCallback: func(*config.Config) { callbacks++ },
	}
	current := &config.Config{AuthDir: authDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()}
	current.APIKeys = currentKeys
	w.SetConfig(current)

	applied := w.reloadConfig()
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	return applied, callbacks, w.config.APIKeys
}

func TestReloadConfigRefusesToRemoveEveryClientKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "keys deleted", body: "api-keys: []\n"},
		{name: "section removed", body: "debug: false\n"},
		{name: "misspelled section", body: "api-keyz:\n  - sk-client-1\n"},
		{name: "blank keys only", body: "api-keys:\n  - \"  \"\n"},
		{name: "v8 layout without keys", body: "config-version: 8\naccess:\n  api-keys: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied, callbacks, keys := reloadWithClientKeys(t, []string{"sk-client-1"}, tc.body)
			if applied {
				t.Fatal("reloadConfig applied a config without client keys")
			}
			if callbacks != 0 {
				t.Fatalf("reload callback ran %d times, want 0", callbacks)
			}
			if !reflect.DeepEqual(keys, []string{"sk-client-1"}) {
				t.Fatalf("client keys in force = %q, want the previous key", keys)
			}
		})
	}
}

// Management handlers share the watcher's config and edit it in place before they
// save, so the guard must not read the keys in force from that pointer.
func TestReloadConfigRefusesKeyRemovalAfterSharedConfigEdit(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+tmpDir+"\napi-keys: []\n"), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	callbacks := 0
	w := &Watcher{
		configPath:     configPath,
		authDir:        tmpDir,
		lastAuthHashes: make(map[string]string),
		reloadCallback: func(*config.Config) { callbacks++ },
	}
	shared := &config.Config{AuthDir: tmpDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()}
	shared.APIKeys = []string{"sk-client-1"}
	w.SetConfig(shared)

	shared.APIKeys = nil // a handler deletes the last key, then saves the file
	if w.reloadConfig() {
		t.Fatal("reloadConfig applied a config without client keys after an in-place edit")
	}
	if callbacks != 0 {
		t.Fatalf("reload callback ran %d times, want 0", callbacks)
	}
}

func TestReloadConfigAppliesClientKeyChangesThatKeepAKey(t *testing.T) {
	applied, callbacks, keys := reloadWithClientKeys(t, []string{"sk-client-1"}, "api-keys:\n  - sk-client-2\n")
	if !applied || callbacks != 1 {
		t.Fatalf("applied = %v, callbacks = %d; want a replaced key to reload", applied, callbacks)
	}
	if !reflect.DeepEqual(keys, []string{"sk-client-2"}) {
		t.Fatalf("client keys in force = %q, want the new key", keys)
	}
}

func TestReloadConfigWithoutClientKeysBeforeOrAfter(t *testing.T) {
	applied, callbacks, keys := reloadWithClientKeys(t, nil, "debug: true\n")
	if !applied || callbacks != 1 {
		t.Fatalf("applied = %v, callbacks = %d; want a reload that never had keys to apply", applied, callbacks)
	}
	if len(keys) != 0 {
		t.Fatalf("client keys in force = %q, want none", keys)
	}
}
