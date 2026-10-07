// Package clientusage aggregates usage per client API key so operators can see how
// much each caller (typically one key per device) consumes. It is a usage plugin:
// it only reads published usage records and never affects request handling.
package clientusage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// PluginName is the name the tracker registers under on the usage manager.
	PluginName = "client-usage"
	// AnonymousKeyID groups records that carry no client API key, e.g. when
	// access.api-keys is empty and the proxy accepts unauthenticated requests.
	AnonymousKeyID = "anonymous"

	maxTrackedKeys     = 1024
	maxModelsPerKey    = 256
	overflowModelName  = "other"
	unknownModelName   = "unknown"
	dailyRetentionDays = 31
	dateLayout         = "2006-01-02"
)

// Tokens is a non-overlapping token summary derived from the v2 token breakdown.
// Input includes cache reads and writes; Output includes reasoning.
type Tokens struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Total      int64 `json:"total_tokens"`
}

func (t *Tokens) add(other Tokens) {
	t.Input += other.Input
	t.Output += other.Output
	t.Reasoning += other.Reasoning
	t.CacheRead += other.CacheRead
	t.CacheWrite += other.CacheWrite
	t.Total += other.Total
}

// Counters accumulates usage for one key, model, or day. Requests counts successful
// upstream responses, so a request retried on another credential counts once; Failed
// counts failed upstream attempts, including those later retried. Blocked counts
// requests refused by the key's Claude allowance before any upstream attempt; they
// are neither Requests nor Failed. Per-model counters never carry Blocked.
type Counters struct {
	Requests int64  `json:"requests"`
	Failed   int64  `json:"failed"`
	Blocked  int64  `json:"blocked"`
	Tokens   Tokens `json:"tokens"`
}

func (c *Counters) add(failed bool, tokens Tokens) {
	if failed {
		c.Failed++
	} else {
		c.Requests++
	}
	c.Tokens.add(tokens)
}

type keyState struct {
	Totals      Counters                `json:"totals"`
	Models      map[string]*Counters    `json:"models,omitempty"`
	Daily       map[string]*Counters    `json:"daily,omitempty"`
	FirstUsedAt time.Time               `json:"first_used_at"`
	LastUsedAt  time.Time               `json:"last_used_at"`
	Claude      map[string]*claudeShare `json:"claude,omitempty"`
	// Window is the key's current Claude allowance window; nil when none was opened.
	Window *keyWindow `json:"window,omitempty"`
	// WindowFloor is the earliest start a window may have: the last reset of the
	// key's window, or the end of its previous window. Requests that started before
	// it open no window.
	WindowFloor time.Time `json:"window_floor,omitempty"`
}

func (k *keyState) modelCounters(model string) *Counters {
	if k.Models == nil {
		k.Models = make(map[string]*Counters)
	}
	if counters, ok := k.Models[model]; ok {
		return counters
	}
	if len(k.Models) >= maxModelsPerKey {
		model = overflowModelName
		if counters, ok := k.Models[model]; ok {
			return counters
		}
	}
	counters := &Counters{}
	k.Models[model] = counters
	return counters
}

func (k *keyState) dayCounters(day time.Time) *Counters {
	if k.Daily == nil {
		k.Daily = make(map[string]*Counters)
	}
	date := day.Format(dateLayout)
	if counters, ok := k.Daily[date]; ok {
		return counters
	}
	cutoff := day.AddDate(0, 0, -(dailyRetentionDays - 1)).Format(dateLayout)
	for existing := range k.Daily {
		if existing < cutoff {
			delete(k.Daily, existing)
		}
	}
	counters := &Counters{}
	k.Daily[date] = counters
	return counters
}

// Tracker aggregates usage records by client API key and, when limits are set,
// enforces each key's Claude allowance (see Admit). The zero value is not usable;
// create trackers with NewTracker.
type Tracker struct {
	flushMu sync.Mutex
	mu      sync.Mutex
	since   time.Time
	keys    map[string]*keyState
	claude  map[string]*claudeCredential
	// limits caps Claude usage per client key in Pro units per weekly window, keyed
	// by full API key or key ID. Entries are positive and finite.
	limits map[string]float64
	// floors holds the window floor of keys whose state was deleted by Reset, until
	// their next record, so a request started before the reset opens no window.
	floors   map[string]time.Time
	dirty    bool
	path     string
	resolve  func(authID string) (CredentialInfo, bool)
	nowFunc  func() time.Time
	location *time.Location
}

// NewTracker creates an empty in-memory tracker.
func NewTracker() *Tracker {
	return &Tracker{
		keys:     make(map[string]*keyState),
		claude:   make(map[string]*claudeCredential),
		floors:   make(map[string]time.Time),
		location: time.Local,
	}
}

var defaultTracker = NewTracker()

// Default returns the process-wide tracker used by the service and management API.
func Default() *Tracker { return defaultTracker }

// SetCredentialResolver sets how Claude credentials are described when usage is
// attributed, which fixes the Pro units of that usage at the plan of the time.
func (t *Tracker) SetCredentialResolver(resolve func(authID string) (CredentialInfo, bool)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.resolve = resolve
	t.mu.Unlock()
}

func (t *Tracker) now() time.Time {
	if t.nowFunc != nil {
		return t.nowFunc()
	}
	return time.Now()
}

// KeyID returns the stable identifier used for a client API key. Raw keys are never
// stored; an empty key maps to AnonymousKeyID.
func KeyID(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return AnonymousKeyID
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

// HandleUsage implements coreusage.Plugin.
func (t *Tracker) HandleUsage(_ context.Context, record coreusage.Record) {
	if t == nil {
		return
	}
	now := t.now()
	at := record.RequestedAt
	if at.IsZero() {
		at = now
	}
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	tokens := tokensFromBreakdown(detail.TokenBreakdown)
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = unknownModelName
	}
	keyID := KeyID(record.APIKey)

	// Resolve the credential before locking: the lookup can wait on auth persistence.
	t.mu.Lock()
	resolve := t.resolve
	t.mu.Unlock()
	var plan *CredentialInfo
	if authID := claudeAuthID(record); authID != "" && resolve != nil {
		if info, ok := resolve(authID); ok {
			plan = &info
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.since.IsZero() {
		t.since = now
	}
	state := t.keys[keyID]
	// Keys with a Claude allowance are always tracked, past the cap, so it is enforced.
	if state == nil && (len(t.keys) < maxTrackedKeys || t.limitedLocked(record.APIKey)) {
		state = &keyState{FirstUsedAt: at, WindowFloor: t.floors[keyID]}
		delete(t.floors, keyID)
		t.keys[keyID] = state
	}
	if state != nil {
		state.Totals.add(record.Failed, tokens)
		state.modelCounters(model).add(record.Failed, tokens)
		state.dayCounters(at.In(t.location)).add(record.Failed, tokens)
		if at.After(state.LastUsedAt) {
			state.LastUsedAt = at
		}
	}
	credential := t.observeClaudeLocked(record, record.RequestedAt, now, plan)
	// A Claude request opens the key's allowance window when none is running. It
	// does so after the increase its response carries is attributed: that increase
	// is usage from before the request started, which belongs to the period that
	// ended, not to the window this request opens. Weight the key queued before a
	// new window opened is from earlier requests and stays outside it.
	if state != nil && claudeAuthID(record) != "" && state.openWindow(at) {
		t.markPendingOutsideLocked(keyID)
	}
	// Untracked keys still take part so their usage is not charged to other keys.
	if credential != nil {
		credential.queue(keyID, claudeRecordWeight(detail.TokenBreakdown), at, state != nil && state.Window.contains(at))
	}
	t.dirty = true
}

// markPendingOutsideLocked marks the weight a key has queued on every credential
// as outside its allowance window. t.mu must be held.
func (t *Tracker) markPendingOutsideLocked(keyID string) {
	for _, credential := range t.claude {
		if pending := credential.Pending[keyID]; pending != nil {
			pending.Outside = pending.Weight
		}
	}
}

// Reset clears the usage of one key ID, or of every key when keyID is empty.
// Claude window baselines are kept so later attribution stays correct, and a
// request of the key that started before the reset opens no window when its
// record arrives. It reports whether anything was removed.
func (t *Tracker) Reset(keyID string) bool {
	if t == nil {
		return false
	}
	keyID = strings.TrimSpace(keyID)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if keyID == "" {
		for id := range t.keys {
			t.rememberFloorLocked(id, now)
		}
		t.keys = make(map[string]*keyState)
		for _, credential := range t.claude {
			credential.Pending = nil
			credential.Unattributed = claudeShare{}
		}
		t.since = now
		t.dirty = true
		return true
	}
	if _, ok := t.keys[keyID]; !ok {
		return false
	}
	// Pending weight stays so the key's last usage is not charged to other keys.
	t.rememberFloorLocked(keyID, now)
	delete(t.keys, keyID)
	t.dirty = true
	return true
}

// rememberFloorLocked keeps the window floor of a key being deleted until its next
// record. Floors older than a window are dropped: no request is that old. t.mu
// must be held.
func (t *Tracker) rememberFloorLocked(keyID string, now time.Time) {
	for id, floor := range t.floors {
		if now.Sub(floor) > claudeWeeklyWindow {
			delete(t.floors, id)
		}
	}
	t.floors[keyID] = now
}

// ResetWindow ends the current Claude allowance window of one key ID, or of every
// key when keyID is empty: the key's current usage is zero and its next Claude
// request opens a fresh window. Requests that started before the reset open no
// window. Totals, daily history, per-credential totals and credential baselines
// are kept. It reports whether the key is known.
func (t *Tracker) ResetWindow(keyID string) bool {
	if t == nil {
		return false
	}
	keyID = strings.TrimSpace(keyID)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if keyID == "" {
		for _, state := range t.keys {
			state.closeWindow(now)
		}
		t.dirty = true
		return true
	}
	state, ok := t.keys[keyID]
	if !ok {
		return false
	}
	state.closeWindow(now)
	t.dirty = true
	return true
}

func tokensFromBreakdown(breakdown coreusage.TokenBreakdown) Tokens {
	return Tokens{
		Input:      breakdown.Input.TotalTokens,
		Output:     breakdown.Output.TotalTokens,
		Reasoning:  breakdown.Output.ReasoningTokens,
		CacheRead:  breakdown.Input.CacheReadTokens,
		CacheWrite: breakdown.Input.CacheWriteTokens,
		Total:      breakdown.TotalTokens,
	}
}
