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
// Records are processed in completion order, so a request that started earlier
// than the window's first known request moves the window back to its own start;
// a request that started inside the open window keeps it, however late its usage
// is processed. Requests that started before the key's window floor (a reset of
// the window, or the end of the previous window) belong to a period that is over
// and open nothing: their usage counts in the totals only.
func (k *keyState) openWindow(at time.Time) {
	if at.Before(k.WindowFloor) {
		return
	}
	if k.Window.open(at) {
		if at.Before(k.Window.StartedAt) {
			k.Window.StartedAt = at
			k.Window.EndsAt = at.Add(claudeWeeklyWindow)
		}
		return
	}
	if k.Window != nil && k.Window.EndsAt.After(k.WindowFloor) {
		k.WindowFloor = k.Window.EndsAt
	}
	k.Window = &keyWindow{StartedAt: at, EndsAt: at.Add(claudeWeeklyWindow)}
}

// closeWindow ends the key's window at now: the next request that starts after
// now opens a fresh one, and requests that started earlier open nothing.
func (k *keyState) closeWindow(now time.Time) {
	k.Window = nil
	if now.After(k.WindowFloor) {
		k.WindowFloor = now
	}
}
