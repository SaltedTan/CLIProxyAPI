package clientusage

import (
	"math"
	"time"
)

// KeyAllowance is what the holder of a client key may see about that key: its Claude
// usage inside the key's own allowance window and its configured allowance, in
// Claude Pro units. It names no credential, so it is safe to return to the key
// holder.
type KeyAllowance struct {
	// Limited is set when the key has a Claude allowance (access.api-key-limits).
	Limited           bool     `json:"limited"`
	LimitProUnits     *float64 `json:"limit_pro_units,omitempty"`
	UsedProUnits      float64  `json:"used_pro_units"`
	RemainingProUnits *float64 `json:"remaining_pro_units,omitempty"`
	// RemainingPercent is the remaining share of the allowance, 0 to 100.
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	LimitReached     bool     `json:"limit_reached"`
	// WindowStartedAt and WindowResetsAt bound the key's open allowance window. Both
	// are unset while no window is open: the next Claude request opens one.
	WindowStartedAt *time.Time `json:"window_started_at,omitempty"`
	WindowResetsAt  *time.Time `json:"window_resets_at,omitempty"`
}

// KeyAllowance reports the Claude allowance of one client key, using the same
// window and comparison as admission, so a key reported at its limit is refused.
func (t *Tracker) KeyAllowance(apiKey string) KeyAllowance {
	var allowance KeyAllowance
	if t == nil {
		return allowance
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var current float64
	if state := t.keys[KeyID(apiKey)]; state != nil && state.Window.open(now) {
		_, current, _ = t.claudeUsageLocked(state, now, t.credentialRefLocked(t.resolve))
		allowance.WindowStartedAt = optionalTime(state.Window.StartedAt)
		allowance.WindowResetsAt = optionalTime(state.Window.EndsAt)
	}
	allowance.UsedProUnits = roundFraction(current)
	if limit, ok := t.limitForLocked(apiKey); ok {
		remaining := roundFraction(math.Max(limit-roundFraction(current), 0))
		percent := math.Round(remaining/limit*1000) / 10
		allowance.Limited = true
		allowance.LimitProUnits = &limit
		allowance.RemainingProUnits = &remaining
		allowance.RemainingPercent = &percent
		allowance.LimitReached = limitReached(current, limit)
	}
	return allowance
}

// FormatProUnits renders Pro units the way allowance messages do, like "1.25".
func FormatProUnits(value float64) string {
	return formatProUnits(value)
}

// FormatResetDuration renders a duration the way allowance messages do, like
// "2d3h", "1h5m" or "47m".
func FormatResetDuration(d time.Duration) string {
	return formatResetDuration(d)
}
