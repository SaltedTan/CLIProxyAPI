package clientusage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var (
	accountA = map[string]any{"organization_uuid": "org-a", "email": "a@example.com"}
	accountB = map[string]any{"organization_uuid": "org-b", "email": "b@example.com"}
)

// accountAuth returns a Claude OAuth credential with the given identity metadata.
func accountAuth(id string, identity map[string]any) *coreauth.Auth {
	metadata := map[string]any{"type": "claude", "access_token": "token-secret-" + id}
	for key, value := range identity {
		metadata[key] = value
	}
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: metadata}
}

// claudeAccounts resolves credentials from the auth currently registered under each ID,
// as the service resolves them from the auth manager.
type claudeAccounts map[string]*coreauth.Auth

func (a claudeAccounts) resolve(authID string) (CredentialInfo, bool) {
	auth, ok := a[authID]
	if !ok {
		return CredentialInfo{}, false
	}
	return CredentialInfoFromAuth(auth), true
}

// expectClaudeQuota checks the saved weekly reading ClaudeQuota returns for auth.
func expectClaudeQuota(t *testing.T, tracker *Tracker, name string, auth *coreauth.Auth, used float64, ok bool) {
	t.Helper()
	reading, got := tracker.ClaudeQuota(auth)
	if got != ok {
		t.Fatalf("%s: ClaudeQuota() ok = %v, want %v (reading %+v)", name, got, ok, reading)
	}
	if ok {
		if reading.Weekly == nil {
			t.Fatalf("%s: ClaudeQuota() has no weekly window", name)
		}
		approx(t, name+" weekly used", reading.Weekly.Used, used)
	}
}

// writeClaudeState writes a state file holding only the given Claude credentials.
func writeClaudeState(t *testing.T, path string, credentials map[string]map[string]any) {
	t.Helper()
	data, errMarshal := json.Marshal(map[string]any{
		"version":            stateVersion,
		"since":              testNow,
		"saved_at":           testNow,
		"keys":               map[string]any{},
		"claude_credentials": credentials,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func savedClaudeReading(utilization float64, resetAt time.Time) map[string]any {
	return map[string]any{"epoch": 1, "reset_at": resetAt, "utilization": utilization, "observed_at": testNow}
}

// savedClaudeAccounts returns the raw "account" field of each saved Claude credential,
// nil when absent.
func savedClaudeAccounts(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var state struct {
		ClaudeCredentials map[string]map[string]json.RawMessage `json:"claude_credentials"`
	}
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	accounts := make(map[string]json.RawMessage, len(state.ClaudeCredentials))
	for authID, credential := range state.ClaudeCredentials {
		accounts[authID] = credential["account"]
	}
	return accounts
}

func savedAccountKeys(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var account struct {
		Keys map[string]string `json:"keys"`
	}
	if errUnmarshal := json.Unmarshal(raw, &account); errUnmarshal != nil {
		t.Fatalf("account %s: %v", raw, errUnmarshal)
	}
	return account.Keys
}

func TestClaudeQuotaIsOnlyForTheAccountItWasTakenFor(t *testing.T) {
	now := testNow
	path := filepath.Join(t.TempDir(), StateFileName)
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	accounts := claudeAccounts{"claude-1": accountAuth("claude-1", accountA)}
	tracker.SetCredentialResolver(accounts.resolve)
	seq := &claudeSeq{tracker: tracker, now: &now}
	seq.send("key-a", "claude-1", claudeObs{0.30, testNow.Add(72 * time.Hour)}, breakdown(100, 0, 0, 0, 0))

	// The auth file is replaced by another account under the same name, so the same ID.
	accounts["claude-1"] = accountAuth("claude-1", accountB)
	expectClaudeQuota(t, tracker, "account B", accounts["claude-1"], 0, false)
	expectClaudeQuota(t, tracker, "account A", accountAuth("claude-1", accountA), 0.30, true)

	// The account is saved with the reading.
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	restarted := newTestTracker(&now)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	expectClaudeQuota(t, restarted, "account B after a restart", accounts["claude-1"], 0, false)
	expectClaudeQuota(t, restarted, "account A after a restart", accountAuth("claude-1", accountA), 0.30, true)
}

func TestClaudeQuotaAcceptsLegacyStateUntilTheNextObservation(t *testing.T) {
	now := testNow.Add(time.Minute)
	path := filepath.Join(t.TempDir(), StateFileName)
	resetAt := testNow.Add(72 * time.Hour)
	writeClaudeState(t, path, map[string]map[string]any{"claude-1": savedClaudeReading(0.25, resetAt)})
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	// Saved by a version that recorded no account: used as before, for any account.
	expectClaudeQuota(t, tracker, "legacy as account A", accountAuth("claude-1", accountA), 0.25, true)
	expectClaudeQuota(t, tracker, "legacy as account B", accountAuth("claude-1", accountB), 0.25, true)

	accounts := claudeAccounts{"claude-1": accountAuth("claude-1", accountA)}
	tracker.SetCredentialResolver(accounts.resolve)
	seq := &claudeSeq{tracker: tracker, now: &now}
	seq.send("key-a", "claude-1", claudeObs{0.30, resetAt}, breakdown(100, 0, 0, 0, 0))
	expectClaudeQuota(t, tracker, "account A after its observation", accounts["claude-1"], 0.30, true)
	expectClaudeQuota(t, tracker, "account B after A's observation", accountAuth("claude-1", accountB), 0, false)
	// The legacy baseline is still compared with the observation.
	approx(t, "unattributed", tracker.Snapshot(SnapshotOptions{}).ClaudeCredentials[0].UnattributedCurrentFraction, 0.05)

	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	want := map[string]string{"organization_uuid": "org-a", "email": "a@example.com"}
	if got := savedAccountKeys(t, savedClaudeAccounts(t, path)["claude-1"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("saved account keys = %v, want %v", got, want)
	}
}

func TestClaudeAccountStatesSurviveARestart(t *testing.T) {
	now := testNow.Add(time.Minute)
	path := filepath.Join(t.TempDir(), StateFileName)
	resetAt := testNow.Add(72 * time.Hour)
	withoutIdentity := savedClaudeReading(0.20, resetAt)
	withoutIdentity["account"] = map[string]any{}
	identified := savedClaudeReading(0.30, resetAt)
	identified["account"] = map[string]any{"keys": map[string]string{"organization_uuid": "org-a", "email": "a@example.com"}}
	writeClaudeState(t, path, map[string]map[string]any{
		"legacy":           savedClaudeReading(0.10, resetAt),
		"without-identity": withoutIdentity,
		"identified":       identified,
	})
	check := func(step string, tracker *Tracker) {
		t.Helper()
		expectClaudeQuota(t, tracker, step+": legacy as account B", accountAuth("legacy", accountB), 0.10, true)
		expectClaudeQuota(t, tracker, step+": without identity as itself", accountAuth("without-identity", nil), 0, false)
		expectClaudeQuota(t, tracker, step+": without identity as account A", accountAuth("without-identity", accountA), 0, false)
		expectClaudeQuota(t, tracker, step+": identified as account A", accountAuth("identified", accountA), 0.30, true)
		expectClaudeQuota(t, tracker, step+": identified as account B", accountAuth("identified", accountB), 0, false)
	}

	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	check("loaded", tracker)
	// Any change saves the whole state.
	tracker.HandleUsage(context.Background(), record("k", "m", breakdown(1, 0, 0, 1, 0)))
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	saved := savedClaudeAccounts(t, path)
	if saved["legacy"] != nil {
		t.Fatalf("legacy state saved with account %s, want none", saved["legacy"])
	}
	if saved["without-identity"] == nil || len(savedAccountKeys(t, saved["without-identity"])) != 0 {
		t.Fatalf("state without identity saved with account %s, want an account without keys", saved["without-identity"])
	}
	if got := savedAccountKeys(t, saved["identified"]); !reflect.DeepEqual(got, map[string]string{"organization_uuid": "org-a", "email": "a@example.com"}) {
		t.Fatalf("identified state saved with account keys %v", got)
	}
	restarted := newTestTracker(&now)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	check("restarted", restarted)
}

func TestClaudeAccountWithoutIdentityKeepsAttributing(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	// No identity metadata, as for a setup token or a refused profile lookup.
	accounts := claudeAccounts{"claude-1": accountAuth("claude-1", nil)}
	tracker.SetCredentialResolver(accounts.resolve)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)

	seq.send("key-a", "claude-1", claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.15, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.25, resetAt}, breakdown(100, 0, 0, 0, 0))
	expectClaudeQuota(t, tracker, "account without identity", accounts["claude-1"], 0, false)
	keyA := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	approx(t, "key A current", keyA.Credentials[0].CurrentFraction, 0.15)

	// The proxy fills the identity from the profile: the account is the same, and its
	// reading is used from its next observation.
	accounts["claude-1"] = accountAuth("claude-1", accountA)
	seq.send("key-a", "claude-1", claudeObs{0.30, resetAt}, breakdown(100, 0, 0, 0, 0))
	keyA = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	approx(t, "key A current after gaining identity", keyA.Credentials[0].CurrentFraction, 0.20)
	expectClaudeQuota(t, tracker, "account with its new identity", accounts["claude-1"], 0.30, true)
}

func TestClaudeReplacedAccountStartsANewBaseline(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	accounts := claudeAccounts{"claude-1": accountAuth("claude-1", accountA)}
	tracker.SetCredentialResolver(accounts.resolve)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetA := testNow.Add(72 * time.Hour)
	resetB := resetA.Add(24 * time.Hour)

	seq.send("key-a", "claude-1", claudeObs{0.10, resetA}, breakdown(100, 0, 0, 0, 0))
	// Usage from outside the proxy is unattributed.
	tracker.mu.Lock()
	tracker.claude["claude-1"].Pending = nil
	tracker.mu.Unlock()
	seq.send("key-a", "claude-1", claudeObs{0.15, resetA}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.30, resetA}, breakdown(100, 0, 0, 0, 0))
	tracker.mu.Lock()
	epoch := tracker.claude["claude-1"].Epoch
	pendingA := tracker.claude["claude-1"].Pending[KeyID("key-a")] != nil
	tracker.mu.Unlock()
	if !pendingA {
		t.Fatal("key A must have weight pending on account A")
	}

	// The auth file is replaced by account B, whose first response reports its own usage.
	accounts["claude-1"] = accountAuth("claude-1", accountB)
	seq.send("key-b", "claude-1", claudeObs{0.70, resetB}, breakdown(100, 0, 0, 0, 0))
	observedAt := now

	snapshot := tracker.Snapshot(SnapshotOptions{})
	keyA := findKey(t, snapshot, KeyID("key-a")).Claude
	approx(t, "key A current", keyA.Credentials[0].CurrentFraction, 0.15)
	approx(t, "key A total", keyA.Credentials[0].TotalFraction, 0.15)
	if keyB := findKey(t, snapshot, KeyID("key-b")).Claude; keyB != nil && len(keyB.Credentials) != 0 {
		t.Fatalf("key B was charged before account B's first increase: %+v", keyB.Credentials)
	}
	credential := snapshot.ClaudeCredentials[0]
	approx(t, "unattributed total", credential.UnattributedTotalFraction, 0.05)
	approx(t, "unattributed current", credential.UnattributedCurrentFraction, 0)
	approx(t, "utilization", credential.WeeklyUtilization, 0.70)
	if credential.WindowResetsAt == nil || !credential.WindowResetsAt.Equal(resetB) || credential.ObservedAt == nil || !credential.ObservedAt.Equal(observedAt) {
		t.Fatalf("credential window = %v observed %v, want %v observed %v", credential.WindowResetsAt, credential.ObservedAt, resetB, observedAt)
	}
	tracker.mu.Lock()
	state := tracker.claude["claude-1"]
	epochOK := state.Epoch == epoch+1 && state.EpochStartedAt.Equal(observedAt) && state.UtilizationSetAt.Equal(observedAt)
	pendingOK := len(state.Pending) == 1 && state.Pending[KeyID("key-b")] != nil
	gotEpoch := state.Epoch
	tracker.mu.Unlock()
	if !epochOK {
		t.Fatalf("epoch = %d, want a new epoch %d started at %v", gotEpoch, epoch+1, observedAt)
	}
	if !pendingOK {
		t.Fatal("pending weight must hold only key B's request to account B")
	}

	// Later increases of account B are attributed as usual.
	seq.send("key-b", "claude-1", claudeObs{0.75, resetB}, breakdown(100, 0, 0, 0, 0))
	snapshot = tracker.Snapshot(SnapshotOptions{})
	approx(t, "key B current", findKey(t, snapshot, KeyID("key-b")).Claude.Credentials[0].CurrentFraction, 0.05)
	approx(t, "key A current after B's increase", findKey(t, snapshot, KeyID("key-a")).Claude.Credentials[0].CurrentFraction, 0.15)
	expectClaudeQuota(t, tracker, "account B", accounts["claude-1"], 0.75, true)
	expectClaudeQuota(t, tracker, "account A", accountAuth("claude-1", accountA), 0, false)
}

func TestClaudeAccountGainingItsProfileKeepsAttributing(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	// The profile lookup was refused, so the proxy synthesized an account_uuid.
	accounts := claudeAccounts{"claude-1": accountAuth("claude-1", map[string]any{"account_uuid": "synthetic-1"})}
	tracker.SetCredentialResolver(accounts.resolve)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)

	seq.send("key-a", "claude-1", claudeObs{0.20, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.mu.Lock()
	epoch := tracker.claude["claude-1"].Epoch
	pendingA := tracker.claude["claude-1"].Pending[KeyID("key-a")] != nil
	tracker.mu.Unlock()
	if !pendingA {
		t.Fatal("key A must have weight pending")
	}

	// A refresh read the profile: the real account_uuid replaces the synthesized one, and
	// the account gains its proof keys. The account is the same.
	profile := map[string]any{"account_uuid": "real-1", "organization_uuid": "org-a", "email": "a@example.com"}
	accounts["claude-1"] = accountAuth("claude-1", profile)
	seq.send("key-a", "claude-1", claudeObs{0.25, resetAt}, breakdown(100, 0, 0, 0, 0))

	snapshot := tracker.Snapshot(SnapshotOptions{})
	keyA := findKey(t, snapshot, KeyID("key-a")).Claude
	if keyA == nil || len(keyA.Credentials) != 1 {
		t.Fatalf("key A was not charged the increase: %+v", keyA)
	}
	approx(t, "key A current", keyA.Credentials[0].CurrentFraction, 0.05)
	approx(t, "unattributed", snapshot.ClaudeCredentials[0].UnattributedTotalFraction, 0)
	approx(t, "utilization", snapshot.ClaudeCredentials[0].WeeklyUtilization, 0.25)
	tracker.mu.Lock()
	state := tracker.claude["claude-1"]
	gotEpoch := state.Epoch
	pendingOK := len(state.Pending) == 1 && state.Pending[KeyID("key-a")] != nil
	var keys map[string]string
	if state.Account != nil {
		keys = state.Account.Keys
	}
	tracker.mu.Unlock()
	if gotEpoch != epoch {
		t.Fatalf("epoch = %d, want %d kept", gotEpoch, epoch)
	}
	if !pendingOK {
		t.Fatal("pending weight must hold key A's latest request only")
	}
	want := map[string]string{"account_uuid": "real-1", "organization_uuid": "org-a", "email": "a@example.com"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("recorded account keys = %v, want %v", keys, want)
	}
	expectClaudeQuota(t, tracker, "account with its profile", accounts["claude-1"], 0.25, true)
}
