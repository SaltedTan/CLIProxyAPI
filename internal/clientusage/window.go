package clientusage

import "time"

// Each client key has its own Claude allowance window, like a subscription period:
// it opens at the key's first Claude request when none is open, lasts exactly
// claudeWeeklyWindow, and the next one opens at the key's next Claude request after
// it ended, so an idle key has no running window. Admission, the report's current
// usage and Retry-After follow this window, not the weekly windows of the
// credentials the key happens to use, which reset on Anthropic's schedule.

// keyWindow is one client key's current allowance window and the Claude usage
// attributed to the key inside it, per credential.
type keyWindow struct {
	StartedAt time.Time `json:"started_at"`
	EndsAt    time.Time `json:"ends_at"`
	// Credentials is the key's usage of each credential during this window, by auth ID.
	Credentials map[string]*windowShare `json:"credentials,omitempty"`
}

// windowShare is usage of one credential inside a key window. Fraction is of that
// credential's weekly limit. ProUnits were priced when attributed; Unpriced is
// converted with the credential's current plan when read.
type windowShare struct {
	Fraction float64 `json:"fraction"`
	ProUnits float64 `json:"pro_units"`
	Unpriced float64 `json:"unpriced"`
}

// open reports whether the window is running at now. A nil window is closed.
func (w *keyWindow) open(now time.Time) bool {
	return w != nil && now.Before(w.EndsAt)
}

// add charges usage of one credential to the window.
func (w *keyWindow) add(authID string, amount float64, plan CredentialInfo, priced bool) {
	if w.Credentials == nil {
		w.Credentials = make(map[string]*windowShare)
	}
	share := w.Credentials[authID]
	if share == nil {
		share = &windowShare{}
		w.Credentials[authID] = share
	}
	share.Fraction += amount
	if priced {
		share.ProUnits += amount * plan.PlanProUnits
		return
	}
	share.Unpriced += amount
}

// share returns the window's usage of one credential, empty when none.
func (w *keyWindow) share(authID string) windowShare {
	if w == nil || w.Credentials == nil || w.Credentials[authID] == nil {
		return windowShare{}
	}
	return *w.Credentials[authID]
}

// proUnits prices the share with the credential's current plan for the unpriced part.
func (s windowShare) proUnits(planProUnits float64) float64 {
	return s.ProUnits + s.Unpriced*planProUnits
}

// openWindow starts a window at the request time when the key has none open then.
// A request that started inside the open window keeps it, however late its usage
// is processed; the next request after the window ended opens the next one.
func (k *keyState) openWindow(at time.Time) {
	if k.Window.open(at) {
		return
	}
	k.Window = &keyWindow{StartedAt: at, EndsAt: at.Add(claudeWeeklyWindow)}
}
