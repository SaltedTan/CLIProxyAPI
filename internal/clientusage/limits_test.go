package clientusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	// Refusals log at info level; keep test output quiet.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// fakeGinContext stands in for the gin context the proxy stores under the "gin"
// context key; only Get is needed to read the client API key.
type fakeGinContext struct {
	values map[string]any
}

func (c fakeGinContext) Get(key string) (any, bool) {
	value, ok := c.values[key]
	return value, ok
}

func requestContext(apiKey string) context.Context {
	return context.WithValue(context.Background(), "gin", fakeGinContext{values: map[string]any{"userApiKey": apiKey}})
}

var claudeAuth = &coreauth.Auth{ID: "claude-1", Provider: "claude"}

// limitedKeyTracker returns a tracker where key-a has exactly 0.25 Pro units of
// current usage on claude-1 (plan unknown, so fractions are Pro units) in a window
// that resets at resetAt. The utilizations are binary fractions so the attributed
// amount is exact and "at the limit" can be tested.
// testWindowEnd is when the allowance window limitedKeyTracker opens ends: its
// first request starts one second after testNow.
var testWindowEnd = testNow.Add(time.Second).Add(claudeWeeklyWindow)

// limitedKeyTracker gives key-a 0.25 Pro units on claude-1 and returns the
// credential's weekly reset, three days out, which the key's window does not follow.
func limitedKeyTracker(t *testing.T, now *time.Time) (*Tracker, time.Time) {
	t.Helper()
	tracker := newTestTracker(now)
	seq := &claudeSeq{tracker: tracker, now: now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("key-a", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	return tracker, resetAt
}

func mustRefuse(t *testing.T, err error) *coreauth.ClientQuotaError {
	t.Helper()
	var quota *coreauth.ClientQuotaError
	if !errors.As(err, &quota) || quota == nil {
		t.Fatalf("admit error = %v, want *coreauth.ClientQuotaError", err)
	}
	return quota
}

// refuse asserts that the request is refused and records the refusal the way the
// conductor does when it returns it to the client.
func refuse(t *testing.T, tracker *Tracker, ctx context.Context, auth *coreauth.Auth) *coreauth.ClientQuotaError {
	t.Helper()
	quota := mustRefuse(t, tracker.Admit(ctx, auth))
	tracker.RecordRefusal(ctx, quota)
	return quota
}

func TestSetLimitsResolvesByFullKeyThenID(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]float64{
		"key-a":        1.5,
		KeyID("key-a"): 0.5,
		KeyID("key-b"): 0.25,
		AnonymousKeyID: 0.1,
		" key-c ":      2,
		"":             3,
		"zero":         0,
		"key-z":        0,
		KeyID("key-z"): 0.125,
		"negative":     -1,
		"nan":          math.NaN(),
		"inf":          math.Inf(1),
	})
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	cases := []struct {
		apiKey string
		limit  float64
		ok     bool
	}{
		{"key-a", 1.5, true}, // the full key entry wins over the id entry
		{"key-b", 0.25, true},
		{"", 0.1, true},
		{"key-c", 2, true},
		{"zero", 0, false},
		{"key-z", 0, false}, // a full-key 0 lifts the id entry's limit
		{"negative", 0, false},
		{"nan", 0, false},
		{"inf", 0, false},
		{"unknown", 0, false},
	}
	for _, tc := range cases {
		limit, ok := tracker.limitForLocked(tc.apiKey)
		if ok != tc.ok || limit != tc.limit {
			t.Fatalf("limitFor(%q) = %v, %v; want %v, %v", tc.apiKey, limit, ok, tc.limit, tc.ok)
		}
	}
	// Zero entries are kept for precedence; empty keys and invalid values are not.
	if len(tracker.limits) != 8 {
		t.Fatalf("limits = %v, want only valid entries", tracker.limits)
	}
}

func TestAdmitComparesCurrentProUnitsWithLimit(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	ctx := requestContext("key-a")

	tracker.SetLimits(map[string]float64{"key-a": 0.5})
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("below the limit must be admitted: %v", errAdmit)
	}
	tracker.SetLimits(map[string]float64{"key-a": 0.25})
	quota := refuse(t, tracker, ctx, claudeAuth)
	if quota.StatusCode() != 429 || quota.Code != coreauth.ErrorCodeClientKeyLimitReached {
		t.Fatalf("quota error = %+v", quota)
	}
	if quota.ResetIn != testWindowEnd.Sub(now) {
		t.Fatalf("reset in = %s, want the end of the key's window %s", quota.ResetIn, testWindowEnd.Sub(now))
	}
	want := "client API key Claude allowance reached: 0.25 of 0.25 Pro units used in the current 7-day window; resets in 6d23h"
	if quota.Error() != want {
		t.Fatalf("message = %q, want %q", quota.Error(), want)
	}
	if strings.Contains(quota.Error(), "key-a") {
		t.Fatal("message must not contain the raw key")
	}
	tracker.SetLimits(map[string]float64{KeyID("key-a"): 0.125})
	refuse(t, tracker, ctx, claudeAuth)

	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Totals.Blocked != 2 || key.Totals.Requests != 2 || key.Totals.Failed != 0 {
		t.Fatalf("totals = %+v, want blocked=2 and requests untouched", key.Totals)
	}
	if len(key.Daily) != 1 || key.Daily[0].Date != "2026-10-07" || key.Daily[0].Blocked != 2 || key.Daily[0].Requests != 2 {
		t.Fatalf("daily = %+v", key.Daily)
	}
	if key.Claude == nil || key.Claude.LimitProUnits == nil || *key.Claude.LimitProUnits != 0.125 || !key.Claude.LimitReached {
		t.Fatalf("claude = %+v", key.Claude)
	}
	if key.Claude.RemainingProUnits == nil || *key.Claude.RemainingProUnits != 0 {
		t.Fatalf("remaining = %v, want 0", key.Claude.RemainingProUnits)
	}
	if key.Claude.LimitResetsAt == nil || !key.Claude.LimitResetsAt.Equal(testWindowEnd) || !key.Claude.WindowResetsAt.Equal(testWindowEnd) {
		t.Fatalf("limit resets at = %v, want %v", key.Claude.LimitResetsAt, testWindowEnd)
	}
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(testNow.Add(time.Second)) {
		t.Fatalf("window started at = %v, want the first request", key.Claude.WindowStartedAt)
	}
	_ = resetAt

	// Refusals on another day land in that day's bucket only.
	now = testNow.Add(24 * time.Hour)
	refuse(t, tracker, ctx, claudeAuth)
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Totals.Blocked != 3 || len(key.Daily) != 2 || key.Daily[1].Date != "2026-10-08" || key.Daily[1].Blocked != 1 || key.Daily[0].Blocked != 2 {
		t.Fatalf("totals = %+v daily = %+v", key.Totals, key.Daily)
	}
}

// TestLimitIsReachedAtTheReportedPrecision pins one rule for the report and
// admission: the key is at its limit when current_pro_units, as reported to six
// decimals, reaches it. A key shown at its limit is refused, a key shown under it
// is admitted, and remaining_pro_units is 0 exactly when the limit is reached.
func TestLimitIsReachedAtTheReportedPrecision(t *testing.T) {
	cases := []struct {
		name      string
		increase  float64
		current   float64
		remaining float64
		reached   bool
	}{
		{"rounds up to the limit", 0.2499996, 0.25, 0, true},
		{"rounds down below the limit", 0.2499994, 0.249999, 0.000001, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := testNow
			tracker := newTestTracker(&now)
			seq := &claudeSeq{tracker: tracker, now: &now}
			resetAt := testNow.Add(72 * time.Hour)
			seq.send("key-a", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
			seq.send("key-a", "claude-1", claudeObs{0.125 + tc.increase, resetAt}, breakdown(100, 0, 0, 0, 0))
			tracker.SetLimits(map[string]float64{"key-a": 0.25})
			claude := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
			if claude.RemainingProUnits == nil {
				t.Fatal("remaining must be reported with a limit")
			}
			if claude.CurrentProUnits != tc.current || claude.LimitReached != tc.reached || *claude.RemainingProUnits != tc.remaining {
				t.Fatalf("current = %v reached = %v remaining = %v, want %v %v %v", claude.CurrentProUnits, claude.LimitReached, *claude.RemainingProUnits, tc.current, tc.reached, tc.remaining)
			}
			errAdmit := tracker.Admit(requestContext("key-a"), claudeAuth)
			if refused := errAdmit != nil; refused != tc.reached {
				t.Fatalf("admission error = %v, want refused = %v like limit_reached", errAdmit, tc.reached)
			}
		})
	}
}

func TestAdmitUnlimitedKeysAndLimitChanges(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	ctx := requestContext("key-a")

	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("no limits configured must admit: %v", errAdmit)
	}
	tracker.SetLimits(map[string]float64{"key-a": 0})
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("limit 0 must admit: %v", errAdmit)
	}
	tracker.SetLimits(map[string]float64{"key-b": 0.01})
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a limit on another key must admit: %v", errAdmit)
	}
	tracker.SetLimits(map[string]float64{"key-a": 0.2})
	refuse(t, tracker, ctx, claudeAuth)
	tracker.SetLimits(map[string]float64{"key-a": 0.3})
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a raised limit must re-admit: %v", errAdmit)
	}
	tracker.SetLimits(nil)
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a removed limit must re-admit: %v", errAdmit)
	}
	tracker.SetLimits(map[string]float64{"key-a": 0.05})
	refuse(t, tracker, ctx, claudeAuth)
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")); key.Totals.Blocked != 2 {
		t.Fatalf("blocked = %d, want one per refusal", key.Totals.Blocked)
	}
}

func TestAdmitOnlyAppliesToClaudeWithAClientKey(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.10})

	if errAdmit := tracker.Admit(requestContext("key-a"), &coreauth.Auth{ID: "codex-1", Provider: "codex"}); errAdmit != nil {
		t.Fatalf("non-Claude providers must be admitted: %v", errAdmit)
	}
	for _, auth := range []*coreauth.Auth{
		{ID: "oauth", Provider: "Claude", Attributes: map[string]string{"auth_kind": "oauth"}},
		{ID: "api-key", Provider: " claude ", Attributes: map[string]string{"api_key": "sk-fake"}},
	} {
		refuse(t, tracker, requestContext("key-a"), auth)
	}
	if errAdmit := tracker.Admit(context.Background(), claudeAuth); errAdmit != nil {
		t.Fatalf("a request without a gin context is anonymous and has no limit: %v", errAdmit)
	}
	if errAdmit := tracker.Admit(requestContext("key-a"), nil); errAdmit != nil {
		t.Fatalf("nil auth must be admitted: %v", errAdmit)
	}
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")); key.Totals.Blocked != 2 {
		t.Fatalf("blocked = %d, want only the Claude refusals", key.Totals.Blocked)
	}

	// Requests without a key are limited through the anonymous id. The increase is
	// split evenly with key-a's pending weight from its last request.
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("", "claude-1", claudeObs{0.625, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.SetLimits(map[string]float64{AnonymousKeyID: 0.125})
	refuse(t, tracker, context.Background(), claudeAuth)
	refuse(t, tracker, requestContext(""), claudeAuth)
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), AnonymousKeyID); key.Totals.Blocked != 2 {
		t.Fatalf("anonymous blocked = %d, want 2", key.Totals.Blocked)
	}
}

func TestAdmitReadmitsAfterWindowResetAndUsageReset(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.125})
	ctx := requestContext("key-a")
	refuse(t, tracker, ctx, claudeAuth)

	// The credential's weekly reset passes; the key's own window is still running.
	now = resetAt.Add(time.Minute)
	refuse(t, tracker, ctx, claudeAuth)

	// Once the key's window ends there is no open window, so nothing counts.
	now = testWindowEnd
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("an ended window must re-admit: %v", errAdmit)
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.LimitReached || key.Claude.CurrentProUnits != 0 || key.Claude.LimitResetsAt != nil || key.Claude.WindowStartedAt != nil || key.Claude.WindowResetsAt != nil || *key.Claude.RemainingProUnits != 0.125 {
		t.Fatalf("claude after the window ended = %+v", key.Claude)
	}
	if key.Claude.TotalProUnits != 0.25 || len(key.Claude.Credentials) != 1 || key.Claude.Credentials[0].CurrentProUnits != 0 || key.Claude.Credentials[0].TotalProUnits != 0.25 {
		t.Fatalf("totals after the window ended = %+v", key.Claude)
	}

	// The next request opens the next window from its own start, not from the end
	// of the last one, and usage in it counts from zero.
	now = testWindowEnd.Add(3 * time.Hour)
	seq := &claudeSeq{tracker: tracker, now: &now}
	nextReset := resetAt.Add(claudeWeeklyWindow)
	seq.send("key-a", "claude-1", claudeObs{0.0625, nextReset}, breakdown(100, 0, 0, 0, 0))
	windowStart := testWindowEnd.Add(3*time.Hour + time.Second)
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(windowStart) || !key.Claude.WindowResetsAt.Equal(windowStart.Add(claudeWeeklyWindow)) {
		t.Fatalf("next window = %v..%v, want %v + 7d", key.Claude.WindowStartedAt, key.Claude.WindowResetsAt, windowStart)
	}
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("below the limit in the new window must admit: %v", errAdmit)
	}
	seq.send("key-a", "claude-1", claudeObs{0.1875, nextReset}, breakdown(100, 0, 0, 0, 0))
	quota := refuse(t, tracker, ctx, claudeAuth)
	if want := windowStart.Add(claudeWeeklyWindow).Sub(now); quota.ResetIn != want {
		t.Fatalf("reset in = %s, want the new window %s", quota.ResetIn, want)
	}

	// The usage reset override clears the key and re-admits it.
	if !tracker.Reset(KeyID("key-a")) {
		t.Fatal("reset must find the key")
	}
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a reset key must be admitted: %v", errAdmit)
	}
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Totals.Blocked != 0 || key.Claude == nil || key.Claude.LimitReached || len(key.Claude.Credentials) != 0 {
		t.Fatalf("reset key = %+v claude = %+v", key.Totals, key.Claude)
	}
}

func TestResetWindowKeepsHistory(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.25})
	ctx := requestContext("key-a")
	refuse(t, tracker, ctx, claudeAuth)

	if tracker.ResetWindow(KeyID("key-unknown")) {
		t.Fatal("an unknown key must not be found")
	}
	if !tracker.ResetWindow(KeyID("key-a")) {
		t.Fatal("reset window must find the key")
	}
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a key whose window was reset must be admitted: %v", errAdmit)
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Totals.Requests != 2 || key.Totals.Blocked != 1 || len(key.Daily) != 1 || key.Daily[0].Requests != 2 {
		t.Fatalf("history must be kept: totals = %+v daily = %+v", key.Totals, key.Daily)
	}
	if key.Claude == nil || key.Claude.CurrentProUnits != 0 || key.Claude.TotalProUnits != 0.25 || key.Claude.LimitReached || *key.Claude.RemainingProUnits != 0.25 {
		t.Fatalf("claude after the window reset = %+v", key.Claude)
	}
	if key.Claude.WindowStartedAt != nil || key.Claude.WindowResetsAt != nil || key.Claude.LimitResetsAt != nil {
		t.Fatalf("no window must be open after a reset: %+v", key.Claude)
	}
	if len(key.Claude.Credentials) != 1 || key.Claude.Credentials[0].CurrentProUnits != 0 || key.Claude.Credentials[0].TotalProUnits != 0.25 {
		t.Fatalf("credential totals must be kept: %+v", key.Claude.Credentials)
	}

	// The next request opens a fresh window at its own start. The increase its
	// response carries is usage from before the reset and stays out of the window.
	now = testNow.Add(time.Hour)
	seq := &claudeSeq{tracker: tracker, now: &now}
	seq.send("key-a", "claude-1", claudeObs{0.5, resetAt}, breakdown(100, 0, 0, 0, 0))
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(testNow.Add(time.Hour+time.Second)) || key.Claude.CurrentProUnits != 0 || key.Claude.TotalProUnits != 0.375 {
		t.Fatalf("claude after the first request of the new window = %+v", key.Claude)
	}
	seq.send("key-a", "claude-1", claudeObs{0.625, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.75, resetAt}, breakdown(100, 0, 0, 0, 0))
	quota := refuse(t, tracker, ctx, claudeAuth)
	if want := testNow.Add(time.Hour + time.Second + claudeWeeklyWindow).Sub(now); quota.ResetIn != want {
		t.Fatalf("reset in = %s, want %s", quota.ResetIn, want)
	}

	// An empty id resets every key's window.
	if !tracker.ResetWindow("") {
		t.Fatal("reset all windows must succeed")
	}
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("after resetting every window the key must be admitted: %v", errAdmit)
	}
}

func TestKeyWindowIsOpenedByClaudeRequestsOnly(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.HandleUsage(context.Background(), record("key-b", "gpt-5", breakdown(1, 0, 0, 1, 0)))
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-b")); key.Claude != nil {
		t.Fatalf("a key without Claude usage has no window: %+v", key.Claude)
	}
	// A Claude request opens the window even before any usage is attributed to the key.
	startedAt := testNow.Add(10 * time.Second)
	now = startedAt.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-b", "claude-1", startedAt, claudeObs{}, breakdown(1, 0, 0, 1, 0)))
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-b"))
	if key.Claude == nil || key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(startedAt) || !key.Claude.WindowResetsAt.Equal(startedAt.Add(claudeWeeklyWindow)) {
		t.Fatalf("window after the first Claude request = %+v", key.Claude)
	}
	if key.Claude.CurrentProUnits != 0 || key.Claude.LimitProUnits != nil || key.Claude.LimitReached || len(key.Claude.Credentials) != 0 {
		t.Fatalf("an unlimited key with no attributed usage = %+v", key.Claude)
	}
	// Without a limit nothing is refused, whatever the window holds.
	if errAdmit := tracker.Admit(requestContext("key-b"), claudeAuth); errAdmit != nil {
		t.Fatalf("unlimited key: %v", errAdmit)
	}
}

func TestAdmitFollowsTheKeyWindowNotTheCredentialWindows(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.SetCredentialResolver(maxPlan)
	seq := &claudeSeq{tracker: tracker, now: &now}
	lateReset := testNow.Add(100 * time.Hour)
	earlyReset := testNow.Add(10 * time.Hour)
	seq.send("key-a", "claude-late", claudeObs{0.125, lateReset}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-late", claudeObs{0.375, lateReset}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-early", claudeObs{0.125, earlyReset}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-early", claudeObs{0.25, earlyReset}, breakdown(100, 0, 0, 0, 0))
	// 0.25 + 0.125 of a Max 20x plan is 3.75 Pro units.
	tracker.SetLimits(map[string]float64{"key-a": 3.75})

	// The window opened with the key's first request and ends seven days later,
	// whatever the credentials' own weekly resets are.
	quota := refuse(t, tracker, requestContext("key-a"), claudeAuth)
	if quota.ResetIn != testWindowEnd.Sub(now) {
		t.Fatalf("reset in = %s, want the key's window %s", quota.ResetIn, testWindowEnd.Sub(now))
	}
	if !strings.Contains(quota.Error(), "3.75 of 3.75 Pro units") {
		t.Fatalf("message = %q", quota.Error())
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.LimitResetsAt == nil || !key.Claude.LimitResetsAt.Equal(testWindowEnd) {
		t.Fatalf("limit resets at = %v, want %v", key.Claude.LimitResetsAt, testWindowEnd)
	}
	// Both credentials' weekly windows reset; the key's usage in its window is unchanged.
	now = lateReset.Add(time.Minute)
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.CurrentProUnits != 3.75 || !key.Claude.LimitReached || len(key.Claude.Credentials) != 2 {
		t.Fatalf("claude after the credential resets = %+v", key.Claude)
	}
	if early, late := key.Claude.Credentials[0], key.Claude.Credentials[1]; early.AuthID != "claude-early" || early.CurrentProUnits != 1.25 || late.CurrentProUnits != 2.5 {
		t.Fatalf("per credential usage in the window = %+v", key.Claude.Credentials)
	}
	refuse(t, tracker, requestContext("key-a"), claudeAuth)
}

func TestSnapshotReportsLimitsAndLimitOnlyKeys(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	tracker.HandleUsage(context.Background(), record("key-b", "gpt-5", breakdown(1, 0, 0, 1, 0)))
	tracker.SetLimits(map[string]float64{
		"key-a":              0.5,
		KeyID("key-b"):       0.25,
		"limit-only-secret":  1,
		"0123456789abcdef":   2,
		"configured-secret":  3,
		KeyID("key-unknown"): 0,
	})

	snapshot := tracker.Snapshot(SnapshotOptions{
		APIKeys:     []string{"key-a", "configured-secret"},
		APIKeyNames: map[string]string{"limit-only-secret": "Tablet", "0123456789abcdef": "By id"},
	})
	if !snapshot.ClaudeLimitsSupported {
		t.Fatal("claude_limits_supported must be true")
	}
	ids := make([]string, 0, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		ids = append(ids, key.ID)
	}
	// Configured keys first in order, then the rest sorted by id.
	rest := []string{KeyID("key-b"), KeyID("limit-only-secret"), "0123456789abcdef"}
	sort.Strings(rest)
	want := append([]string{KeyID("key-a"), KeyID("configured-secret")}, rest...)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("key order = %v, want %v", ids, want)
	}

	keyA := snapshot.Keys[0]
	if keyA.Claude == nil || *keyA.Claude.LimitProUnits != 0.5 || *keyA.Claude.RemainingProUnits != 0.25 || keyA.Claude.LimitReached {
		t.Fatalf("key A claude = %+v", keyA.Claude)
	}
	if keyA.Claude.LimitResetsAt == nil || !keyA.Claude.LimitResetsAt.Equal(testWindowEnd) || resetAt.After(testWindowEnd) {
		t.Fatalf("key A limit resets at = %v, want the key's window %v", keyA.Claude.LimitResetsAt, testWindowEnd)
	}
	configured := snapshot.Keys[1]
	if !configured.Configured || configured.Key != "conf...cret" || configured.Claude == nil || *configured.Claude.LimitProUnits != 3 || *configured.Claude.RemainingProUnits != 3 || len(configured.Claude.Credentials) != 0 {
		t.Fatalf("configured unused key = %+v claude = %+v", configured, configured.Claude)
	}
	keyB := findKey(t, snapshot, KeyID("key-b"))
	if keyB.Claude == nil || *keyB.Claude.LimitProUnits != 0.25 || keyB.Claude.LimitReached || keyB.Claude.LimitResetsAt != nil {
		t.Fatalf("key B (no Claude usage) claude = %+v", keyB.Claude)
	}
	limitOnly := findKey(t, snapshot, KeyID("limit-only-secret"))
	if limitOnly.Configured || limitOnly.Key != "limi...cret" || limitOnly.Name != "Tablet" || limitOnly.Totals != (Counters{}) || limitOnly.Claude == nil || *limitOnly.Claude.LimitProUnits != 1 {
		t.Fatalf("limit-only key = %+v claude = %+v", limitOnly, limitOnly.Claude)
	}
	byID := findKey(t, snapshot, "0123456789abcdef")
	if byID.Configured || byID.Key != "" || byID.Name != "By id" || byID.Claude == nil || *byID.Claude.LimitProUnits != 2 {
		t.Fatalf("limit-only key by id = %+v claude = %+v", byID, byID.Claude)
	}
	for _, key := range snapshot.Keys {
		if key.ID == KeyID("key-unknown") {
			t.Fatal("a limit of 0 must not list a key")
		}
	}

	// A key with Claude usage but no limit reports when its window resets.
	tracker.SetLimits(nil)
	keyA = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if keyA.Claude == nil || keyA.Claude.LimitProUnits != nil || keyA.Claude.RemainingProUnits != nil || keyA.Claude.LimitReached || keyA.Claude.LimitResetsAt == nil || keyA.Claude.WindowResetsAt == nil {
		t.Fatalf("key A without limit = %+v", keyA.Claude)
	}
}

func TestSnapshotJSONNeverContainsRawKeys(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("laptop-secret-key", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("laptop-secret-key", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.SetLimits(map[string]float64{"laptop-secret-key": 0.1, "limit-only-secret": 1})
	refuse(t, tracker, requestContext("laptop-secret-key"), claudeAuth)

	data, errMarshal := json.Marshal(tracker.Snapshot(SnapshotOptions{APIKeys: []string{"laptop-secret-key"}, APIKeyNames: map[string]string{"laptop-secret-key": "Laptop"}}))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	body := string(data)
	for _, secret := range []string{"laptop-secret-key", "limit-only-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("snapshot leaks %q: %s", secret, body)
		}
	}
	for _, want := range []string{`"claude_limits_supported":true`, `"blocked":1`, `"limit_pro_units":0.1`, `"remaining_pro_units":0`, `"limit_reached":true`, `"limit_resets_at":"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("snapshot missing %s: %s", want, body)
		}
	}
	var decoded struct {
		Keys []struct {
			Models map[string]map[string]any `json:"models"`
			Totals map[string]any            `json:"totals"`
			Daily  []map[string]any          `json:"daily"`
		} `json:"keys"`
	}
	if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, counters := range decoded.Keys[0].Models {
		if _, ok := counters["blocked"]; ok {
			t.Fatalf("models must not carry blocked: %s", body)
		}
	}
	if _, ok := decoded.Keys[0].Totals["blocked"]; !ok {
		t.Fatalf("totals must carry blocked: %s", body)
	}
	if _, ok := decoded.Keys[0].Daily[0]["blocked"]; !ok {
		t.Fatalf("daily must carry blocked: %s", body)
	}
}

func TestBlockedPersistsAndOlderStateLoadsWithZero(t *testing.T) {
	now := testNow
	path := filepath.Join(t.TempDir(), StateFileName)
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("secret-key", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("secret-key", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.SetLimits(map[string]float64{"secret-key": 0.1})
	refuse(t, tracker, requestContext("secret-key"), claudeAuth)
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if bytes.Contains(data, []byte("secret-key")) || !bytes.Contains(data, []byte(`"blocked":1`)) {
		t.Fatalf("state = %s", data)
	}

	restarted := newTestTracker(&now)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	key := findKey(t, restarted.Snapshot(SnapshotOptions{}), KeyID("secret-key"))
	if key.Totals.Blocked != 1 || key.Totals.Requests != 2 || len(key.Daily) != 1 || key.Daily[0].Blocked != 1 {
		t.Fatalf("restored = %+v daily = %+v", key.Totals, key.Daily)
	}
	// Limits are configuration, not state: the restarted tracker admits until set.
	if errAdmit := restarted.Admit(requestContext("secret-key"), claudeAuth); errAdmit != nil {
		t.Fatalf("limits must not be restored from state: %v", errAdmit)
	}
	restarted.SetLimits(map[string]float64{"secret-key": 0.1})
	refuse(t, restarted, requestContext("secret-key"), claudeAuth)

	legacy := filepath.Join(t.TempDir(), StateFileName)
	content := `{"version":1,"since":"2026-10-01T08:00:00Z","saved_at":"2026-10-07T11:00:00Z","keys":{"` + KeyID("old-key") + `":{"totals":{"requests":3,"failed":1,"tokens":{"input_tokens":10,"output_tokens":5,"reasoning_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"total_tokens":15}},"daily":{"2026-10-07":{"requests":3,"failed":1,"tokens":{"total_tokens":15}}},"first_used_at":"2026-10-07T10:00:00Z","last_used_at":"2026-10-07T10:30:00Z"}},"claude_credentials":{}}`
	if errWrite := os.WriteFile(legacy, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	older := newTestTracker(&now)
	if errOpen := older.Open(legacy); errOpen != nil {
		t.Fatal(errOpen)
	}
	key = findKey(t, older.Snapshot(SnapshotOptions{}), KeyID("old-key"))
	if key.Totals.Blocked != 0 || key.Totals.Requests != 3 || key.Totals.Failed != 1 || key.Daily[0].Blocked != 0 {
		t.Fatalf("legacy state = %+v daily = %+v", key.Totals, key.Daily)
	}
}

func TestAdmitIsSafeForConcurrentUse(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.1})
	ctx := requestContext("key-a")
	resetAt := testNow.Add(72 * time.Hour)

	var wg sync.WaitGroup
	const workers = 8
	refusals := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
					tracker.RecordRefusal(ctx, errAdmit)
					refusals[worker]++
				}
				if j%10 == 0 {
					tracker.HandleUsage(context.Background(), claudeRecord("key-b", "claude-1", testNow, claudeObs{0.375, resetAt}, breakdown(1, 0, 0, 0, 0)))
					tracker.Snapshot(SnapshotOptions{APIKeys: []string{"key-a"}})
					tracker.SetLimits(map[string]float64{"key-a": 0.1})
				}
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, count := range refusals {
		total += count
	}
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")); int(key.Totals.Blocked) != total || total != workers*50 {
		t.Fatalf("blocked = %d, refusals = %d, want %d", key.Totals.Blocked, total, workers*50)
	}
}

func TestFormatResetDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                               "1s",
		500 * time.Millisecond:          "1s",
		59 * time.Second:                "59s",
		time.Minute:                     "1m",
		47*time.Minute + 30*time.Second: "47m",
		time.Hour:                       "1h",
		time.Hour + 5*time.Minute:       "1h5m",
		24 * time.Hour:                  "1d",
		2*24*time.Hour + 3*time.Hour:    "2d3h",
		6*24*time.Hour + 23*time.Hour + 59*time.Minute: "6d23h",
	}
	for d, want := range cases {
		if got := formatResetDuration(d); got != want {
			t.Errorf("formatResetDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestIsKeyID(t *testing.T) {
	if !isKeyID(KeyID("any")) || !isKeyID(AnonymousKeyID) {
		t.Fatal("key ids and the anonymous id must be recognised")
	}
	for _, value := range []string{"", "sk-0123456789abcd", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdefg"} {
		if isKeyID(value) {
			t.Fatalf("%q must not be treated as a key id", value)
		}
	}
}

func TestZeroFullKeyEntryLiftsAnIDLimit(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	ctx := requestContext("key-a")
	// The full-key entry wins over the id entry even when it says "no limit".
	tracker.SetLimits(map[string]float64{"key-a": 0, KeyID("key-a"): 0.125})
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("a full-key 0 must lift the id limit: %v", errAdmit)
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{APIKeys: []string{"key-a"}}), KeyID("key-a"))
	if key.Claude == nil || key.Claude.LimitProUnits != nil || key.Claude.LimitReached {
		t.Fatalf("claude = %+v, want no limit reported", key.Claude)
	}
	// The other way round the id entry still applies.
	tracker.SetLimits(map[string]float64{KeyID("key-a"): 0.125})
	mustRefuse(t, tracker.Admit(ctx, claudeAuth))
}

func TestSnapshotNeverShowsARealKeyThatLooksLikeAnID(t *testing.T) {
	// A real client key of exactly 16 lowercase hex characters.
	const hexKey = "0123456789abcdef"
	now := testNow
	tracker := newTestTracker(&now)
	tracker.SetLimits(map[string]float64{hexKey: 0.125})

	assertHidden := func(opts SnapshotOptions, label string) {
		t.Helper()
		snapshot := tracker.Snapshot(opts)
		data, errMarshal := json.Marshal(snapshot)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(data), hexKey) {
			t.Fatalf("%s: snapshot shows the raw key: %s", label, data)
		}
		key := findKey(t, snapshot, KeyID(hexKey))
		if key.Claude == nil || key.Claude.LimitProUnits == nil || *key.Claude.LimitProUnits != 0.125 {
			t.Fatalf("%s: the key's own row carries no limit: %+v", label, key.Claude)
		}
		if key.Key != "0123...cdef" {
			t.Fatalf("%s: masked key = %q", label, key.Key)
		}
	}
	// Known because it is configured.
	assertHidden(SnapshotOptions{APIKeys: []string{hexKey}}, "configured")
	// Known because it has been used.
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send(hexKey, "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send(hexKey, "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	assertHidden(SnapshotOptions{}, "used")
	mustRefuse(t, tracker.Admit(requestContext(hexKey), claudeAuth))
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID(hexKey))
	if !key.Claude.LimitReached {
		t.Fatalf("claude = %+v, want the limit reached on the key's own row", key.Claude)
	}
}

func TestLimitedKeysAreTrackedBeyondTheCapacity(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	for i := 0; i < maxTrackedKeys; i++ {
		tracker.HandleUsage(context.Background(), record(fmt.Sprintf("filler-%d", i), "gpt-5", breakdown(1, 0, 0, 1, 0)))
	}
	tracker.SetLimits(map[string]float64{"key-late": 0.125})
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("key-late", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-late", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	quota := mustRefuse(t, tracker.Admit(requestContext("key-late"), claudeAuth))
	if !strings.Contains(quota.Error(), "0.25 of 0.12 Pro units") {
		t.Fatalf("message = %q", quota.Error())
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-late"))
	if key.Totals.Requests != 2 || key.Claude == nil || !key.Claude.LimitReached {
		t.Fatalf("limited key beyond the cap = %+v claude = %+v", key.Totals, key.Claude)
	}
	// Keys without a limit are still capped.
	tracker.HandleUsage(context.Background(), record("key-unlimited", "gpt-5", breakdown(1, 0, 0, 1, 0)))
	for _, entry := range tracker.Snapshot(SnapshotOptions{}).Keys {
		if entry.ID == KeyID("key-unlimited") {
			t.Fatal("an unlimited key beyond the cap must not be tracked")
		}
	}
}

func TestRecordRefusalCountsOnlyRefusalsReturnedToTheClient(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.125})
	ctx := requestContext("key-a")
	// Deciding is not counting: the conductor may still serve the request elsewhere.
	refusal := mustRefuse(t, tracker.Admit(ctx, claudeAuth))
	mustRefuse(t, tracker.Admit(ctx, claudeAuth))
	if key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")); key.Totals.Blocked != 0 {
		t.Fatalf("blocked = %d after admission decisions, want 0", key.Totals.Blocked)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	tracker.RecordRefusal(ctx, refusal)
	tracker.RecordRefusal(ctx, errors.New("not a refusal"))
	tracker.RecordRefusal(ctx, nil)
	log.SetOutput(io.Discard)
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Totals.Blocked != 1 || len(key.Daily) != 1 || key.Daily[0].Blocked != 1 || key.Totals.Requests != 2 {
		t.Fatalf("totals = %+v daily = %+v, want exactly one blocked", key.Totals, key.Daily)
	}
	// One info line per recorded refusal, naming the key by id in the message itself.
	lines := strings.Count(logged.String(), "refused, Claude allowance reached")
	if lines != 1 || !strings.Contains(logged.String(), "key "+KeyID("key-a")+" refused") || strings.Contains(logged.String(), "key-a") {
		t.Fatalf("log = %q, want one refusal line by key id", logged.String())
	}
	// A key reset between the decision and the record has nothing to count.
	tracker.Reset(KeyID("key-a"))
	tracker.RecordRefusal(ctx, refusal)
	for _, entry := range tracker.Snapshot(SnapshotOptions{}).Keys {
		if entry.ID == KeyID("key-a") && entry.Totals.Blocked != 0 {
			t.Fatalf("blocked after reset = %d", entry.Totals.Blocked)
		}
	}
}

// stubExecutor serves one provider and records how often it was called.
type stubExecutor struct {
	provider string
	mu       sync.Mutex
	calls    int
}

func (e *stubExecutor) Identifier() string { return e.provider }

func (e *stubExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *stubExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*stubExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*stubExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (*stubExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *stubExecutor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func registerStubAuth(t *testing.T, manager *coreauth.Manager, id, provider, model string) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		Provider: provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	}); errRegister != nil {
		t.Fatalf("register %s: %v", id, errRegister)
	}
}

func TestConductorCountsBlockedOnlyWhenTheRefusalIsReturned(t *testing.T) {
	for _, kind := range []string{"execute", "stream"} {
		t.Run(kind, func(t *testing.T) {
			now := testNow
			tracker, _ := limitedKeyTracker(t, &now)
			tracker.SetLimits(map[string]float64{"key-a": 0.125})
			manager := coreauth.NewManager(nil, nil, nil)
			// Retry rounds must not count a refusal more than once.
			manager.SetRetryConfig(2, 0, 0)
			manager.SetAdmissionPolicy(tracker)
			model := "blocked-accounting-" + kind
			claude := &stubExecutor{provider: "claude"}
			codex := &stubExecutor{provider: "codex"}
			manager.RegisterExecutor(claude)
			manager.RegisterExecutor(codex)
			registerStubAuth(t, manager, "blocked-claude-1-"+kind, "claude", model)
			registerStubAuth(t, manager, "blocked-claude-2-"+kind, "claude", model)
			registerStubAuth(t, manager, "blocked-codex-"+kind, "codex", model)

			run := func(providers []string) error {
				ctx := requestContext("key-a")
				if kind == "execute" {
					_, errExecute := manager.Execute(ctx, providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
					return errExecute
				}
				result, errStream := manager.ExecuteStream(ctx, providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if result != nil {
					for range result.Chunks {
					}
				}
				return errStream
			}
			blocked := func() int64 {
				return findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Totals.Blocked
			}
			// Served by another provider: refused on Claude, but not a blocked request.
			if errRun := run([]string{"claude", "codex"}); errRun != nil {
				t.Fatalf("fallback error = %v", errRun)
			}
			if claude.count() != 0 || codex.count() != 1 {
				t.Fatalf("calls: claude=%d codex=%d", claude.count(), codex.count())
			}
			if got := blocked(); got != 0 {
				t.Fatalf("blocked = %d after a served fallback, want 0", got)
			}
			// Refused everywhere: counted exactly once despite two credentials and retry rounds.
			errRun := run([]string{"claude"})
			var quota *coreauth.ClientQuotaError
			if !errors.As(errRun, &quota) {
				t.Fatalf("error = %v, want the refusal", errRun)
			}
			if got := blocked(); got != 1 {
				t.Fatalf("blocked = %d after a refused request, want 1", got)
			}
			if claude.count() != 0 {
				t.Fatalf("claude was called %d times", claude.count())
			}
		})
	}
}

func TestLimitEntryAppliesToEveryKeyItNamesWhenIdentitiesOverlap(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	keyA := "key-a"
	// A real client key that spells key-a's id: a limit entry for it applies to both
	// keys under admission (full-key match for B, id match for A).
	keyB := KeyID(keyA)
	seq.send(keyA, "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send(keyA, "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send(keyB, "claude-1", claudeObs{0.5, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send(keyB, "claude-1", claudeObs{0.75, resetAt}, breakdown(100, 0, 0, 0, 0))
	tracker.SetLimits(map[string]float64{keyB: 0.125})
	mustRefuse(t, tracker.Admit(requestContext(keyB), claudeAuth))
	mustRefuse(t, tracker.Admit(requestContext(keyA), claudeAuth))
	snapshot := tracker.Snapshot(SnapshotOptions{})
	for _, id := range []string{KeyID(keyB), keyB} {
		key := findKey(t, snapshot, id)
		if key.Claude == nil || key.Claude.LimitProUnits == nil || *key.Claude.LimitProUnits != 0.125 || !key.Claude.LimitReached {
			t.Fatalf("row %s claude = %+v, want the limit reached like admission", id, key.Claude)
		}
	}
	// A full-key 0 for B lifts B's limit but not A's id entry.
	tracker.SetLimits(map[string]float64{keyB: 0, KeyID(keyB): 0.125})
	if errAdmit := tracker.Admit(requestContext(keyB), claudeAuth); errAdmit != nil {
		t.Fatalf("full-key 0 must admit B: %v", errAdmit)
	}
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID(keyB))
	if key.Claude == nil || key.Claude.LimitProUnits != nil {
		t.Fatalf("B's row = %+v, want no limit", key.Claude)
	}
}

func TestSnapshotMasksShortKeysCompletely(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.SetLimits(map[string]float64{"xy": 1, "abcdef": 2})
	tracker.HandleUsage(context.Background(), record("ab", "gpt-5", breakdown(1, 0, 0, 1, 0)))
	snapshot := tracker.Snapshot(SnapshotOptions{APIKeys: []string{"ab"}})
	data, errMarshal := json.Marshal(snapshot)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, raw := range []string{`"key":"xy"`, `"key":"ab"`, `"key":"abcdef"`} {
		if strings.Contains(string(data), raw) {
			t.Fatalf("snapshot shows a short key: %s", data)
		}
	}
	if key := findKey(t, snapshot, KeyID("xy")); key.Key != "***" {
		t.Fatalf("limit-only short key = %q, want fully masked", key.Key)
	}
	if key := findKey(t, snapshot, KeyID("ab")); key.Key != "***" {
		t.Fatalf("configured short key = %q, want fully masked", key.Key)
	}
	if key := findKey(t, snapshot, KeyID("abcdef")); key.Key != "ab...ef" {
		t.Fatalf("medium key = %q", key.Key)
	}
}

func TestWindowResetIgnoresRecordsOfRequestsStartedBeforeIt(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 0.125})
	ctx := requestContext("key-a")
	refuse(t, tracker, ctx, claudeAuth)

	// A request that started before the reset finishes after it: its record must
	// not reopen a window for the key, and the usage it carries is history only.
	startedBefore := now.Add(-time.Minute)
	now = now.Add(time.Hour)
	if !tracker.ResetWindow(KeyID("key-a")) {
		t.Fatal("reset window must find the key")
	}
	now = now.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedBefore, claudeObs{0.5, resetAt}, breakdown(100, 0, 0, 0, 0)))
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	// The record's own observation is ignored (its request predates the credential
	// epoch), so its weight waits for the next increase.
	if key.Claude == nil || key.Claude.WindowStartedAt != nil || key.Claude.CurrentProUnits != 0 || key.Claude.TotalProUnits != 0.25 || key.Totals.Requests != 3 {
		t.Fatalf("a record of a request from before the reset must not open a window: %+v totals %+v", key.Claude, key.Totals)
	}
	// Another key's response attributes that weight: totals only.
	now = now.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-b", "claude-1", now.Add(-time.Second), claudeObs{0.625, resetAt}, breakdown(100, 0, 0, 0, 0)))
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.WindowStartedAt != nil || key.Claude.CurrentProUnits != 0 || key.Claude.TotalProUnits != 0.5 {
		t.Fatalf("usage attributed after the reset to a pre-reset request must stay out of a window: %+v", key.Claude)
	}
	if errAdmit := tracker.Admit(ctx, claudeAuth); errAdmit != nil {
		t.Fatalf("the key made no request after the reset and must be admitted: %v", errAdmit)
	}
	// The key's next request, started after the reset, opens the window.
	startedAfter := now.Add(time.Second)
	now = startedAfter.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedAfter, claudeObs{0.625, resetAt}, breakdown(100, 0, 0, 0, 0)))
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(startedAfter) {
		t.Fatalf("the first request after the reset must open the window: %+v", key.Claude)
	}

	// The same holds for the usage reset that deletes the key (and for resetting every key).
	for _, reset := range []struct {
		name  string
		reset func() bool
	}{
		{"delete one", func() bool { return tracker.Reset(KeyID("key-a")) }},
		{"delete all", func() bool { return tracker.Reset("") }},
		{"reset every window", func() bool { return tracker.ResetWindow("") }},
	} {
		startedBefore := now.Add(-time.Minute)
		now = now.Add(time.Hour)
		if !reset.reset() {
			t.Fatalf("%s: reset must succeed", reset.name)
		}
		now = now.Add(time.Second)
		tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedBefore, claudeObs{0.75, resetAt}, breakdown(100, 0, 0, 0, 0)))
		key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
		if key.Claude != nil && key.Claude.WindowStartedAt != nil {
			t.Fatalf("%s: a record of a request from before the reset must not open a window: %+v", reset.name, key.Claude)
		}
		startedAfter := now.Add(time.Second)
		now = startedAfter.Add(time.Second)
		tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedAfter, claudeObs{0.75, resetAt}, breakdown(100, 0, 0, 0, 0)))
		key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a"))
		if key.Claude == nil || key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(startedAfter) {
			t.Fatalf("%s: the first request after the reset must open the window: %+v", reset.name, key.Claude)
		}
	}
}

func TestKeyWindowStartsAtTheEarliestRequestOfItsPeriod(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	resetAt := testNow.Add(72 * time.Hour)
	first := testNow
	second := testNow.Add(time.Hour)

	// The second request finishes first and opens the window; the first request's
	// record then moves the window back to the key's actual first request.
	sendAt(tracker, &now, "key-a", second, second.Add(time.Second), claudeObs{0.10, resetAt})
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if key.WindowStartedAt == nil || !key.WindowStartedAt.Equal(second) {
		t.Fatalf("window after the first processed record = %+v", key)
	}
	sendAt(tracker, &now, "key-a", first, second.Add(2*time.Second), claudeObs{0.20, resetAt})
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if key.WindowStartedAt == nil || !key.WindowStartedAt.Equal(first) || !key.WindowResetsAt.Equal(first.Add(claudeWeeklyWindow)) {
		t.Fatalf("window must start at the earliest request: %+v", key)
	}
	// Usage attributed after the move lands in the moved window, which admission
	// measures from the earliest request.
	third := second.Add(3 * time.Second)
	sendAt(tracker, &now, "key-a", third, third.Add(time.Second), claudeObs{0.20, resetAt})
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	approx(t, "current after the move", key.CurrentProUnits, 0.10)
	if !key.WindowStartedAt.Equal(first) {
		t.Fatalf("window moved again: %+v", key)
	}
	tracker.SetLimits(map[string]float64{"key-a": 0.1})
	quota := refuse(t, tracker, requestContext("key-a"), claudeAuth)
	if want := first.Add(claudeWeeklyWindow).Sub(now); quota.ResetIn != want {
		t.Fatalf("reset in = %s, want %s from the earliest request", quota.ResetIn, want)
	}

	// A late record of a request from the previous window does not pull the next
	// window back into that period.
	late := first.Add(claudeWeeklyWindow - time.Minute)
	next := first.Add(claudeWeeklyWindow + time.Hour)
	sendAt(tracker, &now, "key-a", next, next.Add(time.Second), claudeObs{0.30, resetAt.Add(claudeWeeklyWindow)})
	sendAt(tracker, &now, "key-a", late, next.Add(2*time.Second), claudeObs{0.31, resetAt.Add(claudeWeeklyWindow)})
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if key.WindowStartedAt == nil || !key.WindowStartedAt.Equal(next) {
		t.Fatalf("a late record of the previous window must not move the next window: %+v", key)
	}
}

func TestExpiredWindowIsDroppedOnLoad(t *testing.T) {
	now := testNow
	path := filepath.Join(t.TempDir(), StateFileName)
	tracker := newTestTracker(&now)
	if errOpen := tracker.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	seq := &claudeSeq{tracker: tracker, now: &now}
	resetAt := testNow.Add(72 * time.Hour)
	seq.send("key-a", "claude-1", claudeObs{0.125, resetAt}, breakdown(100, 0, 0, 0, 0))
	seq.send("key-a", "claude-1", claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0))
	// Saved an hour after the window ended.
	now = testWindowEnd.Add(time.Hour)
	if errFlush := tracker.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}

	// Reloaded with the clock an hour before the window end: the window stays over.
	behind := testWindowEnd.Add(-time.Hour)
	restarted := newTestTracker(&behind)
	if errOpen := restarted.Open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	restarted.SetLimits(map[string]float64{"key-a": 0.25})
	key := findKey(t, restarted.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude == nil || key.Claude.WindowStartedAt != nil || key.Claude.CurrentProUnits != 0 || key.Claude.LimitReached || key.Claude.TotalProUnits != 0.25 {
		t.Fatalf("an expired window must not come back after a reload: %+v", key.Claude)
	}
	if errAdmit := restarted.Admit(requestContext("key-a"), claudeAuth); errAdmit != nil {
		t.Fatalf("a key whose window ended must be admitted after a reload: %v", errAdmit)
	}
	// The next request opens a window from its own start, even with the clock behind.
	behind = behind.Add(time.Second)
	restarted.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", behind, claudeObs{0.5, resetAt}, breakdown(100, 0, 0, 0, 0)))
	key = findKey(t, restarted.Snapshot(SnapshotOptions{}), KeyID("key-a"))
	if key.Claude.WindowStartedAt == nil || !key.Claude.WindowStartedAt.Equal(behind) {
		t.Fatalf("the next request must open a window: %+v", key.Claude)
	}
}

// TestPendingWeightFromBeforeTheWindowStaysOutOfIt covers usage attributed after a
// key's next window opened to requests from its previous window: the share still
// counts in the totals, but only the share of requests inside the open window
// counts toward it, also when one increase covers requests of both periods.
func TestPendingWeightFromBeforeTheWindowStaysOutOfIt(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	send := func(apiKey, authID string, startedAt time.Time, obs claudeObs) {
		now = startedAt.Add(time.Second)
		tracker.HandleUsage(context.Background(), claudeRecord(apiKey, authID, startedAt, obs, breakdown(100, 0, 0, 0, 0)))
	}
	// key-a's first window opens on claude-2, which reports no quota headers.
	first := testNow
	end := first.Add(claudeWeeklyWindow)
	resetAt := end.Add(72 * time.Hour)
	send("key-a", "claude-2", first, claudeObs{})
	// Shortly before the window ends key-a uses claude-3 and claude-1 (their
	// baseline readings); no increase follows before the window ends.
	send("key-a", "claude-3", end.Add(-2*time.Minute), claudeObs{0.125, resetAt})
	send("key-a", "claude-1", end.Add(-time.Minute), claudeObs{0.125, resetAt})
	// After the window ended, key-a's next request opens the next window elsewhere.
	next := end.Add(time.Hour)
	send("key-a", "claude-2", next, claudeObs{})

	// key-b's response on claude-1 carries the increase key-a's last request of the
	// previous window caused: it belongs to that window, not to the open one.
	send("key-b", "claude-1", end.Add(2*time.Hour), claudeObs{0.25, resetAt})
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if key.WindowStartedAt == nil || !key.WindowStartedAt.Equal(next) {
		t.Fatalf("window = %+v, want the one opened at %s", key, next)
	}
	approx(t, "current after a late increase from the previous window", key.CurrentProUnits, 0)
	approx(t, "total after a late increase from the previous window", key.TotalProUnits, 0.125)

	// One increase on claude-3 covers key-a's request from the previous window and
	// an equal one from the open window: half of key-a's share counts toward it.
	send("key-a", "claude-3", end.Add(3*time.Hour), claudeObs{0.125, resetAt})
	send("key-b", "claude-3", end.Add(4*time.Hour), claudeObs{0.375, resetAt})
	key = findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	approx(t, "current after a mixed increase", key.CurrentProUnits, 0.125)
	approx(t, "total after a mixed increase", key.TotalProUnits, 0.375)
}

// TestPendingWeightFromBeforeAWindowResetStaysOutOfTheNextWindow covers a request
// that started before the key's window was reset and whose usage is attributed
// after the key's next window opened: it counts in the totals only.
func TestPendingWeightFromBeforeAWindowResetStaysOutOfTheNextWindow(t *testing.T) {
	now := testNow
	tracker, resetAt := limitedKeyTracker(t, &now)
	startedBefore := now.Add(-time.Minute)
	now = now.Add(time.Hour)
	if !tracker.ResetWindow(KeyID("key-a")) {
		t.Fatal("reset window must find the key")
	}
	// The pre-reset request's record arrives after the reset; no increase yet.
	now = now.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedBefore, claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0)))
	// The key's next request opens the next window; no increase yet either.
	startedAfter := now.Add(time.Second)
	now = startedAfter.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-a", "claude-1", startedAfter, claudeObs{0.375, resetAt}, breakdown(100, 0, 0, 0, 0)))
	// key-b's response attributes the weight of three equal requests of key-a: the
	// last one of the previous window, the pre-reset one and the first of the next
	// window. Only the last third counts toward the window.
	now = now.Add(time.Second)
	tracker.HandleUsage(context.Background(), claudeRecord("key-b", "claude-1", now.Add(-time.Second), claudeObs{0.625, resetAt}, breakdown(100, 0, 0, 0, 0)))
	key := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if key.WindowStartedAt == nil || !key.WindowStartedAt.Equal(startedAfter) {
		t.Fatalf("window = %+v, want the one opened at %s", key, startedAfter)
	}
	approx(t, "current", key.CurrentProUnits, 0.25/3)
	approx(t, "total", key.TotalProUnits, 0.5)
}

// TestMayRefuseOnlyKeysWithAnAllowance pins which requests admission could refuse
// before a credential is picked: those of a key with a non-zero allowance, by full
// key or by id.
func TestMayRefuseOnlyKeysWithAnAllowance(t *testing.T) {
	now := testNow
	tracker := newTestTracker(&now)
	tracker.SetLimits(map[string]float64{"key-a": 0.5, KeyID("key-b"): 0.25, "key-c": 0})
	for key, want := range map[string]bool{"key-a": true, "key-b": true, "key-c": false, "key-d": false, "": false} {
		if got := tracker.MayRefuse(requestContext(key)); got != want {
			t.Fatalf("MayRefuse(%q) = %v, want %v", key, got, want)
		}
	}
	tracker.SetLimits(map[string]float64{AnonymousKeyID: 1})
	if !tracker.MayRefuse(context.Background()) {
		t.Fatal("a request without a key falls under the anonymous allowance")
	}
	if (*Tracker)(nil).MayRefuse(requestContext("key-a")) {
		t.Fatal("a nil tracker refuses nothing")
	}
}
