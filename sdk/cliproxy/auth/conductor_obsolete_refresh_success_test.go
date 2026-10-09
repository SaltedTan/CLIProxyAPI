package auth

import (
	"context"
	"testing"
	"time"
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

// A Meta auth whose effective device token (in its attributes, or as a dca: access token)
// was replaced while a refresh ran must not receive the key minted for the old token.
func TestManagerRefreshSuccessIgnoresMetaGrantSwapOutsideMetadata(t *testing.T) {
	for _, source := range []string{"dca_token", "access_token"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:         "refresh-meta-" + source,
				Provider:   "meta",
				Status:     StatusActive,
				Attributes: map[string]string{source: "dca:A"},
				Metadata:   map[string]any{},
			}
			if source == "dca_token" {
				auth.Metadata["dca_token"] = "dca:A"
			}
			base := registerForLineage(t, manager, auth)

			replacement := base.Clone()
			replacement.Attributes[source] = "dca:B"
			if _, errUpdate := manager.Update(ctx, replacement); errUpdate != nil {
				t.Fatalf("Update() error = %v", errUpdate)
			}
			replaced := currentForLineage(t, manager, base.ID)
			if replaced.quotaLineage == base.quotaLineage {
				t.Fatal("the swap kept the lineage, want a new one")
			}
			if replaced.CredentialVersion != base.CredentialVersion {
				t.Fatalf("credential version = %d, want %d: the test needs a swap the version misses", replaced.CredentialVersion, base.CredentialVersion)
			}

			minted := base.Clone()
			minted.Metadata["dca_token"] = "dca:A"
			minted.Metadata["api_key"] = "key-for-A"
			minted.Metadata["access_token"] = "key-for-A"
			if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, minted); errUpdate != nil {
				t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
			}
			if got := authMetadataString(currentForLineage(t, manager, base.ID), "api_key"); got != "" {
				t.Fatalf("api_key = %q, want none: the old grant's key was installed on device token B", got)
			}
		})
	}
}

// Accepting a rotation onto a replace that renewed only the lineage gives the auth a fresh
// lineage and keeps the replacement's quota, never the snapshot the refresh started from.
func TestManagerRefreshSuccessAfterLineageRenewalKeepsCurrentQuota(t *testing.T) {
	for _, withCurrentQuota := range []bool{false, true} {
		name := "no current snapshot"
		if withCurrentQuota {
			name = "current snapshot"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, nil, nil)
			synthetic := lineageClaudeAuth("refresh-lineage-quota", "1")
			synthetic.Metadata["account_uuid"] = "synthetic-uuid"
			base := registerForLineage(t, manager, synthetic)

			corrected := base.Clone()
			corrected.Metadata["account_uuid"] = "real-uuid"
			if _, errUpdate := manager.Update(ctx, corrected); errUpdate != nil {
				t.Fatalf("Update() error = %v", errUpdate)
			}
			replaced := currentForLineage(t, manager, base.ID)
			if len(replaced.Quota.Signals) != 0 {
				t.Fatal("the replace kept the old account's quota")
			}
			if withCurrentQuota {
				manager.mu.Lock()
				manager.auths[base.ID].Quota = QuotaState{ObservedAt: time.Unix(200, 0), Signals: map[string]string{"current": "yes"}}
				manager.mu.Unlock()
			}

			rotated := withTokens(base, "2")
			rotated.Quota = QuotaState{ObservedAt: time.Unix(300, 0), Signals: map[string]string{"old-lineage": "yes"}}
			if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, rotated); errUpdate != nil {
				t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
			}
			current := currentForLineage(t, manager, base.ID)
			if got := authRefreshToken(current); got != "refresh-2" {
				t.Fatalf("refresh token = %q, want refresh-2", got)
			}
			if current.quotaLineage == base.quotaLineage || current.quotaLineage == replaced.quotaLineage {
				t.Fatal("the accepted rotation kept an earlier lineage, want a fresh one")
			}
			if current.Quota.Signals["old-lineage"] != "" {
				t.Fatal("the refresh restored the snapshot it started from")
			}
			if withCurrentQuota && current.Quota.Signals["current"] != "yes" {
				t.Fatal("the refresh dropped the replacement's quota")
			}
			if !withCurrentQuota && len(current.Quota.Signals) != 0 {
				t.Fatalf("quota = %v, want none", current.Quota.Signals)
			}
		})
	}
}

// A Devin auth whose session was swapped while a status refresh ran must not receive the
// old session's identity.
func TestManagerRefreshSuccessIgnoresDevinSessionSwap(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	base := registerForLineage(t, manager, &Auth{
		ID:       "refresh-devin",
		Provider: "devin",
		Status:   StatusActive,
		Metadata: map[string]any{"session_token": "A"},
	})
	replacement := base.Clone()
	replacement.Metadata["session_token"] = "B"
	if _, errUpdate := manager.Update(ctx, replacement); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}

	refreshed := base.Clone()
	refreshed.Metadata["email"] = "a@example.test"
	refreshed.Metadata["org_id"] = "org-A"
	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, refreshed); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	if got := authMetadataString(currentForLineage(t, manager, base.ID), "org_id"); got == "org-A" {
		t.Fatal("session A's identity was installed on session B")
	}
}
