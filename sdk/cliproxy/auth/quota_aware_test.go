package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// quotaAwareTestBase anchors the mock clock to wall time once, without sleeping, so the
// session affinity wrapper (which evaluates availability with time.Now) agrees with the
// selector clock. Every reset and cooldown in these tests is minutes or hours from a boundary.
func quotaAwareTestBase() time.Time {
	return time.Now().Truncate(time.Second)
}

func newTestQuotaAwareSelector(now time.Time, fallback Selector) *QuotaAwareSelector {
	selector := NewQuotaAwareSelector(fallback)
	selector.nowFunc = func() time.Time { return now }
	return selector
}

// codexQuotaAuth builds a Codex credential whose snapshot reports a lightly used 5h primary
// window and a weekly secondary window with the given usage and time until reset.
func codexQuotaAuth(id string, now time.Time, weeklyUsedPct int, weeklyResetIn time.Duration) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Quota: QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     "10",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10),
				"X-Codex-Secondary-Used-Percent":   strconv.Itoa(weeklyUsedPct),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(weeklyResetIn).Unix(), 10),
			},
		},
	}
}

// withShortWindow overrides the 5h window usage and time until reset.
func withShortWindow(auth *Auth, now time.Time, usedPct int, resetIn time.Duration) *Auth {
	auth.Quota.Signals["X-Codex-Primary-Used-Percent"] = strconv.Itoa(usedPct)
	auth.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(resetIn).Unix(), 10)
	return auth
}

func coolDownAuth(auth *Auth, until time.Time) *Auth {
	auth.Unavailable = true
	auth.NextRetryAfter = until
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "quota"
	auth.Quota.NextRecoverAt = until
	return auth
}

func mustPick(t *testing.T, selector Selector, opts cliproxyexecutor.Options, auths []*Auth) *Auth {
	t.Helper()
	picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked == nil {
		t.Fatal("Pick() returned nil")
	}
	return picked
}

func assertPickSequence(t *testing.T, selector Selector, opts cliproxyexecutor.Options, auths []*Auth, want ...string) {
	t.Helper()
	for i, wantID := range want {
		if got := mustPick(t, selector, opts, auths); got.ID != wantID {
			t.Fatalf("pick #%d = %s, want %s (sequence %v)", i, got.ID, wantID, want)
		}
	}
}

func sessionOpts(sessionID string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{sessionID}}}
}

func TestQuotaAwareSelector_EarlierResetWinsAtEqualUsage(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	auths := []*Auth{
		codexQuotaAuth("a", now, 40, 2*time.Hour),
		codexQuotaAuth("b", now, 40, 48*time.Hour),
	}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "a", "a", "a")
}

func TestQuotaAwareSelector_PaceBeatsEarlierReset(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	// a resets sooner but has 5% left (0.2%/h needed); b has 90% left over 3 days (1.25%/h).
	auths := []*Auth{
		codexQuotaAuth("a", now, 95, 24*time.Hour),
		codexQuotaAuth("b", now, 10, 72*time.Hour),
	}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "b", "b", "b")
}

// An account with less quota left but an imminent weekly reset must be drained before an
// account with more quota left that resets days later.
func TestQuotaAwareSelector_ClaudeSoonResetBeatsLargerLaterReset(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	claudeAuth := func(id, weeklyUsed string, weeklyResetIn time.Duration) *Auth {
		return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": weeklyUsed,
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(weeklyResetIn).Unix(), 10),
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10),
		}}}
	}
	// 88% left over 6 days needs ~0.61%/h; 32% left over 10 hours needs 3.2%/h.
	later := claudeAuth("a-later", "0.12", 6*24*time.Hour)
	sooner := claudeAuth("b-sooner", "0.68", 10*time.Hour)
	for _, order := range [][]*Auth{{later, sooner}, {sooner, later}} {
		selector := newTestQuotaAwareSelector(now, nil)
		assertPickSequence(t, selector, cliproxyexecutor.Options{}, order, "b-sooner", "b-sooner", "b-sooner")
	}
}

func TestQuotaAwareSelector_CandidateOrderDoesNotAffectResult(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	a := codexQuotaAuth("a", now, 40, 72*time.Hour)
	b := codexQuotaAuth("b", now, 40, 5*time.Hour)
	c := codexQuotaAuth("c", now, 40, 30*time.Hour)
	for _, order := range [][]*Auth{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}} {
		selector := newTestQuotaAwareSelector(now, nil)
		if got := mustPick(t, selector, cliproxyexecutor.Options{}, order); got.ID != "b" {
			t.Fatalf("order %s,%s,%s picked %s, want b", order[0].ID, order[1].ID, order[2].ID, got.ID)
		}
	}
}

func TestQuotaAwareSelector_SkipsUnavailableMoreUrgentCredential(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	auths := []*Auth{
		coolDownAuth(codexQuotaAuth("a", now, 40, time.Hour), now.Add(30*time.Minute)),
		codexQuotaAuth("b", now, 40, 10*time.Hour),
		{ID: "c", Provider: "codex", Status: StatusDisabled, Disabled: true, Quota: codexQuotaAuth("c", now, 0, time.Hour).Quota},
	}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "b", "b")
}

func TestQuotaAwareSelector_MissingDataRanksAfterWeeklyData(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	auths := []*Auth{
		{ID: "a", Provider: "codex", Status: StatusActive},
		codexQuotaAuth("b", now, 40, 10*time.Hour),
	}
	// "a" is probed once, then ranks after "b" until the probe interval passes.
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "a", "b", "b")

	selector.nowFunc = func() time.Time { return now.Add(quotaAwareProbeInterval - time.Second) }
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "b")
	selector.nowFunc = func() time.Time { return now.Add(quotaAwareProbeInterval) }
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "a", "b")
}

// A restart clears every snapshot. The first pick lands on one credential, whose response
// reports heavy use; the other must still be probed rather than starved behind it.
func TestQuotaAwareSelector_ProbesCredentialWithoutDataAfterColdStart(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	pldi := &Auth{ID: "claude-pldi", Provider: "claude", Status: StatusActive}
	team := &Auth{ID: "claude-team", Provider: "claude", Status: StatusActive}
	auths := []*Auth{team, pldi}

	if got := mustPick(t, selector, cliproxyexecutor.Options{}, auths); got != pldi {
		t.Fatalf("cold pick = %s, want %s (round-robin over credentials without data)", got.ID, pldi.ID)
	}
	pldi.Quota = QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.95",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(6*24*time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10),
	}}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-team")

	team.Quota = QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(5*24*time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10),
	}}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-team", "claude-team")
}

func TestQuotaAwareSelector_DoesNotProbeZeroWeightOrSaturatedCredentials(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	zeroWeight := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeWeight: "0"}}
	// Only a 5h window, nearly used up: no weekly data, but skipped as short-saturated.
	saturated := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.9",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
	}}}
	withData := &Auth{ID: "c", Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(3*24*time.Hour).Unix(), 10),
	}}}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, []*Auth{zeroWeight, saturated, withData}, "c", "c")
}

func TestQuotaAwareSelector_NoProbesWithoutWeeklyData(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	auths := []*Auth{
		{ID: "a", Provider: "codex", Status: StatusActive},
		{ID: "b", Provider: "codex", Status: StatusActive},
	}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "a", "b", "a", "b")
	if len(selector.probedAt) != 0 {
		t.Fatalf("probedAt = %v, want no probes when no credential has weekly data", selector.probedAt)
	}
}

type recordingSelector struct {
	calls  int
	seen   []string
	pickID string
}

func (r *recordingSelector) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	r.calls++
	r.seen = r.seen[:0]
	var picked *Auth
	for _, auth := range auths {
		r.seen = append(r.seen, auth.ID)
		if auth.ID == r.pickID {
			picked = auth
		}
	}
	return picked, nil
}

func TestQuotaAwareSelector_AllDataMissingDelegatesToFallback(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	auths := []*Auth{
		{ID: "a", Provider: "codex", Status: StatusActive},
		{ID: "b", Provider: "codex", Status: StatusActive},
	}

	fallback := &recordingSelector{pickID: "b"}
	selector := newTestQuotaAwareSelector(now, fallback)
	if got := mustPick(t, selector, cliproxyexecutor.Options{}, auths); got.ID != "b" {
		t.Fatalf("picked %s, want fallback choice b", got.ID)
	}
	if fallback.calls != 1 || len(fallback.seen) != 2 {
		t.Fatalf("fallback calls=%d candidates=%v, want one call with both candidates", fallback.calls, fallback.seen)
	}

	// With the default round-robin fallback the pool rotates exactly as round-robin would.
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a", "b", "a", "b")
}

func TestQuotaAwareSelector_EqualPaceRotatesDeterministically(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	auths := []*Auth{
		codexQuotaAuth("c", now, 40, 50*time.Hour),
		codexQuotaAuth("b", now, 40, 6*time.Hour),
		codexQuotaAuth("a", now, 40, 6*time.Hour),
	}
	for run := 0; run < 2; run++ {
		assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a", "b", "a", "b")
	}
}

func TestQuotaAwareSelector_NearTiesShareNewSessions(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	auths := []*Auth{
		codexQuotaAuth("a", now, 40, 10*time.Hour), // 6.0%/h
		codexQuotaAuth("b", now, 46, 10*time.Hour), // 5.4%/h, within 80% of a
		codexQuotaAuth("c", now, 40, 20*time.Hour), // 3.0%/h, clearly less urgent
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a", "b", "a", "b")
}

func TestQuotaAwareSelector_ShortWindowSaturationSkipsCredential(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()

	saturated := []*Auth{
		withShortWindow(codexQuotaAuth("a", now, 40, 2*time.Hour), now, 92, 3*time.Hour),
		codexQuotaAuth("b", now, 40, 50*time.Hour),
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, saturated, "b", "b")

	resettingSoon := []*Auth{
		withShortWindow(codexQuotaAuth("a", now, 40, 2*time.Hour), now, 92, 10*time.Minute),
		codexQuotaAuth("b", now, 40, 50*time.Hour),
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, resettingSoon, "a", "a")

	allSaturated := []*Auth{
		withShortWindow(codexQuotaAuth("a", now, 40, 2*time.Hour), now, 92, 3*time.Hour),
		withShortWindow(codexQuotaAuth("b", now, 40, 50*time.Hour), now, 90, 3*time.Hour),
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, allSaturated, "a", "a")
}

func TestQuotaAwareSelector_WeeklyUsedUpRanksAfterMissingData(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	auths := []*Auth{
		codexQuotaAuth("a", now, 100, 10*time.Hour),
		{ID: "b", Provider: "codex", Status: StatusActive},
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "b", "b")
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths[:1], "a")
}

func TestQuotaAwareSelector_WeightScalesByPlanSize(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	// Unweighted, b needs 6%/h and a 2%/h. a is a four-times-larger plan set through the
	// auth JSON "weight" field, so its remaining quota is worth 8 units/h against b's 6.
	large := codexQuotaAuth("a", now, 40, 30*time.Hour)
	large.Metadata = map[string]any{AttributeWeight: float64(4)}
	auths := []*Auth{large, codexQuotaAuth("b", now, 40, 10*time.Hour)}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a", "a", "a")

	// The same pool without the weight follows unweighted pace.
	unweighted := []*Auth{codexQuotaAuth("a", now, 40, 30*time.Hour), codexQuotaAuth("b", now, 40, 10*time.Hour)}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, unweighted, "b", "b")

	// Configured API-key credentials carry the weight as an attribute.
	attributed := codexQuotaAuth("c", now, 40, 30*time.Hour)
	attributed.Attributes = map[string]string{AttributeWeight: "4"}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, []*Auth{attributed, codexQuotaAuth("d", now, 40, 10*time.Hour)}, "c", "c")
}

func TestQuotaAwareSelector_ZeroWeightIsLastResort(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	zero := codexQuotaAuth("a", now, 10, time.Hour)
	zero.Attributes = map[string]string{AttributeWeight: "0"}
	auths := []*Auth{zero, {ID: "b", Provider: "codex", Status: StatusActive}}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "b", "b")
	// It is still used when nothing else is available.
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths[:1], "a")
}

// claudeQuotaAuth builds a Claude credential with the given plan metadata, weekly usage and
// time until the weekly reset, and a lightly used 5h window.
func claudeQuotaAuth(id string, metadata map[string]any, now time.Time, weeklyUsed string, weeklyResetIn time.Duration) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: metadata, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": weeklyUsed,
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(weeklyResetIn).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10),
	}}}
}

// A Claude plan sizes the credential without a weight: 1% of Max 5x is worth four times
// 1% of Team, so a Max account with less left per hour still outranks the Team account.
func TestQuotaAwareSelector_ClaudePlanSetsPlanSize(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	maxPlan := map[string]any{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"}
	teamPlan := map[string]any{"organization_type": "claude_team", "rate_limit_tier": "default_raven"}
	// Unsized, Team needs 0.93%/h and Max 0.64%/h. Sized, Team is 1.16 Pro units/h and Max 3.18.
	auths := []*Auth{
		claudeQuotaAuth("a-team", teamPlan, now, "0", 108*time.Hour),
		claudeQuotaAuth("b-max", maxPlan, now, "0.39", 96*time.Hour),
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "b-max", "b-max", "b-max")

	// Without plan metadata the same pool follows unsized pace.
	unsized := []*Auth{
		claudeQuotaAuth("a-team", nil, now, "0", 108*time.Hour),
		claudeQuotaAuth("b-max", nil, now, "0.39", 96*time.Hour),
	}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, unsized, "a-team", "a-team")

	// A weight does not resize a known plan; a plan without a known allowance uses it.
	weightedTeam := claudeQuotaAuth("a-team", map[string]any{"organization_type": "claude_team", "weight": float64(8)}, now, "0", 108*time.Hour)
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, []*Auth{weightedTeam, auths[1]}, "b-max", "b-max")
	enterprise := claudeQuotaAuth("a-enterprise", map[string]any{"organization_type": "claude_enterprise", "weight": float64(8)}, now, "0", 108*time.Hour)
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, []*Auth{enterprise, auths[1]}, "a-enterprise", "a-enterprise")
}

// After a restart only the credential that served a request has a snapshot. The weekly
// quota source supplies the others' last readings, so they are ranked instead of probed:
// Max, with the most quota at risk, keeps new sessions; Team takes over once Max is
// drained; PLDI, used up, is a last resort. A source reading never replaces a snapshot.
func TestQuotaAwareSelector_WeeklyQuotaSourceRanksCredentialsWithoutSnapshot(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	teamPlan := map[string]any{"organization_type": "claude_team"}
	maxAuth := claudeQuotaAuth("claude-max", map[string]any{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"}, now, "0.39", 47*time.Hour)
	pldi := &Auth{ID: "claude-pldi", Provider: "claude", Status: StatusActive, Metadata: teamPlan}
	team := &Auth{ID: "claude-team", Provider: "claude", Status: StatusActive, Metadata: teamPlan}
	auths := []*Auth{maxAuth, pldi, team}
	readings := map[string]WeeklyQuotaReading{
		"claude-max":  {Used: 1, ResetAt: now.Add(47 * time.Hour), ObservedAt: now.Add(-3 * time.Hour)},
		"claude-pldi": {Used: 1, ResetAt: now.Add(68 * time.Hour), ObservedAt: now.Add(-2 * time.Hour)},
		"claude-team": {Used: 0.55, ResetAt: now.Add(82 * time.Hour), ObservedAt: now.Add(-2 * time.Hour)},
	}
	source := func(authID string) (WeeklyQuotaReading, bool) {
		reading, ok := readings[authID]
		return reading, ok
	}

	// Without the source the credentials lacking a snapshot are probed ahead of Max.
	if got := mustPick(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths); got == maxAuth {
		t.Fatalf("pick without source = %s, want a probe of a credential without data", got.ID)
	}

	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetWeeklyQuotaSource(source)
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-max", "claude-max", "claude-max")
	if len(selector.probedAt) != 0 {
		t.Fatalf("probedAt = %v, want no probes when the source has every credential", selector.probedAt)
	}

	drained := claudeQuotaAuth("claude-max", maxAuth.Metadata, now, "0.98", 47*time.Hour)
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, []*Auth{drained, pldi, team}, "claude-team", "claude-team")

	// A reading whose reset has passed says nothing about the new window: Team is probed.
	readings["claude-team"] = WeeklyQuotaReading{Used: 0.55, ResetAt: now.Add(-time.Hour), ObservedAt: now.Add(-8 * 24 * time.Hour)}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-team", "claude-max")
}

func TestQuotaAwarePlanSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		auth *Auth
		want float64
	}{
		{name: "nil", auth: nil, want: 1},
		{name: "claude max 5x tier", auth: &Auth{Provider: "claude", Metadata: map[string]any{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"}}, want: 5},
		{name: "claude max 20x tier", auth: &Auth{Provider: "claude", Metadata: map[string]any{"rate_limit_tier": "default_claude_max_20x"}}, want: 10},
		{name: "claude team", auth: &Auth{Provider: "claude", Metadata: map[string]any{"organization_type": "claude_team"}}, want: 1.25},
		{name: "claude pro", auth: &Auth{Provider: "claude", Metadata: map[string]any{"organization_type": "claude_pro"}}, want: 1},
		{name: "claude plan_type override", auth: &Auth{Provider: "claude", Metadata: map[string]any{"plan_type": "max-20x", "organization_type": "claude_team"}}, want: 10},
		{name: "claude known plan ignores weight", auth: &Auth{Provider: "claude", Attributes: map[string]string{AttributeWeight: "3"}, Metadata: map[string]any{"organization_type": "claude_team"}}, want: 1.25},
		{name: "claude zero weight is last resort", auth: &Auth{Provider: "claude", Attributes: map[string]string{AttributeWeight: "0"}, Metadata: map[string]any{"rate_limit_tier": "default_claude_max_5x"}}, want: 0},
		{name: "claude unknown plan uses weight", auth: &Auth{Provider: "claude", Metadata: map[string]any{"organization_type": "claude_enterprise", "weight": float64(4)}}, want: 4},
		{name: "claude without plan defaults to one", auth: &Auth{Provider: "claude"}, want: 1},
		{name: "codex plan_type is not a Claude plan", auth: &Auth{Provider: "codex", Metadata: map[string]any{"plan_type": "team"}}, want: 1},
		{name: "codex weight", auth: &Auth{Provider: "codex", Metadata: map[string]any{"plan_type": "pro", "weight": float64(5)}}, want: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaAwarePlanSize(tc.auth); got != tc.want {
				t.Fatalf("quotaAwarePlanSize() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaAwareSelector_SessionAffinityKeepsExistingBinding(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := NewSessionAffinitySelector(newTestQuotaAwareSelector(now, nil))
	t.Cleanup(selector.Stop)

	before := []*Auth{
		codexQuotaAuth("a", now, 40, 2*time.Hour),
		codexQuotaAuth("b", now, 40, 72*time.Hour),
	}
	if got := mustPick(t, selector, sessionOpts("session-x"), before); got.ID != "a" {
		t.Fatalf("new session bound to %s, want a", got.ID)
	}

	// B is now far more urgent than A; the bound session must not move.
	after := []*Auth{
		codexQuotaAuth("a", now, 90, 60*time.Hour),
		codexQuotaAuth("b", now, 10, time.Hour),
	}
	assertPickSequence(t, selector, sessionOpts("session-x"), after, "a", "a", "a")
	// A brand-new session still follows quota pace.
	if got := mustPick(t, selector, sessionOpts("session-y"), after); got.ID != "b" {
		t.Fatalf("new session bound to %s, want b", got.ID)
	}
}

func TestQuotaAwareSelector_SessionAffinityFailoverUsesQuotaPace(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := NewSessionAffinitySelector(newTestQuotaAwareSelector(now, nil))
	t.Cleanup(selector.Stop)

	healthy := func() []*Auth {
		return []*Auth{
			codexQuotaAuth("a", now, 40, time.Hour),
			codexQuotaAuth("b", now, 40, 80*time.Hour),
			codexQuotaAuth("c", now, 40, 20*time.Hour),
		}
	}
	if got := mustPick(t, selector, sessionOpts("session-x"), healthy()); got.ID != "a" {
		t.Fatalf("new session bound to %s, want a", got.ID)
	}

	// A cools down: failover rebinds to the most urgent remaining credential.
	cooling := healthy()
	coolDownAuth(cooling[0], now.Add(2*time.Hour))
	if got := mustPick(t, selector, sessionOpts("session-x"), cooling); got.ID != "c" {
		t.Fatalf("failover picked %s, want c", got.ID)
	}

	// A recovers as the most urgent credential, but the session keeps its new binding.
	assertPickSequence(t, selector, sessionOpts("session-x"), healthy(), "c", "c", "c")
}

func TestQuotaAwareSelector_IndependentSessionsBindIndependently(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	selector := NewSessionAffinitySelector(newTestQuotaAwareSelector(now, nil))
	t.Cleanup(selector.Stop)

	aUrgent := []*Auth{codexQuotaAuth("a", now, 40, 3*time.Hour), codexQuotaAuth("b", now, 40, 30*time.Hour)}
	bUrgent := []*Auth{codexQuotaAuth("a", now, 40, 30*time.Hour), codexQuotaAuth("b", now, 40, 3*time.Hour)}

	if got := mustPick(t, selector, sessionOpts("session-1"), aUrgent); got.ID != "a" {
		t.Fatalf("session-1 bound to %s, want a", got.ID)
	}
	if got := mustPick(t, selector, sessionOpts("session-2"), bUrgent); got.ID != "b" {
		t.Fatalf("session-2 bound to %s, want b", got.ID)
	}
	for _, auths := range [][]*Auth{aUrgent, bUrgent} {
		if got := mustPick(t, selector, sessionOpts("session-1"), auths); got.ID != "a" {
			t.Fatalf("session-1 moved to %s", got.ID)
		}
		if got := mustPick(t, selector, sessionOpts("session-2"), auths); got.ID != "b" {
			t.Fatalf("session-2 moved to %s", got.ID)
		}
	}
}

func TestAuthQuotaUsage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	type window struct {
		label string
		used  float64
		in    time.Duration
	}
	tests := []struct {
		name       string
		observedAt time.Time
		signals    map[string]string
		long       *window
		short      *window
	}{
		{
			name: "codex weekly primary on single-window plans",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "51",
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       unix(40 * time.Hour),
			},
			long: &window{"codex-primary", 0.51, 40 * time.Hour},
		},
		{
			name: "codex weekly secondary with 5h primary",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     "88.5",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         unix(time.Hour),
				"X-Codex-Secondary-Used-Percent":   "20",
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       unix(90 * time.Hour),
			},
			long:  &window{"codex-secondary", 0.20, 90 * time.Hour},
			short: &window{"codex-primary", 0.885, time.Hour},
		},
		{
			name:       "codex reset-after is relative to the observation",
			observedAt: now.Add(-time.Hour),
			signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":        "5",
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "7200",
			},
			long: &window{"codex-secondary", 0.05, time.Hour},
		},
		{
			name: "codex window without usage is unusable",
			signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       unix(time.Hour),
			},
		},
		{
			name: "codex additional limits are ignored",
			signals: map[string]string{
				"X-Codex-Additional-Spark-Secondary-Used-Percent":   "1",
				"X-Codex-Additional-Spark-Secondary-Window-Minutes": "10080",
				"X-Codex-Additional-Spark-Secondary-Reset-At":       unix(time.Hour),
			},
		},
		{
			name: "elapsed weekly reset is stale but the short window remains",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     "30",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         unix(2 * time.Hour),
				"X-Codex-Secondary-Used-Percent":   "70",
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       unix(-time.Minute),
			},
			short: &window{"codex-primary", 0.30, 2 * time.Hour},
		},
		{
			name: "reset beyond one window is malformed",
			signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   "10",
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
			},
		},
		{
			name: "claude shared windows ignore model-specific limits",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.25",
				"Anthropic-Ratelimit-Unified-5h-Reset":          unix(time.Hour),
				"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.53",
				"Anthropic-Ratelimit-Unified-7d-Reset":          unix(100 * time.Hour),
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.99",
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":       unix(2 * time.Hour),
			},
			long:  &window{"claude-7d", 0.53, 100 * time.Hour},
			short: &window{"claude-5h", 0.25, time.Hour},
		},
		{
			name: "claude overdrawn utilization is clamped",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": "1.02",
				"Anthropic-Ratelimit-Unified-7d-Reset":       unix(5 * time.Hour),
			},
			long: &window{"claude-7d", 1, 5 * time.Hour},
		},
		{
			name: "lower-case restored claude keys",
			signals: map[string]string{
				"anthropic-ratelimit-unified-7d-utilization": "0.1",
				"anthropic-ratelimit-unified-7d-reset":       unix(5 * time.Hour),
			},
			long: &window{"claude-7d", 0.1, 5 * time.Hour},
		},
		{
			name: "devin remaining percentages",
			signals: map[string]string{
				"daily_quota_remaining_percent":  "10%",
				"daily_quota_reset_at":           now.Add(time.Hour).Format(time.RFC3339),
				"weekly_quota_remaining_percent": "75%",
				"weekly_quota_reset_at":          now.Add(26 * time.Hour).Format(time.RFC3339),
			},
			long:  &window{"devin-weekly", 0.25, 26 * time.Hour},
			short: &window{"devin-daily", 0.90, time.Hour},
		},
		{name: "no signals"},
	}
	check := func(t *testing.T, kind string, got quotaWindow, has bool, want *window) {
		t.Helper()
		if has != (want != nil) {
			t.Fatalf("%s window present = %v (%+v), want %v", kind, has, got, want != nil)
		}
		if want == nil {
			return
		}
		if got.label != want.label || got.used < want.used-1e-9 || got.used > want.used+1e-9 || !got.resetAt.Equal(now.Add(want.in)) {
			t.Fatalf("%s window = %+v, want %+v", kind, got, *want)
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observedAt := tc.observedAt
			if observedAt.IsZero() {
				observedAt = now
			}
			usage := authQuotaUsage(&Auth{ID: "x", Quota: QuotaState{ObservedAt: observedAt, Signals: tc.signals}}, now)
			check(t, "long", usage.long, usage.hasLong, tc.long)
			check(t, "short", usage.short, usage.hasShort, tc.short)
		})
	}
}

// TestManagerQuotaAwareSessionAffinityCooldownFailover drives the full manager path:
// shared eligibility filtering, affinity binding, in-request failover on a quota error, and
// the resulting cooldown excluding the exhausted credential from later selections.
func TestManagerQuotaAwareSessionAffinityCooldownFailover(t *testing.T) {
	withQuotaCooldownEnabled(t)
	ctx := context.Background()
	now := quotaAwareTestBase()
	const model = "quota-aware-model"

	urgentID, relaxedID, unknownID := "qa-urgent-"+t.Name(), "qa-relaxed-"+t.Name(), "qa-unknown-"+t.Name()
	quotaSelector := newTestQuotaAwareSelector(now, nil)
	// Already probed, so the credential without data ranks after those with it.
	quotaSelector.recordProbe(unknownID, now)
	selector := NewSessionAffinitySelector(quotaSelector)
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)

	for _, candidate := range []*Auth{
		{ID: unknownID, Provider: "codex", Status: StatusActive},
		codexQuotaAuth(relaxedID, now, 40, 90*time.Hour),
		codexQuotaAuth(urgentID, now, 40, 4*time.Hour),
	} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
		if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	exhausted := false
	retryAfter := time.Hour
	var attempts []string
	manager.RegisterExecutor(&mockCustomErrorExecutor{
		identifier: "codex",
		executeFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			if exhausted && auth.ID == urgentID {
				return cliproxyexecutor.Response{}, streamQuotaError{
					customStatusError: customStatusError{code: http.StatusTooManyRequests, msg: "quota exhausted", retryAfter: &retryAfter},
					credentialScoped:  true,
				}
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})

	pick := func(session string) string {
		t.Helper()
		selected, errSelect := manager.SelectAuth(ctx, "codex", model, sessionOpts(session))
		if errSelect != nil {
			t.Fatalf("SelectAuth(%s) error = %v", session, errSelect)
		}
		return selected.ID
	}

	if got := pick("session-x"); got != urgentID {
		t.Fatalf("new session bound to %s, want %s", got, urgentID)
	}
	if got := pick("session-x"); got != urgentID {
		t.Fatalf("bound session moved to %s, want %s", got, urgentID)
	}

	exhausted = true
	response, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, sessionOpts("session-x"))
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if string(response.Payload) != relaxedID || len(attempts) != 2 || attempts[0] != urgentID || attempts[1] != relaxedID {
		t.Fatalf("response=%s attempts=%v, want failover from %s to %s", response.Payload, attempts, urgentID, relaxedID)
	}

	if got := pick("session-x"); got != relaxedID {
		t.Fatalf("post-failover pick %s, want sticky %s", got, relaxedID)
	}
	if got := pick("session-y"); got != relaxedID {
		t.Fatalf("new session picked %s, want %s (cooling credential must be skipped)", got, relaxedID)
	}
}
