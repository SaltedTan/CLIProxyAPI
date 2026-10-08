package keyusage

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestParseFableReading(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    float64
		reset   string
		noFable bool
	}{
		{
			name:  "modern active limit",
			body:  `{"limits":[{"kind":"weekly_scoped","percent":64,"resets_at":"2026-10-10T10:00:00.000000+00:00","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`,
			want:  64,
			reset: "2026-10-10T10:00:00Z",
		},
		{
			name:  "versioned display name and active entry wins",
			body:  `{"limits":[{"kind":"WEEKLY_SCOPED","percent":12,"resets_at":"2026-10-09T10:00:00Z","is_active":false,"scope":{"model":{"display_name":"Fable 5"}}},{"kind":"weekly_scoped","percent":30,"resets_at":"2026-10-10T10:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable 5.1"}}}]}`,
			want:  30,
			reset: "2026-10-10T10:00:00Z",
		},
		{
			name:  "legacy field when the modern percent is missing",
			body:  `{"iguana_necktie":{"utilization":41,"resets_at":"2026-10-11T10:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Fable"}}}]}`,
			want:  41,
			reset: "2026-10-11T10:00:00Z",
		},
		{
			name:    "other models and windows are not Fable",
			body:    `{"seven_day":{"utilization":20,"resets_at":"2026-10-11T10:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":35,"scope":{"model":{"display_name":"Sonnet 5"}}},{"kind":"session","percent":50,"scope":{"model":{"display_name":"Fable"}}}]}`,
			noFable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reading, errParse := parseFableReading([]byte(tc.body))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if reading.hasFable == tc.noFable {
				t.Fatalf("hasFable = %v", reading.hasFable)
			}
			if tc.noFable {
				return
			}
			if reading.used != tc.want {
				t.Fatalf("used = %v, want %v", reading.used, tc.want)
			}
			if want, _ := time.Parse(time.RFC3339, tc.reset); !reading.resetAt.Equal(want) {
				t.Fatalf("reset = %v, want %v", reading.resetAt, want)
			}
		})
	}
}

func TestParseFableReadingReadsTheWeeklyWindow(t *testing.T) {
	reading, errParse := parseFableReading([]byte(`{"seven_day":{"utilization":100,"resets_at":"2026-10-09T08:00:00+00:00"},"limits":[{"kind":"weekly_scoped","percent":20,"resets_at":"2026-10-10T10:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if want := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC); reading.weeklyUsed != 100 || !reading.weeklyResetAt.Equal(want) || reading.used != 20 {
		t.Fatalf("reading = %+v", reading)
	}
}

func fable(used float64, resetIn time.Duration) fableReading {
	return fableReading{at: testNow.Add(-time.Minute), hasFable: true, used: used, resetAt: testNow.Add(resetIn)}
}

func TestCombineFableWeighsAccountsByPlan(t *testing.T) {
	summary := combineFable(testNow, []fableAccount{
		{units: 1, reading: fable(100, 30*time.Hour), ok: true},  // Pro, used up
		{units: 10, reading: fable(20, 50*time.Hour), ok: true},  // Max 20x
		{units: 5, reading: fable(0, 10*time.Hour), ok: true},    // Max 5x, unused
		{units: 5, reading: fableReading{at: testNow}, ok: true}, // reports no Fable window
	})
	// Capacity 16 Pro units, 0 + 8 + 5 left: 81.25%. A plain average of the
	// percentages would say 60%.
	if !summary.Available || summary.Partial || summary.RemainingPercent != 81.3 || summary.UsedPercent != 18.7 {
		t.Fatalf("summary = %+v", summary)
	}
	// The unused account resets first but restores nothing; the Pro one is next.
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(30*time.Hour)) || summary.NextResetRestoresPercent != 6.3 {
		t.Fatalf("next reset = %v +%v%%", summary.NextResetAt, summary.NextResetRestoresPercent)
	}
	if !summary.UpdatedAt.Equal(testNow.Add(-time.Minute)) {
		t.Fatalf("updated at = %v", summary.UpdatedAt)
	}
}

func TestCombineFableEdgeCases(t *testing.T) {
	exhausted := combineFable(testNow, []fableAccount{
		{units: 1, reading: fable(100, 3*time.Hour), ok: true},
		{units: 1, reading: fable(104, 3*time.Hour+20*time.Second), ok: true},
		{units: 1, ok: false},
	})
	// Resets within a minute are one top-up; usage over 100% counts as 100%.
	if exhausted.RemainingPercent != 0 || exhausted.NextResetRestoresPercent != 100 || !exhausted.Partial {
		t.Fatalf("exhausted = %+v", exhausted)
	}

	rolledOver := combineFable(testNow, []fableAccount{{units: 1, reading: fable(80, -time.Minute), ok: true}})
	if rolledOver.RemainingPercent != 100 || rolledOver.NextResetAt != nil {
		t.Fatalf("a window that reset since it was read is empty: %+v", rolledOver)
	}

	none := combineFable(testNow, []fableAccount{{units: 1, reading: fableReading{at: testNow}, ok: true}})
	if none.Available || none.UpdatedAt != nil {
		t.Fatalf("no Fable account must be unavailable: %+v", none)
	}
}

// weeklyUsedUp marks the account's overall weekly window as used up until resetIn.
func weeklyUsedUp(reading fableReading, resetIn time.Duration) fableReading {
	reading.weeklyUsed = 100
	reading.weeklyResetAt = testNow.Add(resetIn)
	return reading
}

func TestCombineFableCountsBlockedAccountsAsEmpty(t *testing.T) {
	summary := combineFable(testNow, []fableAccount{
		// Pro: 80% of its Fable unused but out of weekly allowance for 10 hours.
		{units: 1, reading: weeklyUsedUp(fable(20, 30*time.Hour), 10*time.Hour), ok: true},
		// Max 20x: half its Fable left.
		{units: 10, reading: fable(50, 40*time.Hour), ok: true},
		// Max 5x: blocked until 20 hours, when its Fable window has reset as well.
		{units: 5, reading: weeklyUsedUp(fable(30, 5*time.Hour), 20*time.Hour), ok: true},
	})
	// Only the Max 20x account can serve Fable: 5 of 16 Pro units.
	if summary.RemainingPercent != 31.3 || summary.UsedPercent != 68.7 {
		t.Fatalf("summary = %+v", summary)
	}
	// The Pro account comes back first, with its unused 0.8 units.
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(10*time.Hour)) || summary.NextResetRestoresPercent != 5 {
		t.Fatalf("next reset = %v +%v%%", summary.NextResetAt, summary.NextResetRestoresPercent)
	}

	// A blocked account with no Fable left comes back when its Fable window resets.
	usedUp := combineFable(testNow, []fableAccount{
		{units: 1, reading: weeklyUsedUp(fable(100, 30*time.Hour), 10*time.Hour), ok: true},
	})
	if usedUp.RemainingPercent != 0 || usedUp.NextResetAt == nil || !usedUp.NextResetAt.Equal(testNow.Add(30*time.Hour)) || usedUp.NextResetRestoresPercent != 100 {
		t.Fatalf("used up = %+v", usedUp)
	}

	// A weekly window that has reset since it was read no longer blocks.
	reset := combineFable(testNow, []fableAccount{
		{units: 1, reading: weeklyUsedUp(fable(40, 48*time.Hour), -time.Minute), ok: true},
	})
	if reset.RemainingPercent != 60 {
		t.Fatalf("reset = %+v", reset)
	}
}

type fakeFetcher struct {
	mu     sync.Mutex
	calls  map[string]int
	result map[string]fableReading
	fail   map[string]bool
	// gate, when set, holds lookups until it is closed.
	gate chan struct{}
}

func (f *fakeFetcher) fetch(_ context.Context, auth *coreauth.Auth) (fableReading, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[auth.ID]++
	if f.fail[auth.ID] {
		return fableReading{}, errors.New("status 429")
	}
	return f.result[auth.ID], nil
}

func (f *fakeFetcher) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func claudeOAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "token-" + id}}
}

func newTestPool(now *time.Time, fetcher *fakeFetcher, auths ...*coreauth.Auth) *FablePool {
	pool := NewFablePool(func() []*coreauth.Auth { return auths }, nil)
	pool.servesFable = nil
	pool.fetch = fetcher.fetch
	pool.weight = func(*coreauth.Auth) float64 { return 1 }
	pool.nowFunc = func() time.Time { return *now }
	return pool
}

// settle waits for the lookups in flight.
func settle(pool *FablePool) {
	pool.mu.Lock()
	pending := make([]chan struct{}, 0, len(pool.inflight))
	for _, running := range pool.inflight {
		pending = append(pending, running.done)
	}
	pool.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

func TestFablePoolCachesLookups(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{
		calls: map[string]int{},
		result: map[string]fableReading{
			"a": {hasFable: true, used: 50, resetAt: testNow.Add(48 * time.Hour)},
			"b": {hasFable: true, used: 0, resetAt: testNow.Add(24 * time.Hour)},
		},
	}
	disabled := claudeOAuth("off")
	disabled.Disabled = true
	apiKey := &coreauth.Auth{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "sk"}}
	pool := newTestPool(&now, fetcher, claudeOAuth("a"), claudeOAuth("b"), disabled, apiKey)

	// The first reader waits for accounts never read.
	first := pool.Summary(context.Background(), time.Minute)
	if !first.Available || first.Partial || first.RemainingPercent != 75 {
		t.Fatalf("first summary = %+v", first)
	}
	// Many readers within refreshAfter make no lookup.
	for i := 0; i < 50; i++ {
		now = testNow.Add(time.Duration(i) * 5 * time.Second)
		pool.Summary(context.Background(), time.Minute)
	}
	settle(pool)
	if fetcher.count("a") != 1 || fetcher.count("b") != 1 || fetcher.count("off") != 0 || fetcher.count("key") != 0 {
		t.Fatalf("calls = %v, want one lookup per OAuth account", fetcher.calls)
	}

	// A stale reading is served while it refreshes in the background.
	now = testNow.Add(refreshAfter + time.Second)
	gate := make(chan struct{})
	fetcher.mu.Lock()
	fetcher.result["a"] = fableReading{hasFable: true, used: 100, resetAt: testNow.Add(48 * time.Hour)}
	fetcher.gate = gate
	fetcher.mu.Unlock()
	if stale := pool.Summary(context.Background(), time.Minute); stale.RemainingPercent != 75 {
		t.Fatalf("stale summary = %+v, want the cached figure", stale)
	}
	close(gate)
	settle(pool)
	if fresh := pool.Summary(context.Background(), time.Minute); fresh.RemainingPercent != 50 || fetcher.count("a") != 2 {
		t.Fatalf("fresh summary = %+v after %d lookups", fresh, fetcher.count("a"))
	}
}

func TestFablePoolBacksOffAndAgesOutFailures(t *testing.T) {
	now := testNow
	fetcher := &fakeFetcher{
		calls:  map[string]int{},
		result: map[string]fableReading{"a": {hasFable: true, used: 40, resetAt: testNow.Add(48 * time.Hour)}},
		fail:   map[string]bool{"b": true},
	}
	pool := newTestPool(&now, fetcher, claudeOAuth("a"), claudeOAuth("b"))

	summary := pool.Summary(context.Background(), time.Minute)
	if !summary.Partial || summary.RemainingPercent != 60 {
		t.Fatalf("summary = %+v, want a partial figure from account a", summary)
	}
	now = testNow.Add(time.Minute)
	pool.Summary(context.Background(), time.Minute)
	settle(pool)
	if fetcher.count("b") != 1 {
		t.Fatalf("a failed account was looked up %d times within refreshAfter", fetcher.count("b"))
	}

	// Once every refresh fails for longer than maxReadingAge, a reading stops counting.
	fetcher.mu.Lock()
	fetcher.fail["a"] = true
	fetcher.mu.Unlock()
	for step := time.Duration(0); step <= maxReadingAge; step += refreshAfter {
		now = testNow.Add(step + refreshAfter)
		pool.Summary(context.Background(), 0)
		settle(pool)
	}
	if aged := pool.Summary(context.Background(), 0); aged.Available || !aged.Partial {
		t.Fatalf("aged summary = %+v", aged)
	}
	if math.Abs(float64(fetcher.count("a")-fetcher.count("b"))) > 1 {
		t.Fatalf("calls = %v", fetcher.calls)
	}
}

func TestIsFableModelFollowsAliases(t *testing.T) {
	cases := []struct {
		model registry.ModelInfo
		want  bool
	}{
		{registry.ModelInfo{ID: "claude-fable-5-1"}, true},
		{registry.ModelInfo{ID: "premium", MetadataModelID: "claude-fable-5-1"}, true},
		{registry.ModelInfo{ID: "fable-lite", MetadataModelID: "claude-sonnet-5-5"}, false},
		{registry.ModelInfo{ID: "claude-sonnet-5-5"}, false},
	}
	for _, tc := range cases {
		if got := isFableModel(&tc.model); got != tc.want {
			t.Fatalf("isFableModel(%+v) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestFablePoolForgetsAccountsThatLeave(t *testing.T) {
	now := testNow
	release := make(chan struct{})
	var mu sync.Mutex
	cancelled := false
	fetcher := &fakeFetcher{calls: map[string]int{}, fail: map[string]bool{"failing": true}}
	auths := []*coreauth.Auth{claudeOAuth("failing"), claudeOAuth("hanging")}
	pool := NewFablePool(func() []*coreauth.Auth {
		mu.Lock()
		defer mu.Unlock()
		return auths
	}, nil)
	pool.servesFable = nil
	pool.weight = func(*coreauth.Auth) float64 { return 1 }
	pool.nowFunc = func() time.Time { return now }
	pool.fetch = func(ctx context.Context, auth *coreauth.Auth) (fableReading, error) {
		if auth.ID != "hanging" {
			return fetcher.fetch(ctx, auth)
		}
		select {
		case <-ctx.Done():
			mu.Lock()
			cancelled = true
			mu.Unlock()
		case <-release:
		}
		return fable(10, 24*time.Hour), nil
	}

	pool.Summary(context.Background(), 0)
	pool.mu.Lock()
	hanging := pool.inflight["hanging"]
	pool.mu.Unlock()
	if hanging == nil {
		t.Fatal("the hanging lookup did not start")
	}

	mu.Lock()
	auths = nil
	mu.Unlock()
	pool.Summary(context.Background(), 0)
	<-hanging.done
	close(release)
	settle(pool)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if !cancelled || len(pool.tried) != 0 || len(pool.inflight) != 0 || len(pool.readings) != 0 {
		t.Fatalf("cancelled = %v, tried = %v, inflight = %v, readings = %v", cancelled, pool.tried, pool.inflight, pool.readings)
	}
}

func TestFablePoolWaitsForAHangingLookupOnlyOnce(t *testing.T) {
	now := testNow
	release := make(chan struct{})
	fetcher := &fakeFetcher{calls: map[string]int{}, result: map[string]fableReading{"good": fable(50, 24*time.Hour)}}
	pool := newTestPool(&now, fetcher, claudeOAuth("good"), claudeOAuth("hanging"))
	pool.fetch = func(ctx context.Context, auth *coreauth.Auth) (fableReading, error) {
		if auth.ID == "hanging" {
			<-release
			return fableReading{}, errors.New("hung")
		}
		return fetcher.fetch(ctx, auth)
	}
	defer func() {
		close(release)
		settle(pool)
	}()

	// The first reader waits up to wait for the lookups it started.
	if first := pool.Summary(context.Background(), 20*time.Millisecond); !first.Partial {
		t.Fatalf("first summary = %+v, want a partial figure", first)
	}
	pool.mu.Lock()
	good := pool.inflight["good"]
	pool.mu.Unlock()
	if good != nil {
		<-good.done
	}

	// Once wait has passed since the hanging lookup started, readers no longer wait.
	now = testNow.Add(2 * time.Minute)
	done := make(chan FableSummary, 1)
	go func() { done <- pool.Summary(context.Background(), time.Minute) }()
	select {
	case later := <-done:
		if !later.Partial || later.RemainingPercent != 50 {
			t.Fatalf("later summary = %+v", later)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a later reader waited for the hanging lookup")
	}
}
