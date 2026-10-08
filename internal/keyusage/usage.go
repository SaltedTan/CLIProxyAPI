package keyusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Anthropic reports the 5h, weekly and Fable windows of a Claude account through the
// account's OAuth usage endpoint, including usage made outside the proxy. The usage
// cache reads that endpoint for the enabled Claude OAuth accounts, at most once per
// refreshAfter per account and never on the request path: readers get the cached
// readings. Quota-aware routing reads the 5h, weekly and Fable windows, and the Fable
// pool the Fable and weekly windows.

// usageURL is Anthropic's OAuth usage endpoint. It is a variable only so that
// end-to-end test builds can point it at a local server with -ldflags -X.
var usageURL = "https://api.anthropic.com/api/oauth/usage"

const (
	usageBeta   = "oauth-2025-04-20"
	usageAgent  = "claude-cli/2.1.280 (external, cli)"
	maxBodySize = 1 << 20

	// refreshAfter is how long a reading is reused, and the least time between two
	// lookups of one account whether or not the previous one succeeded.
	refreshAfter = 5 * time.Minute
	// maxReadingAge is how long a reading still counts while refreshes fail.
	maxReadingAge = time.Hour
	// runInterval is how often Run looks for accounts to refresh.
	runInterval = time.Minute

	fiveHourWindow = 5 * time.Hour
	weeklyWindow   = 7 * 24 * time.Hour

	// usageSource labels the cache's readings in quota-aware routing logs.
	usageSource = "oauth-usage"
)

// windowReading is one usage window as last read.
type windowReading struct {
	// ok is set when the endpoint reported the window's utilization.
	ok bool
	// used is the percentage of the window's allowance used, 0 to 100.
	used float64
	// resetAt is zero when the endpoint reported no reset.
	resetAt time.Time
}

// usageReading is one account's OAuth usage endpoint reading.
type usageReading struct {
	// at is when the lookup started, so the reading is no newer than its sample.
	at time.Time
	// auth is the account looked up, cloned when the lookup started.
	auth *coreauth.Auth
	// fiveHour and weekly are the account's 5h and overall 7d windows.
	fiveHour windowReading
	weekly   windowReading
	// fable is the account's weekly Fable window.
	fable windowReading
}

// fableEnded reports whether the reading's Fable window has reset since it was read.
func (r usageReading) fableEnded(now time.Time) bool {
	return r.fable.ok && !r.fable.resetAt.IsZero() && !r.fable.resetAt.After(now)
}

// weeklyBlocked reports whether the account's overall weekly window is used up,
// which stops the account serving Fable until that window resets.
func (r usageReading) weeklyBlocked(now time.Time) bool {
	return r.weekly.used >= 100 && (r.weekly.resetAt.IsZero() || r.weekly.resetAt.After(now))
}

// describe summarises the 5h, weekly and Fable windows for logs.
func (r usageReading) describe() string {
	percent := func(window windowReading) string {
		if !window.ok {
			return "n/a"
		}
		return formatPercent(window.used) + "%"
	}
	return fmt.Sprintf("5h %s, weekly %s, fable %s", percent(r.fiveHour), percent(r.weekly), percent(r.fable))
}

// UsageCache keeps the last OAuth usage endpoint reading of each enabled Claude OAuth
// account. The zero value is not usable; create caches with NewUsageCache.
type UsageCache struct {
	mu       sync.Mutex
	readings map[string]usageReading
	// tried holds each account's last lookup attempt, whether or not it succeeded.
	tried    map[string]attempt
	inflight map[string]*lookup
	// announced holds the accounts whose first reading was logged.
	announced map[string]struct{}

	list    func() []*coreauth.Auth
	fetch   func(ctx context.Context, auth *coreauth.Auth) (usageReading, error)
	nowFunc func() time.Time
}

// attempt is the start of an account's last lookup.
type attempt struct {
	at time.Time
	// auth is the account looked up, cloned when the lookup started. It identifies the
	// account after a failed lookup, which leaves no reading.
	auth *coreauth.Auth
}

// lookup is a running lookup of one account.
type lookup struct {
	// auth is the account looked up, cloned when the lookup started.
	auth   *coreauth.Auth
	done   chan struct{}
	cancel context.CancelFunc
}

// NewUsageCache creates a cache over the Claude accounts list returns. cfg supplies the
// global proxy for lookups.
func NewUsageCache(list func() []*coreauth.Auth, cfg func() *config.Config) *UsageCache {
	return &UsageCache{
		readings:  make(map[string]usageReading),
		tried:     make(map[string]attempt),
		inflight:  make(map[string]*lookup),
		announced: make(map[string]struct{}),
		list:      list,
		fetch: func(ctx context.Context, auth *coreauth.Auth) (usageReading, error) {
			var current *config.Config
			if cfg != nil {
				current = cfg()
			}
			return fetchUsageReading(ctx, helps.NewUtlsHTTPClient(ctx, current, auth, 0), usageURL, auth)
		},
		nowFunc: time.Now,
	}
}

// Run keeps the readings of every account fresh for quota-aware routing until ctx is
// done. Every minute, starting at once, it refreshes the accounts whose reading is
// missing or older than refreshAfter, while active reports true (nil means always);
// otherwise it only forgets the accounts that left or were replaced. When ctx is done
// it cancels the running lookups.
func (c *UsageCache) Run(ctx context.Context, active func() bool) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(runInterval)
	defer ticker.Stop()
	reading := false
	for {
		on := active == nil || active()
		if on != reading {
			reading = on
			if on {
				log.Info("claude usage: reading Claude OAuth accounts from the usage endpoint for quota-aware routing")
			} else {
				log.Info("claude usage: stopped reading Claude OAuth accounts for quota-aware routing")
			}
		}
		if on {
			c.refresh(c.nowFunc())
		} else {
			c.reconcile()
		}
		select {
		case <-ctx.Done():
			c.cancelLookups()
			return
		case <-ticker.C:
		}
	}
}

// refresh starts lookups of the accounts whose reading is missing or older than
// refreshAfter, after forgetting the accounts that left or were replaced.
func (c *UsageCache) refresh(now time.Time) {
	accounts := c.accounts()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileLocked(accounts)
	for _, auth := range accounts {
		if reading, ok := c.readings[auth.ID]; ok && now.Sub(reading.at) < refreshAfter {
			continue
		}
		c.refreshLocked(auth, now)
	}
}

// reconcile forgets the accounts that left or were replaced, without lookups.
func (c *UsageCache) reconcile() {
	accounts := c.accounts()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileLocked(accounts)
}

// cancelLookups cancels the running lookups, which then publish nothing.
func (c *UsageCache) cancelLookups() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, running := range c.inflight {
		running.cancel()
		delete(c.inflight, id)
	}
}

// QuotaReading returns the account's last 5h, weekly and Fable windows for quota-aware
// routing. It never starts or waits for a lookup. It returns false when the account
// has no reading younger than maxReadingAge, or when the reading describes another
// upstream account that had the same auth ID.
func (c *UsageCache) QuotaReading(auth *coreauth.Auth) (coreauth.QuotaReading, bool) {
	if c == nil || auth == nil {
		return coreauth.QuotaReading{}, false
	}
	c.mu.Lock()
	reading, ok := c.readings[auth.ID]
	c.mu.Unlock()
	if !ok || !coreauth.SameQuotaAccount(reading.auth, auth) || c.nowFunc().Sub(reading.at) > maxReadingAge {
		return coreauth.QuotaReading{}, false
	}
	return coreauth.QuotaReading{
		Weekly:     reading.weekly.quotaWindow(reading.at, weeklyWindow),
		Short:      reading.fiveHour.quotaWindow(reading.at, fiveHourWindow),
		Fable:      reading.fable.quotaWindow(reading.at, weeklyWindow),
		ObservedAt: reading.at,
		Source:     usageSource,
	}, true
}

// quotaWindow converts the window for quota-aware routing, or returns nil when it is
// unknown. A window without a reset has not started if nothing is used: it would
// reset one window length after its first use, so at the earliest after the reading.
func (w windowReading) quotaWindow(at time.Time, length time.Duration) *coreauth.QuotaWindowReading {
	if !w.ok {
		return nil
	}
	resetAt := w.resetAt
	if resetAt.IsZero() {
		if w.used != 0 {
			return nil
		}
		resetAt = at.Add(length)
	}
	return &coreauth.QuotaWindowReading{Used: w.used / 100, ResetAt: resetAt}
}

// accounts lists the enabled Claude OAuth accounts.
func (c *UsageCache) accounts() []*coreauth.Auth {
	if c.list == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range c.list() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || oauthToken(auth) == "" {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// reconcileLocked drops the state of the accounts not in accounts, and of the accounts
// whose auth ID now holds another upstream account (an auth file replaced by another
// account's), cancelling their lookups. A replaced account may be looked up again at
// once. c.mu must be held.
func (c *UsageCache) reconcileLocked(accounts []*coreauth.Auth) {
	current := make(map[string]*coreauth.Auth, len(accounts))
	for _, auth := range accounts {
		current[auth.ID] = auth
	}
	// Every reading and lookup has a tried entry.
	for id := range c.tried {
		if auth, ok := current[id]; ok && !c.replacedLocked(auth) {
			continue
		}
		if running, ok := c.inflight[id]; ok {
			running.cancel()
			delete(c.inflight, id)
		}
		delete(c.readings, id)
		delete(c.tried, id)
		delete(c.announced, id)
	}
}

// replacedLocked reports whether the last lookup attempt or the reading of auth's ID is
// of another upstream account. The last attempt covers the running lookup, if any, and
// a failed lookup, which leaves no reading. c.mu must be held.
func (c *UsageCache) replacedLocked(auth *coreauth.Auth) bool {
	if last, ok := c.tried[auth.ID]; ok && !coreauth.SameQuotaAccount(last.auth, auth) {
		return true
	}
	reading, ok := c.readings[auth.ID]
	return ok && !coreauth.SameQuotaAccount(reading.auth, auth)
}

// refreshLocked starts a lookup of the account unless one is running, one started
// within refreshAfter, or the account's access token has expired. It returns the
// running lookup's done channel, or nil. c.mu must be held.
func (c *UsageCache) refreshLocked(auth *coreauth.Auth, now time.Time) chan struct{} {
	if running, ok := c.inflight[auth.ID]; ok {
		return running.done
	}
	if last, ok := c.tried[auth.ID]; ok && now.Sub(last.at) < refreshAfter {
		return nil
	}
	if !auth.HasValidAccessToken(now) {
		// Its refresh failed or is pending; the endpoint would refuse the token.
		return nil
	}
	// The lookup is not tied to the reader's request: it fills the cache for the
	// next reader too. It has no deadline, like other upstream calls, and is
	// cancelled only when the account leaves or is replaced, or Run stops.
	ctx, cancel := context.WithCancel(context.Background())
	running := &lookup{auth: auth.Clone(), done: make(chan struct{}), cancel: cancel}
	id := auth.ID
	c.inflight[id] = running
	c.tried[id] = attempt{at: now, auth: running.auth}
	go func() {
		defer close(running.done)
		defer cancel()
		reading, errFetch := c.fetch(ctx, running.auth)
		c.mu.Lock()
		if c.inflight[id] != running {
			// The account left or was replaced during the lookup, or Run stopped.
			c.mu.Unlock()
			return
		}
		delete(c.inflight, id)
		if errFetch != nil {
			c.mu.Unlock()
			log.Debugf("claude usage: lookup failed for credential %s: %v", id, errFetch)
			return
		}
		// A reading is as old as the start of its lookup, so it never looks newer than
		// a response observed while the lookup ran.
		reading.at = now
		reading.auth = running.auth
		c.readings[id] = reading
		_, announced := c.announced[id]
		c.announced[id] = struct{}{}
		c.mu.Unlock()
		if !announced {
			log.Infof("claude usage: read credential %s (%s)", id, reading.describe())
			return
		}
		log.Debugf("claude usage: read credential %s (%s)", id, reading.describe())
	}()
	return running.done
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

func fetchUsageReading(ctx context.Context, client *http.Client, url string, auth *coreauth.Auth) (usageReading, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errRequest != nil {
		return usageReading{}, fmt.Errorf("build usage request: %w", errRequest)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+oauthToken(auth))
	req.Header.Set("Anthropic-Beta", usageBeta)
	req.Header.Set("User-Agent", usageAgent)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return usageReading{}, fmt.Errorf("usage request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("claude usage: close usage response: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if errRead != nil {
		return usageReading{}, fmt.Errorf("read usage response: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return usageReading{}, fmt.Errorf("usage request returned status %d", resp.StatusCode)
	}
	return parseUsageReading(body)
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// reading returns the window's reading; it is not ok when the window or its
// utilization is missing.
func (w *usageWindow) reading() windowReading {
	if w == nil || w.Utilization == nil {
		return windowReading{}
	}
	return windowReading{ok: true, used: *w.Utilization, resetAt: parseResetTime(w.ResetsAt)}
}

type usagePayload struct {
	FiveHour *usageWindow `json:"five_hour"`
	Weekly   *usageWindow `json:"seven_day"`
	Legacy   *usageWindow `json:"iguana_necktie"`
	Limits   []struct {
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

// parseUsageReading reads an OAuth usage payload: the account's five_hour and overall
// seven_day windows, and its Fable window the way the management panel reads it: the
// active weekly_scoped limit of the Fable model family (any version), else the first
// valid one, else the legacy iguana_necktie window.
func parseUsageReading(body []byte) (usageReading, error) {
	var payload usagePayload
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return usageReading{}, fmt.Errorf("decode usage response: %w", errUnmarshal)
	}
	reading := usageReading{fiveHour: payload.FiveHour.reading(), weekly: payload.Weekly.reading()}
	found := false
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
		reading.fable = windowReading{ok: true, used: *limit.Percent, resetAt: parseResetTime(limit.ResetsAt)}
		found = true
		if limit.IsActive {
			break
		}
	}
	if !found {
		reading.fable = payload.Legacy.reading()
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
