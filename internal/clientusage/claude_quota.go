package clientusage

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Claude subscriptions report weekly usage as a fraction of each plan's own limit via
// the Anthropic-Ratelimit-Unified-7d-* response headers. The tracker attributes every
// increase of that fraction to the client keys that used the credential since the
// previous increase, in proportion to their API-price-weighted tokens. The header is
// sent before a response body, so it reflects usage up to the start of the request
// that carries it; that request's own tokens are attributed by the next increase.
//
// The result is approximate: usage from outside the proxy (claude.ai, other tools)
// that lands while proxy usage is pending is attributed to that usage, and increases
// with nothing pending are reported as unattributed.

const (
	claudeUtilizationHeader = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claudeResetHeader       = "Anthropic-Ratelimit-Unified-7d-Reset"
	claudeWeeklyWindow      = 7 * 24 * time.Hour
	// claudeResetSlack tolerates jitter in the reported reset of one window.
	claudeResetSlack = time.Hour
	// claudeMinDrop is the smallest decrease treated as a reset or rescale of the
	// weekly usage rather than reporting lag.
	claudeMinDrop = 0.02

	// Relative API prices per token class, used only to split an observed increase
	// between keys. Every record also weighs at least claudeMinRecordWeight.
	claudeCacheWriteWeight = 1.25
	claudeCacheReadWeight  = 0.1
	claudeOutputWeight     = 5
	claudeMinRecordWeight  = 1
)

// claudeShare is usage of one credential's weekly limit. Fractions are of that
// credential's limit. Usage attributed while the plan allowance was known is converted
// to Claude Pro units at that time; the rest stays unpriced and is converted at read time.
// The Window values follow the credential's own weekly window and are read for the
// credential's unattributed usage; a key's current usage is kept in its keyWindow.
type claudeShare struct {
	// Epoch is the credential window the Window values belong to.
	Epoch          int64   `json:"epoch"`
	Window         float64 `json:"window"`
	WindowProUnits float64 `json:"window_pro_units"`
	WindowUnpriced float64 `json:"window_unpriced"`
	Total          float64 `json:"total"`
	TotalProUnits  float64 `json:"total_pro_units"`
	TotalUnpriced  float64 `json:"total_unpriced"`
}

func (s *claudeShare) add(amount float64, epoch int64, plan CredentialInfo, priced bool) {
	if s.Epoch != epoch {
		s.Epoch = epoch
		s.Window, s.WindowProUnits, s.WindowUnpriced = 0, 0, 0
	}
	s.Window += amount
	s.Total += amount
	if priced {
		s.WindowProUnits += amount * plan.PlanProUnits
		s.TotalProUnits += amount * plan.PlanProUnits
		return
	}
	s.WindowUnpriced += amount
	s.TotalUnpriced += amount
}

// pendingWeight is the cost weight a key put on a credential since the last increase.
// Outside is the part of it from requests outside the key's current allowance window:
// requests queued before that window opened, and requests that opened none.
type pendingWeight struct {
	Weight  float64   `json:"weight"`
	Outside float64   `json:"outside,omitempty"`
	LastAt  time.Time `json:"last_at"`
}

// claudeCredential tracks the weekly window of one Claude credential.
type claudeCredential struct {
	AuthIndex string `json:"auth_index,omitempty"`
	// Info is the last resolved credential description, kept for deleted credentials.
	Info        *CredentialInfo `json:"info,omitempty"`
	Epoch       int64           `json:"epoch"`
	ResetAt     time.Time       `json:"reset_at"`
	Utilization float64         `json:"utilization"`
	ObservedAt  time.Time       `json:"observed_at"`
	// UtilizationSetAt is when the current Utilization was processed. Only a request
	// that started after it was admitted upstream after that reading, so only such a
	// request can report a genuine drop; lower values from concurrent requests are
	// out of order.
	UtilizationSetAt time.Time `json:"utilization_set_at"`
	// EpochStartedAt is when the current epoch began. Responses to requests that
	// started earlier describe the previous epoch and are ignored.
	EpochStartedAt time.Time                 `json:"epoch_started_at"`
	Pending        map[string]*pendingWeight `json:"pending,omitempty"`
	Unattributed   claudeShare               `json:"unattributed"`
}

type claudeObservation struct {
	utilization float64
	resetAt     time.Time
}

func parseClaudeWeeklyObservation(headers http.Header, observedAt time.Time) (claudeObservation, bool) {
	if len(headers) == 0 {
		return claudeObservation{}, false
	}
	utilization, errParse := strconv.ParseFloat(strings.TrimSpace(headers.Get(claudeUtilizationHeader)), 64)
	if errParse != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) {
		return claudeObservation{}, false
	}
	resetAt, ok := parseClaudeReset(headers.Get(claudeResetHeader))
	if !ok || !resetAt.After(observedAt.Add(-claudeResetSlack)) || resetAt.After(observedAt.Add(claudeWeeklyWindow+claudeResetSlack)) {
		return claudeObservation{}, false
	}
	return claudeObservation{utilization: math.Min(math.Max(utilization, 0), 1), resetAt: resetAt}, true
}

func parseClaudeReset(raw string) (time.Time, bool) {
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
	parsed, errParse := time.Parse(time.RFC3339, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// claudeRecordWeight approximates how much of a subscription a record consumed,
// relative to other records on the same credential.
func claudeRecordWeight(breakdown coreusage.TokenBreakdown) float64 {
	return float64(breakdown.Input.UncachedTokens) +
		claudeCacheWriteWeight*float64(breakdown.Input.CacheWriteTokens) +
		claudeCacheReadWeight*float64(breakdown.Input.CacheReadTokens) +
		claudeOutputWeight*float64(breakdown.Output.TotalTokens) +
		float64(breakdown.UnclassifiedTokens) +
		claudeMinRecordWeight
}

// claudeAuthID returns the credential of a Claude record, or "" for other records.
func claudeAuthID(record coreusage.Record) string {
	if !strings.EqualFold(strings.TrimSpace(record.Provider), "claude") {
		return ""
	}
	return strings.TrimSpace(record.AuthID)
}

// observeClaudeLocked updates the weekly window of the record's credential and
// returns the credential, on which the caller queues the record's weight for the
// next increase; nil when the credential has no baseline yet. requestAt is when
// the request started (zero when unknown); plan describes the credential when
// resolved. t.mu must be held.
func (t *Tracker) observeClaudeLocked(record coreusage.Record, requestAt, now time.Time, plan *CredentialInfo) *claudeCredential {
	authID := claudeAuthID(record)
	if authID == "" {
		return nil
	}
	observation, observed := parseClaudeWeeklyObservation(record.ResponseHeaders, now)
	credential := t.claude[authID]
	if credential == nil {
		if !observed {
			return nil
		}
		// The first observation is a baseline: usage before it cannot be attributed.
		credential = &claudeCredential{
			Epoch:            1,
			ResetAt:          observation.resetAt,
			Utilization:      observation.utilization,
			ObservedAt:       now,
			UtilizationSetAt: now,
			EpochStartedAt:   now,
		}
		t.claude[authID] = credential
	} else if observed {
		t.applyClaudeObservationLocked(authID, credential, observation, requestAt, now, plan)
	}
	if plan != nil {
		credential.Info = plan
	}
	if index := strings.TrimSpace(record.AuthIndex); index != "" {
		credential.AuthIndex = index
	}
	return credential
}

// queue adds the weight of a key's request that started at at for the next
// increase. inWindow tells whether the request belongs to the key's current
// allowance window.
func (c *claudeCredential) queue(keyID string, weight float64, at time.Time, inWindow bool) {
	if c.Pending == nil {
		c.Pending = make(map[string]*pendingWeight)
	}
	pending := c.Pending[keyID]
	if pending == nil {
		pending = &pendingWeight{}
		c.Pending[keyID] = pending
	}
	pending.Weight += weight
	if !inWindow {
		pending.Outside += weight
	}
	if at.After(pending.LastAt) {
		pending.LastAt = at
	}
}

func (t *Tracker) applyClaudeObservationLocked(authID string, credential *claudeCredential, observation claudeObservation, requestAt, now time.Time, plan *CredentialInfo) {
	// Responses to requests without a start time, or started before the current epoch
	// began, may describe an earlier state; their weight is still charged later.
	if requestAt.IsZero() || requestAt.Before(credential.EpochStartedAt) {
		return
	}
	// Only a request that started after the current reading was processed is known to
	// have been admitted upstream after it.
	causallyNewer := requestAt.After(credential.UtilizationSetAt)
	switch {
	case observation.resetAt.After(credential.ResetAt.Add(claudeResetSlack)) && !requestAt.Before(credential.ResetAt.Add(-claudeResetSlack)):
		// The previous window ended. Pending usage from before its reset was spent in
		// that window; everything used in the new one is attributable.
		previousReset := credential.ResetAt
		for keyID, pending := range credential.Pending {
			if pending.LastAt.Before(previousReset.Add(-claudeResetSlack)) {
				delete(credential.Pending, keyID)
			}
		}
		credential.startEpoch(observation.resetAt, now)
		t.attributeClaudeLocked(authID, credential, observation.utilization, plan, now)
		credential.setUtilization(observation.utilization, now)
	case observation.resetAt.Before(credential.ResetAt.Add(-claudeResetSlack)):
		if !causallyNewer || !observation.resetAt.After(now) {
			// A late response from an earlier window carries nothing new.
			return
		}
		// The window was redefined; restart from this value without attributing.
		credential.startEpoch(observation.resetAt, now)
		credential.setUtilization(observation.utilization, now)
	default:
		delta := observation.utilization - credential.Utilization
		switch {
		case delta > 0:
			t.attributeClaudeLocked(authID, credential, delta, plan, now)
			credential.setUtilization(observation.utilization, now)
		case -delta >= claudeMinDrop && causallyNewer:
			// Usage was reset or rescaled (for example a plan change): start a new
			// window epoch from this baseline.
			credential.startEpoch(credential.ResetAt, now)
			credential.setUtilization(observation.utilization, now)
		case delta < 0:
			// Out of order or reporting lag.
			return
		}
		credential.ResetAt = observation.resetAt
	}
	credential.ObservedAt = now
}

// ClaudeWeeklyQuota returns the last weekly window reading of a Claude credential by
// auth ID. The reading is saved with the usage state, so quota-aware routing keeps it
// across restarts and auth file reloads, which clear the credential's own snapshot.
func (t *Tracker) ClaudeWeeklyQuota(authID string) (coreauth.WeeklyQuotaReading, bool) {
	if t == nil {
		return coreauth.WeeklyQuotaReading{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	credential := t.claude[strings.TrimSpace(authID)]
	if credential == nil || credential.ResetAt.IsZero() {
		return coreauth.WeeklyQuotaReading{}, false
	}
	return coreauth.WeeklyQuotaReading{Used: credential.Utilization, ResetAt: credential.ResetAt, ObservedAt: credential.ObservedAt}, true
}

func (c *claudeCredential) startEpoch(resetAt, now time.Time) {
	c.Epoch++
	c.ResetAt = resetAt
	c.EpochStartedAt = now
}

func (c *claudeCredential) setUtilization(utilization float64, now time.Time) {
	c.Utilization = utilization
	c.UtilizationSetAt = now
}

// attributeClaudeLocked splits an observed increase between the keys with pending
// weight on the credential. Each key's share also counts toward its allowance window
// when that window is open at now, in proportion to the key's pending weight from
// requests inside it; usage attributed after the window ended, or to requests from
// before it, belongs to a period that is over and is kept in the totals only. t.mu
// must be held.
func (t *Tracker) attributeClaudeLocked(authID string, credential *claudeCredential, amount float64, plan *CredentialInfo, now time.Time) {
	if amount <= 0 {
		return
	}
	// Plans whose allowance falls back to the weight stay unpriced until read.
	var info CredentialInfo
	priced := false
	if plan != nil {
		info, priced = *plan, plan.PlanSource != PlanSourceWeight
	}
	var total float64
	for _, pending := range credential.Pending {
		total += pending.Weight
	}
	if total <= 0 {
		credential.Unattributed.add(amount, credential.Epoch, info, priced)
		return
	}
	for keyID, pending := range credential.Pending {
		share := amount * pending.Weight / total
		state := t.keys[keyID]
		if state == nil {
			// The key was reset or is beyond the tracked key limit.
			credential.Unattributed.add(share, credential.Epoch, info, priced)
			continue
		}
		if state.Claude == nil {
			state.Claude = make(map[string]*claudeShare)
		}
		keyShare := state.Claude[authID]
		if keyShare == nil {
			keyShare = &claudeShare{}
			state.Claude[authID] = keyShare
		}
		keyShare.add(share, credential.Epoch, info, priced)
		if state.Window.open(now) && pending.Outside < pending.Weight {
			inWindow := share
			if pending.Outside > 0 {
				inWindow = share * (pending.Weight - pending.Outside) / pending.Weight
			}
			state.Window.add(authID, inWindow, info, priced)
		}
	}
	credential.Pending = nil
}
