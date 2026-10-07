package clientusage

import (
	"math"
	"sort"
	"strings"
	"time"
)

// SnapshotOptions supplies the configuration a report is rendered against.
type SnapshotOptions struct {
	// APIKeys lists the configured client API keys; they are reported even when unused.
	APIKeys []string
	// APIKeyNames maps a full client API key, or a key ID, to a display name.
	APIKeyNames map[string]string
	// Credential resolves a Claude credential by auth ID. When nil, the tracker's
	// credential resolver is used.
	Credential func(authID string) (CredentialInfo, bool)
}

// Snapshot is the per client API key usage report.
type Snapshot struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Since       *time.Time `json:"since,omitempty"`
	// ClaudeLimitsSupported tells readers that this backend reports and enforces
	// per key Claude allowances (the limit_* fields and blocked counters).
	ClaudeLimitsSupported bool                    `json:"claude_limits_supported"`
	Keys                  []KeyUsage              `json:"keys"`
	ClaudeCredentials     []ClaudeCredentialUsage `json:"claude_credentials"`
}

// KeyUsage is the usage of one client API key.
type KeyUsage struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name,omitempty"`
	Key         string                   `json:"key,omitempty"`
	Configured  bool                     `json:"configured"`
	FirstUsedAt *time.Time               `json:"first_used_at,omitempty"`
	LastUsedAt  *time.Time               `json:"last_used_at,omitempty"`
	Totals      Counters                 `json:"totals"`
	Models      map[string]ModelCounters `json:"models,omitempty"`
	Daily       []DailyUsage             `json:"daily,omitempty"`
	Claude      *KeyClaudeUsage          `json:"claude,omitempty"`
}

// ModelCounters is the usage of one key on one model. Refusals happen before a
// credential and model are used, so there is no blocked count per model.
type ModelCounters struct {
	Requests int64  `json:"requests"`
	Failed   int64  `json:"failed"`
	Tokens   Tokens `json:"tokens"`
}

// DailyUsage is the usage of one key on one local calendar day.
type DailyUsage struct {
	Date string `json:"date"`
	Counters
}

// KeyClaudeUsage is the Claude subscription usage attributed to one key, in Claude
// Pro units: 1.0 is one full weekly allowance of a Claude Pro plan. The limit
// fields are set when the key has a configured allowance; LimitResetsAt is the
// earliest window reset among the credentials the key currently uses.
type KeyClaudeUsage struct {
	CurrentProUnits   float64                    `json:"current_pro_units"`
	TotalProUnits     float64                    `json:"total_pro_units"`
	LimitProUnits     *float64                   `json:"limit_pro_units,omitempty"`
	RemainingProUnits *float64                   `json:"remaining_pro_units,omitempty"`
	LimitReached      bool                       `json:"limit_reached"`
	LimitResetsAt     *time.Time                 `json:"limit_resets_at,omitempty"`
	Credentials       []KeyClaudeCredentialUsage `json:"credentials"`
}

// ClaudeCredentialRef identifies a Claude credential and its plan in reports.
type ClaudeCredentialRef struct {
	AuthID       string  `json:"auth_id"`
	AuthIndex    string  `json:"auth_index,omitempty"`
	Label        string  `json:"label,omitempty"`
	Plan         string  `json:"plan"`
	PlanProUnits float64 `json:"plan_pro_units"`
	PlanSource   string  `json:"plan_source"`
}

// KeyClaudeCredentialUsage is one key's share of one credential's weekly limit.
// Fractions are of that credential's own weekly limit. Pro units use the plan at the
// time the usage was attributed, or the current plan for usage attributed while the
// plan was unknown.
type KeyClaudeCredentialUsage struct {
	ClaudeCredentialRef
	WindowResetsAt  *time.Time `json:"window_resets_at,omitempty"`
	CurrentFraction float64    `json:"current_fraction"`
	CurrentProUnits float64    `json:"current_pro_units"`
	TotalFraction   float64    `json:"total_fraction"`
	TotalProUnits   float64    `json:"total_pro_units"`
}

// ClaudeCredentialUsage is the latest weekly window state of one Claude credential.
type ClaudeCredentialUsage struct {
	ClaudeCredentialRef
	WeeklyUtilization           float64    `json:"weekly_utilization"`
	WindowResetsAt              *time.Time `json:"window_resets_at,omitempty"`
	ObservedAt                  *time.Time `json:"observed_at,omitempty"`
	UnattributedCurrentFraction float64    `json:"unattributed_current_fraction"`
	UnattributedTotalFraction   float64    `json:"unattributed_total_fraction"`
}

// Snapshot renders the current usage report. Keys are listed configured keys first,
// in configuration order, then every other key that has usage or a configured limit,
// sorted by ID.
func (t *Tracker) Snapshot(opts SnapshotOptions) Snapshot {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	snapshot := Snapshot{GeneratedAt: now, Since: optionalTime(t.since), ClaudeLimitsSupported: true}
	resolve := opts.Credential
	if resolve == nil {
		resolve = t.resolve
	}
	credentialRef := t.credentialRefLocked(resolve)
	configured := make(map[string]struct{}, len(opts.APIKeys))
	for _, apiKey := range opts.APIKeys {
		if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
			configured[apiKey] = struct{}{}
		}
	}
	limits := t.limitsByIDLocked(configured)

	seen := make(map[string]struct{}, len(opts.APIKeys))
	for _, apiKey := range opts.APIKeys {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		id := KeyID(apiKey)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		limit, _ := t.limitForLocked(apiKey)
		usage := t.keyUsageLocked(id, now, credentialRef, limit)
		usage.Key = maskKey(apiKey)
		usage.Configured = true
		usage.Name = keyName(opts.APIKeyNames, apiKey, id)
		snapshot.Keys = append(snapshot.Keys, usage)
	}
	// others maps the remaining key IDs to their full key when a limit was
	// configured for it, so the report can show the masked key.
	others := make(map[string]string, len(t.keys)+len(limits))
	for id := range t.keys {
		if _, ok := seen[id]; !ok {
			others[id] = ""
		}
	}
	for id, limit := range limits {
		if _, ok := seen[id]; !ok && (others[id] == "" || limit.APIKey != "") {
			others[id] = limit.APIKey
		}
	}
	ids := make([]string, 0, len(others))
	for id := range others {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		usage := t.keyUsageLocked(id, now, credentialRef, limits[id].Limit)
		apiKey := others[id]
		if apiKey != "" {
			usage.Key = maskKey(apiKey)
		}
		usage.Name = keyName(opts.APIKeyNames, apiKey, id)
		snapshot.Keys = append(snapshot.Keys, usage)
	}
	if snapshot.Keys == nil {
		snapshot.Keys = []KeyUsage{}
	}

	authIDs := make([]string, 0, len(t.claude))
	for authID := range t.claude {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	snapshot.ClaudeCredentials = make([]ClaudeCredentialUsage, 0, len(authIDs))
	for _, authID := range authIDs {
		credential := t.claude[authID]
		usage := ClaudeCredentialUsage{
			ClaudeCredentialRef:       credentialRef(authID),
			ObservedAt:                optionalTime(credential.ObservedAt),
			UnattributedTotalFraction: roundFraction(credential.Unattributed.Total),
		}
		if credential.windowOpen(now) {
			usage.WeeklyUtilization = roundFraction(credential.Utilization)
			usage.WindowResetsAt = optionalTime(credential.ResetAt)
			usage.UnattributedCurrentFraction = roundFraction(credential.currentShare(credential.Unattributed, now).Window)
		}
		snapshot.ClaudeCredentials = append(snapshot.ClaudeCredentials, usage)
	}
	return snapshot
}

// credentialRefLocked returns a resolver of report details for a credential, falling
// back to the description last seen in usage for credentials that were removed.
// t.mu must be held while the returned function is used.
func (t *Tracker) credentialRefLocked(resolve func(authID string) (CredentialInfo, bool)) func(string) ClaudeCredentialRef {
	return func(authID string) ClaudeCredentialRef {
		ref := ClaudeCredentialRef{AuthID: authID, Plan: PlanUnknown, PlanProUnits: 1, PlanSource: PlanSourceWeight}
		credential := t.claude[authID]
		info, ok := CredentialInfo{}, false
		if resolve != nil {
			info, ok = resolve(authID)
		}
		if !ok && credential != nil && credential.Info != nil {
			// The credential was removed; describe it as it was last seen.
			info, ok = *credential.Info, true
		}
		if ok {
			ref.AuthIndex, ref.Label = info.AuthIndex, info.Label
			ref.Plan, ref.PlanProUnits, ref.PlanSource = info.Plan, info.PlanProUnits, info.PlanSource
		}
		if ref.AuthIndex == "" && credential != nil {
			ref.AuthIndex = credential.AuthIndex
		}
		return ref
	}
}

// keyUsageLocked renders one key. limit is its Claude allowance in Pro units, or 0
// for none. t.mu must be held.
func (t *Tracker) keyUsageLocked(id string, now time.Time, credentialRef func(string) ClaudeCredentialRef, limit float64) KeyUsage {
	usage := KeyUsage{ID: id}
	state := t.keys[id]
	if state == nil {
		if limit > 0 {
			usage.Claude = &KeyClaudeUsage{Credentials: []KeyClaudeCredentialUsage{}}
			applyClaudeLimit(usage.Claude, 0, limit)
		}
		return usage
	}
	usage.FirstUsedAt = optionalTime(state.FirstUsedAt)
	usage.LastUsedAt = optionalTime(state.LastUsedAt)
	usage.Totals = state.Totals
	if len(state.Models) > 0 {
		usage.Models = make(map[string]ModelCounters, len(state.Models))
		for model, counters := range state.Models {
			usage.Models[model] = ModelCounters{Requests: counters.Requests, Failed: counters.Failed, Tokens: counters.Tokens}
		}
	}
	cutoff := now.In(t.location).AddDate(0, 0, -(dailyRetentionDays - 1)).Format(dateLayout)
	dates := make([]string, 0, len(state.Daily))
	for date := range state.Daily {
		if date >= cutoff {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	for _, date := range dates {
		usage.Daily = append(usage.Daily, DailyUsage{Date: date, Counters: *state.Daily[date]})
	}
	if len(state.Claude) == 0 && limit <= 0 {
		return usage
	}
	claude, current, _ := t.claudeUsageLocked(state, now, credentialRef)
	applyClaudeLimit(claude, current, limit)
	usage.Claude = claude
	return usage
}

// claudeUsageLocked renders the Claude usage of one key. It also returns the
// unrounded sum of current-window Pro units, which admission compares with the
// key's limit, and the earliest open-window reset among the credentials with
// current usage (zero when none). t.mu must be held.
func (t *Tracker) claudeUsageLocked(state *keyState, now time.Time, credentialRef func(string) ClaudeCredentialRef) (*KeyClaudeUsage, float64, time.Time) {
	claude := &KeyClaudeUsage{Credentials: make([]KeyClaudeCredentialUsage, 0, len(state.Claude))}
	authIDs := make([]string, 0, len(state.Claude))
	for authID := range state.Claude {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	var current, total float64
	var resetsAt time.Time
	for _, authID := range authIDs {
		share := state.Claude[authID]
		entry := KeyClaudeCredentialUsage{ClaudeCredentialRef: credentialRef(authID)}
		entry.TotalFraction = share.Total
		entry.TotalProUnits = share.TotalProUnits + share.TotalUnpriced*entry.PlanProUnits
		if credential := t.claude[authID]; credential.windowOpen(now) {
			window := credential.currentShare(*share, now)
			entry.CurrentFraction = window.Window
			entry.CurrentProUnits = window.WindowProUnits + window.WindowUnpriced*entry.PlanProUnits
			entry.WindowResetsAt = optionalTime(credential.ResetAt)
			if window.Window > 0 && (resetsAt.IsZero() || credential.ResetAt.Before(resetsAt)) {
				resetsAt = credential.ResetAt
			}
		}
		current += entry.CurrentProUnits
		total += entry.TotalProUnits
		entry.CurrentFraction = roundFraction(entry.CurrentFraction)
		entry.CurrentProUnits = roundFraction(entry.CurrentProUnits)
		entry.TotalFraction = roundFraction(entry.TotalFraction)
		entry.TotalProUnits = roundFraction(entry.TotalProUnits)
		claude.Credentials = append(claude.Credentials, entry)
	}
	claude.CurrentProUnits = roundFraction(current)
	claude.TotalProUnits = roundFraction(total)
	claude.LimitResetsAt = optionalTime(resetsAt)
	return claude, current, resetsAt
}

// applyClaudeLimit fills the limit fields from the unrounded current usage, using
// the same comparison as admission.
func applyClaudeLimit(claude *KeyClaudeUsage, current, limit float64) {
	if limit <= 0 {
		return
	}
	remaining := roundFraction(math.Max(limit-current, 0))
	claude.LimitProUnits = &limit
	claude.RemainingProUnits = &remaining
	claude.LimitReached = current >= limit
}

// windowOpen reports whether the last observed weekly window has not reset yet.
func (c *claudeCredential) windowOpen(now time.Time) bool {
	return c != nil && now.Before(c.ResetAt)
}

// currentShare returns share if it belongs to the open window, else an empty share.
func (c *claudeCredential) currentShare(share claudeShare, now time.Time) claudeShare {
	if !c.windowOpen(now) || share.Epoch != c.Epoch {
		return claudeShare{}
	}
	return share
}

func keyName(names map[string]string, apiKey, id string) string {
	if apiKey != "" {
		if name := strings.TrimSpace(names[apiKey]); name != "" {
			return name
		}
	}
	return strings.TrimSpace(names[id])
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func roundFraction(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}
