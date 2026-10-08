package clientusage

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newTestTracker(now *time.Time) *Tracker {
	tracker := NewTracker()
	tracker.location = time.UTC
	tracker.nowFunc = func() time.Time { return *now }
	return tracker
}

func breakdown(uncached, cacheRead, cacheWrite, output, reasoning int64) coreusage.TokenBreakdown {
	input := uncached + cacheRead + cacheWrite
	return coreusage.TokenBreakdown{
		SchemaVersion: coreusage.TokenAccountingSchemaVersion,
		Quality:       coreusage.TokenAccountingQualityComplete,
		TotalTokens:   input + output,
		Input:         coreusage.TokenInputBreakdown{TotalTokens: input, UncachedTokens: uncached, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite},
		Output:        coreusage.TokenOutputBreakdown{TotalTokens: output, NonReasoningTokens: output - reasoning, ReasoningTokens: reasoning},
	}
}

func record(apiKey, model string, tokens coreusage.TokenBreakdown) coreusage.Record {
	return coreusage.Record{
		Provider: "openai",
		Model:    model,
		APIKey:   apiKey,
		Detail:   coreusage.Detail{TokenBreakdown: tokens},
	}
}

// claudeObs describes the weekly quota headers of a Claude response.
type claudeObs struct {
	utilization float64
	resetAt     time.Time
}

// claudeRecord builds a Claude record for a request started at requestAt. A zero
// obs.resetAt omits the quota headers.
func claudeRecord(apiKey, authID string, requestAt time.Time, obs claudeObs, tokens coreusage.TokenBreakdown) coreusage.Record {
	rec := record(apiKey, "claude-sonnet-4-5", tokens)
	rec.Provider = "claude"
	rec.AuthID = authID
	rec.AuthIndex = "idx-" + authID
	rec.RequestedAt = requestAt
	if !obs.resetAt.IsZero() {
		rec.ResponseHeaders = http.Header{
			claudeUtilizationHeader: []string{strconv.FormatFloat(obs.utilization, 'f', -1, 64)},
			claudeResetHeader:       []string{strconv.FormatInt(obs.resetAt.Unix(), 10)},
		}
	}
	return rec
}

// claudeSeq feeds sequential Claude records: each request starts after the previous
// one was processed.
type claudeSeq struct {
	tracker *Tracker
	now     *time.Time
}

func (s *claudeSeq) send(apiKey, authID string, obs claudeObs, tokens coreusage.TokenBreakdown) {
	requestAt := s.now.Add(time.Second)
	*s.now = requestAt.Add(time.Second)
	s.tracker.HandleUsage(context.Background(), claudeRecord(apiKey, authID, requestAt, obs, tokens))
}

// sendAt processes a record for a request started at startedAt when the clock is at doneAt.
func sendAt(tracker *Tracker, now *time.Time, apiKey string, startedAt, doneAt time.Time, obs claudeObs) {
	*now = doneAt
	tracker.HandleUsage(context.Background(), claudeRecord(apiKey, "claude-1", startedAt, obs, breakdown(100, 0, 0, 0, 0)))
}

func findKey(t *testing.T, snapshot Snapshot, id string) KeyUsage {
	t.Helper()
	for _, key := range snapshot.Keys {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %s not in snapshot", id)
	return KeyUsage{}
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func maxPlan(string) (CredentialInfo, bool) {
	return CredentialInfo{Label: "Max", Plan: PlanMax20x, PlanProUnits: 10, PlanSource: PlanSourceRateLimitTier}, true
}

func TestTrackerAggregatesPerKeyModelAndDay(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	ctx := context.Background()

	tracker.HandleUsage(ctx, record("key-a", "gpt-5", breakdown(100, 20, 0, 50, 10)))
	// A failed attempt retried successfully counts as one request and one failure.
	failed := record("key-a", "gpt-5", breakdown(0, 0, 0, 0, 0))
	failed.Failed = true
	tracker.HandleUsage(ctx, failed)
	tracker.HandleUsage(ctx, record("key-a", "gpt-5", breakdown(10, 0, 0, 5, 0)))
	now = testNow.Add(24 * time.Hour)
	tracker.HandleUsage(ctx, record("key-a", "claude-opus", breakdown(1, 0, 0, 1, 0)))
	tracker.HandleUsage(ctx, record("", "gpt-5", breakdown(7, 0, 0, 3, 0)))

	snapshot := tracker.Snapshot(SnapshotOptions{
		APIKeys:     []string{"key-a", "key-unused-1234", "key-a"},
		APIKeyNames: map[string]string{"key-a": "Laptop", AnonymousKeyID: "No key"},
	})
	if len(snapshot.Keys) != 3 {
		t.Fatalf("keys = %d, want 3 (two configured + anonymous)", len(snapshot.Keys))
	}
	keyA := snapshot.Keys[0]
	if keyA.ID != KeyID("key-a") || keyA.Name != "Laptop" || !keyA.Configured || keyA.Key == "key-a" {
		t.Fatalf("unexpected key entry: %+v", keyA)
	}
	want := Counters{Requests: 3, Failed: 1, Tokens: Tokens{Input: 131, Output: 56, Reasoning: 10, CacheRead: 20, Total: 187}}
	if keyA.Totals != want {
		t.Fatalf("totals = %+v, want %+v", keyA.Totals, want)
	}
	if got := keyA.Models["gpt-5"]; got.Requests != 2 || got.Failed != 1 {
		t.Fatalf("gpt-5 counters = %+v", got)
	}
	if len(keyA.Daily) != 2 || keyA.Daily[0].Date != "2026-10-07" || keyA.Daily[1].Date != "2026-10-08" || keyA.Daily[1].Requests != 1 {
		t.Fatalf("daily = %+v", keyA.Daily)
	}
	if keyA.LastUsedAt == nil || !keyA.LastUsedAt.Equal(now) {
		t.Fatalf("last used = %v, want %v", keyA.LastUsedAt, now)
	}
	unused := snapshot.Keys[1]
	if !unused.Configured || unused.Totals != (Counters{}) || unused.Key != "key-...1234" {
		t.Fatalf("unused key entry: %+v", unused)
	}
	anonymous := snapshot.Keys[2]
	if anonymous.ID != AnonymousKeyID || anonymous.Configured || anonymous.Name != "No key" || anonymous.Totals.Tokens.Total != 10 {
		t.Fatalf("anonymous entry: %+v", anonymous)
	}
}

func TestTrackerReportsOnlyRecentDays(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.HandleUsage(context.Background(), record("key-a", "m", breakdown(1, 0, 0, 1, 0)))
	now = testNow.AddDate(0, 0, dailyRetentionDays)

	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if len(key.Daily) != 0 || key.Totals.Requests != 1 {
		t.Fatalf("an idle key must not report expired days: daily=%+v totals=%+v", key.Daily, key.Totals)
	}
	tracker.HandleUsage(context.Background(), record("key-a", "m", breakdown(1, 0, 0, 1, 0)))
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if len(key.Daily) != 1 || key.Daily[0].Date != now.Format(dateLayout) || key.Totals.Requests != 2 {
		t.Fatalf("daily = %+v totals = %+v", key.Daily, key.Totals)
	}
}

func TestClaudeWeeklyUsageIsAttributedByWeightedTokens(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.SetCredentialResolver(maxPlan)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)

	// Baseline: usage before the first observation is not attributed.
	seq.send("key-a", "claude-1", claudeObs{0.10, resetAt}, breakdown(1000, 0, 0, 0, 0))
	// Key B uses three times key A's weighted tokens before the next increase.
	seq.send("key-b", "claude-1", claudeObs{0.10, resetAt}, breakdown(2000, 0, 0, 200, 0))
	seq.send("key-a", "claude-1", claudeObs{0.18, resetAt}, breakdown(10, 0, 0, 0, 0))

	snapshot := tracker.Snapshot(SnapshotOptions{})
	// Pending weights at the increase: A = 1000+1, B = 2000+5*200+1 = 3001.
	keyA := findKey(t, snapshot, KeyID("key-a")).Claude
	keyB := findKey(t, snapshot, KeyID("key-b")).Claude
	approx(t, "key A fraction", keyA.Credentials[0].CurrentFraction, 0.08*1001/4002)
	approx(t, "key B fraction", keyB.Credentials[0].CurrentFraction, 0.08*3001/4002)
	approx(t, "key A pro units", keyA.CurrentProUnits, 0.8*1001/4002)
	approx(t, "key B total pro units", keyB.TotalProUnits, 0.8*3001/4002)
	if keyA.Credentials[0].Plan != PlanMax20x || keyA.WindowResetsAt == nil {
		t.Fatalf("credential ref = %+v", keyA.Credentials[0])
	}
	if len(snapshot.ClaudeCredentials) != 1 {
		t.Fatalf("claude credentials = %+v", snapshot.ClaudeCredentials)
	}
	credential := snapshot.ClaudeCredentials[0]
	approx(t, "utilization", credential.WeeklyUtilization, 0.18)
	// The resolver gave no index, so the one recorded from usage is reported.
	if credential.AuthIndex != "idx-claude-1" || credential.Label != "Max" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestClaudeWeeklyUsageOrderingAndUnattributedIncreases(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	resetAt := testNow.Add(72 * time.Hour)
	at := func(seconds float64) time.Time { return testNow.Add(time.Duration(seconds * float64(time.Second))) }
	send := func(started, done float64, obs claudeObs) {
		sendAt(tracker, &now, "key-a", at(started), at(done), obs)
	}

	send(1, 2, claudeObs{0.20, resetAt})
	send(3, 4, claudeObs{0.25, resetAt})
	// A late response to a request that started before the 0.25 reading was processed
	// carries an older value, even though the drop is large.
	send(3.5, 5, claudeObs{0.21, resetAt})
	// A record without quota headers only adds pending weight.
	send(6, 7, claudeObs{})
	send(8, 9, claudeObs{0.30, resetAt})
	// Usage observed with nothing pending came from outside the proxy.
	tracker.mu.Lock()
	tracker.claude["claude-1"].Pending = nil
	tracker.mu.Unlock()
	send(10, 11, claudeObs{0.34, resetAt})

	snapshot := tracker.Snapshot(SnapshotOptions{})
	keyA := findKey(t, snapshot, KeyID("key-a")).Claude
	approx(t, "key A fraction", keyA.Credentials[0].CurrentFraction, 0.10)
	approx(t, "unattributed", snapshot.ClaudeCredentials[0].UnattributedCurrentFraction, 0.04)
	// Credentials without a resolver are reported as unknown Pro-sized plans.
	if keyA.Credentials[0].Plan != PlanUnknown || keyA.Credentials[0].PlanProUnits != 1 || keyA.Credentials[0].AuthIndex != "idx-claude-1" {
		t.Fatalf("credential ref = %+v", keyA.Credentials[0])
	}

	// Small decreases are reporting lag, and records without a start time cannot
	// prove they are newer.
	send(12, 13, claudeObs{0.33, resetAt})
	sendAt(tracker, &now, "key-a", time.Time{}, at(14), claudeObs{0.05, resetAt})
	approx(t, "utilization after ignored drops", tracker.Snapshot(SnapshotOptions{}).ClaudeCredentials[0].WeeklyUtilization, 0.34)

	// A lower value from a request started after the last reading is a real drop
	// (limits reset or rescaled): it starts a new credential epoch from that baseline.
	// The key's usage keeps accumulating in its own window across the drop.
	send(15, 16, claudeObs{0.05, resetAt})
	send(17, 18, claudeObs{0.12, resetAt})
	snapshot = tracker.Snapshot(SnapshotOptions{})
	keyA = findKey(t, snapshot, KeyID("key-a")).Claude
	approx(t, "key A current after drop", keyA.Credentials[0].CurrentFraction, 0.17)
	approx(t, "key A total after drop", keyA.Credentials[0].TotalFraction, 0.17)
	approx(t, "utilization after drop", snapshot.ClaudeCredentials[0].WeeklyUtilization, 0.12)
}

func TestClaudeConcurrentResponsesAreNotDoubleCounted(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	resetAt := testNow.Add(72 * time.Hour)
	at := func(seconds float64) time.Time { return testNow.Add(time.Duration(seconds * float64(time.Second))) }

	sendAt(tracker, &now, "key-a", at(0), at(1), claudeObs{0.40, resetAt})
	for step := 0; step < 20; step++ {
		// Two overlapping requests are admitted upstream in the reverse of their
		// local start order: the later-started one reports the lower value.
		base := float64(10 + step*10)
		high := 0.40 + float64(step+1)*0.01
		sendAt(tracker, &now, "key-a", at(base), at(base+2), claudeObs{high, resetAt})
		sendAt(tracker, &now, "key-b", at(base+0.5), at(base+3), claudeObs{high - 0.03, resetAt})
	}

	snapshot := tracker.Snapshot(SnapshotOptions{})
	var attributed float64
	for _, key := range snapshot.Keys {
		attributed += key.Claude.Credentials[0].CurrentFraction
	}
	approx(t, "attributed", attributed, 0.20)
	approx(t, "utilization", snapshot.ClaudeCredentials[0].WeeklyUtilization, 0.60)
}

func TestClaudeResponsesStraddlingTheWeeklyReset(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	reset := testNow.Add(time.Hour)
	nextReset := reset.Add(claudeWeeklyWindow)

	sendAt(tracker, &now, "key-a", testNow, testNow.Add(time.Second), claudeObs{0.50, reset})
	// X started just before the reset but was admitted after it; Y started later but
	// was admitted before it and is processed last.
	sendAt(tracker, &now, "key-a", reset.Add(-50*time.Millisecond), reset.Add(time.Second), claudeObs{0.01, nextReset})
	sendAt(tracker, &now, "key-a", reset.Add(-10*time.Millisecond), reset.Add(2*time.Second), claudeObs{0.70, reset})
	sendAt(tracker, &now, "key-a", reset.Add(5*time.Second), reset.Add(6*time.Second), claudeObs{0.02, nextReset})

	snapshot := tracker.Snapshot(SnapshotOptions{})
	credential := snapshot.ClaudeCredentials[0]
	if credential.WindowResetsAt == nil || !credential.WindowResetsAt.Equal(nextReset) {
		t.Fatalf("window = %+v, want the new window", credential)
	}
	approx(t, "utilization", credential.WeeklyUtilization, 0.02)
	approx(t, "key A current", findKey(t, snapshot, KeyID("key-a")).Claude.Credentials[0].CurrentFraction, 0.02)
}

func TestClaudeLateResponsesCannotUndoADrop(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	resetAt := testNow.Add(72 * time.Hour)
	at := func(seconds float64) time.Time { return testNow.Add(time.Duration(seconds * float64(time.Second))) }

	sendAt(tracker, &now, "key-a", at(0), at(1), claudeObs{0.60, resetAt})
	// Anthropic resets weekly usage; a newer request reports the drop.
	sendAt(tracker, &now, "key-a", at(10), at(11), claudeObs{0.00, resetAt})
	// A long stream admitted before the reset finishes later with the old value.
	sendAt(tracker, &now, "key-b", at(5), at(600), claudeObs{0.60, resetAt.Add(2 * time.Hour)})
	sendAt(tracker, &now, "key-a", at(700), at(701), claudeObs{0.01, resetAt})

	snapshot := tracker.Snapshot(SnapshotOptions{})
	var current, total float64
	for _, key := range snapshot.Keys {
		if key.Claude != nil {
			current += key.Claude.Credentials[0].CurrentFraction
			total += key.Claude.Credentials[0].TotalFraction
		}
	}
	approx(t, "current", current, 0.01)
	approx(t, "total", total, 0.01)
	credential := snapshot.ClaudeCredentials[0]
	approx(t, "utilization", credential.WeeklyUtilization, 0.01)
	if credential.WindowResetsAt == nil || !credential.WindowResetsAt.Equal(resetAt) {
		t.Fatalf("a late response must not move the window: %+v", credential)
	}
}

func TestClaudeRecordWithoutStartTimeCannotSwallowRollover(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	reset := testNow.Add(time.Hour)
	nextReset := reset.Add(claudeWeeklyWindow)

	sendAt(tracker, &now, "key-a", testNow, testNow.Add(time.Second), claudeObs{0.50, reset})
	sendAt(tracker, &now, "key-a", time.Time{}, reset.Add(time.Minute), claudeObs{0.03, nextReset})
	sendAt(tracker, &now, "key-a", reset.Add(2*time.Minute), reset.Add(3*time.Minute), claudeObs{0.06, nextReset})

	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	approx(t, "new window usage", key.Credentials[0].CurrentFraction, 0.06)
}

func TestClaudeDropAfterRestartWithClockBehind(t *testing.T) {
	now := testNow.Add(time.Hour)
	path := filepath.Join(t.TempDir(), StateFileName)
	resetAt := testNow.Add(72 * time.Hour)
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	sendAt(tracker, &now, "key-a", now.Add(-time.Second), now, claudeObs{0.60, resetAt})
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}

	behind := testNow.Add(50 * time.Minute)
	restarted := newTestTracker(&behind)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	sendAt(restarted, &behind, "key-a", behind.Add(time.Minute), behind.Add(2*time.Minute), claudeObs{0.00, resetAt})
	approx(t, "utilization after drop", restarted.Snapshot(SnapshotOptions{}).ClaudeCredentials[0].WeeklyUtilization, 0)
}

func TestClaudeWeeklyWindowRollover(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	ctx := context.Background()
	firstReset := testNow.Add(2 * time.Hour)
	send := func(apiKey string, requestAt time.Time, obs claudeObs) {
		now = requestAt.Add(time.Second)
		tracker.HandleUsage(ctx, claudeRecord(apiKey, "claude-1", requestAt, obs, breakdown(100, 0, 0, 0, 0)))
	}

	send("key-a", testNow, claudeObs{0.50, firstReset})
	send("key-a", testNow.Add(time.Minute), claudeObs{0.60, firstReset})

	// After the credential's reset passes its window is closed, while the key's own
	// window, opened by its first request, still holds the key's usage.
	now = firstReset.Add(time.Minute)
	snapshot := tracker.Snapshot(SnapshotOptions{})
	keyA := findKey(t, snapshot, KeyID("key-a")).Claude
	approx(t, "current after the credential reset", keyA.CurrentProUnits, 0.10)
	approx(t, "total after the credential reset", keyA.TotalProUnits, 0.10)
	if keyA.WindowStartedAt == nil || !keyA.WindowStartedAt.Equal(testNow) || !keyA.WindowResetsAt.Equal(testNow.Add(claudeWeeklyWindow)) {
		t.Fatalf("key window = %v..%v, want the seven days from the first request", keyA.WindowStartedAt, keyA.WindowResetsAt)
	}
	if snapshot.ClaudeCredentials[0].WindowResetsAt != nil {
		t.Fatalf("expired window must not report a reset time")
	}

	// Key A's pending request predates the reset, so the new window's usage
	// (for example from claude.ai) is not charged to it; key B's request is newer.
	now = firstReset.Add(4 * 24 * time.Hour)
	secondReset := firstReset.Add(claudeWeeklyWindow)
	send("key-b", now, claudeObs{0.40, secondReset})
	snapshot = tracker.Snapshot(SnapshotOptions{})
	keyA = findKey(t, snapshot, KeyID("key-a")).Claude
	approx(t, "key A total after stale pending", keyA.TotalProUnits, 0.10)
	approx(t, "unattributed new window", snapshot.ClaudeCredentials[0].UnattributedCurrentFraction, 0.40)

	send("key-a", now.Add(time.Minute), claudeObs{0.43, secondReset})
	keyB := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-b")).Claude
	approx(t, "key B current in new window", keyB.Credentials[0].CurrentFraction, 0.03)
}

func TestClaudeProUnitsUseThePlanAtAttributionTime(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	info := CredentialInfo{Plan: PlanUnknown, PlanProUnits: 1, PlanSource: PlanSourceWeight}
	tracker.SetCredentialResolver(func(string) (CredentialInfo, bool) { return info, true })
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)

	seq.send("key-a", "claude-1", claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 0, 0))
	// Attributed while the plan is unknown: priced with whatever plan is current.
	seq.send("key-a", "claude-1", claudeObs{0.20, resetAt}, breakdown(100, 0, 0, 0, 0))
	info = CredentialInfo{Plan: PlanPro, PlanProUnits: 1, PlanSource: PlanSourceOrganizationType}
	// Attributed as Pro: fixed at 1 unit per weekly limit.
	seq.send("key-a", "claude-1", claudeObs{0.30, resetAt}, breakdown(100, 0, 0, 0, 0))
	info = CredentialInfo{Label: "Upgraded", Plan: PlanMax20x, PlanProUnits: 10, PlanSource: PlanSourceRateLimitTier}

	keyA := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	// 0.1 unpriced at today's 10 units + 0.1 priced at Pro.
	approx(t, "total pro units", keyA.TotalProUnits, 0.1*10+0.1*1)
	approx(t, "current pro units", keyA.CurrentProUnits, 0.1*10+0.1*1)

	// A deleted credential is described as it was last seen.
	tracker.SetCredentialResolver(func(string) (CredentialInfo, bool) { return CredentialInfo{}, false })
	ref := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude.Credentials[0]
	if ref.Plan != PlanPro || ref.PlanProUnits != 1 {
		t.Fatalf("deleted credential ref = %+v, want the last attributed plan", ref)
	}
}

func TestClaudeUsageOfUntrackedKeysIsUnattributed(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)

	seq.send("key-a", "claude-1", claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-b", "claude-1", claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 0, 0))
	// Resetting key A must not hand its pending weight to key B.
	tracker.Reset(KeyID("key-a"))
	seq.send("key-b", "claude-1", claudeObs{0.20, resetAt}, breakdown(100, 0, 0, 0, 0))

	snapshot := tracker.Snapshot(SnapshotOptions{})
	keyB := findKey(t, snapshot, KeyID("key-b")).Claude
	approx(t, "key B fraction", keyB.Credentials[0].CurrentFraction, 0.05)
	approx(t, "unattributed", snapshot.ClaudeCredentials[0].UnattributedCurrentFraction, 0.05)

	// Keys beyond the tracked key limit are not charged to tracked keys either.
	tracker.mu.Lock()
	for i := len(tracker.keys); i < maxTrackedKeys; i++ {
		tracker.keys["filler-"+strconv.Itoa(i)] = &keyState{}
	}
	tracker.mu.Unlock()
	seq.send("key-over-limit", "claude-1", claudeObs{0.20, resetAt}, breakdown(100000, 0, 0, 0, 0))
	seq.send("key-b", "claude-1", claudeObs{0.30, resetAt}, breakdown(1, 0, 0, 0, 0))
	snapshot = tracker.Snapshot(SnapshotOptions{})
	if len(snapshot.Keys) != maxTrackedKeys {
		t.Fatalf("keys = %d, want the limit %d", len(snapshot.Keys), maxTrackedKeys)
	}
	if findKey(t, snapshot, KeyID("key-b")).Claude.Credentials[0].CurrentFraction > 0.051 {
		t.Fatal("over-limit usage was charged to key B")
	}
}

func TestTrackerResetAll(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("key-a", "claude-1", claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.HandleUsage(context.Background(), record("key-b", "m", breakdown(1, 0, 0, 1, 0)))

	if tracker.Reset("missing") {
		t.Fatal("reset of unknown key must report false")
	}
	if !tracker.Reset("") {
		t.Fatal("reset all must succeed")
	}
	if len(tracker.Snapshot(SnapshotOptions{}).Keys) != 0 {
		t.Fatal("reset all must remove every key")
	}
	seq.send("key-a", "claude-1", claudeObs{0.15, resetAt}, breakdown(100, 0, 0, 0, 0))
	snapshot := tracker.Snapshot(SnapshotOptions{})
	if len(snapshot.Keys) != 1 || snapshot.Keys[0].Totals.Requests != 1 {
		t.Fatalf("keys after reset = %+v", snapshot.Keys)
	}
	// The window baseline survives but pending weight was cleared, so the increase is unattributed.
	approx(t, "unattributed", snapshot.ClaudeCredentials[0].UnattributedCurrentFraction, 0.05)
}

func TestTrackerPersistsAcrossRestart(t *testing.T) {
	now := testNow
	path := filepath.Join(t.TempDir(), "nested", StateFileName)
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	resetAt := testNow.Add(72 * time.Hour)
	tracker.HandleUsage(context.Background(), claudeRecord("secret-key", "claude-1", testNow, claudeObs{0.10, resetAt}, breakdown(100, 0, 0, 10, 0)))
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read state: %v", errRead)
	}
	if !json.Valid(data) || bytes.Contains(data, []byte("secret-key")) {
		t.Fatalf("state must be JSON without raw keys: %s", data)
	}
	if info, errStat := os.Stat(path); errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v (%v), want 0600", info.Mode().Perm(), errStat)
	}

	restarted := newTestTracker(&now)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatalf("reopen: %v", errOpen)
	}
	// The credential's weekly reading survives for quota-aware routing.
	if reading, ok := restarted.ClaudeQuota(&coreauth.Auth{ID: "claude-1"}); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.10 || !reading.Weekly.ResetAt.Equal(resetAt) ||
		reading.Short != nil || reading.Fable != nil || !reading.ObservedAt.Equal(testNow) || reading.Source != "last-known" {
		t.Fatalf("weekly reading after restart = %+v (%v)", reading, ok)
	}
	if _, ok := restarted.ClaudeQuota(nil); ok {
		t.Fatal("a nil credential must have no weekly reading")
	}
	if _, ok := restarted.ClaudeQuota(&coreauth.Auth{ID: "claude-2"}); ok {
		t.Fatal("unknown credential must have no weekly reading")
	}
	// The window baseline and pending weight survive, so the next increase is attributed.
	restarted.HandleUsage(context.Background(), claudeRecord("secret-key", "claude-1", testNow.Add(time.Minute), claudeObs{0.20, resetAt}, breakdown(100, 0, 0, 0, 0)))
	snapshot := restarted.Snapshot(SnapshotOptions{APIKeys: []string{"secret-key"}})
	key := snapshot.Keys[0]
	if key.Totals.Requests != 2 || key.Totals.Tokens.Output != 10 {
		t.Fatalf("totals after restart = %+v", key.Totals)
	}
	approx(t, "fraction after restart", key.Claude.Credentials[0].CurrentFraction, 0.10)
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(testNow) {
		t.Fatalf("the key's window must survive a restart: %+v", key.Claude)
	}
	if snapshot.Since == nil || !snapshot.Since.Equal(testNow) {
		t.Fatalf("since = %v, want %v", snapshot.Since, testNow)
	}
}

func TestTrackerOpenMovesInvalidStateAside(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":             "{not json",
		"unsupported version": `{"version":99,"keys":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, StateFileName)
			if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			now := testNow
			tracker := newTestTracker(&now)
			if errOpen := tracker.Open(path); errOpen == nil {
				t.Fatal("expected an error for invalid state")
			}
			aside := path + ".invalid-" + strconv.FormatInt(testNow.UnixNano(), 10)
			if kept, errRead := os.ReadFile(aside); errRead != nil || string(kept) != content {
				t.Fatalf("invalid state must be kept at %s: %v", aside, errRead)
			}
			// The tracker keeps tracking and saves to a fresh file.
			tracker.HandleUsage(context.Background(), record("k", "m", breakdown(1, 0, 0, 1, 0)))
			if errFlush := tracker.Flush(); errFlush != nil {
				t.Fatalf("flush: %v", errFlush)
			}
			if data, errRead := os.ReadFile(path); errRead != nil || !strings.Contains(string(data), `"version":1`) {
				t.Fatalf("fresh state = %s (%v)", data, errRead)
			}
		})
	}
}

func TestTrackerOpenSwitchesPath(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	tracker := NewTracker()
	if errOpen := tracker.Open(first); errOpen != nil {
		t.Fatal(errOpen)
	}
	tracker.HandleUsage(context.Background(), record("k", "m", breakdown(1, 0, 0, 1, 0)))
	if errOpen := tracker.Open(second); errOpen != nil {
		t.Fatal(errOpen)
	}
	if len(tracker.Snapshot(SnapshotOptions{}).Keys) != 0 {
		t.Fatal("switching to a new state file must not carry over usage")
	}
	reloaded := NewTracker()
	if errOpen := reloaded.Open(first); errOpen != nil {
		t.Fatal(errOpen)
	}
	if len(reloaded.Snapshot(SnapshotOptions{}).Keys) != 1 {
		t.Fatal("usage must be saved to the previous state file before switching")
	}
}

func TestTrackerOpenKeepsUsageWhenSavingFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test cannot write")
	}
	readOnly := filepath.Join(t.TempDir(), "ro")
	if errMkdir := os.Mkdir(readOnly, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	tracker := NewTracker()
	if errOpen := tracker.Open(filepath.Join(readOnly, StateFileName)); errOpen != nil {
		t.Fatal(errOpen)
	}
	tracker.HandleUsage(context.Background(), record("k", "m", breakdown(1, 0, 0, 1, 0)))
	if errChmod := os.Chmod(readOnly, 0o500); errChmod != nil {
		t.Fatal(errChmod)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	if errOpen := tracker.Open(filepath.Join(t.TempDir(), StateFileName)); errOpen == nil {
		t.Fatal("switching must fail when usage cannot be saved to the previous file")
	}
	if len(tracker.Snapshot(SnapshotOptions{}).Keys) != 1 {
		t.Fatal("usage must be kept when the switch fails")
	}
}

func TestFlushWithoutPathIsNoop(t *testing.T) {
	tracker := NewTracker()
	tracker.HandleUsage(context.Background(), record("k", "m", breakdown(1, 0, 0, 1, 0)))
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
}

func TestResolveStatePath(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	if got := ResolveStatePath(filepath.Join(dir, "config.yaml")); got != filepath.Join(dir, StateFileName) {
		t.Fatalf("path = %s", got)
	}
	if got := ResolveStatePath(dir); got != filepath.Join(dir, StateFileName) {
		t.Fatalf("directory path = %s", got)
	}
	if got := ResolveStatePath(""); got != "" {
		t.Fatalf("empty config path must disable persistence, got %s", got)
	}
	t.Setenv("WRITABLE_PATH", filepath.Join(dir, "writable"))
	if got := ResolveStatePath(filepath.Join(dir, "config.yaml")); got != filepath.Join(dir, "writable", StateFileName) {
		t.Fatalf("writable path = %s", got)
	}
}

func TestCredentialInfoFromAuthResolvesPlan(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
		plan     string
		units    float64
		source   string
		label    string
	}{
		{name: "max 20x tier", metadata: map[string]any{"rate_limit_tier": "default_claude_max_20x", "organization_type": "claude_max"}, plan: PlanMax20x, units: 10, source: PlanSourceRateLimitTier},
		{name: "max 5x tier", metadata: map[string]any{"rate_limit_tier": "default_claude_max_5x"}, plan: PlanMax5x, units: 5, source: PlanSourceRateLimitTier},
		{name: "pro organization", metadata: map[string]any{"organization_type": "claude_pro"}, plan: PlanPro, units: 1, source: PlanSourceOrganizationType},
		{name: "stale max tier after downgrade", metadata: map[string]any{"organization_type": "claude_pro", "rate_limit_tier": "default_claude_max_20x"}, plan: PlanPro, units: 1, source: PlanSourceOrganizationType},
		{name: "manual override wins", metadata: map[string]any{"plan_type": "Max-5x", "rate_limit_tier": "default_claude_max_20x"}, plan: PlanMax5x, units: 5, source: PlanSourcePlanType},
		{name: "team organization", metadata: map[string]any{"organization_type": "claude_team", "rate_limit_tier": "default_raven", "weight": 4}, plan: PlanTeam, units: 1.25, source: PlanSourceOrganizationType},
		{name: "manual team override", metadata: map[string]any{"plan_type": "Team", "organization_type": "claude_pro"}, plan: PlanTeam, units: 1.25, source: PlanSourcePlanType},
		{name: "enterprise falls back to weight", metadata: map[string]any{"organization_type": "claude_enterprise", "weight": 4}, plan: "enterprise", units: 4, source: PlanSourceWeight},
		{name: "max without tier falls back to weight", metadata: map[string]any{"organization_type": "claude_max"}, plan: "max", units: 1, source: PlanSourceWeight},
		{name: "unknown defaults to one", metadata: map[string]any{"email": "me@example.com"}, plan: PlanUnknown, units: 1, source: PlanSourceWeight, label: "me@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := CredentialInfoFromAuth(&coreauth.Auth{ID: "a", Provider: "claude", Metadata: tc.metadata})
			if info.Plan != tc.plan || info.PlanProUnits != tc.units || info.PlanSource != tc.source || info.Label != tc.label {
				t.Fatalf("info = %+v, want plan=%s units=%v source=%s label=%s", info, tc.plan, tc.units, tc.source, tc.label)
			}
		})
	}
	labelled := CredentialInfoFromAuth(&coreauth.Auth{ID: "a", Label: "ANU", Index: "7", Metadata: map[string]any{"email": "x@example.com"}})
	if labelled.Label != "ANU" || labelled.AuthIndex != "7" {
		t.Fatalf("labelled info = %+v", labelled)
	}
}
