//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestGitTokenStoreCheckoutIsOwnerOnly(t *testing.T) {
	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })

	root := t.TempDir()
	remote := setupGitRemoteRepository(t, root, "main", testBranchSpec{name: "main", contents: "seed"})
	seed, errOpen := git.PlainOpen(filepath.Join(root, "seed"))
	if errOpen != nil {
		t.Fatalf("open seed repo: %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := seed.Close(); errClose != nil {
			t.Errorf("close seed repo: %v", errClose)
		}
	})
	seedWorktree, errWorktree := seed.Worktree()
	if errWorktree != nil {
		t.Fatalf("open seed worktree: %v", errWorktree)
	}
	secrets := []string{filepath.Join("auths", "credential.json"), filepath.Join("config", "config.yaml")}
	pushSecrets := func(version string) {
		t.Helper()
		for _, rel := range secrets {
			full := filepath.Join(root, "seed", rel)
			if errMkdir := os.MkdirAll(filepath.Dir(full), 0o755); errMkdir != nil {
				t.Fatalf("create %s parent: %v", rel, errMkdir)
			}
			if errWrite := os.WriteFile(full, []byte("test-secret-"+version), 0o600); errWrite != nil {
				t.Fatalf("write %s: %v", rel, errWrite)
			}
			if _, errAdd := seedWorktree.Add(filepath.ToSlash(rel)); errAdd != nil {
				t.Fatalf("add %s: %v", rel, errAdd)
			}
		}
		signature := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(1700000000, 0)}
		if _, errCommit := seedWorktree.Commit(version, &git.CommitOptions{Author: signature}); errCommit != nil {
			t.Fatalf("commit %s: %v", version, errCommit)
		}
		if errPush := seed.Push(&git.PushOptions{RemoteName: "origin"}); errPush != nil {
			t.Fatalf("push %s: %v", version, errPush)
		}
	}
	requireMode := func(stage, path string, want os.FileMode) {
		t.Helper()
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("%s: stat %s: %v", stage, path, errStat)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: %s mode = %o, want %o", stage, path, got, want)
		}
	}

	pushSecrets("initial")
	// The checkout directory already exists and is traversable, as a
	// GITSTORE_LOCAL_PATH directory created by hand would be.
	workspace := filepath.Join(root, "workspace")
	if errMkdir := os.Mkdir(workspace, 0o755); errMkdir != nil {
		t.Fatalf("create workspace: %v", errMkdir)
	}
	store := NewGitTokenStore(remote, "", "", "main")
	store.SetBaseDir(filepath.Join(workspace, "auths"))

	for _, stage := range []string{"clone", "pull"} {
		if stage == "pull" {
			pushSecrets("updated")
		}
		if errEnsure := store.EnsureRepository(); errEnsure != nil {
			t.Fatalf("%s: EnsureRepository: %v", stage, errEnsure)
		}
		requireMode(stage, workspace, 0o700)
		requireMode(stage, filepath.Join(workspace, ".git"), 0o700)
		for _, rel := range secrets {
			full := filepath.Join(workspace, rel)
			requireMode(stage, full, 0o600)
			data, errRead := os.ReadFile(full)
			if errRead != nil {
				t.Fatalf("%s: read %s: %v", stage, rel, errRead)
			}
			want := "test-secret-initial"
			if stage == "pull" {
				want = "test-secret-updated"
			}
			if string(data) != want {
				t.Fatalf("%s: %s = %q, want %q", stage, rel, data, want)
			}
		}
	}
}

func TestRestrictGitRepositoryOnlyTightensStoreSecrets(t *testing.T) {
	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })
	root := t.TempDir()
	requireMode := func(path string, want os.FileMode) {
		t.Helper()
		info, errStat := os.Lstat(path)
		if errStat != nil {
			t.Fatalf("stat %s: %v", path, errStat)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	mkdir := func(path string) {
		t.Helper()
		if errMkdir := os.MkdirAll(path, 0o755); errMkdir != nil {
			t.Fatalf("mkdir %s: %v", path, errMkdir)
		}
	}
	writeFile := func(path string, mode os.FileMode) {
		t.Helper()
		if errWrite := os.WriteFile(path, []byte("data"), mode); errWrite != nil {
			t.Fatalf("write %s: %v", path, errWrite)
		}
	}

	// A populated directory that is not a checkout, such as a home directory
	// a misconfigured base dir points at, is not touched.
	other := filepath.Join(root, "home")
	mkdir(other)
	writeFile(filepath.Join(other, "notes.txt"), 0o644)
	restrictGitRepository(other, filepath.Join(other, "auths"), filepath.Join(other, "config"))
	requireMode(other, 0o755)
	requireMode(filepath.Join(other, "notes.txt"), 0o644)

	// An empty directory about to receive the clone is tightened, also when it
	// is reached through a symlink.
	empty := filepath.Join(root, "empty")
	mkdir(empty)
	linkedEmpty := filepath.Join(root, "linked-empty")
	if errLink := os.Symlink(empty, linkedEmpty); errLink != nil {
		t.Fatalf("symlink empty: %v", errLink)
	}
	restrictGitRepository(linkedEmpty)
	requireMode(empty, 0o700)

	// In a checkout that also holds other files, such as an application
	// checkout the store was pointed at, only .git and the store's own
	// directories lose their group and other bits. Owner bits are kept, and a
	// symlink does not lead the walk outside the checkout.
	checkout := filepath.Join(root, "checkout")
	mkdir(filepath.Join(checkout, ".git"))
	mkdir(filepath.Join(checkout, "auths", "nested"))
	mkdir(filepath.Join(checkout, "config"))
	writeFile(filepath.Join(checkout, "run-app.sh"), 0o755)
	writeFile(filepath.Join(checkout, "auths", "credential.json"), 0o644)
	writeFile(filepath.Join(checkout, "auths", "nested", "read-only.json"), 0o444)
	writeFile(filepath.Join(checkout, "config", "config.yaml"), 0o644)
	outside := filepath.Join(root, "outside.txt")
	writeFile(outside, 0o644)
	if errLink := os.Symlink(outside, filepath.Join(checkout, "auths", "link.json")); errLink != nil {
		t.Fatalf("symlink: %v", errLink)
	}
	restrictGitRepository(checkout, filepath.Join(checkout, "auths"), filepath.Join(checkout, "config"))
	requireMode(checkout, 0o755)
	requireMode(filepath.Join(checkout, "run-app.sh"), 0o755)
	requireMode(filepath.Join(checkout, ".git"), 0o700)
	requireMode(filepath.Join(checkout, "auths"), 0o700)
	requireMode(filepath.Join(checkout, "auths", "nested"), 0o700)
	requireMode(filepath.Join(checkout, "auths", "credential.json"), 0o600)
	requireMode(filepath.Join(checkout, "auths", "nested", "read-only.json"), 0o400)
	requireMode(filepath.Join(checkout, "config"), 0o700)
	requireMode(filepath.Join(checkout, "config", "config.yaml"), 0o600)
	requireMode(outside, 0o644)
}
