package auth

import (
	"context"
	"testing"
	"time"
)

// reloadQuotaObservedAt is a fixed observation time; the update path never compares it with now.
var reloadQuotaObservedAt = time.Unix(1_800_000_000, 0)

func reloadQuotaSnapshot(observedAt time.Time, fiveHourUsed string) QuotaState {
	return QuotaState{ObservedAt: observedAt, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": fiveHourUsed,
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.40",
	}}
}

// updateAfterReload registers existing, applies incoming the way an auth file reload does,
// and returns the stored result.
func updateAfterReload(t *testing.T, existing, incoming *Auth) *Auth {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), existing); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if _, errUpdate := manager.Update(context.Background(), incoming); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	updated, ok := manager.GetByID(existing.ID)
	if !ok || updated == nil {
		t.Fatalf("auth %s missing after update", existing.ID)
	}
	return updated
}

func claudeReloadMetadata(token, account, organization, email string) map[string]any {
	return map[string]any{
		"type":              "claude",
		"access_token":      "access-" + token,
		"refresh_token":     "refresh-" + token,
		"account_uuid":      account,
		"organization_uuid": organization,
		"email":             email,
	}
}

func TestManagerUpdateQuotaSnapshotFollowsAccount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		provider           string
		existingMetadata   map[string]any
		existingAttributes map[string]string
		incomingProvider   string
		incomingMetadata   map[string]any
		incomingAttributes map[string]string
		wantKept           bool
	}{
		{
			name:             "claude token refresh",
			provider:         "claude",
			existingMetadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
			incomingMetadata: claudeReloadMetadata("2", "ACC-1", "org-1", " User@Example.com "),
			wantKept:         true,
		},
		{
			name:             "claude other organization",
			provider:         "claude",
			existingMetadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
			incomingMetadata: claudeReloadMetadata("2", "acc-1", "org-2", "user@example.com"),
		},
		{
			name:             "claude other email",
			provider:         "claude",
			existingMetadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
			incomingMetadata: claudeReloadMetadata("2", "acc-1", "org-1", "other@example.com"),
		},
		{
			name:             "claude other account uuid",
			provider:         "claude",
			existingMetadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
			incomingMetadata: claudeReloadMetadata("2", "acc-2", "org-1", "user@example.com"),
		},
		{
			// A synthesized account_uuid survives a token swap, so it cannot prove the account.
			name:             "synthesized account uuid with new token",
			provider:         "claude",
			existingMetadata: map[string]any{"access_token": "sk-ant-oat-1", "account_uuid": "synthetic-1"},
			incomingMetadata: map[string]any{"access_token": "sk-ant-oat-2", "account_uuid": "synthetic-1"},
		},
		{
			name:             "synthesized account uuid with same token",
			provider:         "claude",
			existingMetadata: map[string]any{"access_token": "sk-ant-oat-1", "account_uuid": "synthetic-1"},
			incomingMetadata: map[string]any{"access_token": "sk-ant-oat-1", "account_uuid": "synthetic-1", "note": "edited"},
			wantKept:         true,
		},
		{
			// Config credentials gain identity metadata at runtime that a config reload lacks.
			name:               "runtime identity on one side with same key",
			provider:           "claude",
			existingMetadata:   map[string]any{"account_uuid": "acc-1", "organization_uuid": "org-1", "email": "user@example.com"},
			existingAttributes: map[string]string{AttributeAPIKey: "sk-ant-oat-1"},
			incomingAttributes: map[string]string{AttributeAPIKey: "sk-ant-oat-1"},
			wantKept:           true,
		},
		{
			name:               "runtime identity on one side with new key",
			provider:           "claude",
			existingMetadata:   map[string]any{"account_uuid": "acc-1", "organization_uuid": "org-1", "email": "user@example.com"},
			existingAttributes: map[string]string{AttributeAPIKey: "sk-ant-oat-1"},
			incomingAttributes: map[string]string{AttributeAPIKey: "sk-ant-oat-2"},
		},
		{
			name:     "organization on one side only",
			provider: "claude",
			existingMetadata: map[string]any{
				"access_token": "access-1", "organization_uuid": "org-1", "email": "user@example.com",
			},
			incomingMetadata: map[string]any{"access_token": "access-2", "email": "user@example.com"},
			wantKept:         true,
		},
		{
			name:     "codex token refresh",
			provider: "codex",
			existingMetadata: map[string]any{
				"type": "codex", "access_token": "access-1", "refresh_token": "refresh-1", "id_token": "id-1",
				"account_id": "acct-1", "email": "user@example.com",
			},
			incomingMetadata: map[string]any{
				"type": "codex", "access_token": "access-2", "refresh_token": "refresh-2", "id_token": "id-2",
				"account_id": "acct-1", "email": "user@example.com",
			},
			wantKept: true,
		},
		{
			// CredentialsChanged does not compare session_token, and the identity is one-sided.
			name:             "devin session token replaced",
			provider:         "devin",
			existingMetadata: map[string]any{"email": "a@example.com", "org_id": "org-a", "session_token": "session-a"},
			incomingMetadata: map[string]any{"session_token": "session-b"},
		},
		{
			name:             "devin session token unchanged",
			provider:         "devin",
			existingMetadata: map[string]any{"session_token": "session-a"},
			incomingMetadata: map[string]any{"session_token": "session-a"},
			wantKept:         true,
		},
		{
			name:             "no credential and no identity",
			provider:         "devin",
			existingMetadata: map[string]any{"type": "devin"},
			incomingMetadata: map[string]any{"type": "devin"},
		},
		{
			name:             "devin same email with new session token",
			provider:         "devin",
			existingMetadata: map[string]any{"email": "a@example.com", "session_token": "session-a"},
			incomingMetadata: map[string]any{"email": "a@example.com", "session_token": "session-b"},
			wantKept:         true,
		},
		{
			name:               "api key unchanged",
			provider:           "claude",
			existingAttributes: map[string]string{AttributeAPIKey: "sk-1"},
			incomingAttributes: map[string]string{AttributeAPIKey: "sk-1"},
			wantKept:           true,
		},
		{
			name:               "api key changed",
			provider:           "claude",
			existingAttributes: map[string]string{AttributeAPIKey: "sk-1"},
			incomingAttributes: map[string]string{AttributeAPIKey: "sk-2"},
		},
		{
			name:             "provider changed",
			provider:         "claude",
			existingMetadata: map[string]any{"email": "user@example.com"},
			incomingProvider: "codex",
			incomingMetadata: map[string]any{"email": "user@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			incomingProvider := tt.incomingProvider
			if incomingProvider == "" {
				incomingProvider = tt.provider
			}
			existing := &Auth{
				ID:         "reload-quota",
				Provider:   tt.provider,
				Status:     StatusActive,
				Metadata:   tt.existingMetadata,
				Attributes: tt.existingAttributes,
				Quota:      reloadQuotaSnapshot(reloadQuotaObservedAt, "0.95"),
			}
			incoming := &Auth{
				ID:         "reload-quota",
				Provider:   incomingProvider,
				Status:     StatusActive,
				Metadata:   tt.incomingMetadata,
				Attributes: tt.incomingAttributes,
			}
			updated := updateAfterReload(t, existing, incoming)
			if !tt.wantKept {
				if len(updated.Quota.Signals) != 0 || !updated.Quota.ObservedAt.IsZero() {
					t.Fatalf("quota snapshot = %+v, want cleared", updated.Quota)
				}
				return
			}
			if !updated.Quota.ObservedAt.Equal(reloadQuotaObservedAt) {
				t.Fatalf("ObservedAt = %v, want %v", updated.Quota.ObservedAt, reloadQuotaObservedAt)
			}
			if got := updated.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"]; got != "0.95" {
				t.Fatalf("5h utilization = %q, want 0.95 (signals %v)", got, updated.Quota.Signals)
			}
		})
	}
}

func TestManagerUpdateKeepsNewerIncomingQuotaSnapshot(t *testing.T) {
	t.Parallel()
	newer := reloadQuotaObservedAt.Add(time.Minute)
	updated := updateAfterReload(t, &Auth{
		ID:       "reload-quota-newer",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
		Quota:    reloadQuotaSnapshot(reloadQuotaObservedAt, "0.95"),
	}, &Auth{
		ID:       "reload-quota-newer",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: claudeReloadMetadata("2", "acc-1", "org-1", "user@example.com"),
		Quota:    reloadQuotaSnapshot(newer, "0.10"),
	})
	if !updated.Quota.ObservedAt.Equal(newer) {
		t.Fatalf("ObservedAt = %v, want %v", updated.Quota.ObservedAt, newer)
	}
	if got := updated.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"]; got != "0.10" {
		t.Fatalf("5h utilization = %q, want the newer 0.10", got)
	}
}

// The carry-over copies only cooldown fields; the snapshot follows the account rules.
func TestManagerUpdateCarriesActiveCredentialQuotaCooldown(t *testing.T) {
	t.Parallel()
	newer := reloadQuotaObservedAt.Add(time.Minute)
	tests := []struct {
		name           string
		incomingMeta   map[string]any
		incomingQuota  QuotaState
		wantObservedAt time.Time
		wantFiveHour   string
	}{
		{
			name:           "same account without snapshot",
			incomingMeta:   claudeReloadMetadata("2", "acc-1", "org-1", "user@example.com"),
			wantObservedAt: reloadQuotaObservedAt,
			wantFiveHour:   "0.95",
		},
		{
			name:           "same account with newer snapshot",
			incomingMeta:   claudeReloadMetadata("2", "acc-1", "org-1", "user@example.com"),
			incomingQuota:  reloadQuotaSnapshot(newer, "0.10"),
			wantObservedAt: newer,
			wantFiveHour:   "0.10",
		},
		{
			name:         "other organization",
			incomingMeta: claudeReloadMetadata("2", "acc-1", "org-2", "user@example.com"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recoverAt := time.Now().Add(2 * time.Hour)
			quota := reloadQuotaSnapshot(reloadQuotaObservedAt, "0.95")
			quota.Exceeded = true
			quota.Reason = "credential_quota"
			quota.NextRecoverAt = recoverAt
			quota.BackoffLevel = 2
			updated := updateAfterReload(t, &Auth{
				ID:             "reload-quota-cooldown",
				Provider:       "claude",
				Status:         StatusActive,
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Metadata:       claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
				Quota:          quota,
			}, &Auth{
				ID:       "reload-quota-cooldown",
				Provider: "claude",
				Status:   StatusActive,
				Metadata: tt.incomingMeta,
				Quota:    tt.incomingQuota,
			})
			if !updated.Unavailable || !updated.NextRetryAfter.Equal(recoverAt) {
				t.Fatalf("Unavailable = %v, NextRetryAfter = %v, want cooldown until %v", updated.Unavailable, updated.NextRetryAfter, recoverAt)
			}
			got := updated.Quota
			if !got.Exceeded || got.Reason != "credential_quota" || !got.NextRecoverAt.Equal(recoverAt) || got.BackoffLevel != 2 {
				t.Fatalf("quota = %+v, want the active credential_quota cooldown", got)
			}
			if tt.wantObservedAt.IsZero() {
				if len(got.Signals) != 0 || !got.ObservedAt.IsZero() {
					t.Fatalf("quota snapshot = %+v, want cleared", got)
				}
				return
			}
			if !got.ObservedAt.Equal(tt.wantObservedAt) || got.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != tt.wantFiveHour {
				t.Fatalf("quota snapshot = %+v, want ObservedAt %v and 5h %s", got, tt.wantObservedAt, tt.wantFiveHour)
			}
		})
	}
}

func TestManagerUpdateQuotaMergeDoesNotResurrectCooldown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		reason    string
		recoverAt time.Time
	}{
		{name: "other reason", reason: "quota", recoverAt: time.Now().Add(2 * time.Hour)},
		{name: "expired credential quota", reason: "credential_quota", recoverAt: time.Now().Add(-2 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			quota := reloadQuotaSnapshot(reloadQuotaObservedAt, "0.95")
			quota.Exceeded = true
			quota.Reason = tt.reason
			quota.NextRecoverAt = tt.recoverAt
			quota.BackoffLevel = 3
			updated := updateAfterReload(t, &Auth{
				ID:       "reload-quota-no-resurrect",
				Provider: "claude",
				Status:   StatusActive,
				Metadata: claudeReloadMetadata("1", "acc-1", "org-1", "user@example.com"),
				Quota:    quota,
			}, &Auth{
				ID:       "reload-quota-no-resurrect",
				Provider: "claude",
				Status:   StatusActive,
				Metadata: claudeReloadMetadata("2", "acc-1", "org-1", "user@example.com"),
			})
			got := updated.Quota
			if got.Exceeded || got.Reason != "" || !got.NextRecoverAt.IsZero() || got.BackoffLevel != 0 {
				t.Fatalf("quota = %+v, want no cooldown fields", got)
			}
			if !got.ObservedAt.Equal(reloadQuotaObservedAt) || got.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != "0.95" {
				t.Fatalf("quota snapshot = %+v, want it kept", got)
			}
		})
	}
}

// lineageClaudeAuth is a Claude OAuth credential without identity metadata, as the auth file
// of a setup token holds it, with a passive quota snapshot.
func lineageClaudeAuth(id, token string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "claude", "access_token": "access-" + token, "refresh_token": "refresh-" + token},
		Quota:    reloadQuotaSnapshot(reloadQuotaObservedAt, "0.95"),
	}
}

// withTokens returns a clone of auth whose tokens are rotated to token.
func withTokens(auth *Auth, token string) *Auth {
	clone := auth.Clone()
	clone.Metadata["access_token"] = "access-" + token
	clone.Metadata["refresh_token"] = "refresh-" + token
	return clone
}

func registerForLineage(t *testing.T, manager *Manager, auth *Auth) *Auth {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return currentForLineage(t, manager, auth.ID)
}

func currentForLineage(t *testing.T, manager *Manager, id string) *Auth {
	t.Helper()
	current, ok := manager.GetByID(id)
	if !ok || current == nil {
		t.Fatalf("auth %s missing", id)
	}
	return current
}

// Quota data taken for an auth without identity metadata (a usage reading keeps a clone of
// the auth it read) still describes it after the manager's own token refresh and request
// preparation, though its tokens changed.
func TestManagerOwnChangesKeepTheQuotaAccountOfAnAuthWithoutIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	stored := registerForLineage(t, manager, lineageClaudeAuth("lineage-own", "1"))

	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, stored, withTokens(stored, "2")); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	refreshed := currentForLineage(t, manager, stored.ID)
	if authAccessToken(refreshed) != "access-2" {
		t.Fatalf("access token = %q, want the refreshed one", authAccessToken(refreshed))
	}
	if !SameQuotaAccount(stored, refreshed) {
		t.Fatal("a clone taken before the manager's refresh must describe the refreshed auth")
	}

	// Request preparation synthesizes an account_uuid.
	prepared := refreshed.Clone()
	prepared.Metadata["account_uuid"] = "synthetic-1"
	if _, errUpdate := manager.UpdatePreparedAuth(ctx, refreshed, prepared); errUpdate != nil {
		t.Fatalf("UpdatePreparedAuth() error = %v", errUpdate)
	}
	current := currentForLineage(t, manager, stored.ID)
	if !SameQuotaAccount(stored, current) || !SameQuotaAccount(refreshed, current) {
		t.Fatal("clones taken before the manager's request preparation must describe the prepared auth")
	}
	if !current.Quota.ObservedAt.Equal(reloadQuotaObservedAt) {
		t.Fatalf("quota snapshot = %+v, want it kept", current.Quota)
	}
}

// A management edit replaces the auth with a clone of the manager's auth. A clone whose
// tokens were changed carries the old quota lineage, which must not prove the account: the
// passive snapshot is dropped as before, and quota data taken before the edit, even after
// the manager's own refresh, no longer describes the auth.
func TestManagerReplaceWithAnEditedCloneDoesNotKeepTheQuotaAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	stored := registerForLineage(t, manager, lineageClaudeAuth("lineage-edit", "1"))
	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, stored, withTokens(stored, "2")); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	refreshed := currentForLineage(t, manager, stored.ID)
	if !SameQuotaAccount(stored, refreshed) {
		t.Fatal("a clone taken before the manager's refresh must describe the refreshed auth")
	}

	// An edit of other fields keeps the account.
	renamed := refreshed.Clone()
	renamed.Prefix = "team"
	if _, errUpdate := manager.Update(ctx, renamed); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if current := currentForLineage(t, manager, stored.ID); !SameQuotaAccount(stored, current) {
		t.Fatal("an edit that keeps the credentials must keep the account")
	}

	// An edit of the tokens may hold another account. The clone's own snapshot is cleared
	// so that only the carry-over decides the snapshot.
	edited := withTokens(currentForLineage(t, manager, stored.ID), "3")
	edited.Quota = QuotaState{}
	if _, errUpdate := manager.Update(ctx, edited); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	current := currentForLineage(t, manager, stored.ID)
	if len(current.Quota.Signals) != 0 || !current.Quota.ObservedAt.IsZero() {
		t.Fatalf("quota snapshot = %+v, want cleared", current.Quota)
	}
	if SameQuotaAccount(stored, current) || SameQuotaAccount(refreshed, current) {
		t.Fatal("quota data taken before the token edit must not describe the edited auth")
	}
}
