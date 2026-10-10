package keyusage

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestParseUsageReadingReadsEveryWindow(t *testing.T) {
	reading, errParse := parseUsageReading([]byte(`{
		"five_hour":{"utilization":12.5,"resets_at":"2026-10-08T14:00:00.000000+00:00"},
		"seven_day":{"utilization":40,"resets_at":"2026-10-11T10:00:00+00:00"},
		"limits":[{"kind":"weekly_scoped","percent":64,"resets_at":"2026-10-10T10:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	want := usageReading{
		fiveHour: windowReading{ok: true, used: 12.5, resetAt: time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)},
		weekly:   windowReading{ok: true, used: 40, resetAt: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)},
		fable:    windowReading{ok: true, used: 64, resetAt: time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)},
	}
	if !sameReading(reading, want) {
		t.Fatalf("reading = %+v, want %+v", reading, want)
	}
}

func TestParseUsageReadingWithoutResetsOrWindows(t *testing.T) {
	cases := []struct {
		name string
		body string
		want usageReading
	}{
		{
			name: "null and missing resets",
			body: `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":3}}`,
			want: usageReading{fiveHour: windowReading{ok: true}, weekly: windowReading{ok: true, used: 3}},
		},
		{
			name: "null window and null utilization",
			body: `{"five_hour":null,"seven_day":{"utilization":null,"resets_at":"2026-10-11T10:00:00Z"}}`,
		},
		{
			name: "no windows",
			body: `{}`,
		},
		{
			name: "unparsable reset",
			body: `{"five_hour":{"utilization":20,"resets_at":"soon"}}`,
			want: usageReading{fiveHour: windowReading{ok: true, used: 20}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reading, errParse := parseUsageReading([]byte(tc.body))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if !sameReading(reading, tc.want) {
				t.Fatalf("reading = %+v, want %+v", reading, tc.want)
			}
		})
	}
	if _, errParse := parseUsageReading([]byte(`not json`)); errParse == nil {
		t.Fatal("an invalid payload must fail")
	}
}

func sameReading(a, b usageReading) bool {
	same := func(x, y windowReading) bool {
		return x.ok == y.ok && x.used == y.used && x.resetAt.Equal(y.resetAt)
	}
	return a.at.Equal(b.at) && same(a.fiveHour, b.fiveHour) && same(a.weekly, b.weekly) && same(a.fable, b.fable)
}

func TestUsageReadingDescribe(t *testing.T) {
	reading := usageReading{
		fiveHour: windowReading{ok: true, used: 12.5},
		weekly:   windowReading{ok: true, used: 40},
		fable:    windowReading{ok: true, used: 64},
	}
	if got := reading.describe(); got != "5h 12.5%, weekly 40%, fable 64%" {
		t.Fatalf("describe() = %q", got)
	}
	reading.fable = windowReading{}
	if got := reading.describe(); got != "5h 12.5%, weekly 40%, fable n/a" {
		t.Fatalf("describe() without a Fable window = %q", got)
	}
}

func TestUsageCacheQuotaReading(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{
		"read": {
			fiveHour: windowReading{ok: true, used: 12, resetAt: testNow.Add(2 * time.Hour)},
			weekly:   windowReading{ok: true, used: 40, resetAt: testNow.Add(72 * time.Hour)},
			fable:    windowReading{ok: true, used: 64, resetAt: testNow.Add(48 * time.Hour)},
		},
		"unstarted": {fiveHour: windowReading{ok: true}, weekly: windowReading{ok: true}, fable: windowReading{ok: true}},
		"no-reset":  {fiveHour: windowReading{ok: true, used: 30}, weekly: windowReading{ok: true, used: 50}, fable: windowReading{ok: true, used: 20}},
		"no-fable":  {weekly: windowReading{ok: true, used: 10, resetAt: testNow.Add(72 * time.Hour)}},
	}}
	cache := newTestCache(&now, fetcher, claudeOAuth("read"), claudeOAuth("unstarted"), claudeOAuth("no-reset"), claudeOAuth("no-fable"))
	cache.refresh(now)
	settle(cache)

	reading, ok := cache.QuotaReading(claudeOAuth("read"))
	if !ok || reading.Source != "oauth-usage" || !reading.ObservedAt.Equal(testNow) {
		t.Fatalf("reading = %+v (%v)", reading, ok)
	}
	if reading.Short == nil || reading.Short.Used != 0.12 || !reading.Short.ResetAt.Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("5h window = %+v", reading.Short)
	}
	if reading.Weekly == nil || reading.Weekly.Used != 0.4 || !reading.Weekly.ResetAt.Equal(testNow.Add(72*time.Hour)) {
		t.Fatalf("weekly window = %+v", reading.Weekly)
	}
	if reading.Fable == nil || reading.Fable.Used != 0.64 || !reading.Fable.ResetAt.Equal(testNow.Add(48*time.Hour)) {
		t.Fatalf("fable window = %+v", reading.Fable)
	}

	// An unused window without a reset has not started: it resets a window after use.
	unstarted, ok := cache.QuotaReading(claudeOAuth("unstarted"))
	if !ok || unstarted.Short == nil || unstarted.Short.Used != 0 || !unstarted.Short.ResetAt.Equal(testNow.Add(5*time.Hour)) ||
		unstarted.Weekly == nil || unstarted.Weekly.Used != 0 || !unstarted.Weekly.ResetAt.Equal(testNow.Add(7*24*time.Hour)) ||
		unstarted.Fable == nil || unstarted.Fable.Used != 0 || !unstarted.Fable.ResetAt.Equal(testNow.Add(7*24*time.Hour)) {
		t.Fatalf("unstarted = %+v (%v)", unstarted, ok)
	}
	// A used window without a reset is unknown.
	if noReset, ok := cache.QuotaReading(claudeOAuth("no-reset")); !ok || noReset.Short != nil || noReset.Weekly != nil || noReset.Fable != nil {
		t.Fatalf("no reset = %+v (%v)", noReset, ok)
	}
	// An account whose payload has no Fable window has no Fable reading.
	if noFable, ok := cache.QuotaReading(claudeOAuth("no-fable")); !ok || noFable.Weekly == nil || noFable.Fable != nil {
		t.Fatalf("no fable = %+v (%v)", noFable, ok)
	}
	if _, ok := cache.QuotaReading(claudeOAuth("never-read")); ok {
		t.Fatal("an account never read must have no reading")
	}

	// A reading counts until it is older than maxReadingAge.
	now = testNow.Add(maxReadingAge)
	if _, ok := cache.QuotaReading(claudeOAuth("read")); !ok {
		t.Fatal("a reading maxReadingAge old must still count")
	}
	now = testNow.Add(maxReadingAge + time.Second)
	if _, ok := cache.QuotaReading(claudeOAuth("read")); ok {
		t.Fatal("a reading older than maxReadingAge must not count")
	}
	var nilCache *UsageCache
	if _, ok := nilCache.QuotaReading(claudeOAuth("read")); ok {
		t.Fatal("a nil cache has no readings")
	}
}

// A window reported fully used without a reset time is used up with its reset unknown,
// for routing as for the key usage pool, rather than unknown.
func TestUsageCacheQuotaReadingUsedUpWithoutReset(t *testing.T) {
	parsed, errParse := parseUsageReading([]byte(`{
		"five_hour":{"utilization":30,"resets_at":null},
		"seven_day":{"utilization":100,"resets_at":null},
		"limits":[{"kind":"weekly_scoped","percent":104,"resets_at":null,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !parsed.weeklyBlocked(testNow) {
		t.Fatal("the key usage pool must count the account as blocked")
	}
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{"a": parsed}}
	cache := newTestCache(&now, fetcher, claudeOAuth("a"))
	cache.refresh(now)
	settle(cache)

	reading, ok := cache.QuotaReading(claudeOAuth("a"))
	if !ok {
		t.Fatal("the account must have a reading")
	}
	if reading.Weekly == nil || reading.Weekly.Used != 1 || !reading.Weekly.ResetAt.IsZero() {
		t.Fatalf("weekly window = %+v, want used up with the reset unknown", reading.Weekly)
	}
	if reading.Fable == nil || reading.Fable.Used != 1 || !reading.Fable.ResetAt.IsZero() {
		t.Fatalf("fable window = %+v, want used up with the reset unknown", reading.Fable)
	}
	// A window partly used without a reset stays unknown.
	if reading.Short != nil {
		t.Fatalf("5h window = %+v, want unknown", reading.Short)
	}
}

func TestUsageCacheQuotaReadingDoesNotWaitForALookup(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{
		calls:  map[string]int{},
		result: map[string]usageReading{"a": {weekly: windowReading{ok: true, used: 40, resetAt: testNow.Add(72 * time.Hour)}}},
		fail:   map[string]bool{"b": true},
	}
	cache := newTestCache(&now, fetcher, claudeOAuth("a"), claudeOAuth("b"))
	cache.refresh(now)
	settle(cache)

	// Both the refresh of a and the retry of b, which has no reading, hang.
	now = testNow.Add(refreshAfter)
	gate := make(chan struct{})
	fetcher.mu.Lock()
	fetcher.gate = gate
	fetcher.mu.Unlock()
	cache.refresh(now)
	defer func() {
		close(gate)
		settle(cache)
	}()

	type result struct{ a, b bool }
	done := make(chan result, 1)
	go func() {
		_, okA := cache.QuotaReading(claudeOAuth("a"))
		_, okB := cache.QuotaReading(claudeOAuth("b"))
		done <- result{a: okA, b: okB}
	}()
	select {
	case got := <-done:
		if !got.a || got.b {
			t.Fatalf("readings = %+v, want the cached reading of a and none of b", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("QuotaReading waited for a lookup")
	}
}

func TestUsageCacheRefreshLooksUpEligibleStaleAccounts(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{
		calls:  map[string]int{},
		result: map[string]usageReading{"ok": {weekly: windowReading{ok: true, used: 10, resetAt: testNow.Add(72 * time.Hour)}}},
		fail:   map[string]bool{"failing": true},
	}
	disabled := claudeOAuth("disabled")
	disabled.Disabled = true
	paused := claudeOAuth("paused")
	paused.Status = coreauth.StatusDisabled
	apiKey := &coreauth.Auth{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "sk"}, Metadata: map[string]any{"access_token": "token-key"}}
	expired := claudeOAuth("expired")
	expired.Metadata["expired"] = testNow.Add(-time.Minute).Format(time.RFC3339)
	codex := &coreauth.Auth{ID: "codex", Provider: "codex", Metadata: map[string]any{"access_token": "token-codex"}}
	cache := newTestCache(&now, fetcher, claudeOAuth("ok"), claudeOAuth("failing"), disabled, paused, apiKey, expired, codex)

	cache.refresh(now)
	settle(cache)
	for id, want := range map[string]int{"ok": 1, "failing": 1, "disabled": 0, "paused": 0, "key": 0, "expired": 0, "codex": 0} {
		if got := fetcher.count(id); got != want {
			t.Fatalf("lookups of %s = %d, want %d (calls %v)", id, got, want, fetcher.calls)
		}
	}

	// Neither a success nor a failure is looked up again within refreshAfter.
	now = testNow.Add(refreshAfter - time.Second)
	cache.refresh(now)
	settle(cache)
	if fetcher.count("ok") != 1 || fetcher.count("failing") != 1 {
		t.Fatalf("calls = %v, want no lookup within refreshAfter", fetcher.calls)
	}
	now = testNow.Add(refreshAfter)
	cache.refresh(now)
	settle(cache)
	if fetcher.count("ok") != 2 || fetcher.count("failing") != 2 {
		t.Fatalf("calls = %v, want one more lookup each after refreshAfter", fetcher.calls)
	}
}

// An account whose access token expired is not looked up, since its refresh failed or
// is pending, but it keeps its reading until the reading ages out.
func TestUsageCacheKeepsTheReadingOfAnExpiredToken(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{
		"a": {weekly: windowReading{ok: true, used: 40, resetAt: testNow.Add(72 * time.Hour)}},
	}}
	auth := claudeOAuth("a")
	auth.Metadata["expired"] = testNow.Add(10 * time.Minute).Format(time.RFC3339)
	auths := []*coreauth.Auth{auth}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.fetch = fetcher.fetch
	cache.nowFunc = func() time.Time { return now }

	cache.refresh(now)
	settle(cache)
	now = testNow.Add(10 * time.Minute)
	cache.refresh(now)
	settle(cache)
	if fetcher.count("a") != 1 {
		t.Fatalf("lookups = %d, want none with an expired token", fetcher.count("a"))
	}
	if _, ok := cache.QuotaReading(claudeOAuth("a")); !ok {
		t.Fatal("an account with an expired token must keep its reading")
	}
	now = testNow.Add(maxReadingAge + time.Second)
	cache.refresh(now)
	settle(cache)
	if _, ok := cache.QuotaReading(claudeOAuth("a")); ok || fetcher.count("a") != 1 {
		t.Fatalf("lookups = %d, want the old reading aged out without a lookup", fetcher.count("a"))
	}

	// A refreshed token is looked up again.
	refreshed := claudeOAuth("a")
	refreshed.Metadata["expired"] = now.Add(time.Hour).Format(time.RFC3339)
	auths = []*coreauth.Auth{refreshed}
	cache.refresh(now)
	settle(cache)
	if _, ok := cache.QuotaReading(claudeOAuth("a")); !ok || fetcher.count("a") != 2 {
		t.Fatalf("lookups = %d, want a reading after the token refresh", fetcher.count("a"))
	}
}

func TestUsageCacheRefreshForgetsAccountsThatLeave(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{
		"read": {weekly: windowReading{ok: true, used: 40, resetAt: testNow.Add(72 * time.Hour)}},
	}}
	cancelled := make(chan struct{})
	var auths []*coreauth.Auth
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.nowFunc = func() time.Time { return now }
	cache.fetch = func(ctx context.Context, auth *coreauth.Auth) (usageReading, error) {
		if auth.ID != "hanging" {
			return fetcher.fetch(ctx, auth)
		}
		<-ctx.Done()
		close(cancelled)
		return usageReading{}, ctx.Err()
	}

	auths = []*coreauth.Auth{claudeOAuth("read")}
	cache.refresh(now)
	settle(cache)
	auths = []*coreauth.Auth{claudeOAuth("read"), claudeOAuth("hanging")}
	cache.refresh(now)
	cache.mu.Lock()
	hanging := cache.inflight["hanging"]
	cache.mu.Unlock()
	if hanging == nil {
		t.Fatal("the hanging lookup did not start")
	}

	auths = nil
	cache.refresh(now)
	<-hanging.done
	select {
	case <-cancelled:
	default:
		t.Fatal("the lookup of an account that left was not cancelled")
	}
	if _, ok := cache.QuotaReading(claudeOAuth("read")); ok {
		t.Fatal("an account that left must have no reading")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.tried) != 0 || len(cache.inflight) != 0 || len(cache.readings) != 0 || len(cache.announced) != 0 {
		t.Fatalf("tried = %v, inflight = %v, readings = %v, announced = %v", cache.tried, cache.inflight, cache.readings, cache.announced)
	}
}

// Routing and the key usage pool share the cache, so one lookup serves both.
func TestUsageCacheServesThePoolAndRouting(t *testing.T) {
	now := testNow
	reading := withFable(25, testNow.Add(48*time.Hour))
	reading.weekly = windowReading{ok: true, used: 40, resetAt: testNow.Add(72 * time.Hour)}
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{"a": reading}}
	cache := newTestCache(&now, fetcher, claudeOAuth("a"))
	pool := NewPool(cache)
	pool.servesFable = nil
	pool.weight = func(*coreauth.Auth) float64 { return 1 }

	cache.refresh(now)
	settle(cache)
	if summary := pool.Summary(context.Background(), time.Minute).Fable; !summary.Available || summary.Partial || summary.RemainingPercent != 75 {
		t.Fatalf("summary = %+v", summary)
	}
	if quota, ok := cache.QuotaReading(claudeOAuth("a")); !ok || quota.Weekly == nil || quota.Weekly.Used != 0.4 {
		t.Fatalf("quota reading = %+v (%v)", quota, ok)
	}
	settle(cache)
	if fetcher.count("a") != 1 {
		t.Fatalf("lookups = %d, want one shared lookup", fetcher.count("a"))
	}

	// A summary's refresh fills the routing reading too.
	now = testNow.Add(refreshAfter)
	fetcher.mu.Lock()
	reading.weekly.used = 60
	fetcher.result["a"] = reading
	fetcher.mu.Unlock()
	pool.Summary(context.Background(), time.Minute)
	settle(cache)
	cache.refresh(now)
	settle(cache)
	if quota, ok := cache.QuotaReading(claudeOAuth("a")); !ok || quota.Weekly == nil || quota.Weekly.Used != 0.6 || fetcher.count("a") != 2 {
		t.Fatalf("quota reading = %+v (%v) after %d lookups", quota, ok, fetcher.count("a"))
	}
}

func TestUsageCacheRunRefreshesAtOnce(t *testing.T) {
	now := testNow
	cache := newTestCache(&now, &fakeFetcher{calls: map[string]int{}}, claudeOAuth("a"))
	called := make(chan string, 1)
	cache.fetch = func(_ context.Context, auth *coreauth.Auth) (usageReading, error) {
		called <- auth.ID
		return usageReading{weekly: windowReading{ok: true, used: 10, resetAt: testNow.Add(72 * time.Hour)}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		// A nil active means always.
		cache.Run(ctx, nil)
	}()

	select {
	case id := <-called:
		if id != "a" {
			t.Fatalf("looked up %s, want a", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not refresh at once")
	}
	// Let the lookup finish before Run stops, which would cancel it.
	settle(cache)
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if _, ok := cache.QuotaReading(claudeOAuth("a")); !ok {
		t.Fatal("Run's lookup left no reading")
	}
}

// When Run stops it cancels the running lookups, which then publish nothing.
func TestUsageCacheRunCancelsLookupsWhenItStops(t *testing.T) {
	now := testNow
	cache := newTestCache(&now, &fakeFetcher{calls: map[string]int{}}, claudeOAuth("a"))
	started := make(chan struct{})
	finished := make(chan struct{})
	cache.fetch = func(ctx context.Context, _ *coreauth.Auth) (usageReading, error) {
		defer close(finished)
		close(started)
		<-ctx.Done()
		// A late success must not be published either.
		return usageReading{weekly: windowReading{ok: true, used: 10, resetAt: testNow.Add(72 * time.Hour)}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cache.Run(ctx, nil)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start a lookup")
	}
	cancel()
	<-stopped
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup was not cancelled when Run stopped")
	}
	settle(cache)
	cache.mu.Lock()
	inflight, readings := len(cache.inflight), len(cache.readings)
	cache.mu.Unlock()
	if inflight != 0 || readings != 0 {
		t.Fatalf("inflight = %d, readings = %d, want the cancelled lookup gone without a reading", inflight, readings)
	}
}

// After Run stops, a pool summary (a key usage request served during shutdown)
// starts no lookup that nothing would cancel, and readers keep the cached readings.
// Run starts the cache again.
func TestUsageCacheStartsNoLookupAfterRunStops(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{
		"read":   withFable(40, testNow.Add(48*time.Hour)),
		"unread": withFable(10, testNow.Add(48*time.Hour)),
	}}
	auths := []*coreauth.Auth{claudeOAuth("read")}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.nowFunc = func() time.Time { return now }
	// Room for every lookup, so a lookup the cache wrongly starts never blocks.
	looked := make(chan string, 8)
	cache.fetch = func(ctx context.Context, auth *coreauth.Auth) (usageReading, error) {
		defer func() { looked <- auth.ID }()
		return fetcher.fetch(ctx, auth)
	}
	pool := NewPool(cache)
	pool.servesFable = nil
	pool.weight = func(*coreauth.Auth) float64 { return 1 }
	run := func(lookups int) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			cache.Run(ctx, nil)
		}()
		defer func() {
			cancel()
			<-stopped
		}()
		for i := 0; i < lookups; i++ {
			select {
			case <-looked:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not refresh at once")
			}
		}
		// Let the lookups finish before Run stops, which would cancel them.
		settle(cache)
	}

	run(1)
	// Later the reading is stale and a new account is listed.
	auths = []*coreauth.Auth{claudeOAuth("read"), claudeOAuth("unread")}
	now = testNow.Add(refreshAfter + time.Second)
	summary := pool.Summary(context.Background(), time.Minute).Fable
	cache.refresh(now)
	settle(cache)
	if fetcher.count("read") != 1 || fetcher.count("unread") != 0 {
		t.Fatalf("calls = %v, want no lookup after Run stopped", fetcher.calls)
	}
	if !summary.Available || !summary.Partial || summary.RemainingPercent != 60 {
		t.Fatalf("summary = %+v, want the cached reading only", summary)
	}
	if _, ok := cache.QuotaReading(claudeOAuth("read")); !ok {
		t.Fatal("the cached reading must still count after Run stopped")
	}

	// A new Run looks up both accounts again.
	run(2)
	if fetcher.count("read") != 2 || fetcher.count("unread") != 1 {
		t.Fatalf("calls = %v, want both accounts looked up once Run restarted", fetcher.calls)
	}
	if summary := pool.Summary(context.Background(), 0).Fable; summary.Partial || summary.RemainingPercent != 75 {
		t.Fatalf("summary after restart = %+v", summary)
	}
}

// While inactive, Run starts no lookup but still forgets accounts that left or were
// replaced by another account under the same auth ID.
func TestUsageCacheRunForgetsAccountsWhileInactive(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{
		"gone":    {weekly: windowReading{ok: true, used: 10, resetAt: testNow.Add(72 * time.Hour)}},
		"swapped": {weekly: windowReading{ok: true, used: 20, resetAt: testNow.Add(72 * time.Hour)}},
		"kept":    {weekly: windowReading{ok: true, used: 30, resetAt: testNow.Add(72 * time.Hour)}},
	}}
	kept := claudeAccount("kept", "kept@example.com", "token-kept")
	auths := []*coreauth.Auth{claudeAccount("gone", "gone@example.com", "token-gone"), claudeAccount("swapped", "a@example.com", "token-a"), kept}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.fetch = fetcher.fetch
	cache.nowFunc = func() time.Time { return now }
	cache.refresh(now)
	settle(cache)

	auths = []*coreauth.Auth{claudeAccount("swapped", "b@example.com", "token-b"), kept}
	asked := make(chan struct{}, 1)
	active := func() bool {
		select {
		case asked <- struct{}{}:
		default:
		}
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cache.Run(ctx, active)
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not check whether it is active")
	}
	// The tick that asked finishes before Run sees the cancellation.
	cancel()
	<-stopped

	cache.mu.Lock()
	_, keptRead := cache.readings["kept"]
	readings, tried := len(cache.readings), len(cache.tried)
	cache.mu.Unlock()
	if !keptRead || readings != 1 || tried != 1 {
		t.Fatalf("readings = %d (kept %v), tried = %d, want only the kept account", readings, keptRead, tried)
	}
	for id, want := range map[string]int{"gone": 1, "swapped": 1, "kept": 1} {
		if got := fetcher.count(id); got != want {
			t.Fatalf("lookups of %s = %d, want %d while inactive", id, got, want)
		}
	}
}

// claudeAccount is a Claude OAuth credential whose upstream account is email.
func claudeAccount(id, email, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": token, "email": email}}
}

// weeklyUsed is a reading of a weekly window used percent.
func weeklyUsed(used float64) usageReading {
	return usageReading{weekly: windowReading{ok: true, used: used, resetAt: testNow.Add(72 * time.Hour)}}
}

// An auth file replaced by another account's under the same auth ID must not inherit
// the old account's reading; a token refresh of the same account keeps it.
func TestUsageCacheDropsTheReadingOfAReplacedAccount(t *testing.T) {
	now := testNow
	var mu sync.Mutex
	calls := map[string]int{}
	used := map[string]float64{"a@example.com": 40, "b@example.com": 70}
	accountA := claudeAccount("x", "a@example.com", "token-a")
	accountB := claudeAccount("x", "b@example.com", "token-b")
	auths := []*coreauth.Auth{accountA}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.nowFunc = func() time.Time { return now }
	cache.fetch = func(_ context.Context, auth *coreauth.Auth) (usageReading, error) {
		email, _ := auth.Metadata["email"].(string)
		mu.Lock()
		defer mu.Unlock()
		calls[email]++
		return weeklyUsed(used[email]), nil
	}
	count := func(email string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[email]
	}

	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(accountA); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.4 {
		t.Fatalf("reading of A = %+v (%v)", reading, ok)
	}
	if reading, ok := cache.QuotaReading(accountB); ok {
		t.Fatalf("B read A's reading %+v", reading)
	}

	// A token refresh keeps the account, its reading and the refresh interval.
	refreshed := claudeAccount("x", "a@example.com", "token-a2")
	auths = []*coreauth.Auth{refreshed}
	now = testNow.Add(time.Minute)
	cache.refresh(now)
	settle(cache)
	if _, ok := cache.QuotaReading(refreshed); !ok || count("a@example.com") != 1 {
		t.Fatalf("refreshed token: reading %v after %d lookups, want the kept reading", ok, count("a@example.com"))
	}

	// The replacement is looked up at once, within refreshAfter of A's lookup.
	auths = []*coreauth.Auth{accountB}
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(accountB); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.7 || count("b@example.com") != 1 {
		t.Fatalf("reading of B = %+v (%v) after %d lookups", reading, ok, count("b@example.com"))
	}
	if reading, ok := cache.QuotaReading(refreshed); ok {
		t.Fatalf("A read B's reading %+v", reading)
	}
}

// Claude quota is per organization. An auth file that drops its organization_uuid may now
// hold another organization of the same user, so the matching email does not prove the
// account: with the same tokens the reading is kept, with other tokens it is dropped and
// the account is looked up again at once.
func TestUsageCacheDropsTheReadingWhenTheOrganizationIsDropped(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{"x": weeklyUsed(40)}}
	withOrganization := claudeAccount("x", "a@example.com", "token-1")
	withOrganization.Metadata["organization_uuid"] = "org-a"
	auths := []*coreauth.Auth{withOrganization}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.fetch = fetcher.fetch
	cache.nowFunc = func() time.Time { return now }
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(withOrganization); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.4 {
		t.Fatalf("reading = %+v (%v), want the first lookup's", reading, ok)
	}

	sameTokens := claudeAccount("x", "a@example.com", "token-1")
	auths = []*coreauth.Auth{sameTokens}
	now = testNow.Add(time.Minute)
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(sameTokens); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.4 || fetcher.count("x") != 1 {
		t.Fatalf("same tokens: reading %+v (%v) after %d lookups, want the kept reading", reading, ok, fetcher.count("x"))
	}

	fetcher.mu.Lock()
	fetcher.result["x"] = weeklyUsed(70)
	fetcher.mu.Unlock()
	otherTokens := claudeAccount("x", "a@example.com", "token-2")
	auths = []*coreauth.Auth{otherTokens}
	if reading, ok := cache.QuotaReading(otherTokens); ok {
		t.Fatalf("the file without its organization read the previous reading %+v", reading)
	}
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(otherTokens); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.7 || fetcher.count("x") != 2 {
		t.Fatalf("other tokens: reading %+v (%v) after %d lookups, want a new lookup at once", reading, ok, fetcher.count("x"))
	}
}

// A lookup of the old account that is still running when the auth file is replaced is
// cancelled, and its late result is not published over the new account's reading.
func TestUsageCacheReplacementCancelsTheOldAccountsLookup(t *testing.T) {
	now := testNow
	accountA := claudeAccount("x", "a@example.com", "token-a")
	accountB := claudeAccount("x", "b@example.com", "token-b")
	cancelledA := make(chan struct{})
	releaseA := make(chan struct{})
	auths := []*coreauth.Auth{accountA}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.nowFunc = func() time.Time { return now }
	cache.fetch = func(ctx context.Context, auth *coreauth.Auth) (usageReading, error) {
		if auth.Metadata["email"] == "b@example.com" {
			return weeklyUsed(70), nil
		}
		<-ctx.Done()
		close(cancelledA)
		<-releaseA
		return weeklyUsed(40), nil
	}

	cache.refresh(now)
	cache.mu.Lock()
	lookupA := cache.inflight["x"]
	cache.mu.Unlock()
	if lookupA == nil {
		t.Fatal("the lookup of A did not start")
	}

	auths = []*coreauth.Auth{accountB}
	cache.refresh(now)
	settle(cache)
	select {
	case <-cancelledA:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup of the replaced account was not cancelled")
	}
	close(releaseA)
	<-lookupA.done

	if reading, ok := cache.QuotaReading(accountB); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.7 {
		t.Fatalf("reading of B = %+v (%v), want B's own", reading, ok)
	}
	if _, ok := cache.QuotaReading(accountA); ok {
		t.Fatal("A's late lookup was published")
	}
}

// After a failed lookup, which leaves no reading, an auth file replaced by another
// account's under the same auth ID is looked up at once, while the same account still
// waits out refreshAfter.
func TestUsageCacheLooksUpAReplacementAfterAFailedLookup(t *testing.T) {
	now := testNow
	var mu sync.Mutex
	calls := map[string]int{}
	accountA := claudeAccount("x", "a@example.com", "token-a")
	accountA.Metadata["organization_uuid"] = "org-a"
	accountB := claudeAccount("x", "b@example.com", "token-b")
	accountB.Metadata["organization_uuid"] = "org-b"
	auths := []*coreauth.Auth{accountA}
	cache := NewUsageCache(func() []*coreauth.Auth { return auths }, nil)
	cache.nowFunc = func() time.Time { return now }
	cache.fetch = func(_ context.Context, auth *coreauth.Auth) (usageReading, error) {
		email, _ := auth.Metadata["email"].(string)
		mu.Lock()
		defer mu.Unlock()
		calls[email]++
		if email == "a@example.com" {
			return usageReading{}, errors.New("status 429")
		}
		return weeklyUsed(70), nil
	}
	count := func(email string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[email]
	}

	cache.refresh(now)
	settle(cache)
	if count("a@example.com") != 1 {
		t.Fatalf("lookups of A = %d, want 1", count("a@example.com"))
	}

	// The same account, even with its token refreshed, waits out refreshAfter.
	refreshed := claudeAccount("x", "a@example.com", "token-a2")
	refreshed.Metadata["organization_uuid"] = "org-a"
	auths = []*coreauth.Auth{refreshed}
	now = testNow.Add(10 * time.Second)
	cache.refresh(now)
	settle(cache)
	if count("a@example.com") != 1 {
		t.Fatalf("lookups of A = %d, want no retry within refreshAfter of the failure", count("a@example.com"))
	}

	// The replacement is looked up at once, seconds after A's failed lookup.
	auths = []*coreauth.Auth{accountB}
	now = testNow.Add(20 * time.Second)
	cache.refresh(now)
	settle(cache)
	if count("b@example.com") != 1 {
		t.Fatalf("lookups of B = %d, want B looked up at once", count("b@example.com"))
	}
	if reading, ok := cache.QuotaReading(accountB); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.7 {
		t.Fatalf("reading of B = %+v (%v)", reading, ok)
	}
	if count("a@example.com") != 1 {
		t.Fatalf("lookups of A = %d, want 1", count("a@example.com"))
	}
}

// A reading is stamped with the start of its lookup, so a lookup that completes after a
// response was observed does not override that response's newer quota snapshot.
func TestUsageCacheReadingIsAsOldAsItsLookupStart(t *testing.T) {
	wall := time.Now().Truncate(time.Second)
	snapshotAt := wall.Add(-time.Minute)
	unix := func(d time.Duration) string { return strconv.FormatInt(wall.Add(d).Unix(), 10) }
	withSnapshot := func(id, email string, weeklyResetIn time.Duration) *coreauth.Auth {
		auth := claudeAccount(id, email, "token-"+id)
		auth.Status = coreauth.StatusActive
		auth.Quota = coreauth.QuotaState{ObservedAt: snapshotAt, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3",
			"Anthropic-Ratelimit-Unified-7d-Reset":       unix(weeklyResetIn),
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
			"Anthropic-Ratelimit-Unified-5h-Reset":       unix(2 * time.Hour),
		}}
		return auth
	}
	urgent := withSnapshot("a-urgent", "a@example.com", 10*time.Hour)
	relaxed := withSnapshot("b-relaxed", "b@example.com", 100*time.Hour)
	// The endpoint reports the urgent account's 5h window nearly used up.
	saturated := usageReading{
		fiveHour: windowReading{ok: true, used: 90, resetAt: wall.Add(2 * time.Hour)},
		weekly:   windowReading{ok: true, used: 30, resetAt: wall.Add(10 * time.Hour)},
	}

	pick := func(startedAt time.Time) string {
		t.Helper()
		now := startedAt
		gate := make(chan struct{})
		cache := NewUsageCache(func() []*coreauth.Auth { return []*coreauth.Auth{urgent} }, nil)
		cache.nowFunc = func() time.Time { return now }
		cache.fetch = func(context.Context, *coreauth.Auth) (usageReading, error) {
			<-gate
			return saturated, nil
		}
		cache.refresh(now)
		// The lookup completes after the snapshot was observed.
		now = wall
		close(gate)
		settle(cache)
		if reading, ok := cache.QuotaReading(urgent); !ok || !reading.ObservedAt.Equal(startedAt) {
			t.Fatalf("reading = %+v (%v), want it observed at the lookup start %v", reading, ok, startedAt)
		}

		selector := coreauth.NewQuotaAwareSelector(nil)
		selector.SetQuotaSources(cache.QuotaReading)
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, []*coreauth.Auth{urgent, relaxed})
		if errPick != nil || picked == nil {
			t.Fatalf("Pick() = %v, %v", picked, errPick)
		}
		return picked.ID
	}

	// Started before the snapshot, the reading is older: the snapshot's light 5h usage wins.
	if got := pick(snapshotAt.Add(-time.Minute)); got != "a-urgent" {
		t.Fatalf("picked %s, want a-urgent ranked by its newer snapshot", got)
	}
	// Started after the snapshot, the reading is newer and saturates the urgent account.
	if got := pick(wall); got != "b-relaxed" {
		t.Fatalf("picked %s, want b-relaxed while a-urgent's 5h window is nearly used up", got)
	}
}

func TestUsageCacheRunIdlesWhileInactive(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}}
	cache := newTestCache(&now, fetcher, claudeOAuth("a"))
	asked := make(chan struct{}, 1)
	active := func() bool {
		select {
		case asked <- struct{}{}:
		default:
		}
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cache.Run(ctx, active)
	}()

	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not check whether it is active")
	}
	cancel()
	<-stopped
	cache.mu.Lock()
	tried := len(cache.tried)
	cache.mu.Unlock()
	if tried != 0 || fetcher.count("a") != 0 {
		t.Fatalf("tried = %d, lookups = %d, want none while inactive", tried, fetcher.count("a"))
	}
}

// tokenRotatingExecutor is a Claude executor whose refresh rotates both tokens to
// access-<next> and refresh-<next>, like the proxy's own refresh of a credential whose
// upstream reports no identity (a setup token, or a profile lookup refused with 403).
type tokenRotatingExecutor struct{ next string }

func (tokenRotatingExecutor) Identifier() string { return "claude" }

func (tokenRotatingExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (tokenRotatingExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e tokenRotatingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	auth.Metadata["access_token"] = "access-" + e.next
	auth.Metadata["refresh_token"] = "refresh-" + e.next
	return auth, nil
}

func (tokenRotatingExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (tokenRotatingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// identitylessClaude is a Claude OAuth credential without identity metadata, as the auth
// file of a setup token holds it, optionally with an account_uuid.
func identitylessClaude(id, token, accountUUID string) *coreauth.Auth {
	metadata := map[string]any{"type": "claude", "access_token": "access-" + token, "refresh_token": "refresh-" + token}
	if accountUUID != "" {
		metadata["account_uuid"] = accountUUID
	}
	return &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: metadata}
}

// newManagedCache registers auth with a manager whose Claude refresh rotates the tokens
// to "2", and creates a cache over the manager's credentials that has read auth.
func newManagedCache(t *testing.T, now *time.Time, fetcher *fakeFetcher, auth *coreauth.Auth) (*coreauth.Manager, *UsageCache) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(tokenRotatingExecutor{next: "2"})
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	cache := NewUsageCache(manager.List, nil)
	cache.fetch = fetcher.fetch
	cache.nowFunc = func() time.Time { return *now }
	cache.refresh(*now)
	settle(cache)
	if got := fetcher.count(auth.ID); got != 1 {
		t.Fatalf("lookups = %d, want the first lookup", got)
	}
	return manager, cache
}

// managedAuth returns the manager's current auth of id.
func managedAuth(t *testing.T, manager *coreauth.Manager, id string) *coreauth.Auth {
	t.Helper()
	auth, ok := manager.GetByID(id)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", id)
	}
	return auth
}

// The proxy's own token refresh changes the tokens of an account without identity
// metadata but not the account: its reading is kept, and it is not looked up again
// before refreshAfter.
func TestUsageCacheKeepsTheReadingAcrossTheProxysTokenRefresh(t *testing.T) {
	for _, tc := range []struct {
		name        string
		accountUUID string
	}{
		{name: "no identity"},
		{name: "synthesized account uuid", accountUUID: "synthetic-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := testNow
			fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{"x": weeklyUsed(40)}}
			manager, cache := newManagedCache(t, &now, fetcher, identitylessClaude("x", "1", tc.accountUUID))

			refreshed, errRefresh := manager.ForceRefreshAuth(context.Background(), "x")
			if errRefresh != nil || refreshed == nil || refreshed.Metadata["access_token"] != "access-2" {
				t.Fatalf("ForceRefreshAuth() = %v, %v, want the rotated tokens", refreshed, errRefresh)
			}
			current := managedAuth(t, manager, "x")
			if reading, ok := cache.QuotaReading(current); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.4 {
				t.Fatalf("reading after the proxy's refresh = %+v (%v), want the kept reading", reading, ok)
			}
			now = testNow.Add(time.Minute)
			cache.refresh(now)
			settle(cache)
			if got := fetcher.count("x"); got != 1 {
				t.Fatalf("lookups = %d, want none within refreshAfter of the first", got)
			}
			if _, ok := cache.QuotaReading(current); !ok {
				t.Fatal("the reading was dropped after the refresh")
			}
		})
	}
}

// Only the proxy's own changes keep the reading of an account without identity metadata.
// The file its refresh wrote reloads with the same tokens and keeps it. A file replaced
// with other tokens (a new login, or another tool) may hold another account, so its
// reading is dropped and it is looked up again at once.
func TestUsageCacheDropsTheReadingWhenAnotherLoginReplacesTheTokens(t *testing.T) {
	ctx := context.Background()
	now := testNow
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]usageReading{"x": weeklyUsed(40)}}
	manager, cache := newManagedCache(t, &now, fetcher, identitylessClaude("x", "1", "synthetic-x"))

	if _, errRefresh := manager.ForceRefreshAuth(ctx, "x"); errRefresh != nil {
		t.Fatalf("ForceRefreshAuth() error = %v", errRefresh)
	}
	// The watcher reloads the file the refresh wrote.
	if _, errUpdate := manager.Update(ctx, identitylessClaude("x", "2", "synthetic-x")); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	reloaded := managedAuth(t, manager, "x")
	now = testNow.Add(time.Minute)
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(reloaded); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.4 || fetcher.count("x") != 1 {
		t.Fatalf("reloaded file: reading %+v (%v) after %d lookups, want the kept reading", reading, ok, fetcher.count("x"))
	}

	// Another login replaces the file with other tokens and the same synthesized account_uuid.
	fetcher.mu.Lock()
	fetcher.result["x"] = weeklyUsed(70)
	fetcher.mu.Unlock()
	if _, errUpdate := manager.Update(ctx, identitylessClaude("x", "3", "synthetic-x")); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	replaced := managedAuth(t, manager, "x")
	if reading, ok := cache.QuotaReading(replaced); ok {
		t.Fatalf("the replaced file read the previous reading %+v", reading)
	}
	cache.refresh(now)
	settle(cache)
	if reading, ok := cache.QuotaReading(replaced); !ok || reading.Weekly == nil || reading.Weekly.Used != 0.7 || fetcher.count("x") != 2 {
		t.Fatalf("replaced file: reading %+v (%v) after %d lookups, want a new lookup at once", reading, ok, fetcher.count("x"))
	}
}
