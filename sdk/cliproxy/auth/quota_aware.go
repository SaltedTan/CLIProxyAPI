package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudeplan"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// quotaAwareLongWindowMin is the shortest window treated as the subscription budget.
	// Shorter windows (the 5h Claude/Codex windows, Devin's daily quota) are rate caps.
	quotaAwareLongWindowMin = 24 * time.Hour
	// quotaAwareWindowSlack tolerates clock skew when checking that an observed reset lies
	// within one window of the observation that reported it.
	quotaAwareWindowSlack = time.Hour
	// quotaAwareShortSaturation is the short-window usage at which a credential stops
	// receiving new sessions, because a long session started there would likely hit the cap.
	quotaAwareShortSaturation = 0.85
	// quotaAwareShortResetGrace keeps a saturated short window eligible when it resets soon.
	quotaAwareShortResetGrace = 15 * time.Minute
	// quotaAwareNearTieRatio treats credentials whose score is within this ratio of
	// the most urgent one as equally urgent, so bursts of new sessions are spread across them.
	quotaAwareNearTieRatio = 0.8
	// quotaAwareMinHorizon bounds the required pace for resets that are moments away.
	quotaAwareMinHorizon = time.Minute
	// quotaAwareProbeInterval is how often a credential without usable weekly data is sent a
	// new session while others have data. Its quota is then only observed from its responses,
	// so without probes such a credential would never be chosen and never report its quota.
	quotaAwareProbeInterval = 30 * time.Minute
)

// QuotaAwareSelector routes new work by subscription pace. For each eligible credential it
// reads the quota windows (see below) and computes how fast the remaining long-lived
// (weekly) quota must be used to avoid losing it at the reset:
//
//	required pace = remaining weekly fraction / time until the weekly reset
//	score         = required pace * plan size
//
// Usage is reported as a percentage of each subscription's own limit, so the plan size
// expresses its relative weekly allowance: a size-4 credential's remaining percent is worth
// four times a size-1 credential's. A Claude credential with a known plan is sized by the
// plan's allowance in Claude Pro units (Pro 1, Team 1.25, Max 5x 5, Max 20x 10); any other
// credential by its weight (default 1). A weight of zero leaves the credential with no quota
// to protect, so it is only used as a last resort.
//
// Selection, applied only to credentials the shared eligibility checks consider available:
//  1. Credentials whose short window (5h, or Devin's daily quota) is at least 85% used and
//     does not reset within 15 minutes are skipped, unless every candidate is in that state.
//  2. Credentials with weekly data, quota left, and a positive plan size rank first, by highest
//     score. Credentials within 80% of the highest score are rotated by the fallback selector.
//  3. Credentials without usable weekly data come next, then credentials whose weekly quota
//     is used up or whose plan size is zero; each group is rotated by the fallback selector.
//
// Each of the weekly and short windows comes from the newest usable of several readings:
// the passive quota snapshot (Auth.Quota.Signals) and the readings of the quota sources
// (see SetQuotaSources), such as a usage endpoint or a reading saved across restarts.
// Source readings follow the snapshot's rules; on equal observation times the snapshot
// wins, then the earlier source. The snapshot is cleared by a restart and by an auth file
// reload that changes the account; other reloads (such as the token refreshes that rewrite
// the file) keep it.
//
// Rule 2 alone would starve a credential without weekly data from any reading (for example
// after a restart, or once its weekly window rolled over), since its quota is then only
// observed from its responses. So while some candidates have weekly data, a positive-size
// credential without it is probed: it takes precedence over the ranking for one new session
// per quotaAwareProbeInterval.
//
// The selector is stateless with respect to sessions. When session affinity is enabled it
// runs only for unbound sessions and failover rebinding; established bindings never reach it.
type QuotaAwareSelector struct {
	fallback Selector
	nowFunc  func() time.Time

	mu       sync.Mutex
	probedAt map[string]time.Time // last probe time by auth ID
	sources  []QuotaSource
}

// QuotaWindowReading is one quota window outside the passive snapshot.
type QuotaWindowReading struct {
	// Used is the fraction of the window's quota consumed.
	Used    float64
	ResetAt time.Time
}

// QuotaReading is a credential's quota windows as known outside its passive quota
// snapshot, for example from a usage endpoint or a reading saved across restarts.
type QuotaReading struct {
	// Weekly is the weekly window; nil when unknown.
	Weekly *QuotaWindowReading
	// Short is the 5h window; nil when unknown.
	Short *QuotaWindowReading
	// ObservedAt is when the reading was taken.
	ObservedAt time.Time
	// Source prefixes the reading's window labels in logs, for example "oauth-usage".
	Source string
}

// QuotaSource returns a credential's quota reading. It is called on the selection path
// without manager locks and must not block on the network. A source keyed by auth ID
// should check that its reading describes the credential's current upstream account
// (see SameQuotaAccount).
type QuotaSource func(auth *Auth) (QuotaReading, bool)

// SameQuotaAccount reports whether incoming draws quota from the same upstream account as
// existing (see sameQuotaAccount).
func SameQuotaAccount(existing, incoming *Auth) bool {
	return sameQuotaAccount(existing, incoming)
}

// NewQuotaAwareSelector creates a quota-aware selector. A nil fallback defaults to round-robin.
func NewQuotaAwareSelector(fallback Selector) *QuotaAwareSelector {
	if fallback == nil {
		fallback = &RoundRobinSelector{}
	}
	return &QuotaAwareSelector{fallback: fallback}
}

// SetQuotaSources sets the readings consulted besides each credential's quota snapshot,
// replacing earlier ones. Nil sources are ignored.
func (s *QuotaAwareSelector) SetQuotaSources(sources ...QuotaSource) {
	if s == nil {
		return
	}
	kept := make([]QuotaSource, 0, len(sources))
	for _, source := range sources {
		if source != nil {
			kept = append(kept, source)
		}
	}
	s.mu.Lock()
	s.sources = kept
	s.mu.Unlock()
}

func (s *QuotaAwareSelector) quotaSources() []QuotaSource {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sources
}

func (s *QuotaAwareSelector) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

func (s *QuotaAwareSelector) fallbackSelector() Selector {
	if s == nil || s.fallback == nil {
		return &RoundRobinSelector{}
	}
	return s.fallback
}

// quotaWindow is one observed quota window of a credential.
type quotaWindow struct {
	label      string
	used       float64 // fraction of the window's quota consumed, clamped to [0, 1]
	resetAt    time.Time
	observedAt time.Time
}

// quotaUsage is the subscription state of one credential.
type quotaUsage struct {
	long     quotaWindow
	hasLong  bool
	short    quotaWindow
	hasShort bool
}

// requiredPace returns the remaining long-window fraction per hour until the reset.
func (u quotaUsage) requiredPace(now time.Time) float64 {
	horizon := u.long.resetAt.Sub(now)
	if horizon < quotaAwareMinHorizon {
		horizon = quotaAwareMinHorizon
	}
	return (1 - u.long.used) / horizon.Hours()
}

func (u quotaUsage) shortSaturated(now time.Time) bool {
	return u.hasShort && u.short.used >= quotaAwareShortSaturation && u.short.resetAt.Sub(now) > quotaAwareShortResetGrace
}

type quotaAwareCandidate struct {
	auth  *Auth
	usage quotaUsage
	score float64 // required pace scaled by the plan size
}

// quotaAwareDecision describes which candidates remain after ranking.
type quotaAwareDecision struct {
	chosen         []quotaAwareCandidate
	unknown        []quotaAwareCandidate // candidates without usable weekly data
	reason         string
	shortSaturated int
	withWeekly     int
}

// rankQuotaAware applies the selection rules to available candidates. Input order only
// matters within the returned group, which keeps the ID-sorted availability order.
// sources supply readings besides the snapshot; per window the newest usable one wins.
func rankQuotaAware(auths []*Auth, now time.Time, sources []QuotaSource) quotaAwareDecision {
	all := make([]quotaAwareCandidate, 0, len(auths))
	for _, auth := range auths {
		candidate := quotaAwareCandidate{auth: auth, usage: authQuotaUsage(auth, now)}
		applyQuotaSources(auth, sources, now, &candidate.usage)
		if candidate.usage.hasLong {
			candidate.score = candidate.usage.requiredPace(now) * quotaAwarePlanSize(auth)
		}
		all = append(all, candidate)
	}
	decision := quotaAwareDecision{}
	candidates := make([]quotaAwareCandidate, 0, len(all))
	for _, candidate := range all {
		if candidate.usage.shortSaturated(now) {
			decision.shortSaturated++
			continue
		}
		candidates = append(candidates, candidate)
	}
	// When every candidate is near its short-window cap, skipping them all would leave
	// nothing to choose; rank them on weekly pace instead.
	allSaturated := len(candidates) == 0
	if allSaturated {
		candidates = all
	}

	var withQuota, unknown, usedUp []quotaAwareCandidate
	bestScore := 0.0
	for _, candidate := range candidates {
		switch {
		case !candidate.usage.hasLong:
			unknown = append(unknown, candidate)
		case candidate.score <= 0:
			decision.withWeekly++
			usedUp = append(usedUp, candidate)
		default:
			decision.withWeekly++
			withQuota = append(withQuota, candidate)
			if candidate.score > bestScore {
				bestScore = candidate.score
			}
		}
	}

	switch {
	case len(withQuota) > 0:
		for _, candidate := range withQuota {
			if candidate.score >= bestScore*quotaAwareNearTieRatio {
				decision.chosen = append(decision.chosen, candidate)
			}
		}
		decision.reason = "weekly_pace"
	case len(unknown) > 0:
		decision.chosen = unknown
		decision.reason = "no_weekly_data"
	default:
		decision.chosen = usedUp
		decision.reason = "no_weekly_quota_left"
	}
	if allSaturated && len(auths) > 0 {
		decision.reason += ",all_short_saturated"
	}
	decision.unknown = unknown
	return decision
}

// Pick selects the available credential whose remaining subscription quota is most at risk
// of being lost at its reset.
func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	decision := rankQuotaAware(available, now, s.quotaSources())
	probes := s.dueProbes(decision, now)
	if len(probes) > 0 {
		decision.chosen = probes
		decision.reason = "probe_no_weekly_data"
	}
	if len(decision.chosen) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	picked := decision.chosen[0]
	if len(decision.chosen) > 1 {
		// Availability is settled above, so the fallback must not re-evaluate it against a
		// different clock or route model.
		if ctx == nil {
			ctx = context.Background()
		}
		group := make([]*Auth, 0, len(decision.chosen))
		for _, candidate := range decision.chosen {
			group = append(group, candidate.auth)
		}
		fallbackCtx := context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
		selected, errPick := s.fallbackSelector().Pick(fallbackCtx, provider, model, opts, group)
		if errPick != nil {
			return nil, errPick
		}
		if selected == nil {
			return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		picked = quotaAwareCandidate{auth: selected}
		for _, candidate := range decision.chosen {
			if candidate.auth == selected {
				picked = candidate
				break
			}
		}
	}
	if len(probes) > 0 {
		s.recordProbe(picked.auth.ID, now)
	}

	fields := log.Fields{
		"provider":        provider,
		"model":           model,
		"auth":            picked.auth.ID,
		"reason":          decision.reason,
		"candidates":      len(available),
		"with_weekly":     decision.withWeekly,
		"short_saturated": decision.shortSaturated,
		"rotated_among":   len(decision.chosen),
		"session":         quotaAwareSessionField(opts.Metadata),
	}
	if picked.usage.hasLong {
		fields["weekly_window"] = picked.usage.long.label
		fields["weekly_used_pct"] = roundPercent(picked.usage.long.used)
		fields["weekly_reset_at"] = picked.usage.long.resetAt.UTC().Format(time.RFC3339)
		fields["required_pct_per_hour"] = math.Round(picked.usage.requiredPace(now)*10000) / 100
		fields["plan_size"] = quotaAwarePlanSize(picked.auth)
	}
	if picked.usage.hasShort {
		fields["short_window"] = picked.usage.short.label
		fields["short_used_pct"] = roundPercent(picked.usage.short.used)
		fields["short_reset_at"] = picked.usage.short.resetAt.UTC().Format(time.RFC3339)
	}
	selectorLogEntry(ctx).WithFields(fields).Debug("quota-aware: selected credential")
	note := routingPickNoteFromContext(ctx)
	note.setStrategyReason(decision.reason)
	note.setCandidates(len(available))
	return picked.auth, nil
}

// dueProbes returns the candidates without usable weekly data that are due a probe. Probes
// are only needed while other candidates have weekly data; otherwise the unknown group is
// already the one rotated. Zero-size credentials are last resorts and are never probed.
func (s *QuotaAwareSelector) dueProbes(decision quotaAwareDecision, now time.Time) []quotaAwareCandidate {
	if s == nil || decision.withWeekly == 0 || len(decision.unknown) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []quotaAwareCandidate
	for _, candidate := range decision.unknown {
		if quotaAwarePlanSize(candidate.auth) <= 0 {
			continue
		}
		if last, ok := s.probedAt[candidate.auth.ID]; ok && now.Sub(last) < quotaAwareProbeInterval {
			continue
		}
		due = append(due, candidate)
	}
	return due
}

// recordProbe marks a credential as probed and drops expired probe entries. Concurrent picks
// may probe the same credential twice before either records it, which is harmless.
func (s *QuotaAwareSelector) recordProbe(authID string, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probedAt == nil {
		s.probedAt = make(map[string]time.Time)
	}
	for id, last := range s.probedAt {
		if now.Sub(last) >= quotaAwareProbeInterval {
			delete(s.probedAt, id)
		}
	}
	s.probedAt[authID] = now
}

// quotaAwarePlanSize returns the credential's weekly allowance relative to the others. A
// Claude credential with a known plan uses the plan's allowance in Claude Pro units, the
// same figure the client usage report shows; any other credential uses its weight. A
// non-positive weight always yields zero, which makes the credential a last resort.
func quotaAwarePlanSize(auth *Auth) float64 {
	weight := authWeight(auth)
	if weight <= 0 {
		return 0
	}
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		plan, _ := claudeplan.Resolve(auth.Metadata)
		if units, ok := claudeplan.ProUnits(plan); ok {
			return units
		}
	}
	return float64(weight)
}

func roundPercent(fraction float64) float64 {
	return math.Round(fraction*1000) / 10
}

// authQuotaUsage reads the credential-wide long and short windows from the passive quota
// snapshot. The signals consulted are:
//
//   - Codex: the base (non-additional, non-code-review) primary and secondary windows. The
//     largest window of at least 24h is the long window and the largest shorter window is the
//     short window. Usage is X-Codex-*-Used-Percent; the reset is X-Codex-*-Reset-At (unix
//     seconds), otherwise ObservedAt plus X-Codex-*-Reset-After-Seconds.
//   - Claude: Anthropic-Ratelimit-Unified-7d-* (long) and -5h-* (short), each from its
//     Utilization fraction and Reset timestamp. Model-specific windows (7d_oi) are ignored.
//   - Devin: weekly_quota_* (long) and daily_quota_* (short), from the remaining percent and
//     reset timestamp.
//
// A window is usable only with both usage and a reset that is in the future and no further
// than one window after the observation. An elapsed reset means the window rolled over and
// its new state is unknown until another response is observed.
func authQuotaUsage(auth *Auth, now time.Time) quotaUsage {
	var usage quotaUsage
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return usage
	}
	signals := auth.Quota.Signals
	observedAt := auth.Quota.ObservedAt

	if codexQuotaUsage(signals, observedAt, now, &usage) {
		return usage
	}
	if window, ok := observedQuotaWindow("claude-7d", claudeUsed(signals, "7d"), quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset"), 7*24*time.Hour, observedAt, now); ok {
		usage.long, usage.hasLong = window, true
	}
	if window, ok := observedQuotaWindow("claude-5h", claudeUsed(signals, "5h"), quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-5h-Reset"), 5*time.Hour, observedAt, now); ok {
		usage.short, usage.hasShort = window, true
	}
	if usage.hasLong || usage.hasShort {
		return usage
	}
	if window, ok := observedQuotaWindow("devin-weekly", devinUsed(signals, "weekly"), quotaSignalValue(signals, "weekly_quota_reset_at"), 7*24*time.Hour, observedAt, now); ok {
		usage.long, usage.hasLong = window, true
	}
	if window, ok := observedQuotaWindow("devin-daily", devinUsed(signals, "daily"), quotaSignalValue(signals, "daily_quota_reset_at"), 24*time.Hour, observedAt, now); ok {
		usage.short, usage.hasShort = window, true
	}
	return usage
}

// applyQuotaSources replaces the weekly and short windows of usage, each on its own, by a
// usable source reading observed strictly later. Ties keep the window already held, so the
// snapshot wins over a source and an earlier source over a later one.
func applyQuotaSources(auth *Auth, sources []QuotaSource, now time.Time, usage *quotaUsage) {
	if auth == nil {
		return
	}
	for _, source := range sources {
		reading, ok := source(auth)
		if !ok {
			continue
		}
		if window, okWindow := sourcedQuotaWindow(reading.Source+"-weekly", reading.Weekly, 7*24*time.Hour, reading.ObservedAt, now); okWindow && (!usage.hasLong || window.observedAt.After(usage.long.observedAt)) {
			usage.long, usage.hasLong = window, true
		}
		if window, okWindow := sourcedQuotaWindow(reading.Source+"-5h", reading.Short, 5*time.Hour, reading.ObservedAt, now); okWindow && (!usage.hasShort || window.observedAt.After(usage.short.observedAt)) {
			usage.short, usage.hasShort = window, true
		}
	}
}

// sourcedQuotaWindow reads one window of a source reading. It must be as usable as a
// snapshot's: known usage and a reset in the future, within one window of the reading.
func sourcedQuotaWindow(label string, reading *QuotaWindowReading, window time.Duration, observedAt, now time.Time) (quotaWindow, bool) {
	if reading == nil || math.IsNaN(reading.Used) || reading.Used < 0 || !validQuotaWindowReset(reading.ResetAt, window, observedAt, now) {
		return quotaWindow{}, false
	}
	return quotaWindow{label: label, used: math.Min(reading.Used, 1), resetAt: reading.ResetAt, observedAt: observedAt}, true
}

func codexQuotaUsage(signals map[string]string, observedAt, now time.Time, usage *quotaUsage) bool {
	found := false
	var longWindow, shortWindow time.Duration
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		minutes, errMinutes := strconv.ParseInt(quotaSignalValue(signals, prefix+"Window-Minutes"), 10, 64)
		if errMinutes != nil || minutes <= 0 || minutes > math.MaxInt64/int64(time.Minute) {
			continue
		}
		window := time.Duration(minutes) * time.Minute
		used, okUsed := parseUsedFraction(quotaSignalValue(signals, prefix+"Used-Percent"), 100)
		resetAt, okReset := parseQuotaResetTimestamp(quotaSignalValue(signals, prefix+"Reset-At"))
		if !okReset {
			resetAt, okReset = codexResetAfter(quotaSignalValue(signals, prefix+"Reset-After-Seconds"), observedAt)
		}
		found = true
		if !okUsed || !okReset || !validQuotaWindowReset(resetAt, window, observedAt, now) {
			continue
		}
		label := "codex-" + strings.ToLower(name)
		if window >= quotaAwareLongWindowMin {
			if window > longWindow {
				usage.long, usage.hasLong, longWindow = quotaWindow{label: label, used: used, resetAt: resetAt, observedAt: observedAt}, true, window
			}
		} else if window > shortWindow {
			usage.short, usage.hasShort, shortWindow = quotaWindow{label: label, used: used, resetAt: resetAt, observedAt: observedAt}, true, window
		}
	}
	return found
}

func observedQuotaWindow(label string, used float64, rawReset string, window time.Duration, observedAt, now time.Time) (quotaWindow, bool) {
	if used < 0 || rawReset == "" {
		return quotaWindow{}, false
	}
	resetAt, ok := parseQuotaResetTimestamp(rawReset)
	if !ok || !validQuotaWindowReset(resetAt, window, observedAt, now) {
		return quotaWindow{}, false
	}
	return quotaWindow{label: label, used: used, resetAt: resetAt, observedAt: observedAt}, true
}

// claudeUsed returns the Claude window utilization, or -1 when unknown.
func claudeUsed(signals map[string]string, window string) float64 {
	used, ok := parseUsedFraction(quotaSignalValue(signals, "Anthropic-Ratelimit-Unified-"+window+"-Utilization"), 1)
	if !ok {
		return -1
	}
	return used
}

// devinUsed converts Devin's remaining percent into a used fraction, or -1 when unknown.
func devinUsed(signals map[string]string, window string) float64 {
	remaining, ok := parseUsedFraction(quotaSignalValue(signals, window+"_quota_remaining_percent"), 100)
	if !ok {
		return -1
	}
	return 1 - remaining
}

// parseUsedFraction parses a usage value expressed on the given scale (1 for fractions, 100
// for percentages, with an optional % suffix) and clamps it to [0, 1]. Upstreams report
// values above the scale once a window is overdrawn.
func parseUsedFraction(raw string, scale float64) (float64, bool) {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "%")
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return math.Min(value/scale, 1), true
}

func codexResetAfter(raw string, observedAt time.Time) (time.Time, bool) {
	if raw == "" || observedAt.IsZero() {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if errParse != nil || seconds < 0 || seconds > math.MaxInt64/int64(time.Second) {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(seconds) * time.Second), true
}

func validQuotaWindowReset(resetAt time.Time, window time.Duration, observedAt, now time.Time) bool {
	if !resetAt.After(now) {
		return false
	}
	base := observedAt
	if base.IsZero() || base.After(now) {
		base = now
	}
	return !resetAt.After(base.Add(window + quotaAwareWindowSlack))
}

// parseQuotaResetTimestamp parses unix seconds or an RFC 3339 timestamp.
func parseQuotaResetTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
		if seconds <= 0 {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0), true
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// quotaSignalValue looks up a snapshot signal by name. Header-derived names are stored in
// canonical form, but restored snapshots may not be, so a case-insensitive scan follows.
func quotaSignalValue(signals map[string]string, name string) string {
	if value, ok := signals[name]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quotaAwareSessionField(metadata map[string]any) string {
	if sessionID := sessionMetadataString(metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey); sessionID != "" {
		return truncateSessionID(sessionID)
	}
	return ""
}
