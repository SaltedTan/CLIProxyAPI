package keyusage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fiveHour is a reading of a 5-hour window used percent, resetting in resetIn unless
// it is zero.
func fiveHour(used float64, resetIn time.Duration) usageReading {
	reading := usageReading{at: testNow.Add(-time.Minute), fiveHour: windowReading{ok: true, used: used}}
	if resetIn != 0 {
		reading.fiveHour.resetAt = testNow.Add(resetIn)
	}
	return reading
}

// planAuth is a Claude OAuth account recording plan as its plan_type.
func planAuth(id, plan string) *coreauth.Auth {
	auth := claudeOAuth(id)
	auth.Metadata["plan_type"] = plan
	return auth
}

func TestCombineFiveHourWeighsAccountsByPlan(t *testing.T) {
	summary := combineFiveHour(testNow, []poolAccount{
		{units: sessionProUnits(planAuth("pro", "pro")), reading: fiveHour(70, 4*time.Hour), ok: true},
		{units: sessionProUnits(planAuth("max5", "max_5x")), reading: fiveHour(100, 2*time.Hour+13*time.Minute), ok: true},
		{units: sessionProUnits(planAuth("max20", "max_20x")), reading: fiveHour(10, 3*time.Hour), ok: true},
		// Reports no 5-hour window: left out without making the figure partial.
		{units: 5, reading: usageReading{at: testNow}, ok: true},
	})
	// Capacity 1 + 5 + 20 Pro units, 0.3 + 0 + 18 left.
	if !summary.Available || summary.Partial || summary.CapacityProUnits != 26 || summary.RemainingProUnits != 18.3 {
		t.Fatalf("summary = %+v", summary)
	}
	// The used-up Max 5x account resets first and restores its 5 units.
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(2*time.Hour+13*time.Minute)) || summary.NextResetRestoresProUnits != 5 {
		t.Fatalf("next reset = %v +%v", summary.NextResetAt, summary.NextResetRestoresProUnits)
	}
	if summary.UpdatedAt == nil || !summary.UpdatedAt.Equal(testNow.Add(-time.Minute)) {
		t.Fatalf("updated at = %v", summary.UpdatedAt)
	}
}

func TestCombineFiveHourEdgeCases(t *testing.T) {
	grouped := combineFiveHour(testNow, []poolAccount{
		{units: 1, reading: fiveHour(100, 3*time.Hour), ok: true},
		// Usage over 100% counts as 100%; resets within a minute are one top-up.
		{units: 5, reading: fiveHour(104, 3*time.Hour+20*time.Second), ok: true},
		{units: 20, reading: fiveHour(50, 3*time.Hour+2*time.Minute), ok: true},
		// An account without a reading is skipped and makes the figure partial.
		{units: 20, ok: false},
		// So is one that weighs nothing, silently.
		{units: 0, reading: fiveHour(0, 0), ok: true},
	})
	if !grouped.Partial || grouped.CapacityProUnits != 26 || grouped.RemainingProUnits != 10 || grouped.NextResetRestoresProUnits != 6 {
		t.Fatalf("grouped = %+v", grouped)
	}

	// A window that reset since it was read is empty, and one that nothing used and has
	// no reset has not started: both are all left and top up nothing.
	fresh := combineFiveHour(testNow, []poolAccount{
		{units: 5, reading: fiveHour(80, -time.Minute), ok: true},
		{units: 20, reading: fiveHour(0, 0), ok: true},
	})
	if fresh.CapacityProUnits != 25 || fresh.RemainingProUnits != 25 || fresh.NextResetAt != nil {
		t.Fatalf("fresh = %+v", fresh)
	}

	// Pro units are rounded to 0.001, 0.1% of a Pro plan's limit.
	team := combineFiveHour(testNow, []poolAccount{{units: 1.25, reading: fiveHour(33.3, time.Hour), ok: true}})
	if team.CapacityProUnits != 1.25 || team.RemainingProUnits != 0.834 || team.NextResetRestoresProUnits != 0.416 {
		t.Fatalf("team = %+v", team)
	}

	none := combineFiveHour(testNow, []poolAccount{{units: 1, reading: usageReading{at: testNow}, ok: true}, {units: 1, ok: false}})
	if none.Available || !none.Partial || none.UpdatedAt != nil {
		t.Fatalf("no 5-hour window must be unavailable: %+v", none)
	}
}

func TestCombineFiveHourCountsBlockedAccountsAsEmpty(t *testing.T) {
	summary := combineFiveHour(testNow, []poolAccount{
		// Pro: out of weekly allowance for 10 hours, when its 5-hour window has reset too.
		{units: 1, reading: weeklyUsedUp(fiveHour(20, 3*time.Hour), 10*time.Hour), ok: true},
		// Max 5x: 60% of its 5-hour limit left.
		{units: 5, reading: fiveHour(40, 2*time.Hour), ok: true},
		// Max 20x: blocked for an hour, while half its 5-hour limit is used until 4 hours.
		{units: 20, reading: weeklyUsedUp(fiveHour(50, 4*time.Hour), time.Hour), ok: true},
	})
	// Only the Max 5x account can serve requests: 3 of 26 Pro units.
	if summary.CapacityProUnits != 26 || summary.RemainingProUnits != 3 {
		t.Fatalf("summary = %+v", summary)
	}
	// The Max 20x account comes back first, with the unused half of its limit.
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(time.Hour)) || summary.NextResetRestoresProUnits != 10 {
		t.Fatalf("next reset = %v +%v", summary.NextResetAt, summary.NextResetRestoresProUnits)
	}

	// A blocked account whose 5-hour window resets first comes back whole at its weekly
	// reset; one whose weekly reset is unknown never tops up.
	unknownReset := fiveHour(0, 0)
	unknownReset.weekly = windowReading{ok: true, used: 100}
	blocked := combineFiveHour(testNow, []poolAccount{
		{units: 1, reading: weeklyUsedUp(fiveHour(20, 3*time.Hour), 10*time.Hour), ok: true},
		{units: 5, reading: unknownReset, ok: true},
	})
	if blocked.CapacityProUnits != 6 || blocked.RemainingProUnits != 0 || blocked.NextResetAt == nil || !blocked.NextResetAt.Equal(testNow.Add(10*time.Hour)) || blocked.NextResetRestoresProUnits != 1 {
		t.Fatalf("blocked = %+v", blocked)
	}
}

// An account of a plan without a known 5-hour limit weighs what it weighs in the weekly
// figures; a known plan uses its 5-hour limit, which differs from its weekly allowance.
func TestSessionProUnitsFallsBackToTheWeeklyWeight(t *testing.T) {
	max20 := planAuth("max20", "max_20x")
	if got, weekly := sessionProUnits(max20), clientusage.CredentialInfoFromAuth(max20).PlanProUnits; got != 20 || weekly != 10 {
		t.Fatalf("max 20x = %v per 5 hours and %v per week, want 20 and 10", got, weekly)
	}
	tier := claudeOAuth("tier")
	tier.Metadata["rate_limit_tier"] = "default_claude_max_5x"
	team := planAuth("team", "team")
	weighted := planAuth("enterprise", "enterprise")
	weighted.Attributes = map[string]string{"weight": "3"}
	zero := claudeOAuth("zero")
	zero.Attributes = map[string]string{"weight": "0"}
	cases := map[*coreauth.Auth]float64{tier: 5, team: 1.25, weighted: 3, claudeOAuth("unknown"): 1, zero: 1}
	for auth, want := range cases {
		if got := sessionProUnits(auth); got != want {
			t.Errorf("sessionProUnits(%s) = %v, want %v", auth.ID, got, want)
		}
	}
}

// stubLookupWait makes Summary call wait instead of waiting for its lookups, until the
// test ends.
func stubLookupWait(t *testing.T, wait func(pending []chan struct{}, budget time.Duration)) {
	t.Helper()
	previous := waitForLookups
	t.Cleanup(func() { waitForLookups = previous })
	waitForLookups = func(_ context.Context, pending []chan struct{}, budget time.Duration) {
		wait(pending, budget)
	}
}

// One summary lists the accounts once and waits once, under one budget, for the lookups
// of every account never read, whichever figures it counts in.
func TestPoolListsAndWaitsOnceForBothFigures(t *testing.T) {
	now := testNow
	listed := 0
	auths := []*coreauth.Auth{planAuth("fable", "max_20x"), planAuth("sonnet", "pro")}
	cache := NewUsageCache(func() []*coreauth.Auth {
		listed++
		return auths
	}, nil)
	cache.nowFunc = func() time.Time { return now }
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	cache.fetch = func(_ context.Context, auth *coreauth.Auth) (usageReading, error) {
		<-gate
		if auth.ID == "fable" {
			reading := withFable(40, testNow.Add(48*time.Hour))
			reading.fiveHour = windowReading{ok: true, used: 25, resetAt: testNow.Add(4 * time.Hour)}
			return reading, nil
		}
		return usageReading{fiveHour: windowReading{ok: true, used: 100, resetAt: testNow.Add(time.Hour)}}, nil
	}
	pool := NewPool(cache)
	pool.servesFable = func(authID string) bool { return authID == "fable" }
	type waitCall struct {
		pending, open int
		budget        time.Duration
	}
	var waits []waitCall
	stubLookupWait(t, func(pending []chan struct{}, budget time.Duration) {
		open := 0
		for _, done := range pending {
			select {
			case <-done:
			default:
				open++
			}
		}
		// The budget expires with the lookups still running.
		waits = append(waits, waitCall{pending: len(pending), open: open, budget: budget})
	})
	t.Cleanup(func() {
		release()
		settle(cache)
	})

	summary := pool.Summary(context.Background(), 2*time.Second)
	if want := (waitCall{pending: 2, open: 2, budget: 2 * time.Second}); listed != 1 || len(waits) != 1 || waits[0] != want {
		t.Fatalf("listed %d times, waits = %+v, want one listing and one wait of %+v", listed, waits, want)
	}
	if summary.FiveHour.Available || !summary.FiveHour.Partial || summary.Fable.Available || !summary.Fable.Partial {
		t.Fatalf("summary = %+v, want both figures partial while the lookups run", summary)
	}

	// Once the lookups finish, the next summary combines both figures without waiting.
	release()
	settle(cache)
	summary = pool.Summary(context.Background(), 2*time.Second)
	if listed != 2 || len(waits) != 1 {
		t.Fatalf("listed %d times, waits = %+v, want a second listing and no wait", listed, waits)
	}
	if fable := summary.Fable; fable.Partial || fable.RemainingPercent != 60 {
		t.Fatalf("fable = %+v", fable)
	}
	// Max 20x and Pro: 21 Pro units, 15 left; the Pro account resets first.
	five := summary.FiveHour
	if five.Partial || five.CapacityProUnits != 21 || five.RemainingProUnits != 15 || five.NextResetAt == nil || !five.NextResetAt.Equal(testNow.Add(time.Hour)) || five.NextResetRestoresProUnits != 1 {
		t.Fatalf("5h = %+v", five)
	}
}

// A reading that ages past maxReadingAge while a summary waits for another account no
// longer counts in either figure.
func TestPoolChecksReadingAgeAfterTheWait(t *testing.T) {
	now := testNow
	reading := withFable(40, testNow.Add(48*time.Hour))
	reading.fiveHour = windowReading{ok: true, used: 20, resetAt: testNow.Add(2 * time.Hour)}
	fetcher := &fakeFetcher{
		calls:  map[string]int{},
		result: map[string]usageReading{"old": reading},
		fail:   map[string]bool{"unread": true},
	}
	auths := []*coreauth.Auth{claudeOAuth("old")}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.fetch = fetcher.fetch
	cache.nowFunc = func() time.Time { return now }
	pool := NewPool(cache)
	pool.servesFable = nil
	pool.weight = func(*coreauth.Auth) float64 { return 1 }
	pool.sessionWeight = func(*coreauth.Auth) float64 { return 1 }
	cache.refresh(now)
	settle(cache)

	// The old account's refreshes now fail, and a new account holds the next summary
	// for its two-second budget, across the old reading's maxReadingAge.
	fetcher.mu.Lock()
	fetcher.fail["old"] = true
	fetcher.mu.Unlock()
	auths = append(auths, claudeOAuth("unread"))
	now = testNow.Add(maxReadingAge - time.Second)
	waits := 0
	stubLookupWait(t, func(_ []chan struct{}, budget time.Duration) {
		waits++
		now = now.Add(budget)
	})
	summary := pool.Summary(context.Background(), 2*time.Second)
	settle(cache)
	if waits != 1 || summary.FiveHour.Available || !summary.FiveHour.Partial || summary.Fable.Available || !summary.Fable.Partial {
		t.Fatalf("summary = %+v after %d waits, want the aged reading not counted", summary, waits)
	}
}
