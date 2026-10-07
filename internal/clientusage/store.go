package clientusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// StateFileName is the file usage is persisted to, next to the config file
	// (or under WRITABLE_PATH when set).
	StateFileName = "client-usage.json"
	stateVersion  = 1
	flushInterval = time.Minute
)

type stateFile struct {
	Version           int                          `json:"version"`
	Since             time.Time                    `json:"since"`
	SavedAt           time.Time                    `json:"saved_at"`
	Keys              map[string]*keyState         `json:"keys"`
	ClaudeCredentials map[string]*claudeCredential `json:"claude_credentials"`
}

// ResolveStatePath returns where usage is persisted for the given config file path.
func ResolveStatePath(configFilePath string) string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, StateFileName)
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}
	base := filepath.Dir(configFilePath)
	if info, errStat := os.Stat(configFilePath); errStat == nil && info.IsDir() {
		base = configFilePath
	}
	return filepath.Join(base, StateFileName)
}

// Open sets the state file and loads usage saved there. An empty path keeps usage in
// memory only. Opening the already open path is a no-op; switching to another path
// first saves usage to the previous one (and keeps it if that fails), then starts from
// the new file's contents. A state file that cannot be parsed is moved aside rather
// than overwritten.
func (t *Tracker) Open(path string) error {
	if t == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	// Hold both locks so no record or flush lands between saving and switching.
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if path == t.path {
		return nil
	}
	if t.path != "" {
		if t.dirty {
			data, errMarshal := t.marshalLocked()
			if errMarshal == nil {
				errMarshal = writeFileAtomic(t.path, data)
			}
			if errMarshal != nil {
				return fmt.Errorf("save client usage before switching state files: %w", errMarshal)
			}
		}
		t.since = time.Time{}
		t.keys = make(map[string]*keyState)
		t.claude = make(map[string]*claudeCredential)
		t.dirty = false
	}
	t.path = path
	if path == "" {
		return nil
	}
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		// Keep the unreadable file intact; do not persist over it.
		t.path = ""
		return fmt.Errorf("read client usage state: %w", errRead)
	}
	var state stateFile
	errState := json.Unmarshal(data, &state)
	if errState == nil && state.Version != stateVersion {
		errState = fmt.Errorf("unsupported version %d", state.Version)
	}
	if errState != nil {
		aside := fmt.Sprintf("%s.invalid-%d", path, t.now().UnixNano())
		if errRename := os.Rename(path, aside); errRename != nil {
			t.path = ""
			return fmt.Errorf("client usage state %s is invalid (%v) and could not be moved aside: %w", path, errState, errRename)
		}
		return fmt.Errorf("client usage state %s is invalid (%v); moved it to %s", path, errState, aside)
	}
	t.since = state.Since
	t.keys = make(map[string]*keyState, len(state.Keys))
	for id, key := range state.Keys {
		if key != nil {
			t.keys[id] = key
		}
	}
	// No request survives a restart, so ordering restarts from now. This also keeps a
	// wall clock that moved back from blocking genuine drops.
	loadedAt := t.now()
	t.claude = make(map[string]*claudeCredential, len(state.ClaudeCredentials))
	for authID, credential := range state.ClaudeCredentials {
		if credential != nil {
			credential.UtilizationSetAt = loadedAt
			credential.EpochStartedAt = loadedAt
			t.claude[authID] = credential
		}
	}
	return nil
}

// Flush writes usage to the state file when it changed since the last write.
func (t *Tracker) Flush() error {
	if t == nil {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()

	t.mu.Lock()
	if t.path == "" || !t.dirty {
		t.mu.Unlock()
		return nil
	}
	path := t.path
	data, errMarshal := t.marshalLocked()
	if errMarshal != nil {
		t.mu.Unlock()
		return errMarshal
	}
	t.dirty = false
	t.mu.Unlock()

	if errWrite := writeFileAtomic(path, data); errWrite != nil {
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
		return errWrite
	}
	return nil
}

func (t *Tracker) marshalLocked() ([]byte, error) {
	data, errMarshal := json.Marshal(stateFile{
		Version:           stateVersion,
		Since:             t.since,
		SavedAt:           t.now(),
		Keys:              t.keys,
		ClaudeCredentials: t.claude,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode client usage state: %w", errMarshal)
	}
	return data, nil
}

// Run flushes usage periodically until ctx is done, then flushes once more.
func (t *Tracker) Run(ctx context.Context) {
	if t == nil {
		return
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.logFlush()
			return
		case <-ticker.C:
			t.logFlush()
		}
	}
}

func (t *Tracker) logFlush() {
	if errFlush := t.Flush(); errFlush != nil {
		log.Warnf("client usage: %v", errFlush)
	}
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create client usage state directory: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(dir, ".client-usage-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create client usage state temp file: %w", errCreate)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
			log.Warnf("client usage: remove temp file: %v", errRemove)
		}
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write client usage state: %w", errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync client usage state: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		cleanup()
		return fmt.Errorf("close client usage state: %w", errClose)
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		cleanup()
		return fmt.Errorf("replace client usage state: %w", errRename)
	}
	return nil
}
