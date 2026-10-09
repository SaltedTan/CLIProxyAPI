//go:build unix

package test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/vertex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// withPermissiveUmask clears the umask so any secret file written without an
// owner-only mode shows up as group- and world-readable.
func withPermissiveUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previous) })
}

func requireOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", filepath.Base(path), got)
	}
}

// writeTwice writes a secret file into a fresh path, then loosens it to 0664
// (the mode the live service's umask used to produce) and writes it again.
func writeTwice(t *testing.T, path string, write func() error) {
	t.Helper()
	if errWrite := write(); errWrite != nil {
		t.Fatalf("first write: %v", errWrite)
	}
	requireOwnerOnly(t, path)
	if errChmod := os.Chmod(path, 0o664); errChmod != nil {
		t.Fatalf("loosen %s: %v", path, errChmod)
	}
	if errWrite := write(); errWrite != nil {
		t.Fatalf("overwrite: %v", errWrite)
	}
	requireOwnerOnly(t, path)
}

func TestOAuthTokenFilesAreOwnerOnly(t *testing.T) {
	withPermissiveUmask(t)
	dir := t.TempDir()

	cases := map[string]func(path string) error{
		"claude": func(path string) error {
			return (&claude.ClaudeTokenStorage{AccessToken: "secret"}).SaveTokenToFile(path)
		},
		"codex": func(path string) error {
			return (&codex.CodexTokenStorage{AccessToken: "secret"}).SaveTokenToFile(path)
		},
		"kimi": func(path string) error {
			return (&kimi.KimiTokenStorage{AccessToken: "secret"}).SaveTokenToFile(path)
		},
		"xai": func(path string) error {
			return (&xai.TokenStorage{AccessToken: "secret"}).SaveTokenToFile(path)
		},
		"vertex": func(path string) error {
			storage := &vertex.VertexCredentialStorage{ServiceAccount: map[string]any{"private_key": "secret"}}
			return storage.SaveTokenToFile(path)
		},
	}
	for name, save := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			writeTwice(t, path, func() error { return save(path) })
		})
	}
}

func TestFileTokenStoreWritesOwnerOnlyAuthFiles(t *testing.T) {
	withPermissiveUmask(t)
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	path := filepath.Join(dir, "metadata.json")

	refresh := 0
	writeTwice(t, path, func() error {
		refresh++
		auth := &cliproxyauth.Auth{
			ID:       "metadata.json",
			FileName: "metadata.json",
			Provider: "claude",
			Metadata: map[string]any{"type": "claude", "access_token": "secret", "refresh": refresh},
		}
		_, errSave := store.Save(context.Background(), auth)
		return errSave
	})
}

func TestConfigSavesAreOwnerOnly(t *testing.T) {
	withPermissiveUmask(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8318\napi-keys:\n  - secret-key\n"), 0o664); errWrite != nil {
		t.Fatalf("seed config: %v", errWrite)
	}

	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	if errSave := config.SaveConfigPreserveComments(path, cfg); errSave != nil {
		t.Fatalf("save config: %v", errSave)
	}
	requireOwnerOnly(t, path)

	if errChmod := os.Chmod(path, 0o664); errChmod != nil {
		t.Fatalf("loosen config: %v", errChmod)
	}
	if errSave := config.SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"remote-management", "secret-key"}, "hashed"); errSave != nil {
		t.Fatalf("update nested scalar: %v", errSave)
	}
	requireOwnerOnly(t, path)
}
