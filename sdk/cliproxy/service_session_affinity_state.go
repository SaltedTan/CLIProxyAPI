package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// sessionAffinityStateFileName is the file session affinity bindings are saved to,
	// next to the config file (or under WRITABLE_PATH when set).
	sessionAffinityStateFileName = "session-affinity.json"
	sessionAffinityStateVersion  = 1
	sessionAffinitySaveInterval  = time.Minute
)

type sessionAffinityStateFile struct {
	Version  int             `json:"version"`
	SavedAt  time.Time       `json:"saved_at"`
	Bindings json.RawMessage `json:"bindings"`
}

// sessionAffinityStore keeps session affinity bindings across restarts, so a live
// conversation stays on the credential that holds its prompt cache. Only the session
// cache is saved; prefix-matched (LCP) bindings stay in memory.
type sessionAffinityStore struct {
	path string

	mu        sync.Mutex
	lastSaved []byte // encoded bindings of the last write, without saved_at
}

// resolveSessionAffinityStatePath returns where bindings are saved for the given config
// file path, or "" when they cannot be saved.
func resolveSessionAffinityStatePath(configFilePath string) string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, sessionAffinityStateFileName)
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}
	base := filepath.Dir(configFilePath)
	if info, errStat := os.Stat(configFilePath); errStat == nil && info.IsDir() {
		base = configFilePath
	}
	return filepath.Join(base, sessionAffinityStateFileName)
}

func newSessionAffinityStore(configFilePath string) *sessionAffinityStore {
	path := resolveSessionAffinityStatePath(configFilePath)
	if path == "" {
		return nil
	}
	return &sessionAffinityStore{path: path}
}

// startSessionAffinityPersistence restores saved session affinity bindings and keeps
// saving them every minute until ctx is done. Shutdown saves them a last time.
func (s *Service) startSessionAffinityPersistence(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	store := newSessionAffinityStore(s.configPath)
	if store == nil {
		return
	}
	// Restore into the selector that stays: an unapplied routing state would make the
	// first config commit replace the selector and drop the restored bindings.
	s.ensureRoutingSelector()
	store.restore(s.coreManager, time.Now())
	s.sessionAffinityState.Store(store)
	go store.run(ctx, s.coreManager)
}

// saveSessionAffinityState writes the current bindings once; Shutdown calls it after
// the HTTP server has stopped so bindings made by the last requests are kept.
func (s *Service) saveSessionAffinityState() {
	if s == nil {
		return
	}
	if store := s.sessionAffinityState.Load(); store != nil {
		store.logSave(s.coreManager, time.Now())
	}
}

func (st *sessionAffinityStore) run(ctx context.Context, manager *coreauth.Manager) {
	ticker := time.NewTicker(sessionAffinitySaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.logSave(manager, time.Now())
		}
	}
}

func (st *sessionAffinityStore) logSave(manager *coreauth.Manager, now time.Time) {
	if errSave := st.save(manager, now); errSave != nil {
		log.Warnf("session affinity: %v", errSave)
	}
}

// restore loads the saved bindings into the manager's session affinity selector and
// returns how many were restored. Only bindings whose credential still exists and is
// enabled are kept; a missing, unreadable or unsupported state file restores nothing.
func (st *sessionAffinityStore) restore(manager *coreauth.Manager, now time.Time) int {
	if st == nil || manager == nil {
		return 0
	}
	selector, ok := manager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		return 0
	}
	bindings, errLoad := loadSessionAffinityState(st.path)
	if errLoad != nil {
		if !errors.Is(errLoad, fs.ErrNotExist) {
			log.Warnf("session affinity: ignoring saved bindings: %v", errLoad)
		}
		return 0
	}
	usable := make(map[string]bool)
	keep := func(authID string) bool {
		enabled, seen := usable[authID]
		if !seen {
			auth, exists := manager.GetByID(authID)
			enabled = exists && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
			usable[authID] = enabled
		}
		return enabled
	}
	restored := selector.RestoreSessionBindings(bindings, now, keep)
	log.Infof("session affinity: restored %d of %d saved bindings", restored, len(bindings))
	return restored
}

// save writes the bindings of the manager's session affinity selector. It does nothing
// while session affinity is off and skips the write when the bindings are unchanged.
func (st *sessionAffinityStore) save(manager *coreauth.Manager, now time.Time) error {
	if st == nil || manager == nil {
		return nil
	}
	selector, ok := manager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	bindings := selector.SessionBindings(now)
	if bindings == nil {
		bindings = []coreauth.SessionBinding{}
	}
	encoded, errEncode := json.Marshal(bindings)
	if errEncode != nil {
		return fmt.Errorf("encode bindings: %w", errEncode)
	}
	if st.lastSaved != nil && bytes.Equal(encoded, st.lastSaved) {
		return nil
	}
	data, errEncode := json.Marshal(sessionAffinityStateFile{
		Version:  sessionAffinityStateVersion,
		SavedAt:  now.UTC().Truncate(time.Second),
		Bindings: encoded,
	})
	if errEncode != nil {
		return fmt.Errorf("encode state: %w", errEncode)
	}
	if errWrite := writeSessionAffinityStateAtomic(st.path, data); errWrite != nil {
		return errWrite
	}
	st.lastSaved = encoded
	return nil
}

// loadSessionAffinityState reads saved bindings. A missing file returns an error
// wrapping fs.ErrNotExist.
func loadSessionAffinityState(path string) ([]coreauth.SessionBinding, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", path, errRead)
	}
	var state sessionAffinityStateFile
	if errDecode := json.Unmarshal(data, &state); errDecode != nil {
		return nil, fmt.Errorf("parse %s: %w", path, errDecode)
	}
	if state.Version != sessionAffinityStateVersion {
		return nil, fmt.Errorf("%s has unsupported version %d", path, state.Version)
	}
	var bindings []coreauth.SessionBinding
	if len(state.Bindings) > 0 {
		if errDecode := json.Unmarshal(state.Bindings, &bindings); errDecode != nil {
			return nil, fmt.Errorf("parse bindings in %s: %w", path, errDecode)
		}
	}
	return bindings, nil
}

func writeSessionAffinityStateAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state directory: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(dir, ".session-affinity-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create state temp file: %w", errCreate)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
			log.Warnf("session affinity: remove temp file: %v", errRemove)
		}
	}
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("set state file mode: %w", errChmod)
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write state: %w", errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync state: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		cleanup()
		return fmt.Errorf("close state: %w", errClose)
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		cleanup()
		return fmt.Errorf("replace state: %w", errRename)
	}
	return nil
}
