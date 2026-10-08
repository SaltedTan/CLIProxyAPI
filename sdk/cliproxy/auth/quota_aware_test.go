package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
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

// weeklyReading is a source reading with only a weekly window.
func weeklyReading(source string, used float64, resetAt, observedAt time.Time) QuotaReading {
	return QuotaReading{Weekly: &QuotaWindowReading{Used: used, ResetAt: resetAt}, ObservedAt: observedAt, Source: source}
}

// mapQuotaSource serves the readings by auth ID.
func mapQuotaSource(readings map[string]QuotaReading) QuotaSource {
	return func(auth *Auth) (QuotaReading, bool) {
		reading, ok := readings[auth.ID]
		return reading, ok
	}
}

// After a restart only the credential that served a request has a snapshot. The saved
// readings supply the others' weekly windows, so they are ranked instead of probed:
// Max, with the most quota at risk, keeps new sessions; Team takes over once Max is
// drained; PLDI, used up, is a last resort. An older reading never replaces a snapshot.
func TestQuotaAwareSelector_QuotaSourceRanksCredentialsWithoutSnapshot(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	teamPlan := map[string]any{"organization_type": "claude_team"}
	maxAuth := claudeQuotaAuth("claude-max", map[string]any{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"}, now, "0.39", 47*time.Hour)
	pldi := &Auth{ID: "claude-pldi", Provider: "claude", Status: StatusActive, Metadata: teamPlan}
	team := &Auth{ID: "claude-team", Provider: "claude", Status: StatusActive, Metadata: teamPlan}
	auths := []*Auth{maxAuth, pldi, team}
	readings := map[string]QuotaReading{
		"claude-max":  weeklyReading("last-known", 1, now.Add(47*time.Hour), now.Add(-3*time.Hour)),
		"claude-pldi": weeklyReading("last-known", 1, now.Add(68*time.Hour), now.Add(-2*time.Hour)),
		"claude-team": weeklyReading("last-known", 0.55, now.Add(82*time.Hour), now.Add(-2*time.Hour)),
	}

	// Without the source the credentials lacking a snapshot are probed ahead of Max.
	if got := mustPick(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths); got == maxAuth {
		t.Fatalf("pick without source = %s, want a probe of a credential without data", got.ID)
	}

	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(nil, mapQuotaSource(readings))
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-max", "claude-max", "claude-max")
	if len(selector.probedAt) != 0 {
		t.Fatalf("probedAt = %v, want no probes when the source has every credential", selector.probedAt)
	}

	drained := claudeQuotaAuth("claude-max", maxAuth.Metadata, now, "0.98", 47*time.Hour)
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, []*Auth{drained, pldi, team}, "claude-team", "claude-team")

	// A reading whose reset has passed says nothing about the new window: Team is probed.
	readings["claude-team"] = weeklyReading("last-known", 0.55, now.Add(-time.Hour), now.Add(-8*24*time.Hour))
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "claude-team", "claude-max")
}

// claudeSnapshotAuth builds a Claude credential whose snapshot, observed at observedAt,
// reports the given weekly and 5h usage fractions; an empty usage omits the window.
func claudeSnapshotAuth(id string, now, observedAt time.Time, weeklyUsed string, weeklyResetIn time.Duration, shortUsed string, shortResetIn time.Duration) *Auth {
	signals := map[string]string{}
	if weeklyUsed != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = weeklyUsed
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(weeklyResetIn).Unix(), 10)
	}
	if shortUsed != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = shortUsed
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(shortResetIn).Unix(), 10)
	}
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: observedAt, Signals: signals}}
}

// Each window is taken from the newest usable reading among the snapshot, the endpoint
// reading and the saved reading, independently of the other window.
func TestApplyQuotaSources_NewestReadingWinsPerWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(minutesAgo int) time.Time { return now.Add(-time.Duration(minutesAgo) * time.Minute) }
	endpoint := func(observedAt time.Time) QuotaReading {
		return QuotaReading{
			Weekly:     &QuotaWindowReading{Used: 0.6, ResetAt: now.Add(50 * time.Hour)},
			Short:      &QuotaWindowReading{Used: 0.3, ResetAt: now.Add(2 * time.Hour)},
			ObservedAt: observedAt,
			Source:     "oauth-usage",
		}
	}
	saved := func(observedAt time.Time) QuotaReading {
		return weeklyReading("last-known", 0.7, now.Add(50*time.Hour), observedAt)
	}
	tests := []struct {
		name       string
		snapshotAt time.Time // zero: no snapshot
		endpoint   *QuotaReading
		saved      *QuotaReading
		long       string
		longUsed   float64
		short      string
		shortUsed  float64
	}{
		{name: "snapshot only", snapshotAt: at(10), long: "claude-7d", longUsed: 0.5, short: "claude-5h", shortUsed: 0.2},
		{name: "newer endpoint replaces both windows", snapshotAt: at(10), endpoint: ptr(endpoint(at(5))), long: "oauth-usage-weekly", longUsed: 0.6, short: "oauth-usage-5h", shortUsed: 0.3},
		{name: "newest per window", snapshotAt: at(10), endpoint: ptr(endpoint(at(5))), saved: ptr(saved(at(1))), long: "last-known-weekly", longUsed: 0.7, short: "oauth-usage-5h", shortUsed: 0.3},
		{name: "older endpoint loses to a newer snapshot", snapshotAt: at(5), endpoint: ptr(endpoint(at(10))), saved: ptr(saved(at(20))), long: "claude-7d", longUsed: 0.5, short: "claude-5h", shortUsed: 0.2},
		{name: "tie keeps the snapshot", snapshotAt: at(10), endpoint: ptr(endpoint(at(10))), saved: ptr(saved(at(10))), long: "claude-7d", longUsed: 0.5, short: "claude-5h", shortUsed: 0.2},
		{name: "tie between sources keeps the earlier source", endpoint: ptr(endpoint(at(5))), saved: ptr(saved(at(5))), long: "last-known-weekly", longUsed: 0.7, short: "oauth-usage-5h", shortUsed: 0.3},
		{name: "saved older than endpoint without snapshot", endpoint: ptr(endpoint(at(5))), saved: ptr(saved(at(30))), long: "oauth-usage-weekly", longUsed: 0.6, short: "oauth-usage-5h", shortUsed: 0.3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
			if !tc.snapshotAt.IsZero() {
				auth = claudeSnapshotAuth("a", now, tc.snapshotAt, "0.5", 50*time.Hour, "0.2", 2*time.Hour)
			}
			// The service installs the saved readings before the endpoint readings.
			var sources []QuotaSource
			for _, reading := range []*QuotaReading{tc.saved, tc.endpoint} {
				if reading != nil {
					sources = append(sources, mapQuotaSource(map[string]QuotaReading{"a": *reading}))
				}
			}
			usage := authQuotaUsage(auth, now)
			applyQuotaSources(auth, sources, now, &usage)
			if !usage.hasLong || usage.long.label != tc.long || math.Abs(usage.long.used-tc.longUsed) > 1e-9 {
				t.Fatalf("long = %+v (%v), want %s at %v", usage.long, usage.hasLong, tc.long, tc.longUsed)
			}
			if !usage.hasShort || usage.short.label != tc.short || math.Abs(usage.short.used-tc.shortUsed) > 1e-9 {
				t.Fatalf("short = %+v (%v), want %s at %v", usage.short, usage.hasShort, tc.short, tc.shortUsed)
			}
		})
	}

	// A newer saved reading without a short window fills in the snapshot's missing weekly
	// window and leaves its short window alone.
	auth := claudeSnapshotAuth("a", now, at(10), "", 0, "0.2", 2*time.Hour)
	usage := authQuotaUsage(auth, now)
	applyQuotaSources(auth, []QuotaSource{mapQuotaSource(map[string]QuotaReading{"a": saved(at(1))})}, now, &usage)
	if usage.long.label != "last-known-weekly" || usage.short.label != "claude-5h" {
		t.Fatalf("usage = %+v, want the saved weekly and the snapshot 5h window", usage)
	}
}

func ptr[T any](value T) *T {
	return &value
}

func TestApplyQuotaSources_IgnoresUnusableWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-time.Minute)
	tests := []struct {
		name   string
		window QuotaWindowReading
		length time.Duration
	}{
		{name: "NaN usage", window: QuotaWindowReading{Used: math.NaN(), ResetAt: now.Add(time.Hour)}},
		{name: "negative usage", window: QuotaWindowReading{Used: -0.1, ResetAt: now.Add(time.Hour)}},
		{name: "elapsed reset", window: QuotaWindowReading{Used: 0.4, ResetAt: now.Add(-time.Minute)}},
		{name: "no reset", window: QuotaWindowReading{Used: 0.4}},
		{name: "used up without reset", window: QuotaWindowReading{Used: 1}},
		{name: "weekly reset beyond window and slack", window: QuotaWindowReading{Used: 0.4, ResetAt: observedAt.Add(7*24*time.Hour + quotaAwareWindowSlack + time.Minute)}, length: 7 * 24 * time.Hour},
		{name: "5h reset beyond window and slack", window: QuotaWindowReading{Used: 0.4, ResetAt: observedAt.Add(5*time.Hour + quotaAwareWindowSlack + time.Minute)}, length: 5 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window := tc.window
			reading := QuotaReading{ObservedAt: observedAt, Source: "oauth-usage"}
			if tc.length != 5*time.Hour {
				reading.Weekly = &window
			}
			if tc.length != 7*24*time.Hour {
				reading.Short = &window
			}
			source := mapQuotaSource(map[string]QuotaReading{"a": reading})

			bare := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
			usage := authQuotaUsage(bare, now)
			applyQuotaSources(bare, []QuotaSource{source}, now, &usage)
			if usage.hasLong || usage.hasShort {
				t.Fatalf("usage = %+v, want the unusable windows ignored", usage)
			}

			snapshot := claudeSnapshotAuth("a", now, now.Add(-time.Hour), "0.5", 50*time.Hour, "0.2", 2*time.Hour)
			usage = authQuotaUsage(snapshot, now)
			applyQuotaSources(snapshot, []QuotaSource{source}, now, &usage)
			if usage.long.label != "claude-7d" || usage.short.label != "claude-5h" {
				t.Fatalf("usage = %+v, want the older snapshot kept", usage)
			}
		})
	}

	// A usable reading is accepted, with overdrawn usage clamped.
	reading := QuotaReading{Short: &QuotaWindowReading{Used: 1.3, ResetAt: now.Add(time.Hour)}, ObservedAt: observedAt, Source: "oauth-usage"}
	auth := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
	usage := authQuotaUsage(auth, now)
	applyQuotaSources(auth, []QuotaSource{mapQuotaSource(map[string]QuotaReading{"a": reading})}, now, &usage)
	if !usage.hasShort || usage.short.used != 1 || usage.hasLong {
		t.Fatalf("usage = %+v, want a clamped 5h window only", usage)
	}
}

// An endpoint 5h reading saturates a credential whose snapshot has no 5h window, so it
// gets no new session; an endpoint reading older than the snapshot does not.
func TestQuotaAwareSelector_EndpointShortWindowSaturationSkipsCredential(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	urgent := claudeSnapshotAuth("a-urgent", now, now, "0.3", 10*time.Hour, "", 0)
	relaxed := claudeSnapshotAuth("b-relaxed", now, now, "0.3", 100*time.Hour, "", 0)
	auths := []*Auth{urgent, relaxed}
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a-urgent", "a-urgent")

	readings := map[string]QuotaReading{
		"a-urgent": {Short: &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(2 * time.Hour)}, ObservedAt: now.Add(-time.Minute), Source: "oauth-usage"},
	}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(readings))
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "b-relaxed", "b-relaxed")

	// A response since then reported the 5h window as lightly used.
	fresh := claudeSnapshotAuth("a-urgent", now, now, "0.3", 10*time.Hour, "0.1", 2*time.Hour)
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, []*Auth{fresh, relaxed}, "a-urgent", "a-urgent")
}

// A Claude credential with only an endpoint weekly reading has weekly data: it is ranked
// by it and never probed.
func TestQuotaAwareSelector_EndpointWeeklyReadingIsRankedNotProbed(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	unread := &Auth{ID: "a-unread", Provider: "claude", Status: StatusActive}
	urgent := claudeSnapshotAuth("b-urgent", now, now, "0.3", 10*time.Hour, "0.1", 2*time.Hour)
	auths := []*Auth{unread, urgent}

	// Without the reading the credential is probed ahead of the ranking.
	assertPickSequence(t, newTestQuotaAwareSelector(now, nil), cliproxyexecutor.Options{}, auths, "a-unread", "b-urgent")

	readings := map[string]QuotaReading{
		"a-unread": {
			Weekly:     &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(100 * time.Hour)},
			ObservedAt: now.Add(-2 * time.Minute),
			Source:     "oauth-usage",
		},
	}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(readings))
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "b-urgent", "b-urgent", "b-urgent")
	if len(selector.probedAt) != 0 {
		t.Fatalf("probedAt = %v, want no probe of a credential with an endpoint reading", selector.probedAt)
	}

	// Once it is the most urgent, it takes the new sessions.
	readings["a-unread"] = QuotaReading{
		Weekly:     &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(5 * time.Hour)},
		ObservedAt: now.Add(-time.Minute),
		Source:     "oauth-usage",
	}
	assertPickSequence(t, selector, cliproxyexecutor.Options{}, auths, "a-unread", "a-unread")
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

const (
	testFableModel = "claude-fable-5-1"
	testOtherModel = "claude-sonnet-5"
)

// fableQuotaReading is an endpoint reading, observed a minute ago, with the given overall
// weekly usage resetting in 72h, a lightly used 5h window and the given Fable window.
func fableQuotaReading(now time.Time, weeklyUsed float64, fable *QuotaWindowReading) QuotaReading {
	return QuotaReading{
		Weekly:     &QuotaWindowReading{Used: weeklyUsed, ResetAt: now.Add(72 * time.Hour)},
		Short:      &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(2 * time.Hour)},
		Fable:      fable,
		ObservedAt: now.Add(-time.Minute),
		Source:     "oauth-usage",
	}
}

func assertModelPickSequence(t *testing.T, selector Selector, model string, auths []*Auth, want ...string) {
	t.Helper()
	for i, wantID := range want {
		picked, errPick := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatalf("Pick(%s) error = %v", model, errPick)
		}
		if picked == nil || picked.ID != wantID {
			t.Fatalf("pick #%d for %s = %v, want %s", i, model, picked, wantID)
		}
	}
}

func chosenIDs(decision quotaAwareDecision) []string {
	ids := make([]string, 0, len(decision.chosen))
	for _, candidate := range decision.chosen {
		ids = append(ids, candidate.auth.ID)
	}
	return ids
}

// A Fable request goes to the account with the most Fable at risk, not the one with the
// most overall weekly quota at risk; other requests still follow the overall window.
func TestQuotaAwareSelector_FableRequestRanksByFableWindow(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	x := &Auth{ID: "x-little-fable", Provider: "claude", Status: StatusActive}
	y := &Auth{ID: "y-more-fable", Provider: "claude", Status: StatusActive}
	auths := []*Auth{x, y}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(map[string]QuotaReading{
		// 80% of the week left but only 10% of Fable.
		x.ID: fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(72 * time.Hour)}),
		// 40% of the week left and 90% of Fable.
		y.ID: fableQuotaReading(now, 0.6, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
	}))
	assertModelPickSequence(t, selector, testFableModel, auths, y.ID, y.ID, y.ID)
	assertModelPickSequence(t, selector, testOtherModel, auths, x.ID, x.ID, x.ID)

	decision := rankQuotaAware(auths, testFableModel, now, selector.quotaSources(), nil)
	if len(decision.chosen) != 1 || !decision.chosen[0].fable || decision.chosen[0].usage.long.label != "oauth-usage-fable" ||
		math.Abs(decision.chosen[0].usage.long.used-0.1) > 1e-9 || decision.reason != "weekly_pace" {
		t.Fatalf("decision = %+v, want y ranked by its Fable window", decision)
	}
}

// An account whose overall weekly window is used up cannot serve Fable, whatever its Fable
// window says, so it joins the used-up group for a Fable request.
func TestRankQuotaAware_FableRequestOverallWeeklyUsedUp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	unusedFable := &QuotaWindowReading{Used: 0, ResetAt: now.Add(72 * time.Hour)}
	blockedReading := fableQuotaReading(now, 1, unusedFable)
	blockedReading.Weekly.ResetAt = now.Add(30 * time.Hour)
	readings := map[string]QuotaReading{
		"a-blocked": blockedReading,
		"b-heavy":   fableQuotaReading(now, 0.3, &QuotaWindowReading{Used: 0.95, ResetAt: now.Add(72 * time.Hour)}),
	}
	sources := []QuotaSource{mapQuotaSource(readings)}
	blocked := &Auth{ID: "a-blocked", Provider: "claude", Status: StatusActive}
	heavy := &Auth{ID: "b-heavy", Provider: "claude", Status: StatusActive}

	decision := rankQuotaAware([]*Auth{blocked, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "b-heavy" || decision.reason != "weekly_pace" || decision.withWeekly != 2 {
		t.Fatalf("chosen = %v (%s, with_weekly %d), want b-heavy ahead of the blocked account", got, decision.reason, decision.withWeekly)
	}
	alone := rankQuotaAware([]*Auth{blocked}, testFableModel, now, sources, nil)
	if len(alone.chosen) != 1 || alone.reason != "no_weekly_quota_left" || alone.chosen[0].usage.long.label != "oauth-usage-fable" || alone.chosen[0].usage.long.used != 1 {
		t.Fatalf("decision = %+v, want the blocked account used up", alone)
	}

	// A newer response reporting the overall window used up blocks it as well.
	snapshotBlocked := claudeSnapshotAuth("a-blocked", now, now, "1", 30*time.Hour, "0.1", 2*time.Hour)
	readings["a-blocked"] = fableQuotaReading(now, 0.5, unusedFable)
	decision = rankQuotaAware([]*Auth{snapshotBlocked, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "b-heavy" {
		t.Fatalf("chosen = %v, want b-heavy ahead of the account whose snapshot is used up", got)
	}

	// With overall quota left, the unused Fable allowance ranks first.
	decision = rankQuotaAware([]*Auth{blocked, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-blocked" {
		t.Fatalf("chosen = %v, want a-blocked once its overall window has quota left", got)
	}
}

// usedUpWithoutResetReading is an endpoint reading, observed a minute ago, whose overall
// weekly window is reported fully used with no reset time, with the given Fable window.
func usedUpWithoutResetReading(now time.Time, fable *QuotaWindowReading) QuotaReading {
	reading := fableQuotaReading(now, 0, fable)
	reading.Weekly = &QuotaWindowReading{Used: 1}
	return reading
}

// An overall weekly window reported used up without a reset time leaves no Fable, unless a
// usable overall window observed at the same time or later says otherwise.
func TestRankQuotaAware_FableRequestOverallWeeklyUsedUpWithoutReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reportedAt := now.Add(-time.Minute)
	endpoint := map[string]QuotaReading{
		// More Fable left than b-heavy, but the overall window is used up.
		"a-used-up": usedUpWithoutResetReading(now, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
		"b-heavy":   fableQuotaReading(now, 0.3, &QuotaWindowReading{Used: 0.6, ResetAt: now.Add(72 * time.Hour)}),
	}
	saved := map[string]QuotaReading{}
	// The service installs the saved readings before the endpoint readings.
	sources := []QuotaSource{mapQuotaSource(saved), mapQuotaSource(endpoint)}
	usedUp := &Auth{ID: "a-used-up", Provider: "claude", Status: StatusActive}
	heavy := &Auth{ID: "b-heavy", Provider: "claude", Status: StatusActive}

	decision := rankQuotaAware([]*Auth{usedUp, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "b-heavy" || decision.reason != "weekly_pace" || decision.withWeekly != 2 {
		t.Fatalf("chosen = %v (%s, with_weekly %d), want b-heavy ahead of the used-up account", got, decision.reason, decision.withWeekly)
	}
	alone := rankQuotaAware([]*Auth{usedUp}, testFableModel, now, sources, nil)
	if len(alone.chosen) != 1 || alone.reason != "no_weekly_quota_left" || !alone.chosen[0].fable ||
		alone.chosen[0].usage.long.label != "oauth-usage-fable" || alone.chosen[0].usage.long.used != 1 || alone.chosen[0].score != 0 {
		t.Fatalf("decision = %+v, want the used-up account ranked as out of Fable", alone)
	}

	// Usable overall windows older than the report, from a response and a saved reading,
	// do not outrank it.
	saved["a-used-up"] = weeklyReading("last-known", 0.2, now.Add(72*time.Hour), reportedAt.Add(-10*time.Minute))
	older := claudeSnapshotAuth("a-used-up", now, reportedAt.Add(-5*time.Minute), "0.2", 72*time.Hour, "0.1", 2*time.Hour)
	for _, auth := range []*Auth{usedUp, older} {
		decision = rankQuotaAware([]*Auth{auth, heavy}, testFableModel, now, sources, nil)
		if got := chosenIDs(decision); len(got) != 1 || got[0] != "b-heavy" {
			t.Fatalf("chosen = %v, want b-heavy while the report is newer than the usable window", got)
		}
	}

	// A usable overall window observed at the same time or later carries a reset, so it
	// wins: the account is ranked by its Fable window again.
	for name, observedAt := range map[string]time.Time{"same time": reportedAt, "later": now} {
		newer := claudeSnapshotAuth("a-used-up", now, observedAt, "0.2", 72*time.Hour, "0.1", 2*time.Hour)
		decision = rankQuotaAware([]*Auth{newer, heavy}, testFableModel, now, sources, nil)
		if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-used-up" || decision.chosen[0].usage.long.label != "oauth-usage-fable" ||
			math.Abs(decision.chosen[0].usage.long.used-0.1) > 1e-9 {
			t.Fatalf("%s: chosen = %v (%+v), want a-used-up ranked by its Fable window", name, got, decision.chosen)
		}
	}
	saved["a-used-up"] = weeklyReading("last-known", 0.2, now.Add(72*time.Hour), now)
	decision = rankQuotaAware([]*Auth{usedUp, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-used-up" {
		t.Fatalf("chosen = %v, want a-used-up once a newer saved reading has overall quota left", got)
	}
}

// An overall weekly window reported used up without a reset time applies to every model, so
// a non-Fable request ranks the credential as used up too: a last resort behind credentials
// without weekly data, still used when it is the only one, and logged without a reset.
func TestQuotaAwareSelector_OverallWeeklyUsedUpWithoutResetIsLastResort(t *testing.T) {
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.DebugLevel)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	readings := map[string]QuotaReading{
		"a-used-up": usedUpWithoutResetReading(now, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
		"b-heavy":   fableQuotaReading(now, 0.3, &QuotaWindowReading{Used: 0.6, ResetAt: now.Add(72 * time.Hour)}),
	}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(readings))
	usedUp := &Auth{ID: "a-used-up", Provider: "claude", Status: StatusActive}
	heavy := &Auth{ID: "b-heavy", Provider: "claude", Status: StatusActive}
	unread := &Auth{ID: "c-unread", Provider: "claude", Status: StatusActive}

	assertModelPickSequence(t, selector, testOtherModel, []*Auth{usedUp, heavy}, "b-heavy", "b-heavy")
	decision := rankQuotaAware([]*Auth{usedUp, unread}, testOtherModel, now, selector.quotaSources(), nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "c-unread" || decision.reason != "no_weekly_data" || decision.withWeekly != 1 {
		t.Fatalf("chosen = %v (%s, with_weekly %d), want the credential without weekly data first", got, decision.reason, decision.withWeekly)
	}

	hook.Reset()
	assertModelPickSequence(t, selector, testOtherModel, []*Auth{usedUp}, "a-used-up")
	var fields log.Fields
	for _, entry := range hook.AllEntries() {
		if entry.Message == "quota-aware: selected credential" {
			fields = entry.Data
		}
	}
	if fields == nil || fields["reason"] != "no_weekly_quota_left" || fields["weekly_window"] != "oauth-usage-weekly(used up, reset unknown)" ||
		fields["weekly_used_pct"] != 100.0 || fields["required_pct_per_hour"] != 0.0 {
		t.Fatalf("selection fields = %v, want the used-up report logged", fields)
	}
	if _, ok := fields["weekly_reset_at"]; ok {
		t.Fatalf("selection fields = %v, want no reset time for a reset that is unknown", fields)
	}

	// A newer usable overall window ranks it by that window again.
	fresh := claudeSnapshotAuth("a-used-up", now, now, "0.1", 72*time.Hour, "0.1", 2*time.Hour)
	assertModelPickSequence(t, selector, testOtherModel, []*Auth{fresh, heavy}, "a-used-up", "a-used-up")
}

// A Fable window reported used up without a reset time ranks a Fable request as used up,
// unless a usable Fable window observed later says otherwise; other requests are unaffected.
func TestRankQuotaAware_FableWindowUsedUpWithoutReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	endpoint := map[string]QuotaReading{
		"a-no-fable": fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 1}),
		"b-heavy":    fableQuotaReading(now, 0.5, &QuotaWindowReading{Used: 0.6, ResetAt: now.Add(72 * time.Hour)}),
	}
	other := map[string]QuotaReading{}
	sources := []QuotaSource{mapQuotaSource(endpoint), mapQuotaSource(other)}
	noFable := &Auth{ID: "a-no-fable", Provider: "claude", Status: StatusActive}
	heavy := &Auth{ID: "b-heavy", Provider: "claude", Status: StatusActive}

	decision := rankQuotaAware([]*Auth{noFable, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "b-heavy" {
		t.Fatalf("chosen = %v, want b-heavy for a Fable request", got)
	}
	alone := rankQuotaAware([]*Auth{noFable}, testFableModel, now, sources, nil)
	if len(alone.chosen) != 1 || alone.reason != "no_weekly_quota_left" || alone.chosen[0].usage.long.label != "oauth-usage-fable(used up, reset unknown)" {
		t.Fatalf("decision = %+v, want the account out of Fable", alone)
	}
	decision = rankQuotaAware([]*Auth{noFable, heavy}, testOtherModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-no-fable" || decision.chosen[0].usage.long.label != "oauth-usage-weekly" {
		t.Fatalf("chosen = %v, want a-no-fable ranked by its overall window for other models", got)
	}

	// A newer usable Fable window wins.
	other["a-no-fable"] = QuotaReading{Fable: &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}, ObservedAt: now, Source: "other"}
	decision = rankQuotaAware([]*Auth{noFable, heavy}, testFableModel, now, sources, nil)
	if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-no-fable" || decision.chosen[0].usage.long.label != "other-fable" {
		t.Fatalf("chosen = %v, want a-no-fable ranked by its newer Fable window", got)
	}
}

// Without a Fable reading a Fable request is ranked by the overall weekly window.
func TestRankQuotaAware_FableRequestWithoutFableReadingUsesOverallWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	sources := []QuotaSource{mapQuotaSource(map[string]QuotaReading{
		"a-roomy":   fableQuotaReading(now, 0.2, nil),
		"b-drained": fableQuotaReading(now, 0.6, nil),
	})}
	auths := []*Auth{
		{ID: "a-roomy", Provider: "claude", Status: StatusActive},
		{ID: "b-drained", Provider: "claude", Status: StatusActive},
		claudeSnapshotAuth("c-snapshot", now, now, "0.7", 72*time.Hour, "0.1", 2*time.Hour),
	}
	fable := rankQuotaAware(auths, testFableModel, now, sources, nil)
	other := rankQuotaAware(auths, testOtherModel, now, sources, nil)
	for _, decision := range []quotaAwareDecision{fable, other} {
		if got := chosenIDs(decision); len(got) != 1 || got[0] != "a-roomy" || decision.reason != "weekly_pace" {
			t.Fatalf("chosen = %v (%s), want a-roomy for Fable and other models alike", got, decision.reason)
		}
	}
	if fable.chosen[0].fable || fable.chosen[0].usage.long.label != "oauth-usage-weekly" || fable.chosen[0].score != other.chosen[0].score {
		t.Fatalf("fable candidate = %+v, want the overall window, no Fable ranking and the same score as other models", fable.chosen[0])
	}
}

// The 5h window is overall, so a saturated one still keeps new Fable sessions away.
func TestQuotaAwareSelector_FableRequestShortWindowSaturationSkipsCredential(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	saturated := fableQuotaReading(now, 0.6, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)})
	saturated.Short = &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(2 * time.Hour)}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(map[string]QuotaReading{
		"x-little-fable": fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(72 * time.Hour)}),
		"y-more-fable":   saturated,
	}))
	auths := []*Auth{
		{ID: "x-little-fable", Provider: "claude", Status: StatusActive},
		{ID: "y-more-fable", Provider: "claude", Status: StatusActive},
	}
	assertModelPickSequence(t, selector, testFableModel, auths, "x-little-fable", "x-little-fable")
	if decision := rankQuotaAware(auths, testFableModel, now, selector.quotaSources(), nil); decision.shortSaturated != 1 {
		t.Fatalf("short saturated = %d, want 1", decision.shortSaturated)
	}
}

// The Fable window follows the weekly window's rules: the newest usable reading wins, ties
// keep the earlier source, usage is clamped, and unusable windows are ignored.
func TestApplyQuotaSources_FableWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-time.Minute)
	reading := func(source string, observedAt time.Time, fable QuotaWindowReading) QuotaReading {
		return QuotaReading{
			Weekly:     &QuotaWindowReading{Used: 0.3, ResetAt: now.Add(50 * time.Hour)},
			Fable:      &fable,
			ObservedAt: observedAt,
			Source:     source,
		}
	}
	usageFor := func(readings ...QuotaReading) quotaUsage {
		auth := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
		var sources []QuotaSource
		for _, r := range readings {
			sources = append(sources, mapQuotaSource(map[string]QuotaReading{"a": r}))
		}
		usage := authQuotaUsage(auth, now)
		applyQuotaSources(auth, sources, now, &usage)
		usage.rankByFable()
		return usage
	}

	unusable := []struct {
		name  string
		fable QuotaWindowReading
	}{
		{name: "NaN usage", fable: QuotaWindowReading{Used: math.NaN(), ResetAt: now.Add(time.Hour)}},
		{name: "negative usage", fable: QuotaWindowReading{Used: -0.1, ResetAt: now.Add(time.Hour)}},
		{name: "elapsed reset", fable: QuotaWindowReading{Used: 0.4, ResetAt: now.Add(-time.Minute)}},
		{name: "no reset", fable: QuotaWindowReading{Used: 0.4}},
		{name: "reset beyond a week and slack", fable: QuotaWindowReading{Used: 0.4, ResetAt: observedAt.Add(7*24*time.Hour + quotaAwareWindowSlack + time.Minute)}},
	}
	for _, tc := range unusable {
		t.Run(tc.name, func(t *testing.T) {
			usage := usageFor(reading("oauth-usage", observedAt, tc.fable))
			if usage.hasFable || usage.long.label != "oauth-usage-weekly" {
				t.Fatalf("usage = %+v, want the unusable Fable window ignored", usage)
			}
		})
	}

	newer := usageFor(
		reading("older", now.Add(-10*time.Minute), QuotaWindowReading{Used: 0.2, ResetAt: now.Add(40 * time.Hour)}),
		reading("newer", now.Add(-5*time.Minute), QuotaWindowReading{Used: 1.3, ResetAt: now.Add(40 * time.Hour)}),
	)
	if newer.long.label != "newer-fable" || newer.long.used != 1 || !newer.long.resetAt.Equal(now.Add(40*time.Hour)) {
		t.Fatalf("long = %+v, want the newer, clamped Fable window", newer.long)
	}
	tie := usageFor(
		reading("first", observedAt, QuotaWindowReading{Used: 0.2, ResetAt: now.Add(40 * time.Hour)}),
		reading("second", observedAt, QuotaWindowReading{Used: 0.4, ResetAt: now.Add(40 * time.Hour)}),
	)
	if tie.long.label != "first-fable" {
		t.Fatalf("long = %+v, want the earlier source on a tie", tie.long)
	}
	// A usable older reading is kept when a newer one has an unusable Fable window.
	kept := usageFor(
		reading("older", now.Add(-10*time.Minute), QuotaWindowReading{Used: 0.2, ResetAt: now.Add(40 * time.Hour)}),
		reading("newer", now.Add(-5*time.Minute), QuotaWindowReading{Used: 0.4, ResetAt: now.Add(-time.Minute)}),
	)
	if kept.long.label != "older-fable" {
		t.Fatalf("long = %+v, want the older usable Fable window", kept.long)
	}
}

// A request is a Fable request for a credential by the upstream model the credential
// registered under the route model, else by the route model's name.
func TestQuotaAwareFableRequest(t *testing.T) {
	t.Parallel()
	aliased := &Auth{ID: "fable-alias-" + t.Name(), Provider: "claude"}
	other := &Auth{ID: "fable-other-" + t.Name(), Provider: "claude"}
	registry.GetGlobalRegistry().RegisterClient(aliased.ID, "claude", []*registry.ModelInfo{
		{ID: "my-fable", MetadataModelID: testFableModel},
		{ID: "best", MetadataModelID: "Claude-FABLE-5-1"},
		{ID: "fable-lookalike", MetadataModelID: testOtherModel},
		{ID: "claude-fable-5"},
		{ID: testOtherModel},
	})
	registry.GetGlobalRegistry().RegisterClient(other.ID, "claude", []*registry.ModelInfo{{ID: testOtherModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(aliased.ID)
		registry.GetGlobalRegistry().UnregisterClient(other.ID)
	})

	cases := []struct {
		name  string
		auth  *Auth
		model string
		want  bool
	}{
		{name: "alias to a Fable model", auth: aliased, model: "my-fable", want: true},
		{name: "alias with other case and a thinking suffix", auth: aliased, model: "My-Fable(high)", want: true},
		{name: "upstream model decides case-insensitively", auth: aliased, model: "best", want: true},
		{name: "alias to a non-Fable model", auth: aliased, model: "fable-lookalike", want: false},
		{name: "registered Fable model without alias", auth: aliased, model: "claude-fable-5", want: true},
		{name: "registered non-Fable model", auth: aliased, model: testOtherModel, want: false},
		{name: "model not registered for the credential is decided by name", auth: aliased, model: testFableModel, want: true},
		{name: "unregistered non-Fable name", auth: other, model: "best", want: false},
		{name: "unregistered Fable name", auth: other, model: "my-fable", want: true},
		{name: "nil credential is decided by name", auth: nil, model: testFableModel, want: true},
		{name: "empty model", auth: aliased, model: "", want: false},
	}
	for _, tc := range cases {
		if got := quotaAwareFableRequest(tc.auth, tc.model, nil); got != tc.want {
			t.Errorf("%s: quotaAwareFableRequest(%q) = %v, want %v", tc.name, tc.model, got, tc.want)
		}
	}
}

// A suffixed alias is matched exactly before the route model without its suffix, whatever
// the registration order, as alias resolution does.
func TestQuotaAwareFableRequestPrefersExactSuffixedAlias(t *testing.T) {
	t.Parallel()
	orders := map[string][]*registry.ModelInfo{
		"plain first": {
			{ID: "best", MetadataModelID: testOtherModel},
			{ID: "best(high)", MetadataModelID: testFableModel},
		},
		"suffixed first": {
			{ID: "best(high)", MetadataModelID: testFableModel},
			{ID: "best", MetadataModelID: testOtherModel},
		},
	}
	for name, models := range orders {
		auth := &Auth{ID: "fable-suffixed-" + name + "-" + t.Name(), Provider: "claude"}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		for model, want := range map[string]bool{"best(high)": true, "Best(High)": true, "best": false, "best(low)": false} {
			if got := quotaAwareFableRequest(auth, model, nil); got != want {
				t.Errorf("%s: quotaAwareFableRequest(%q) = %v, want %v", name, model, got, want)
			}
		}
	}
}

// With a resolver, the resolved upstream model decides, case-insensitively and without its
// thinking suffix; the route model and the registered models no longer matter.
func TestQuotaAwareFableRequestWithResolver(t *testing.T) {
	t.Parallel()
	auth := &Auth{ID: "fable-resolver-" + t.Name(), Provider: "claude"}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
		{ID: "my-fable", MetadataModelID: testFableModel},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	resolveTo := func(upstream string) func(*Auth) string {
		return func(*Auth) string { return upstream }
	}
	cases := []struct {
		name     string
		model    string
		upstream string
		want     bool
	}{
		{name: "alias to a Fable model", model: "big", upstream: "Claude-FABLE-5-1(high)", want: true},
		{name: "registered Fable alias sent as another model", model: "my-fable", upstream: testOtherModel, want: false},
		{name: "route name mentioning Fable", model: "fable-fast", upstream: testOtherModel, want: false},
		{name: "Fable only in the thinking suffix", model: "big", upstream: testOtherModel + "(fable)", want: false},
	}
	for _, tc := range cases {
		if got := quotaAwareFableRequest(auth, tc.model, resolveTo(tc.upstream)); got != tc.want {
			t.Errorf("%s: quotaAwareFableRequest(%q -> %q) = %v, want %v", tc.name, tc.model, tc.upstream, got, tc.want)
		}
	}
}

// Pick reads the manager's resolver from the context. Only credentials with a Fable window
// are resolved; without a resolver the registered models decide, as before.
func TestQuotaAwareSelector_PickUsesUpstreamModelResolver(t *testing.T) {
	t.Parallel()
	now := quotaAwareTestBase()
	x := &Auth{ID: "x-little-fable-" + t.Name(), Provider: "claude", Status: StatusActive}
	y := &Auth{ID: "y-more-fable-" + t.Name(), Provider: "claude", Status: StatusActive}
	z := &Auth{ID: "z-no-fable-" + t.Name(), Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{x, y, z} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
			{ID: "my-fable", MetadataModelID: testFableModel},
		})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(map[string]QuotaReading{
		x.ID: fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(72 * time.Hour)}),
		y.ID: fableQuotaReading(now, 0.6, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
		z.ID: fableQuotaReading(now, 0.9, nil),
	}))
	auths := []*Auth{x, y, z}

	assertModelPickSequence(t, selector, "my-fable", auths, y.ID)
	assertModelPickSequence(t, selector, "big", auths, x.ID)

	var resolved []string
	pick := func(model, upstream string) string {
		t.Helper()
		ctx := withUpstreamModelResolver(context.Background(), func(auth *Auth) string {
			resolved = append(resolved, auth.ID)
			return upstream
		})
		picked, errPick := selector.Pick(ctx, "claude", model, cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatalf("Pick(%s) error = %v", model, errPick)
		}
		return picked.ID
	}
	if got := pick("my-fable", testOtherModel); got != x.ID {
		t.Fatalf("pick for my-fable sent as %s = %s, want %s by the overall window", testOtherModel, got, x.ID)
	}
	if got := pick("big", testFableModel); got != y.ID {
		t.Fatalf("pick for big sent as %s = %s, want %s by the Fable window", testFableModel, got, y.ID)
	}
	if len(resolved) != 4 {
		t.Fatalf("resolved %v, want x and y once per pick", resolved)
	}
	for _, id := range resolved {
		if id == z.ID {
			t.Fatalf("resolved %v, want no resolution for a credential without a Fable window", resolved)
		}
	}
}

// End to end through Pick: an aliased Fable route model ranks by the Fable window and logs
// it, while an alias to a non-Fable upstream ranks by the overall window.
func TestQuotaAwareSelector_PickLogsFableRanking(t *testing.T) {
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.DebugLevel)
	now := quotaAwareTestBase()
	x := &Auth{ID: "x-little-fable-" + t.Name(), Provider: "claude", Status: StatusActive}
	y := &Auth{ID: "y-more-fable-" + t.Name(), Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{x, y} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
			{ID: "my-fable", MetadataModelID: testFableModel},
			{ID: "fable-lookalike", MetadataModelID: testOtherModel},
		})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	selector := newTestQuotaAwareSelector(now, nil)
	selector.SetQuotaSources(mapQuotaSource(map[string]QuotaReading{
		x.ID: fableQuotaReading(now, 0.2, &QuotaWindowReading{Used: 0.9, ResetAt: now.Add(72 * time.Hour)}),
		y.ID: fableQuotaReading(now, 0.6, &QuotaWindowReading{Used: 0.1, ResetAt: now.Add(72 * time.Hour)}),
	}))
	auths := []*Auth{x, y}

	pickEntry := func(model, wantID string) log.Fields {
		t.Helper()
		hook.Reset()
		assertModelPickSequence(t, selector, model, auths, wantID)
		for _, entry := range hook.AllEntries() {
			if entry.Message == "quota-aware: selected credential" && entry.Data["model"] == model {
				return entry.Data
			}
		}
		t.Fatalf("no quota-aware selection logged for %s", model)
		return nil
	}

	fields := pickEntry("my-fable", y.ID)
	if fields["fable_request"] != true || fields["weekly_window"] != "oauth-usage-fable" || fields["weekly_used_pct"] != 10.0 ||
		fields["reason"] != "weekly_pace" || fields["short_window"] != "oauth-usage-5h" || fields["auth"] != y.ID {
		t.Fatalf("Fable selection fields = %v", fields)
	}
	fields = pickEntry("fable-lookalike", x.ID)
	if _, ok := fields["fable_request"]; ok || fields["weekly_window"] != "oauth-usage-weekly" || fields["weekly_used_pct"] != 20.0 || fields["auth"] != x.ID {
		t.Fatalf("non-Fable selection fields = %v", fields)
	}
}
