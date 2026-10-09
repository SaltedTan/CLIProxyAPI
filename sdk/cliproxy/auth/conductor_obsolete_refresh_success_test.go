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
// version, so the refresh compares it explicitly.
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

// A successful refresh is kept when a replace committed the same credentials meanwhile,
// though the replace renewed the quota lineage: the provider has already invalidated the
// old refresh token, so dropping the rotated one would force a new login.
func TestManagerRefreshSuccessSurvivesReplaceWithSameCredentials(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	synthetic := lineageClaudeAuth("refresh-same-credentials", "1")
	synthetic.Metadata["account_uuid"] = "synthetic-uuid"
	base := registerForLineage(t, manager, synthetic)

	corrected := base.Clone()
	corrected.Metadata["account_uuid"] = "real-uuid"
	if _, errUpdate := manager.Update(ctx, corrected); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	replaced := currentForLineage(t, manager, base.ID)
	if replaced.quotaLineage == base.quotaLineage {
		t.Fatal("replace kept the quota lineage; the test needs a renewed one")
	}
	if replaced.CredentialVersion != base.CredentialVersion {
		t.Fatalf("credential version = %d, want %d: the replace changed no credential", replaced.CredentialVersion, base.CredentialVersion)
	}

	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, withTokens(base, "2")); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	current := currentForLineage(t, manager, base.ID)
	if got := authRefreshToken(current); got != "refresh-2" {
		t.Fatalf("refresh token = %q, want refresh-2: the rotated tokens were dropped", got)
	}
	if got := authAccessToken(current); got != "access-2" {
		t.Fatalf("access token = %q, want access-2", got)
	}
}
