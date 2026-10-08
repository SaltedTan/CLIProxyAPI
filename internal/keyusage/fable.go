package keyusage

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Anthropic reports the weekly Fable allowance of a Claude account only through the
// account's OAuth usage endpoint, never in response headers the proxy sees. The pool
// reads it from the usage cache for every enabled Claude OAuth account that serves a
// Fable model: readers get the cached figures, and a stale figure triggers a
// background refresh.
//
// Accounts are combined in Claude Pro units. Each account's Fable percentage is of
// its own plan's Fable allowance, so percentages are weighted by the plan's weekly
// allowance (clientusage.CredentialInfo.PlanProUnits), assuming a plan's Fable
// allowance scales like its overall weekly allowance: the pool's capacity is the sum
// of the weights, and what is left is the sum of each weight times the unused share.
// An account whose overall weekly window is used up cannot serve Fable until that
// window resets, so none of its Fable counts as left until then.

// resetGroup merges accounts whose windows reset within it into one top-up.
const resetGroup = time.Minute

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

// FablePool combines the Fable windows of the Claude accounts. The zero value is not
// usable; create pools with NewFablePool.
type FablePool struct {
	cache       *UsageCache
	servesFable func(authID string) bool
	weight      func(auth *coreauth.Auth) float64
}

// NewFablePool creates a pool over the accounts of cache.
func NewFablePool(cache *UsageCache) *FablePool {
	return &FablePool{
		cache:       cache,
		servesFable: registryServesFable,
		weight: func(auth *coreauth.Auth) float64 {
			return clientusage.CredentialInfoFromAuth(auth).PlanProUnits
		},
	}
}

// Summary combines the cached readings. Accounts whose reading is stale are refreshed
// in the background, unless the cache's Run has stopped. Summary waits (until ctx ends
// at the latest) for accounts that have never been read, but only until wait has passed
// since their lookup started, so a lookup that hangs delays the first readers rather
// than every reader. The lookups themselves continue after Summary returns.
func (p *FablePool) Summary(ctx context.Context, wait time.Duration) FableSummary {
	if p == nil || p.cache == nil || p.cache.list == nil {
		return FableSummary{}
	}
	c := p.cache
	all := c.accounts()
	accounts := make([]*coreauth.Auth, 0, len(all))
	for _, auth := range all {
		if p.servesFable == nil || p.servesFable(auth.ID) {
			accounts = append(accounts, auth)
		}
	}
	now := c.nowFunc()

	c.mu.Lock()
	c.reconcileLocked(all)
	var pending []chan struct{}
	var waitUntil time.Time
	for _, auth := range accounts {
		reading, ok := c.readings[auth.ID]
		if ok && now.Sub(reading.at) < refreshAfter {
			continue
		}
		done := c.refreshLocked(auth, now)
		if done == nil || ok {
			continue
		}
		if until := c.tried[auth.ID].at.Add(wait); until.After(now) {
			pending = append(pending, done)
			if until.After(waitUntil) {
				waitUntil = until
			}
		}
	}
	c.mu.Unlock()

	if len(pending) > 0 {
		waitAll(ctx, pending, waitUntil.Sub(now))
	}

	c.mu.Lock()
	states := make([]fableAccount, 0, len(accounts))
	for _, auth := range accounts {
		reading, ok := c.readings[auth.ID]
		// A reading of another account that had the same auth ID counts as missing.
		ok = ok && coreauth.SameQuotaAccount(reading.auth, auth) && now.Sub(reading.at) <= maxReadingAge
		states = append(states, fableAccount{units: p.weight(auth), reading: reading, ok: ok})
	}
	c.mu.Unlock()
	return combineFable(c.nowFunc(), states)
}

func waitAll(ctx context.Context, pending []chan struct{}, wait time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for _, done := range pending {
		select {
		case <-done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// fableAccount is one account's weight and reading for combineFable; ok is false
// when it has no usable reading.
type fableAccount struct {
	units   float64
	reading usageReading
	ok      bool
}

// topUp is a time at which the pool grows, and by how many Pro units.
type topUp struct {
	at    time.Time
	units float64
}

// combineFable weighs each account's Fable window by its plan's weekly allowance.
func combineFable(now time.Time, accounts []fableAccount) FableSummary {
	var summary FableSummary
	var capacity, remaining float64
	var oldest time.Time
	var topUps []topUp
	for _, account := range accounts {
		if !account.ok {
			summary.Partial = true
			continue
		}
		reading := account.reading
		if !reading.fable.ok || account.units <= 0 {
			continue
		}
		share := math.Min(math.Max(reading.fable.used, 0), 100) / 100
		if reading.fableEnded(now) {
			// Anthropic restarted the window empty.
			share = 0
		}
		capacity += account.units
		if oldest.IsZero() || reading.at.Before(oldest) {
			oldest = reading.at
		}
		if !reading.weeklyBlocked(now) {
			remaining += account.units * (1 - share)
			if share > 0 && !reading.fable.resetAt.IsZero() {
				topUps = append(topUps, topUp{at: reading.fable.resetAt, units: account.units * share})
			}
			continue
		}
		// None of the account's Fable is usable before its weekly window resets.
		if reading.weekly.resetAt.IsZero() {
			continue
		}
		if share > 0 && (reading.fable.resetAt.IsZero() || reading.fable.resetAt.After(reading.weekly.resetAt)) {
			topUps = append(topUps, topUp{at: reading.weekly.resetAt, units: account.units * (1 - share)})
			if !reading.fable.resetAt.IsZero() {
				topUps = append(topUps, topUp{at: reading.fable.resetAt, units: account.units * share})
			}
			continue
		}
		// The Fable window has reset by then too.
		topUps = append(topUps, topUp{at: reading.weekly.resetAt, units: account.units})
	}
	if capacity <= 0 {
		return summary
	}
	summary.Available = true
	summary.RemainingPercent = roundPercent(100 * remaining / capacity)
	summary.UsedPercent = roundPercent(100 - summary.RemainingPercent)
	summary.UpdatedAt = &oldest
	var next time.Time
	for _, event := range topUps {
		if event.units > 0 && (next.IsZero() || event.at.Before(next)) {
			next = event.at
		}
	}
	if !next.IsZero() {
		var restored float64
		for _, event := range topUps {
			if event.at.Sub(next) < resetGroup {
				restored += event.units
			}
		}
		summary.NextResetAt = &next
		summary.NextResetRestoresPercent = roundPercent(100 * restored / capacity)
	}
	return summary
}

func roundPercent(value float64) float64 {
	return math.Round(value*10) / 10
}

// registryServesFable reports whether the credential registered a Fable model, so
// accounts whose Fable models are excluded by configuration stay out of the pool.
// Aliased models count by the upstream model they stand for.
func registryServesFable(authID string) bool {
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(authID) {
		if model != nil && isFableModel(model) {
			return true
		}
	}
	return false
}

// isFableModel is mirrored by quota-aware routing (quotaAwareFableRequest in
// sdk/cliproxy/auth); keep the two rules in step.
func isFableModel(model *registry.ModelInfo) bool {
	id := strings.TrimSpace(model.MetadataModelID)
	if id == "" {
		id = model.ID
	}
	return strings.Contains(strings.ToLower(id), "fable")
}
