package auth

import (
	"context"
	"testing"
)

func TestManagerQuotaLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	first := registerForLineage(t, manager, lineageClaudeAuth("lineage-a", "1"))
	other := registerForLineage(t, manager, lineageClaudeAuth("lineage-b", "1"))
	if first.quotaLineage == 0 || other.quotaLineage == 0 || first.quotaLineage == other.quotaLineage {
		t.Fatalf("lineages = %d, %d, want distinct non-zero lineages for new auths", first.quotaLineage, other.quotaLineage)
	}
	lineage := first.quotaLineage
	expect := func(step string, kept bool) *Auth {
		t.Helper()
		current := currentForLineage(t, manager, first.ID)
		if current.quotaLineage == 0 {
			t.Fatalf("%s: lineage is zero", step)
		}
		if kept && current.quotaLineage != lineage {
			t.Fatalf("%s: lineage = %d, want %d kept", step, current.quotaLineage, lineage)
		}
		if !kept && current.quotaLineage == lineage {
			t.Fatalf("%s: lineage %d kept, want a new one", step, lineage)
		}
		lineage = current.quotaLineage
		return current
	}

	current := first
	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, current, withTokens(current, "2")); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	current = expect("refresh", true)

	prepared := current.Clone()
	prepared.Metadata["account_uuid"] = "synthetic-1"
	if _, errUpdate := manager.UpdatePreparedAuth(ctx, current, prepared); errUpdate != nil {
		t.Fatalf("UpdatePreparedAuth() error = %v", errUpdate)
	}
	current = expect("prepare", true)

	// The watcher reloads the file the refresh wrote: unchanged credentials prove the account.
	reloaded := lineageClaudeAuth(first.ID, "2")
	reloaded.Quota = QuotaState{}
	if _, errUpdate := manager.Update(ctx, reloaded); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	current = expect("reload with the same tokens", true)

	// A replace with other tokens and no identity gets a new lineage, even from a clone that
	// carries the current one.
	if _, errUpdate := manager.Update(ctx, withTokens(current, "3")); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	current = expect("replace with other tokens", false)

	// A refresh whose base predates such a replace mixes two accounts' data.
	base := current.Clone()
	if _, errUpdate := manager.Update(ctx, withTokens(current, "4")); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	current = expect("replace during a refresh", false)
	if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, withTokens(base, "5")); errUpdate != nil {
		t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
	}
	current = expect("refresh from a replaced base", false)
	if current.quotaLineage == base.quotaLineage {
		t.Fatalf("lineage %d of the replaced base was restored", base.quotaLineage)
	}

	// Register over an existing ID follows the replace rule.
	if _, errRegister := manager.Register(ctx, withTokens(current, "5")); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	current = expect("register with the same tokens", true)
	if _, errRegister := manager.Register(ctx, withTokens(current, "6")); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	current = expect("register with other tokens", false)

	// A reload that gains identity with the same tokens keeps the lineage.
	identified := withTokens(current, "6")
	identified.Metadata["organization_uuid"] = "org-1"
	identified.Metadata["email"] = "user@example.com"
	if _, errUpdate := manager.Update(ctx, identified); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	current = expect("replace that gains identity", true)

	// A replace that drops the organization with other tokens may hold another organization
	// of the same user, though the email matches.
	dropped := withTokens(current, "7")
	delete(dropped.Metadata, "organization_uuid")
	if _, errUpdate := manager.Update(ctx, dropped); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	expect("replace that drops the organization with other tokens", false)
}

func TestManagerLoadAssignsQuotaLineages(t *testing.T) {
	t.Parallel()
	store := &weightValidationStore{auths: []*Auth{lineageClaudeAuth("load-a", "1"), lineageClaudeAuth("load-b", "1")}}
	manager := NewManager(store, nil, nil)
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	a := currentForLineage(t, manager, "load-a")
	b := currentForLineage(t, manager, "load-b")
	if a.quotaLineage == 0 || b.quotaLineage == 0 || a.quotaLineage == b.quotaLineage {
		t.Fatalf("lineages = %d, %d, want distinct non-zero lineages", a.quotaLineage, b.quotaLineage)
	}

	// A reload keeps the lineage of an unchanged auth and replaces that of a changed one.
	store.auths = []*Auth{lineageClaudeAuth("load-a", "1"), lineageClaudeAuth("load-b", "2")}
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if got := currentForLineage(t, manager, "load-a").quotaLineage; got != a.quotaLineage {
		t.Fatalf("unchanged auth lineage = %d, want %d", got, a.quotaLineage)
	}
	if got := currentForLineage(t, manager, "load-b").quotaLineage; got == 0 || got == b.quotaLineage {
		t.Fatalf("changed auth lineage = %d, want a new one (was %d)", got, b.quotaLineage)
	}
}

func TestSameQuotaAccountByLineage(t *testing.T) {
	t.Parallel()
	claude := func(lineage uint64, token string, metadata map[string]any) *Auth {
		auth := lineageClaudeAuth("lineage-rule", token)
		auth.quotaLineage = lineage
		for key, value := range metadata {
			auth.Metadata[key] = value
		}
		return auth
	}
	codex := claude(7, "2", nil)
	codex.Provider = "codex"
	tests := []struct {
		name     string
		existing *Auth
		incoming *Auth
		want     bool
	}{
		{name: "same lineage with other tokens", existing: claude(7, "1", nil), incoming: claude(7, "2", nil), want: true},
		{
			name:     "same lineage with one-sided identity",
			existing: claude(7, "1", nil),
			incoming: claude(7, "2", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			want:     true,
		},
		{
			name:     "same lineage with conflicting identity",
			existing: claude(7, "1", map[string]any{"email": "a@example.com"}),
			incoming: claude(7, "2", map[string]any{"email": "b@example.com"}),
		},
		{
			name:     "same lineage with conflicting account uuid",
			existing: claude(7, "1", map[string]any{"account_uuid": "synthetic-1"}),
			incoming: claude(7, "2", map[string]any{"account_uuid": "synthetic-2"}),
		},
		{name: "same lineage with another provider", existing: claude(7, "1", nil), incoming: codex},
		{name: "no lineage with other tokens", existing: claude(0, "1", nil), incoming: claude(0, "2", nil)},
		{name: "other lineage with other tokens", existing: claude(7, "1", nil), incoming: claude(8, "2", nil)},
		{name: "other lineage with unchanged tokens", existing: claude(7, "1", nil), incoming: claude(8, "1", nil), want: true},
		{
			name:     "other lineage with proven identity",
			existing: claude(7, "1", map[string]any{"organization_uuid": "org-1"}),
			incoming: claude(8, "2", map[string]any{"organization_uuid": "org-1"}),
			want:     true,
		},
		{
			name:     "other lineage with a dropped organization and other tokens",
			existing: claude(7, "1", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			incoming: claude(8, "2", map[string]any{"email": "a@example.com"}),
		},
		{
			name:     "other lineage with a dropped organization and unchanged tokens",
			existing: claude(7, "1", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			incoming: claude(8, "1", map[string]any{"email": "a@example.com"}),
			want:     true,
		},
		{
			name:     "other lineage with a gained organization and other tokens",
			existing: claude(7, "1", map[string]any{"email": "a@example.com"}),
			incoming: claude(8, "2", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			want:     true,
		},
		{
			name:     "other lineage with a dropped account uuid and other tokens",
			existing: claude(7, "1", map[string]any{"account_uuid": "acc-1", "email": "a@example.com"}),
			incoming: claude(8, "2", map[string]any{"email": "a@example.com"}),
			want:     true,
		},
		{
			// The Manager keeps a lineage across a replace only when the content rule proved it.
			name:     "same lineage with a dropped organization",
			existing: claude(7, "1", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			incoming: claude(7, "2", map[string]any{"email": "a@example.com"}),
			want:     true,
		},
		{
			name:     "dropped organization with conflicting email",
			existing: claude(7, "1", map[string]any{"organization_uuid": "org-1", "email": "a@example.com"}),
			incoming: claude(7, "1", map[string]any{"email": "b@example.com"}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sameQuotaAccount(tt.existing, tt.incoming); got != tt.want {
				t.Fatalf("sameQuotaAccount() = %v, want %v", got, tt.want)
			}
			if sameQuotaAccountByIdentity(tt.existing, tt.incoming) && !tt.want {
				t.Fatal("sameQuotaAccountByIdentity() proves what sameQuotaAccount rejects")
			}
		})
	}
}
