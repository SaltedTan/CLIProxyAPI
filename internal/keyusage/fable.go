package keyusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Anthropic reports the weekly Fable allowance of a Claude account only through the
// account's OAuth usage endpoint, never in response headers the proxy sees. The
// pool reads that endpoint for every enabled Claude OAuth account that serves a
// Fable model, at most once per refreshAfter per account and never on the request
// path: readers get the cached figures, and a stale figure triggers a background
// refresh.
//
// Accounts are combined in Claude Pro units. Each account's Fable percentage is of
// its own plan's Fable allowance, so percentages are weighted by the plan's weekly
// allowance (clientusage.CredentialInfo.PlanProUnits), assuming a plan's Fable
// allowance scales like its overall weekly allowance: the pool's capacity is the sum
// of the weights, and what is left is the sum of each weight times the unused share.

const (
	defaultUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// usageURLEnv overrides the usage endpoint, for tests against a local server.
	usageURLEnv = "CLIPROXY_CLAUDE_USAGE_URL"
	usageBeta   = "oauth-2025-04-20"
	usageAgent  = "claude-cli/2.1.280 (external, cli)"
	maxBodySize = 1 << 20

	// refreshAfter is how long a reading is reused, and the least time between two
	// lookups of one account whether or not the previous one succeeded.
	refreshAfter = 5 * time.Minute
	// maxReadingAge is how long a reading still counts while refreshes fail.
	maxReadingAge = time.Hour
	// resetGroup merges accounts whose windows reset within it into one top-up.
	resetGroup = time.Minute
)

// FableSummary is the Fable allowance left across the accounts that serve Fable, as
// one figure. It names no account.
type FableSummary struct {
	// Available is set when at least one account reported its Fable window.
	Available        bool    `json:"available"`
	RemainingPercent float64 `json:"remaining_percent"`
	UsedPercent      float64 `json:"used_percent"`
	// NextResetAt is the earliest upcoming reset of an account with Fable usage: the
	// next time the pool grows, by NextResetRestoresPercent of its capacity. When
	// nothing is left, it is when Fable becomes available again.
	NextResetAt              *time.Time `json:"next_reset_at,omitempty"`
	NextResetRestoresPercent float64    `json:"next_reset_restores_percent,omitempty"`
	// UpdatedAt is when the oldest reading in the figure was taken.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Partial is set when an account that may serve Fable has no current reading.
	Partial bool `json:"partial"`
}

// fableReading is one account's Fable window as last read.
type fableReading struct {
	at       time.Time
	hasFable bool
	// used is the percentage of the account's Fable allowance used, 0 to 100.
	used    float64
	resetAt time.Time
}

// windowEnded reports whether the reading's Fable window has reset since it was read.
func (r fableReading) windowEnded(now time.Time) bool {
	return r.hasFable && !r.resetAt.IsZero() && !r.resetAt.After(now)
}

// FablePool caches the Fable windows of the Claude accounts. The zero value is not
// usable; create pools with NewFablePool.
type FablePool struct {
	mu       sync.Mutex
	readings map[string]fableReading
	tried    map[string]time.Time
	inflight map[string]chan struct{}

	list        func() []*coreauth.Auth
	servesFable func(authID string) bool
	fetch       func(ctx context.Context, auth *coreauth.Auth) (fableReading, error)
	weight      func(auth *coreauth.Auth) float64
	nowFunc     func() time.Time
}

// NewFablePool creates a pool over the accounts list returns. cfg supplies the
// global proxy for lookups.
func NewFablePool(list func() []*coreauth.Auth, cfg func() *config.Config) *FablePool {
	url := strings.TrimSpace(os.Getenv(usageURLEnv))
	if url == "" {
		url = defaultUsageURL
	}
	return &FablePool{
		readings:    make(map[string]fableReading),
		tried:       make(map[string]time.Time),
		inflight:    make(map[string]chan struct{}),
		list:        list,
		servesFable: registryServesFable,
		fetch: func(ctx context.Context, auth *coreauth.Auth) (fableReading, error) {
			var current *config.Config
			if cfg != nil {
				current = cfg()
			}
			return fetchFableReading(ctx, helps.NewUtlsHTTPClient(ctx, current, auth, 0), url, auth)
		},
		weight: func(auth *coreauth.Auth) float64 {
			return clientusage.CredentialInfoFromAuth(auth).PlanProUnits
		},
		nowFunc: time.Now,
	}
}

// Summary combines the cached readings. Accounts whose reading is stale are refreshed
// in the background; when wait is positive, Summary waits up to wait (or until ctx
// ends) for accounts that have never been read. The lookups themselves continue
// after Summary returns.
func (p *FablePool) Summary(ctx context.Context, wait time.Duration) FableSummary {
	if p == nil || p.list == nil {
		return FableSummary{}
	}
	accounts := p.accounts()
	now := p.nowFunc()

	p.mu.Lock()
	known := make(map[string]struct{}, len(accounts))
	var pending []chan struct{}
	for _, auth := range accounts {
		known[auth.ID] = struct{}{}
		reading, ok := p.readings[auth.ID]
		if ok && now.Sub(reading.at) < refreshAfter && !reading.windowEnded(now) {
			continue
		}
		if done := p.refreshLocked(auth, now); done != nil && !ok {
			pending = append(pending, done)
		}
	}
	for id := range p.readings {
		if _, ok := known[id]; !ok {
			delete(p.readings, id)
			delete(p.tried, id)
		}
	}
	p.mu.Unlock()

	if wait > 0 && len(pending) > 0 {
		waitAll(ctx, pending, wait)
	}

	p.mu.Lock()
	states := make([]fableAccount, 0, len(accounts))
	for _, auth := range accounts {
		reading, ok := p.readings[auth.ID]
		states = append(states, fableAccount{
			units:   p.weight(auth),
			reading: reading,
			ok:      ok && now.Sub(reading.at) <= maxReadingAge,
		})
	}
	p.mu.Unlock()
	return combineFable(p.nowFunc(), states)
}

// accounts lists the enabled Claude OAuth accounts that serve a Fable model.
func (p *FablePool) accounts() []*coreauth.Auth {
	var out []*coreauth.Auth
	for _, auth := range p.list() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || oauthToken(auth) == "" {
			continue
		}
		if p.servesFable != nil && !p.servesFable(auth.ID) {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// refreshLocked starts a lookup of the account unless one is running or one started
// within refreshAfter. It returns the running lookup's done channel, or nil. p.mu
// must be held.
func (p *FablePool) refreshLocked(auth *coreauth.Auth, now time.Time) chan struct{} {
	if done, ok := p.inflight[auth.ID]; ok {
		return done
	}
	if last, ok := p.tried[auth.ID]; ok && now.Sub(last) < refreshAfter {
		return nil
	}
	done := make(chan struct{})
	p.inflight[auth.ID] = done
	p.tried[auth.ID] = now
	go func() {
		defer close(done)
		// The lookup is not tied to the reader's request: it fills the cache for
		// the next reader too. It has no deadline, like other upstream calls.
		reading, errFetch := p.fetch(context.Background(), auth)
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.inflight, auth.ID)
		if errFetch != nil {
			log.Debugf("key usage: Fable lookup failed for credential %s: %v", auth.ID, errFetch)
			return
		}
		reading.at = p.nowFunc()
		p.readings[auth.ID] = reading
	}()
	return done
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
	reading fableReading
	ok      bool
}

// combineFable weighs each account's Fable window by its plan's weekly allowance.
func combineFable(now time.Time, accounts []fableAccount) FableSummary {
	var summary FableSummary
	var capacity, remaining float64
	var oldest, nextReset time.Time
	used := make([]float64, len(accounts))
	for i, account := range accounts {
		if !account.ok {
			summary.Partial = true
			continue
		}
		if !account.reading.hasFable || account.units <= 0 {
			continue
		}
		share := math.Min(math.Max(account.reading.used, 0), 100) / 100
		if account.reading.windowEnded(now) {
			// Anthropic restarted the window empty.
			share = 0
		}
		used[i] = share
		capacity += account.units
		remaining += account.units * (1 - share)
		if oldest.IsZero() || account.reading.at.Before(oldest) {
			oldest = account.reading.at
		}
		if share > 0 && !account.reading.resetAt.IsZero() && (nextReset.IsZero() || account.reading.resetAt.Before(nextReset)) {
			nextReset = account.reading.resetAt
		}
	}
	if capacity <= 0 {
		return summary
	}
	summary.Available = true
	summary.RemainingPercent = roundPercent(100 * remaining / capacity)
	summary.UsedPercent = roundPercent(100 - summary.RemainingPercent)
	summary.UpdatedAt = &oldest
	if !nextReset.IsZero() {
		var restored float64
		for i, account := range accounts {
			if used[i] > 0 && !account.reading.resetAt.IsZero() && account.reading.resetAt.Sub(nextReset) < resetGroup {
				restored += account.units * used[i]
			}
		}
		summary.NextResetAt = &nextReset
		summary.NextResetRestoresPercent = roundPercent(100 * restored / capacity)
	}
	return summary
}

func roundPercent(value float64) float64 {
	return math.Round(value*10) / 10
}

// registryServesFable reports whether the credential registered a Fable model, so
// accounts whose Fable models are excluded by configuration stay out of the pool.
func registryServesFable(authID string) bool {
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(authID) {
		if model != nil && strings.Contains(strings.ToLower(model.ID), "fable") {
			return true
		}
	}
	return false
}

// oauthToken returns the OAuth access token of a Claude account, or "" for API key
// credentials, which have no usage endpoint.
func oauthToken(auth *coreauth.Auth) string {
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

func fetchFableReading(ctx context.Context, client *http.Client, url string, auth *coreauth.Auth) (fableReading, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errRequest != nil {
		return fableReading{}, fmt.Errorf("build usage request: %w", errRequest)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+oauthToken(auth))
	req.Header.Set("Anthropic-Beta", usageBeta)
	req.Header.Set("User-Agent", usageAgent)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return fableReading{}, fmt.Errorf("usage request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("key usage: close usage response: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if errRead != nil {
		return fableReading{}, fmt.Errorf("read usage response: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return fableReading{}, fmt.Errorf("usage request returned status %d", resp.StatusCode)
	}
	return parseFableReading(body)
}

type usagePayload struct {
	Legacy *struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	} `json:"iguana_necktie"`
	Limits []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
		IsActive bool     `json:"is_active"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

// parseFableReading reads the Fable window of an OAuth usage payload the way the
// management panel does: the active weekly_scoped limit of the Fable model family
// (any version), else the first valid one, else the legacy iguana_necktie window.
func parseFableReading(body []byte) (fableReading, error) {
	var payload usagePayload
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return fableReading{}, fmt.Errorf("decode usage response: %w", errUnmarshal)
	}
	found := false
	var reading fableReading
	for _, limit := range payload.Limits {
		if !strings.EqualFold(strings.TrimSpace(limit.Kind), "weekly_scoped") || limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		family := strings.Fields(strings.ToLower(limit.Scope.Model.DisplayName))
		if len(family) == 0 || family[0] != "fable" {
			continue
		}
		if found && !limit.IsActive {
			continue
		}
		reading = fableReading{hasFable: true, used: *limit.Percent, resetAt: parseResetTime(limit.ResetsAt)}
		found = true
		if limit.IsActive {
			break
		}
	}
	if !found && payload.Legacy != nil && payload.Legacy.Utilization != nil {
		reading = fableReading{hasFable: true, used: *payload.Legacy.Utilization, resetAt: parseResetTime(payload.Legacy.ResetsAt)}
	}
	return reading, nil
}

func parseResetTime(raw string) time.Time {
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if errParse != nil {
		return time.Time{}
	}
	return parsed
}
