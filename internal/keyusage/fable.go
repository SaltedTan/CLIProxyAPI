package keyusage

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// Anthropic reports the weekly Fable allowance of a Claude account only through the
// account's OAuth usage endpoint, never in response headers the proxy sees. The pool
// combines it across every enabled Claude OAuth account that serves a Fable model.
//
// Each account's Fable percentage is of its own plan's Fable allowance, so percentages
// are weighted by the plan's weekly allowance (clientusage.CredentialInfo.PlanProUnits),
// assuming a plan's Fable allowance scales like its overall weekly allowance. An
// account whose overall weekly window is used up cannot serve Fable until that window
// resets, so none of its Fable counts as left until then.

// FableSummary is the Fable allowance left across the accounts that serve Fable, as
// one figure. It names no account.
type FableSummary struct {
	// Available is set when at least one account reported its Fable window.
	Available        bool    `json:"available"`
	RemainingPercent float64 `json:"remaining_percent"`
	UsedPercent      float64 `json:"used_percent"`
	// NextResetAt is the next time the pool grows, by NextResetRestoresPercent of its
	// capacity: the earliest reset of an account's used Fable window, or of the weekly
	// window of an account that used it up. When nothing is left, it is when Fable
	// becomes available again.
	NextResetAt              *time.Time `json:"next_reset_at,omitempty"`
	NextResetRestoresPercent float64    `json:"next_reset_restores_percent,omitempty"`
	// UpdatedAt is when the oldest reading in the figure was taken.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Partial is set when an account that may serve Fable has no current reading.
	Partial bool `json:"partial"`
}

// combineFable weighs each account's Fable window by its plan's weekly allowance.
func combineFable(now time.Time, accounts []poolAccount) FableSummary {
	pooled := combineWindow(now, accounts, func(reading usageReading) windowReading { return reading.fable })
	summary := FableSummary{Partial: pooled.partial}
	if pooled.capacity <= 0 {
		return summary
	}
	summary.Available = true
	summary.RemainingPercent = roundPercent(100 * pooled.remaining / pooled.capacity)
	summary.UsedPercent = roundPercent(100 - summary.RemainingPercent)
	summary.UpdatedAt = &pooled.oldest
	if !pooled.next.IsZero() {
		summary.NextResetAt = &pooled.next
		summary.NextResetRestoresPercent = roundPercent(100 * pooled.restored / pooled.capacity)
	}
	return summary
}

// registryServesFable reports whether the credential registered a Fable model, so
// accounts whose Fable models are excluded by configuration stay out of the pool.
// Aliased models count by the upstream model they stand for. Quota-aware routing asks
// another question, whether one request's model is sent to a credential as Fable, and
// answers it from the manager's per-credential model resolution instead of the registry.
func registryServesFable(authID string) bool {
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(authID) {
		if model != nil && isFableModel(model) {
			return true
		}
	}
	return false
}

// isFableModel is mirrored by quota-aware routing when a selector is called without the
// manager's resolver (quotaAwareFableRequest in sdk/cliproxy/auth); keep the two rules in
// step.
func isFableModel(model *registry.ModelInfo) bool {
	id := strings.TrimSpace(model.MetadataModelID)
	if id == "" {
		id = model.ID
	}
	return strings.Contains(strings.ToLower(id), "fable")
}
