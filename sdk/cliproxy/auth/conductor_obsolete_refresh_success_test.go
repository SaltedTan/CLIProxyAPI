package auth

import (
	"context"
	"testing"
)

// blockingMetaKeyRefresher exchanges the Meta device token it receives for an API key once
// released.
type blockingMetaKeyRefresher struct {
	schedulerProviderTestExecutor
	started chan struct{}
	release chan struct{}
}

func (e *blockingMetaKeyRefresher) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	close(e.started)
	<-e.release
	auth.Metadata["api_key"] = "key-for-" + authMetadataString(auth, "dca_token")
	return auth, nil
}

// A successful refresh that finished after a replace moved the auth to another account must
// not install the old account's key. Meta's device token does not bump the credential
// version, so only the quota lineage shows the replace.
func TestManagerRefreshAuthObsoleteSuccessKeepsReplacementCredentials(t *testing.T) {
	ctx := context.Background()
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &blockingMetaKeyRefresher{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "meta"},
		started:                       make(chan struct{}),
		release:                       make(chan struct{}),
	}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(ctx, staleMintMetaAuth("dca-a", "a@example.com"))
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	refreshDone := make(chan struct{})
	go func() {
		manager.refreshAuth(ctx, registered.ID)
		close(refreshDone)
	}()
	<-executor.started

	if _, errUpdate := manager.Update(ctx, staleMintMetaAuth("dca-b", "b@example.com")); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	before := currentForLineage(t, manager, registered.ID)

	close(executor.release)
	<-refreshDone

	current := currentForLineage(t, manager, registered.ID)
	if got := authMetadataString(current, "dca_token"); got != "dca-b" {
		t.Fatalf("dca_token = %q, want dca-b", got)
	}
	if got := authMetadataString(current, "api_key"); got != "" {
		t.Fatalf("api_key = %q, want none: account A's key was installed on account B", got)
	}
	if current.Generation != before.Generation {
		t.Fatalf("generation = %d, want %d: the obsolete refresh mutated the replacement", current.Generation, before.Generation)
	}
	store.mu.Lock()
	persisted := store.auths[registered.ID].Clone()
	store.mu.Unlock()
	if got := authMetadataString(persisted, "api_key"); got != "" {
		t.Fatalf("persisted api_key = %q, want none", got)
	}
}
