//go:build unix

package secretfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// permissiveUmask clears the process umask for the test, so a file created
// without an explicit owner-only mode would be world-readable.
func permissiveUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previous) })
}

func assertMode(t *testing.T, path string) {
	t.Helper()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	if got := info.Mode().Perm(); got != Mode {
		t.Fatalf("%s mode = %o, want %o", filepath.Base(path), got, Mode)
	}
}

func TestWriteFileRestrictsNewAndExistingFiles(t *testing.T) {
	permissiveUmask(t)
	dir := t.TempDir()

	created := filepath.Join(dir, "new.json")
	if errWrite := WriteFile(created, []byte(`{"token":"a"}`)); errWrite != nil {
		t.Fatalf("WriteFile new: %v", errWrite)
	}
	assertMode(t, created)

	existing := filepath.Join(dir, "existing.json")
	if errWrite := os.WriteFile(existing, []byte(`{"token":"old-and-longer"}`), 0o664); errWrite != nil {
		t.Fatalf("seed existing file: %v", errWrite)
	}
	if errWrite := WriteFile(existing, []byte(`{"token":"b"}`)); errWrite != nil {
		t.Fatalf("WriteFile existing: %v", errWrite)
	}
	assertMode(t, existing)
	content, errRead := os.ReadFile(existing)
	if errRead != nil {
		t.Fatalf("read existing: %v", errRead)
	}
	if string(content) != `{"token":"b"}` {
		t.Fatalf("existing content = %q, want the new content only", content)
	}
}

func TestCreateRestrictsExistingFileBeforeWriting(t *testing.T) {
	permissiveUmask(t)
	path := filepath.Join(t.TempDir(), "token.json")
	if errWrite := os.WriteFile(path, []byte("old"), 0o666); errWrite != nil {
		t.Fatalf("seed file: %v", errWrite)
	}

	file, errCreate := Create(path)
	if errCreate != nil {
		t.Fatalf("Create: %v", errCreate)
	}
	assertMode(t, path)
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
}
