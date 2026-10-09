//go:build unix

package management

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// seedLooseSecret writes path with the group- and world-readable mode older
// releases left behind, under a cleared umask so nothing hides a loose write.
func seedLooseSecret(t *testing.T, path string, data string) {
	t.Helper()
	previous := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previous) })
	if errWrite := os.WriteFile(path, []byte(data), 0o664); errWrite != nil {
		t.Fatalf("seed %s: %v", filepath.Base(path), errWrite)
	}
}

func requireOwnerOnlyFile(t *testing.T, path string) {
	t.Helper()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", filepath.Base(path), got)
	}
}

func TestWriteConfigTightensExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seedLooseSecret(t, path, "port: 8318\napi-keys:\n  - old-key-with-a-longer-value\n")

	data := []byte("port: 8318\napi-keys:\n  - new-key\n")
	if errWrite := WriteConfig(path, data); errWrite != nil {
		t.Fatalf("WriteConfig: %v", errWrite)
	}
	requireOwnerOnlyFile(t, path)
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if string(got) != string(data) {
		t.Fatalf("config = %q, want %q", got, data)
	}
}

func TestUploadedCredentialReplacingLooseFileIsOwnerOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	name := "codex-upload@example.com-plus.json"
	path := filepath.Join(authDir, name)
	seedLooseSecret(t, path, `{"type":"codex","email":"upload@example.com"}`)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	if errWrite := h.writeAuthFile(context.Background(), name, []byte(`{"type":"codex","email":"upload@example.com","priority":5}`)); errWrite != nil {
		t.Fatalf("writeAuthFile: %v", errWrite)
	}
	requireOwnerOnlyFile(t, path)
}

func TestSourceCredentialStatusPatchIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-source.json")
	seedLooseSecret(t, path, `{"type":"codex","disabled":false}`)

	if errSet := setSourceAuthFileDisabled(path, true); errSet != nil {
		t.Fatalf("setSourceAuthFileDisabled: %v", errSet)
	}
	requireOwnerOnlyFile(t, path)
}
