package clientusage

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
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
	GeneratedAt       time.Time               `json:"generated_at"`
	Since             *time.Time              `json:"since,omitempty"`
	Keys              []KeyUsage              `json:"keys"`
	ClaudeCredentials []ClaudeCredentialUsage `json:"claude_credentials"`
}

// KeyUsage is the usage of one client API key.
type KeyUsage struct {
	ID          string              `json:"id"`
	Name        string              `json:"name,omitempty"`
	Key         string              `json:"key,omitempty"`
	Configured  bool                `json:"configured"`
	FirstUsedAt *time.Time          `json:"first_used_at,omitempty"`
	LastUsedAt  *time.Time          `json:"last_used_at,omitempty"`
	Totals      Counters            `json:"totals"`
	Models      map[string]Counters `json:"models,omitempty"`
	Daily       []DailyUsage        `json:"daily,omitempty"`
	Claude      *KeyClaudeUsage     `json:"claude,omitempty"`
}

// DailyUsage is the usage of one key on one local calendar day.
type DailyUsage struct {
	Date string `json:"date"`
	Counters
}

// KeyClaudeUsage is the Claude subscription usage attributed to one key, in Claude
// Pro units: 1.0 is one full weekly allowance of a Claude Pro plan.
type KeyClaudeUsage struct {
	CurrentProUnits float64                    `json:"current_pro_units"`
	TotalProUnits   float64                    `json:"total_pro_units"`
	Credentials     []KeyClaudeCredentialUsage `json:"credentials"`
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

// Snapshot renders the current usage report.
func (t *Tracker) Snapshot(opts SnapshotOptions) Snapshot {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	snapshot := Snapshot{GeneratedAt: now, Since: optionalTime(t.since)}
	resolve := opts.Credential
	if resolve == nil {
		resolve = t.resolve
	}
	credentialRef := func(authID string) ClaudeCredentialRef {
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
		usage := t.keyUsageLocked(id, now, credentialRef)
		usage.Key = util.HideAPIKey(apiKey)
		usage.Configured = true
		usage.Name = keyName(opts.APIKeyNames, apiKey, id)
		snapshot.Keys = append(snapshot.Keys, usage)
	}
	others := make([]string, 0, len(t.keys))
	for id := range t.keys {
		if _, ok := seen[id]; !ok {
			others = append(others, id)
		}
	}
	sort.Strings(others)
	for _, id := range others {
		usage := t.keyUsageLocked(id, now, credentialRef)
		usage.Name = keyName(opts.APIKeyNames, "", id)
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

func (t *Tracker) keyUsageLocked(id string, now time.Time, credentialRef func(string) ClaudeCredentialRef) KeyUsage {
	usage := KeyUsage{ID: id}
	state := t.keys[id]
	if state == nil {
		return usage
	}
	usage.FirstUsedAt = optionalTime(state.FirstUsedAt)
	usage.LastUsedAt = optionalTime(state.LastUsedAt)
	usage.Totals = state.Totals
	if len(state.Models) > 0 {
		usage.Models = make(map[string]Counters, len(state.Models))
		for model, counters := range state.Models {
			usage.Models[model] = *counters
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
	if len(state.Claude) == 0 {
		return usage
	}
	claude := &KeyClaudeUsage{Credentials: make([]KeyClaudeCredentialUsage, 0, len(state.Claude))}
	authIDs := make([]string, 0, len(state.Claude))
	for authID := range state.Claude {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	for _, authID := range authIDs {
		share := state.Claude[authID]
		entry := KeyClaudeCredentialUsage{ClaudeCredentialRef: credentialRef(authID)}
		entry.TotalFraction = share.Total
		entry.TotalProUnits = share.TotalProUnits + share.TotalUnpriced*entry.PlanProUnits
		if credential := t.claude[authID]; credential.windowOpen(now) {
			current := credential.currentShare(*share, now)
			entry.CurrentFraction = current.Window
			entry.CurrentProUnits = current.WindowProUnits + current.WindowUnpriced*entry.PlanProUnits
			entry.WindowResetsAt = optionalTime(credential.ResetAt)
		}
		claude.CurrentProUnits += entry.CurrentProUnits
		claude.TotalProUnits += entry.TotalProUnits
		entry.CurrentFraction = roundFraction(entry.CurrentFraction)
		entry.CurrentProUnits = roundFraction(entry.CurrentProUnits)
		entry.TotalFraction = roundFraction(entry.TotalFraction)
		entry.TotalProUnits = roundFraction(entry.TotalProUnits)
		claude.Credentials = append(claude.Credentials, entry)
	}
	claude.CurrentProUnits = roundFraction(claude.CurrentProUnits)
	claude.TotalProUnits = roundFraction(claude.TotalProUnits)
	usage.Claude = claude
	return usage
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
