// Package secretfile writes files that hold credentials so that only their
// owner can read them, whatever the process umask is.
package secretfile

import (
	"os"

	log "github.com/sirupsen/logrus"
)

// Mode is the permission every secret-bearing file is written with.
const Mode os.FileMode = 0o600

// Create opens path for writing and truncates it. A new file is created with
// Mode, and an existing file is tightened to Mode before anything is written.
func Create(path string) (*os.File, error) {
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, Mode)
	if errOpen != nil {
		return nil, errOpen
	}
	restrict(file)
	return file, nil
}

// OpenExisting opens an existing file for writing and truncates it, tightening it
// to Mode first. Unlike Create it never creates the file, so an update cannot bring
// back a file that was deleted after it was read.
func OpenExisting(path string) (*os.File, error) {
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, Mode)
	if errOpen != nil {
		return nil, errOpen
	}
	restrict(file)
	return file, nil
}

// WriteFile writes data to path like os.WriteFile, with the permissions of Create.
func WriteFile(path string, data []byte) error {
	file, errCreate := Create(path)
	if errCreate != nil {
		return errCreate
	}
	_, errWrite := file.Write(data)
	if errClose := file.Close(); errClose != nil && errWrite == nil {
		errWrite = errClose
	}
	return errWrite
}

// restrict tightens an existing file to Mode. A failure only logs a warning: a
// file owned by another user can be writable but not chmod-able, and refusing
// to save a refreshed credential would break the account.
func restrict(file *os.File) {
	info, errStat := file.Stat()
	if errStat == nil && info.Mode().Perm() == Mode {
		return
	}
	if errChmod := file.Chmod(Mode); errChmod != nil {
		log.WithError(errChmod).Warnf("could not restrict %s to owner-only access", file.Name())
	}
}
