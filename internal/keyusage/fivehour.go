package keyusage

import (
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudeplan"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The pool combines the 5-hour window of every enabled Claude OAuth account, whatever
// models it serves. Each account's percentage is of its own plan's 5-hour limit, so
// percentages are weighted by that limit in Pro units (claudeplan.SessionProUnits:
// Pro 1, Team 1.25, Max 5x 5, Max 20x 20). An account of a plan without a known limit
// weighs what it weighs in the weekly figures, its credential weight.

// FiveHourSummary is the 5-hour (session) allowance left across the enabled Claude
// accounts, in Claude Pro units: 1 is one Pro plan's 5-hour limit, so a Max 5x
// account adds 5 and a Max 20x account 20. It names no account.
type FiveHourSummary struct {
	// Available is set when at least one account reported its 5-hour window.
	Available         bool    `json:"available"`
	CapacityProUnits  float64 `json:"capacity_pro_units"`
	RemainingProUnits float64 `json:"remaining_pro_units"`
	// NextResetAt is the next time the pool grows, by NextResetRestoresProUnits: the
	// earliest reset of an account's used 5-hour window, or of the weekly window of an
	// account that used it up.
	NextResetAt               *time.Time `json:"next_reset_at,omitempty"`
	NextResetRestoresProUnits float64    `json:"next_reset_restores_pro_units,omitempty"`
	// UpdatedAt is when the oldest reading in the figure was taken.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Partial is set when an account has no current reading.
	Partial bool `json:"partial"`
}

// combineFiveHour weighs each account's 5-hour window by its plan's 5-hour limit.
func combineFiveHour(now time.Time, accounts []poolAccount) FiveHourSummary {
	pooled := combineWindow(now, accounts, func(reading usageReading) windowReading { return reading.fiveHour })
	summary := FiveHourSummary{Partial: pooled.partial}
	if pooled.capacity <= 0 {
		return summary
	}
	summary.Available = true
	summary.CapacityProUnits = roundProUnits(pooled.capacity)
	summary.RemainingProUnits = roundProUnits(pooled.remaining)
	summary.UpdatedAt = &pooled.oldest
	if !pooled.next.IsZero() {
		summary.NextResetAt = &pooled.next
		summary.NextResetRestoresProUnits = roundProUnits(pooled.restored)
	}
	return summary
}

// sessionProUnits is an account's 5-hour limit in Pro units: its plan's, or for a plan
// without a known limit the account's weight in the weekly figures.
func sessionProUnits(auth *coreauth.Auth) float64 {
	if auth != nil {
		plan, _ := claudeplan.Resolve(auth.Metadata)
		if units, ok := claudeplan.SessionProUnits(plan); ok {
			return units
		}
	}
	return clientusage.CredentialInfoFromAuth(auth).PlanProUnits
}

// roundProUnits rounds to 0.001 Pro units, 0.1% of a Pro plan's limit.
func roundProUnits(value float64) float64 {
	return math.Round(value*1000) / 1000
}
