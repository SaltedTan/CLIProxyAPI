package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// groupOtherBits are the permission bits restrictGitRepository removes.
const groupOtherBits os.FileMode = 0o077

// restrictGitRepository keeps the git token store's secrets private to the
// owner. git writes worktree files and objects with the process umask (0644
// under the usual 0022) on every clone, checkout and pull, so the credential
// and config directories, every file in them, and the .git directory holding
// their history lose their group and other permission bits. Owner bits are
// kept, so nothing gains write or loses execute permission, and the rest of the
// checkout is left alone. A directory that is not a checkout yet is only
// tightened when it is empty and about to receive the clone, so a misconfigured
// path never touches someone else's files. Symlinks inside the secret
// directories are not followed. Failures are logged rather than returned so a
// sync never fails over a mode it cannot change.
func restrictGitRepository(repoDir string, secretDirs ...string) {
	if resolved, errResolve := filepath.EvalSymlinks(repoDir); errResolve == nil {
		repoDir = resolved
	}
	gitDir := filepath.Join(repoDir, ".git")
	if _, errStat := os.Lstat(gitDir); errStat != nil {
		if entries, errRead := os.ReadDir(repoDir); errRead == nil && len(entries) == 0 {
			stripGroupOther(repoDir)
		}
		return
	}
	stripGroupOther(gitDir)
	for _, dir := range secretDirs {
		restrictTree(dir)
	}
}

// restrictTree strips group and other bits from dir and everything below it.
func restrictTree(dir string) {
	if dir == "" {
		return
	}
	if resolved, errResolve := filepath.EvalSymlinks(dir); errResolve == nil {
		dir = resolved
	}
	if !stripGroupOther(dir) {
		return
	}
	errWalk := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, errEntry error) error {
		if errEntry != nil {
			return errEntry
		}
		if path != dir && (entry.IsDir() || entry.Type().IsRegular()) {
			stripGroupOther(path)
		}
		return nil
	})
	if errWalk != nil {
		log.Warnf("git token store: restrict modes under %s: %v", dir, errWalk)
	}
}

// stripGroupOther removes the group and other permission bits from path,
// leaving symlinks alone. It reports whether path exists.
func stripGroupOther(path string) bool {
	info, errStat := os.Lstat(path)
	if errStat != nil {
		if !errors.Is(errStat, fs.ErrNotExist) {
			log.Warnf("git token store: stat %s: %v", path, errStat)
		}
		return false
	}
	perm := info.Mode().Perm()
	if info.Mode()&os.ModeSymlink != 0 || perm&groupOtherBits == 0 {
		return true
	}
	if errChmod := os.Chmod(path, perm&^groupOtherBits); errChmod != nil {
		log.Warnf("git token store: restrict %s: %v", path, errChmod)
	}
	return true
}
