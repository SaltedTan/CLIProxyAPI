package keyusage

import (
	"context"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The pool reads the windows of every enabled Claude OAuth account from the usage
// cache: readers get the cached figures, and a stale figure triggers a background
// refresh. It combines them into two figures that name no account: the 5-hour limit
// left across all the accounts (FiveHourSummary) and the Fable allowance left across
// the accounts that serve Fable (FableSummary).
//
// Accounts are combined in Claude Pro units. Each account's percentage is of its own
// plan's limit, so percentages are weighted by the plan's size in Pro units: a
// figure's capacity is the sum of the weights, and what is left is the sum of each
// weight times the unused share. An account whose overall weekly window is used up
// cannot serve requests until that window resets, so none of its allowance counts as
// left until then.

// resetGroup merges accounts whose windows reset within it into one top-up.
const resetGroup = time.Minute

// PoolSummary is the pool's figures, combined from one reading of the accounts.
type PoolSummary struct {
	FiveHour FiveHourSummary
	Fable    FableSummary
}

// Pool combines the 5-hour and Fable windows of the Claude accounts. The zero value is
// not usable; create pools with NewPool.
type Pool struct {
	cache       *UsageCache
	servesFable func(authID string) bool
	// weight is an account's weekly allowance in Pro units, which weighs its Fable window.
	weight func(auth *coreauth.Auth) float64
	// sessionWeight is an account's 5-hour limit in Pro units.
	sessionWeight func(auth *coreauth.Auth) float64
}

// NewPool creates a pool over the accounts of cache.
func NewPool(cache *UsageCache) *Pool {
	return &Pool{
		cache:       cache,
		servesFable: registryServesFable,
		weight: func(auth *coreauth.Auth) float64 {
			return clientusage.CredentialInfoFromAuth(auth).PlanProUnits
		},
		sessionWeight: sessionProUnits,
	}
}

// Summary combines the cached readings into both figures, listing the accounts once.
// Accounts whose reading is stale are refreshed in the background, unless the cache's
// Run has stopped. Summary waits once (until ctx ends at the latest) for accounts that
// have never been read, but only until wait has passed since their lookup started, so a
// lookup that hangs delays the first readers rather than every reader. The lookups
// themselves continue after Summary returns.
func (p *Pool) Summary(ctx context.Context, wait time.Duration) PoolSummary {
	if p == nil || p.cache == nil || p.cache.list == nil {
		return PoolSummary{}
	}
	c := p.cache
	accounts := c.accounts()
	servesFable := make([]bool, len(accounts))
	for i, auth := range accounts {
		servesFable[i] = p.servesFable == nil || p.servesFable(auth.ID)
	}
	now := c.nowFunc()

	c.mu.Lock()
	c.reconcileLocked(accounts)
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
	fiveHour := make([]poolAccount, 0, len(accounts))
	fable := make([]poolAccount, 0, len(accounts))
	for i, auth := range accounts {
		reading, ok := c.readings[auth.ID]
		// A reading of another account that had the same auth ID counts as missing.
		ok = ok && coreauth.SameQuotaAccount(reading.auth, auth) && now.Sub(reading.at) <= maxReadingAge
		fiveHour = append(fiveHour, poolAccount{units: p.sessionWeight(auth), reading: reading, ok: ok})
		if servesFable[i] {
			fable = append(fable, poolAccount{units: p.weight(auth), reading: reading, ok: ok})
		}
	}
	c.mu.Unlock()
	now = c.nowFunc()
	return PoolSummary{FiveHour: combineFiveHour(now, fiveHour), Fable: combineFable(now, fable)}
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

// poolAccount is one account's weight and reading for combineWindow; ok is false when
// it has no usable reading.
type poolAccount struct {
	units   float64
	reading usageReading
	ok      bool
}

// topUp is a time at which a figure grows, and by how many Pro units.
type topUp struct {
	at    time.Time
	units float64
}

// pooledWindow is one window combined across the accounts, in Pro units.
type pooledWindow struct {
	capacity  float64
	remaining float64
	// next is the earliest top-up, or zero, and restored what the top-ups within
	// resetGroup of it add.
	next     time.Time
	restored float64
	// oldest is when the oldest reading in the figure was taken.
	oldest time.Time
	// partial is set when an account has no usable reading.
	partial bool
}

// combineWindow weighs the window that window picks from each account's reading by the
// account's units. A window without a reset that nothing used has not started: it is
// all left and tops up nothing.
func combineWindow(now time.Time, accounts []poolAccount, window func(usageReading) windowReading) pooledWindow {
	var pooled pooledWindow
	var topUps []topUp
	for _, account := range accounts {
		if !account.ok {
			pooled.partial = true
			continue
		}
		reading := account.reading
		current := window(reading)
		if !current.ok || account.units <= 0 {
			continue
		}
		share := math.Min(math.Max(current.used, 0), 100) / 100
		if current.ended(now) {
			// Anthropic restarted the window empty.
			share = 0
		}
		pooled.capacity += account.units
		if pooled.oldest.IsZero() || reading.at.Before(pooled.oldest) {
			pooled.oldest = reading.at
		}
		if !reading.weeklyBlocked(now) {
			pooled.remaining += account.units * (1 - share)
			if share > 0 && !current.resetAt.IsZero() {
				topUps = append(topUps, topUp{at: current.resetAt, units: account.units * share})
			}
			continue
		}
		// None of the account's allowance is usable before its weekly window resets.
		if reading.weekly.resetAt.IsZero() {
			continue
		}
		if share > 0 && (current.resetAt.IsZero() || current.resetAt.After(reading.weekly.resetAt)) {
			topUps = append(topUps, topUp{at: reading.weekly.resetAt, units: account.units * (1 - share)})
			if !current.resetAt.IsZero() {
				topUps = append(topUps, topUp{at: current.resetAt, units: account.units * share})
			}
			continue
		}
		// The window has reset by then too.
		topUps = append(topUps, topUp{at: reading.weekly.resetAt, units: account.units})
	}
	for _, event := range topUps {
		if event.units > 0 && (pooled.next.IsZero() || event.at.Before(pooled.next)) {
			pooled.next = event.at
		}
	}
	if !pooled.next.IsZero() {
		for _, event := range topUps {
			if event.at.Sub(pooled.next) < resetGroup {
				pooled.restored += event.units
			}
		}
	}
	return pooled
}

func roundPercent(value float64) float64 {
	return math.Round(value*10) / 10
}
