package auth

import (
	"context"
	"testing"
)

// blockingMintPreparer mints a Meta API key for the auth it receives once released.
type blockingMintPreparer struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingMintPreparer) ShouldPrepareRequestAuth(auth *Auth) bool {
	return authMetadataString(auth, "api_key") == ""
}

func (p *blockingMintPreparer) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	close(p.started)
	<-p.release
	auth.Metadata["api_key"] = "minted-for-" + authMetadataString(auth, "dca_token")
	return auth, nil
}

func staleMintMetaAuth(dcaToken, email string) *Auth {
	return &Auth{
		ID:       "stale-mint-meta",
		Provider: "meta",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "meta", "dca_token": dcaToken, "email": email},
	}
}

// A preparation that minted a key for the account it started with must not install that key
// over another account's credentials that replaced the auth meanwhile. A replace that keeps
// the account keeps the minted key.
func TestManagerPrepareRequestAuthIgnoresMintForReplacedAccount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replacement *Auth
		wantAPIKey  string
	}{
		{
			// The replacement has no key yet: A's key must not be grafted onto it.
			name:        "another account",
			replacement: staleMintMetaAuth("dca-b", "b@example.com"),
			wantAPIKey:  "",
		},
		{
			name: "same account",
			replacement: func() *Auth {
				// Other credentials bump the credential version, the email keeps the account.
				auth := staleMintMetaAuth("dca-a", "a@example.com")
				auth.Metadata["access_token"] = "access-a-2"
				return auth
			}(),
			wantAPIKey: "minted-for-dca-a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newMemoryAuthTestStore()
			manager := NewManager(store, nil, nil)
			registered, errRegister := manager.Register(ctx, staleMintMetaAuth("dca-a", "a@example.com"))
			if errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			preparer := &blockingMintPreparer{started: make(chan struct{}), release: make(chan struct{})}
			type prepareResult struct {
				auth *Auth
				err  error
			}
			done := make(chan prepareResult, 1)
			go func() {
				prepared, errPrepare := manager.PrepareRequestAuth(ctx, preparer, registered)
				done <- prepareResult{auth: prepared, err: errPrepare}
			}()
			<-preparer.started

			if _, errUpdate := manager.Update(ctx, tc.replacement.Clone()); errUpdate != nil {
				t.Fatalf("Update() error = %v", errUpdate)
			}
			close(preparer.release)
			result := <-done
			if result.err != nil {
				t.Fatalf("PrepareRequestAuth() error = %v", result.err)
			}
			if got := authMetadataString(result.auth, "api_key"); got != tc.wantAPIKey {
				t.Fatalf("returned api_key = %q, want %q", got, tc.wantAPIKey)
			}
			current := currentForLineage(t, manager, registered.ID)
			if got := authMetadataString(current, "api_key"); got != tc.wantAPIKey {
				t.Fatalf("api_key = %q, want %q", got, tc.wantAPIKey)
			}
			if got := authMetadataString(current, "dca_token"); got != authMetadataString(tc.replacement, "dca_token") {
				t.Fatalf("dca_token = %q, want the replacement's", got)
			}
			store.mu.Lock()
			persisted := store.auths[registered.ID].Clone()
			store.mu.Unlock()
			if got := authMetadataString(persisted, "api_key"); got != tc.wantAPIKey {
				t.Fatalf("persisted api_key = %q, want %q", got, tc.wantAPIKey)
			}
		})
	}
}
