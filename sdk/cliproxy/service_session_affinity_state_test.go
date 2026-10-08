package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func newSessionAffinityTestManager(t *testing.T, auths ...*coreauth.Auth) (*coreauth.Manager, *coreauth.SessionAffinitySelector) {
	t.Helper()
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{TTL: time.Hour})
	t.Cleanup(selector.Stop)
	manager := coreauth.NewManager(nil, selector, nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	return manager, selector
}

func TestResolveSessionAffinityStatePath(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()

	if got := resolveSessionAffinityStatePath(""); got != "" {
		t.Fatalf("empty config path = %q, want persistence disabled", got)
	}
	if got, want := resolveSessionAffinityStatePath(filepath.Join(dir, "config.yaml")), filepath.Join(dir, "session-affinity.json"); got != want {
		t.Fatalf("config file path = %q, want %q", got, want)
	}
	if got, want := resolveSessionAffinityStatePath(dir), filepath.Join(dir, "session-affinity.json"); got != want {
		t.Fatalf("config directory path = %q, want %q", got, want)
	}
	writable := filepath.Join(dir, "writable")
	t.Setenv("WRITABLE_PATH", writable)
	if got, want := resolveSessionAffinityStatePath(filepath.Join(dir, "config.yaml")), filepath.Join(writable, "session-affinity.json"); got != want {
		t.Fatalf("WRITABLE_PATH = %q, want %q", got, want)
	}
}

func TestSessionAffinityStateSaveLoadRestore(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "state", "session-affinity.json")
	bindings := []coreauth.SessionBinding{
		{Keys: []string{"claude::kept::model", "claude::kept-parent::model"}, AuthID: "auth-kept", ExpiresAt: now.Add(40 * time.Minute)},
		{Keys: []string{"claude::removed::model"}, AuthID: "auth-removed", ExpiresAt: now.Add(40 * time.Minute)},
		{Keys: []string{"claude::disabled::model"}, AuthID: "auth-disabled", ExpiresAt: now.Add(40 * time.Minute)},
		{Keys: []string{"claude::status-disabled::model"}, AuthID: "auth-status-disabled", ExpiresAt: now.Add(40 * time.Minute)},
	}

	before, beforeSelector := newSessionAffinityTestManager(t)
	if restored := beforeSelector.RestoreSessionBindings(bindings, now, nil); restored != len(bindings) {
		t.Fatalf("seed RestoreSessionBindings() = %d, want %d", restored, len(bindings))
	}
	saver := &sessionAffinityStore{path: path}
	if errSave := saver.save(before, now); errSave != nil {
		t.Fatalf("save() error = %v", errSave)
	}

	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat state file: %v", errStat)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read state file: %v", errRead)
	}
	var state sessionAffinityStateFile
	if errDecode := json.Unmarshal(raw, &state); errDecode != nil {
		t.Fatalf("decode state file: %v", errDecode)
	}
	if state.Version != 1 || state.SavedAt.IsZero() {
		t.Fatalf("state header = version %d saved_at %v, want version 1 and a save time", state.Version, state.SavedAt)
	}

	loaded, errLoad := loadSessionAffinityState(path)
	if errLoad != nil {
		t.Fatalf("loadSessionAffinityState() error = %v", errLoad)
	}
	if len(loaded) != len(bindings) {
		t.Fatalf("loaded %d bindings, want %d", len(loaded), len(bindings))
	}

	after, afterSelector := newSessionAffinityTestManager(t,
		&coreauth.Auth{ID: "auth-kept", Provider: "claude", Status: coreauth.StatusActive},
		&coreauth.Auth{ID: "auth-disabled", Provider: "claude", Status: coreauth.StatusActive, Disabled: true},
		&coreauth.Auth{ID: "auth-status-disabled", Provider: "claude", Status: coreauth.StatusDisabled},
	)
	restorer := &sessionAffinityStore{path: path}
	if restored := restorer.restore(after, now); restored != 1 {
		t.Fatalf("restore() = %d, want 1", restored)
	}
	got := afterSelector.SessionBindings(now)
	if len(got) != 1 || got[0].AuthID != "auth-kept" || len(got[0].Keys) != 2 || !got[0].ExpiresAt.Equal(bindings[0].ExpiresAt) {
		t.Fatalf("restored bindings = %+v, want only auth-kept with its saved expiry", got)
	}
}

func TestSessionAffinityStateSkipsUnchangedWrites(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "session-affinity.json")
	manager, selector := newSessionAffinityTestManager(t)
	selector.RestoreSessionBindings([]coreauth.SessionBinding{
		{Keys: []string{"claude::one::model"}, AuthID: "auth-a", ExpiresAt: now.Add(time.Hour)},
	}, now, nil)

	store := &sessionAffinityStore{path: path}
	if errSave := store.save(manager, now); errSave != nil {
		t.Fatalf("first save() error = %v", errSave)
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove state file: %v", errRemove)
	}
	// Same bindings at a later save time: nothing is written.
	if errSave := store.save(manager, now.Add(time.Minute)); errSave != nil {
		t.Fatalf("unchanged save() error = %v", errSave)
	}
	if _, errStat := os.Stat(path); !errors.Is(errStat, fs.ErrNotExist) {
		t.Fatalf("unchanged save wrote the state file (stat error %v)", errStat)
	}

	selector.RestoreSessionBindings([]coreauth.SessionBinding{
		{Keys: []string{"claude::two::model"}, AuthID: "auth-b", ExpiresAt: now.Add(time.Hour)},
	}, now, nil)
	if errSave := store.save(manager, now.Add(2*time.Minute)); errSave != nil {
		t.Fatalf("changed save() error = %v", errSave)
	}
	loaded, errLoad := loadSessionAffinityState(path)
	if errLoad != nil || len(loaded) != 2 {
		t.Fatalf("loaded after change = %+v, %v; want 2 bindings", loaded, errLoad)
	}
}

func TestSessionAffinityStateIgnoresBadFiles(t *testing.T) {
	now := time.Now()
	validBindings := `[{"keys":["claude::s::model"],"auth_id":"auth-a","expires_at":"` + now.Add(time.Hour).Format(time.RFC3339Nano) + `"}]`
	for name, content := range map[string]string{
		"corrupt":         `{"version":1,"bindings":[`,
		"unknown version": `{"version":99,"saved_at":"2026-01-01T00:00:00Z","bindings":` + validBindings + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session-affinity.json")
			if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
				t.Fatalf("write state file: %v", errWrite)
			}
			if _, errLoad := loadSessionAffinityState(path); errLoad == nil || errors.Is(errLoad, fs.ErrNotExist) {
				t.Fatalf("loadSessionAffinityState() error = %v, want a parse or version error", errLoad)
			}
			manager, selector := newSessionAffinityTestManager(t, &coreauth.Auth{ID: "auth-a", Provider: "claude", Status: coreauth.StatusActive})
			if restored := (&sessionAffinityStore{path: path}).restore(manager, now); restored != 0 {
				t.Fatalf("restore() = %d, want 0", restored)
			}
			if got := selector.SessionBindings(now); len(got) != 0 {
				t.Fatalf("bindings after bad file = %+v, want none", got)
			}
			if raw, errRead := os.ReadFile(path); errRead != nil || string(raw) != content {
				t.Fatalf("bad state file was changed or removed: %q, %v", raw, errRead)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session-affinity.json")
		if _, errLoad := loadSessionAffinityState(path); !errors.Is(errLoad, fs.ErrNotExist) {
			t.Fatalf("loadSessionAffinityState() error = %v, want fs.ErrNotExist", errLoad)
		}
		manager, _ := newSessionAffinityTestManager(t)
		if restored := (&sessionAffinityStore{path: path}).restore(manager, now); restored != 0 {
			t.Fatalf("restore() = %d, want 0", restored)
		}
	})
}

func TestSessionAffinityStateLeavesFileAloneWhenAffinityOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-affinity.json")
	content := []byte(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","bindings":[]}`)
	if errWrite := os.WriteFile(path, content, 0o600); errWrite != nil {
		t.Fatalf("write state file: %v", errWrite)
	}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	store := &sessionAffinityStore{path: path}
	if errSave := store.save(manager, time.Now()); errSave != nil {
		t.Fatalf("save() error = %v", errSave)
	}
	if restored := store.restore(manager, time.Now()); restored != 0 {
		t.Fatalf("restore() = %d, want 0", restored)
	}
	if raw, errRead := os.ReadFile(path); errRead != nil || !bytes.Equal(raw, content) {
		t.Fatalf("state file changed with session affinity off: %q, %v", raw, errRead)
	}
}

func TestSessionAffinityRestoreSurvivesFirstConfigCommitWithSuppliedManager(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	now := time.Now()
	dir := t.TempDir()
	encoded, errEncode := json.Marshal([]coreauth.SessionBinding{
		{Keys: []string{"claude::restored::model"}, AuthID: "auth-a", ExpiresAt: now.Add(30 * time.Minute)},
	})
	if errEncode != nil {
		t.Fatalf("encode bindings: %v", errEncode)
	}
	data, errEncode := json.Marshal(sessionAffinityStateFile{Version: sessionAffinityStateVersion, SavedAt: now, Bindings: encoded})
	if errEncode != nil {
		t.Fatalf("encode state: %v", errEncode)
	}
	if errWrite := writeSessionAffinityStateAtomic(filepath.Join(dir, "session-affinity.json"), data); errWrite != nil {
		t.Fatalf("write state file: %v", errWrite)
	}

	cfg := &config.Config{Routing: config.RoutingConfig{SessionAffinity: true, SessionAffinityTTL: "1h"}}
	// A manager supplied by an embedder keeps its own (empty) affinity selector and leaves
	// the routing state unapplied, so the first config commit would replace that selector.
	embedderSelector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{TTL: time.Hour})
	t.Cleanup(embedderSelector.Stop)
	manager := coreauth.NewManager(nil, embedderSelector, nil)
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "auth-a", Provider: "claude", Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatalf("Register(auth-a): %v", errRegister)
	}
	service := &Service{cfg: cfg, configPath: filepath.Join(dir, "config.yaml"), coreManager: manager}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	service.startSessionAffinityPersistence(ctx)
	restoredSelector, ok := manager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector after restore = %T, want *SessionAffinitySelector", manager.Selector())
	}
	t.Cleanup(restoredSelector.Stop)
	if got := restoredSelector.SessionBindings(now); len(got) != 1 {
		t.Fatalf("bindings after restore = %+v, want the saved binding", got)
	}

	// The watcher's first config callback applies the same config.
	if !service.applyConfigRuntime(context.Background(), service.commitConfigUpdate(cfg), false) {
		t.Fatal("config runtime apply failed")
	}
	current, ok := manager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector after config commit = %T, want *SessionAffinitySelector", manager.Selector())
	}
	got := current.SessionBindings(now)
	if len(got) != 1 || got[0].AuthID != "auth-a" {
		t.Fatalf("bindings after config commit = %+v, want the restored auth-a binding", got)
	}
}

func TestSessionAffinityRestoreKeepsAppliedSelector(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	cfg := &config.Config{
		AuthDir: t.TempDir(),
		Routing: config.RoutingConfig{SessionAffinity: true, SessionAffinityTTL: "1h"},
	}
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	initial := service.coreManager.Selector()
	if affinity, ok := initial.(*coreauth.SessionAffinitySelector); ok {
		t.Cleanup(affinity.Stop)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	service.startSessionAffinityPersistence(ctx)
	if got := service.coreManager.Selector(); got != initial {
		t.Fatalf("selector = %p, want the builder's selector %p kept", got, initial)
	}
}

func TestServiceShutdownSavesSessionAffinityBindings(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "session-affinity.json")
	manager, selector := newSessionAffinityTestManager(t)
	selector.RestoreSessionBindings([]coreauth.SessionBinding{
		{Keys: []string{"claude::last::model"}, AuthID: "auth-a", ExpiresAt: now.Add(time.Hour)},
	}, now, nil)

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.sessionAffinityState.Store(&sessionAffinityStore{path: path})
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("Shutdown() error = %v", errShutdown)
	}
	loaded, errLoad := loadSessionAffinityState(path)
	if errLoad != nil || len(loaded) != 1 || loaded[0].AuthID != "auth-a" {
		t.Fatalf("bindings saved at shutdown = %+v, %v; want the auth-a binding", loaded, errLoad)
	}
}

// A config reload that changes the routing settings replaces the selector. Bindings to
// credentials that still exist and are enabled are carried over to the new session
// affinity selector, with expiries capped at its TTL; turning affinity off drops them.
func TestRoutingChangeKeepsSessionAffinityBindings(t *testing.T) {
	authA := &coreauth.Auth{ID: "routing-change-a", Provider: "claude", Status: coreauth.StatusActive}
	authB := &coreauth.Auth{ID: "routing-change-b", Provider: "claude", Status: coreauth.StatusActive}
	disabled := &coreauth.Auth{ID: "routing-change-disabled", Provider: "claude", Status: coreauth.StatusDisabled, Disabled: true}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{authA, authB, disabled} {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	service := &Service{coreManager: manager}
	sequence := uint64(0)
	apply := func(routing config.RoutingConfig) coreauth.Selector {
		t.Helper()
		sequence++
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: &config.Config{Routing: routing}, sequence: sequence}) {
			t.Fatal("applyManagerConfig failed")
		}
		selector := manager.Selector()
		if affinity, ok := selector.(*coreauth.SessionAffinitySelector); ok {
			t.Cleanup(affinity.Stop)
		}
		return selector
	}
	opts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"routing-change-session"}}}
	}

	before, ok := apply(config.RoutingConfig{Strategy: "round-robin", SessionAffinity: true, SessionAffinityTTL: "1h"}).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector = %T, want *SessionAffinitySelector", manager.Selector())
	}
	// Only authB is offered, so a real pick binds the session to it for an hour.
	if picked, errPick := before.Pick(context.Background(), "claude", "claude-model", opts(), []*coreauth.Auth{authB}); errPick != nil || picked == nil || picked.ID != authB.ID {
		t.Fatalf("initial Pick() = %v, %v; want %s", picked, errPick, authB.ID)
	}
	now := time.Now()
	shortExpiry := now.Add(5 * time.Minute)
	seeded := []coreauth.SessionBinding{
		{Keys: []string{"claude::short::claude-model"}, AuthID: authA.ID, ExpiresAt: shortExpiry},
		{Keys: []string{"claude::removed::claude-model"}, AuthID: "routing-change-removed", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::disabled::claude-model"}, AuthID: disabled.ID, ExpiresAt: now.Add(30 * time.Minute)},
	}
	if restored := before.RestoreSessionBindings(seeded, now, nil); restored != len(seeded) {
		t.Fatalf("seed RestoreSessionBindings() = %d, want %d", restored, len(seeded))
	}

	// A new strategy and a shorter TTL replace the selector. The swap reads the clock
	// once, between swapStart and swapEnd.
	swapStart := time.Now()
	after, ok := apply(config.RoutingConfig{Strategy: "quota-aware", SessionAffinity: true, SessionAffinityTTL: "10m"}).(*coreauth.SessionAffinitySelector)
	swapEnd := time.Now()
	if !ok || after == before {
		t.Fatalf("selector after routing change = %T (same=%v), want a new *SessionAffinitySelector", manager.Selector(), after == before)
	}
	got := make(map[string]coreauth.SessionBinding)
	for _, binding := range after.SessionBindings(swapEnd) {
		got[binding.AuthID] = binding
	}
	if len(got) != 2 {
		t.Fatalf("bindings after routing change = %+v, want the %s and %s bindings only", got, authA.ID, authB.ID)
	}
	if binding, found := got[authA.ID]; !found || !binding.ExpiresAt.Equal(shortExpiry) {
		t.Fatalf("%s binding = %+v, want its expiry %v kept", authA.ID, binding, shortExpiry)
	}
	binding, found := got[authB.ID]
	if !found || binding.ExpiresAt.Before(swapStart.Add(10*time.Minute)) || binding.ExpiresAt.After(swapEnd.Add(10*time.Minute)) {
		t.Fatalf("%s binding = %+v, want its hour-long expiry capped at the new 10m TTL", authB.ID, binding)
	}
	// On a miss the new selector would pick authA, which sorts first.
	if picked, errPick := after.Pick(context.Background(), "claude", "claude-model", opts(), []*coreauth.Auth{authA, authB}); errPick != nil || picked == nil || picked.ID != authB.ID {
		t.Fatalf("Pick() after routing change = %v, %v; want the session kept on %s", picked, errPick, authB.ID)
	}

	// Turning affinity off drops the bindings, so turning it back on starts empty.
	if _, isAffinity := apply(config.RoutingConfig{Strategy: "quota-aware"}).(*coreauth.SessionAffinitySelector); isAffinity {
		t.Fatalf("selector with affinity off = %T, want no session affinity", manager.Selector())
	}
	reenabled, ok := apply(config.RoutingConfig{Strategy: "quota-aware", SessionAffinity: true, SessionAffinityTTL: "10m"}).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector after re-enabling affinity = %T, want *SessionAffinitySelector", manager.Selector())
	}
	if bindings := reenabled.SessionBindings(time.Now()); len(bindings) != 0 {
		t.Fatalf("bindings after re-enabling affinity = %+v, want none", bindings)
	}
}
