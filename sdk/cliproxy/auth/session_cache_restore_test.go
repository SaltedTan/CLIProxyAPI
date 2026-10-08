package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSessionCacheBindingsRestoreIntoFreshCache(t *testing.T) {
	t.Parallel()

	source := NewSessionCache(time.Hour)
	defer source.Stop()
	source.SetAliases("auth-a", "claude::child::model", "claude::parent::model")
	source.Set("codex::solo::model", "auth-b")
	// Taken after the bindings were made, so their expiries are within now+ttl.
	now := time.Now()

	bindings := source.Bindings(now)
	if len(bindings) != 2 {
		t.Fatalf("Bindings() = %d bindings, want 2: %+v", len(bindings), bindings)
	}
	if want := []string{"claude::child::model", "claude::parent::model"}; !reflect.DeepEqual(bindings[0].Keys, want) {
		t.Fatalf("first binding keys = %v, want %v", bindings[0].Keys, want)
	}

	// A binding saved 40 minutes ago keeps its remaining 20 minutes after a restore.
	partial := SessionBinding{
		Keys:      []string{"claude::older::model"},
		AuthID:    "auth-c",
		ExpiresAt: now.Add(20 * time.Minute),
	}
	bindings = append(bindings, partial)

	target := NewSessionCache(time.Hour)
	defer target.Stop()
	if restored := target.RestoreBindings(bindings, now, nil); restored != 3 {
		t.Fatalf("RestoreBindings() = %d, want 3", restored)
	}

	for key, wantAuth := range map[string]string{
		"claude::child::model":  "auth-a",
		"claude::parent::model": "auth-a",
		"codex::solo::model":    "auth-b",
		"claude::older::model":  "auth-c",
	} {
		if got, ok := target.Get(key); !ok || got != wantAuth {
			t.Fatalf("Get(%q) = %q, %v; want %q, true", key, got, ok, wantAuth)
		}
	}
	got := target.Bindings(now)
	if len(got) != len(bindings) {
		t.Fatalf("Bindings() after restore = %d bindings, want %d", len(got), len(bindings))
	}
	for index := range bindings {
		if !reflect.DeepEqual(got[index].Keys, bindings[index].Keys) || got[index].AuthID != bindings[index].AuthID ||
			!got[index].ExpiresAt.Equal(bindings[index].ExpiresAt) {
			t.Fatalf("binding %d after restore = %+v, want %+v", index, got[index], bindings[index])
		}
	}
}

func TestSessionCacheRestoreBindingsSkipsAndCaps(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	cache.Set("claude::live::model", "auth-live")

	bindings := []SessionBinding{
		{Keys: []string{"claude::expired::model"}, AuthID: "auth-a", ExpiresAt: now.Add(-time.Minute)},
		{Keys: []string{"claude::removed::model"}, AuthID: "auth-removed", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::disabled::model"}, AuthID: "auth-disabled", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::no-auth::model"}, AuthID: "", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"", ""}, AuthID: "auth-a", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::other::model", "claude::live::model"}, AuthID: "auth-a", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::long::model"}, AuthID: "auth-a", ExpiresAt: now.Add(5 * time.Hour)},
	}
	keep := func(authID string) bool {
		return authID != "auth-removed" && authID != "auth-disabled"
	}
	if restored := cache.RestoreBindings(bindings, now, keep); restored != 1 {
		t.Fatalf("RestoreBindings() = %d, want 1", restored)
	}

	for _, key := range []string{"claude::expired::model", "claude::removed::model", "claude::disabled::model", "claude::no-auth::model", "claude::other::model"} {
		if got, ok := cache.Get(key); ok {
			t.Fatalf("Get(%q) = %q, want no binding", key, got)
		}
	}
	if got, ok := cache.Get("claude::live::model"); !ok || got != "auth-live" {
		t.Fatalf("live binding = %q, %v; want auth-live kept", got, ok)
	}
	if got, ok := cache.Get("claude::long::model"); !ok || got != "auth-a" {
		t.Fatalf("Get(long) = %q, %v; want auth-a", got, ok)
	}
	for _, binding := range cache.Bindings(now) {
		if binding.AuthID == "auth-a" && !binding.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("restored expiry = %v, want capped at now+ttl %v", binding.ExpiresAt, now.Add(time.Hour))
		}
	}
}

func TestSessionCacheRestoreBindingsKeepsEvictionOrder(t *testing.T) {
	t.Parallel()

	now := time.Now()
	source := NewSessionCache(time.Hour)
	defer source.Stop()
	source.Set("s1", "auth-1")
	source.Set("s2", "auth-2")
	source.Set("s3", "auth-3")
	// Refreshing s1 makes s2 the least recently used session.
	if _, ok := source.GetAndRefresh("s1"); !ok {
		t.Fatal("GetAndRefresh(s1) missed")
	}

	bindings := source.Bindings(now)
	var order []string
	for _, binding := range bindings {
		order = append(order, binding.Keys[0])
	}
	if want := []string{"s2", "s3", "s1"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("Bindings() order = %v, want %v", order, want)
	}

	target := NewSessionCacheWithCapacity(time.Hour, 2)
	defer target.Stop()
	target.RestoreBindings(bindings, now, nil)
	if _, ok := target.Get("s2"); ok {
		t.Fatal("expected least recently used s2 to be evicted on restore")
	}
	for _, key := range []string{"s3", "s1"} {
		if _, ok := target.Get(key); !ok {
			t.Fatalf("expected %s to survive restore", key)
		}
	}
}

func TestSessionBindingsNilSafety(t *testing.T) {
	t.Parallel()

	var cache *SessionCache
	if got := cache.Bindings(time.Now()); got != nil {
		t.Fatalf("nil cache Bindings() = %v, want nil", got)
	}
	if got := cache.RestoreBindings([]SessionBinding{{Keys: []string{"k"}, AuthID: "a", ExpiresAt: time.Now().Add(time.Hour)}}, time.Now(), nil); got != 0 {
		t.Fatalf("nil cache RestoreBindings() = %d, want 0", got)
	}
	var selector *SessionAffinitySelector
	if got := selector.SessionBindings(time.Now()); got != nil {
		t.Fatalf("nil selector SessionBindings() = %v, want nil", got)
	}
	if got := selector.RestoreSessionBindings(nil, time.Now(), nil); got != 0 {
		t.Fatalf("nil selector RestoreSessionBindings() = %d, want 0", got)
	}
}

func TestSessionAffinityRestoredBindingHitsAndFailsOver(t *testing.T) {
	t.Parallel()

	now := time.Now()
	authA := &Auth{ID: "auth-a", Provider: "claude", Status: StatusActive}
	authB := &Auth{ID: "auth-b", Provider: "claude", Status: StatusActive}
	opts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			Headers: http.Header{"X-Claude-Code-Session-Id": []string{"restored-session-1"}},
		}
	}

	// The binding is made by a real pick before the restart, then carried over.
	before := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: lastAuthSelector{}, TTL: time.Hour})
	defer before.Stop()
	if got, errPick := before.Pick(context.Background(), "claude", "claude-model", opts(), []*Auth{authA}); errPick != nil || got == nil || got.ID != authA.ID {
		t.Fatalf("initial Pick() = %v, %v; want %s", got, errPick, authA.ID)
	}
	saved := before.SessionBindings(now)
	if len(saved) != 1 {
		t.Fatalf("SessionBindings() = %+v, want one binding", saved)
	}

	t.Run("bound auth available", func(t *testing.T) {
		after := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: lastAuthSelector{}, TTL: time.Hour})
		defer after.Stop()
		if restored := after.RestoreSessionBindings(saved, now, nil); restored != 1 {
			t.Fatalf("RestoreSessionBindings() = %d, want 1", restored)
		}
		// lastAuthSelector would pick auth-b on a miss, so auth-a proves a cache hit.
		got, errPick := after.Pick(context.Background(), "claude", "claude-model", opts(), []*Auth{authA, authB})
		if errPick != nil || got == nil || got.ID != authA.ID {
			t.Fatalf("Pick() after restore = %v, %v; want cache hit on %s", got, errPick, authA.ID)
		}
	})

	t.Run("bound auth unavailable", func(t *testing.T) {
		after := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: lastAuthSelector{}, TTL: time.Hour})
		defer after.Stop()
		if restored := after.RestoreSessionBindings(saved, now, nil); restored != 1 {
			t.Fatalf("RestoreSessionBindings() = %d, want 1", restored)
		}
		disabledA := &Auth{ID: authA.ID, Provider: "claude", Status: StatusDisabled, Disabled: true}
		got, errPick := after.Pick(context.Background(), "claude", "claude-model", opts(), []*Auth{disabledA, authB})
		if errPick != nil || got == nil || got.ID != authB.ID {
			t.Fatalf("Pick() with bound auth disabled = %v, %v; want failover to %s", got, errPick, authB.ID)
		}
		rebound := after.SessionBindings(time.Now())
		if len(rebound) != 1 || rebound[0].AuthID != authB.ID || !reflect.DeepEqual(rebound[0].Keys, saved[0].Keys) {
			t.Fatalf("bindings after failover = %+v, want session rebound to %s", rebound, authB.ID)
		}
	})
}
