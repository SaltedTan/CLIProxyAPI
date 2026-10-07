package clientusage

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Per client key Claude allowance. Operators cap each key's Claude usage in Pro units
// per weekly window (access.api-key-limits). The tracker compares the key's current
// Pro units, attributed from the weekly utilization headers, with that cap before a
// Claude credential is used, and refuses the request with a 429 once the cap is
// reached. The comparison uses the same arithmetic as the usage report so what the
// dashboard shows is what is enforced. The attribution is an estimate, requests in
// flight when the cap is reached still complete, and nothing is enforced in Home mode.

// clientQuotaUnknownReset is the recovery hint when no open window is known.
const clientQuotaUnknownReset = time.Minute

// SetLimits replaces the Claude allowance per client key, in Pro units per weekly
// window, keyed by full API key or key ID. Keys are trimmed; entries that are not
// positive finite numbers mean no limit and are dropped. Limits take effect on the
// next admission check, so a raised or removed limit re-admits a key immediately.
func (t *Tracker) SetLimits(limits map[string]float64) {
	if t == nil {
		return
	}
	cleaned := make(map[string]float64, len(limits))
	for key, limit := range limits {
		key = strings.TrimSpace(key)
		if key == "" || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
			continue
		}
		cleaned[key] = limit
	}
	t.mu.Lock()
	t.limits = cleaned
	t.mu.Unlock()
	log.Debugf("client usage: %d client key Claude limits configured", len(cleaned))
}

// limitForLocked resolves the Claude allowance of a client key: an entry for the
// full key wins over one for its ID; requests without a key use the anonymous ID.
// t.mu must be held.
func (t *Tracker) limitForLocked(apiKey string) (float64, bool) {
	if len(t.limits) == 0 {
		return 0, false
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		if limit, ok := t.limits[apiKey]; ok {
			return limit, true
		}
	}
	limit, ok := t.limits[KeyID(apiKey)]
	return limit, ok
}

// keyLimit is a limit indexed by key ID for reports; APIKey is set when the limit
// was configured for the full key.
type keyLimit struct {
	Limit  float64
	APIKey string
}

// limitsByIDLocked indexes the configured limits by key ID. An entry that looks
// like a key ID is reported under that ID; any other entry is a full key and is
// reported under its ID, taking precedence over an ID entry for the same key.
// t.mu must be held.
func (t *Tracker) limitsByIDLocked() map[string]keyLimit {
	index := make(map[string]keyLimit, len(t.limits))
	for entry, limit := range t.limits {
		if isKeyID(entry) {
			if existing, ok := index[entry]; ok && existing.APIKey != "" {
				continue
			}
			index[entry] = keyLimit{Limit: limit}
			continue
		}
		index[KeyID(entry)] = keyLimit{Limit: limit, APIKey: entry}
	}
	return index
}

// isKeyID reports whether value has the shape of a key ID (16 lowercase hex digits)
// or is the anonymous ID.
func isKeyID(value string) bool {
	if value == AnonymousKeyID {
		return true
	}
	if len(value) != 16 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Admit implements coreauth.AdmissionPolicy. Claude credentials (OAuth and API key
// alike) are refused for a client key whose current Pro units reached its limit;
// other providers and keys without a limit are always admitted. A refusal counts as
// blocked on the key's totals and on today's daily bucket, and returns a
// *coreauth.ClientQuotaError whose ResetIn is the time to the earliest open-window
// reset among the credentials the key used (one minute when unknown).
func (t *Tracker) Admit(ctx context.Context, auth *coreauth.Auth) error {
	if t == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return nil
	}
	// Read the request context before locking; it is not needed under the lock.
	apiKey := apiKeyFromContext(ctx)
	keyID := KeyID(apiKey)
	now := t.now()

	t.mu.Lock()
	limit, ok := t.limitForLocked(apiKey)
	if !ok {
		t.mu.Unlock()
		return nil
	}
	var current float64
	var resetsAt time.Time
	state := t.keys[keyID]
	if state != nil && len(state.Claude) > 0 {
		_, current, resetsAt = t.claudeUsageLocked(state, now, t.credentialRefLocked(t.resolve))
	}
	if current < limit {
		t.mu.Unlock()
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("client usage: key %s admitted to Claude with %s of %s Pro units used", keyID, formatProUnits(current), formatProUnits(limit))
		}
		return nil
	}
	// current >= limit > 0 implies the key has attributed usage, so state is set.
	if state != nil {
		state.Totals.Blocked++
		state.dayCounters(now.In(t.location)).Blocked++
		t.dirty = true
	}
	t.mu.Unlock()

	resetIn := clientQuotaUnknownReset
	if !resetsAt.IsZero() {
		resetIn = resetsAt.Sub(now)
		if resetIn < time.Second {
			resetIn = time.Second
		}
	}
	fields := log.Fields{"key_id": keyID, "used_pro_units": formatProUnits(current), "limit_pro_units": formatProUnits(limit)}
	if !resetsAt.IsZero() {
		fields["resets_at"] = resetsAt.UTC().Format(time.RFC3339)
	}
	log.WithFields(fields).Info("client usage: Claude allowance reached, request refused")
	return &coreauth.ClientQuotaError{
		Code:    coreauth.ErrorCodeClientKeyLimitReached,
		Message: fmt.Sprintf("client API key Claude allowance reached: %s of %s Pro units used this week; resets in %s", formatProUnits(current), formatProUnits(limit), formatResetDuration(resetIn)),
		ResetIn: resetIn,
	}
}

// apiKeyFromContext returns the client API key of the request, read from the gin
// context the same way usage records get it, or "" when there is none.
func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return ""
	}
	raw, ok := ginCtx.Get("userApiKey")
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return fmt.Sprint(value)
	}
}

func formatProUnits(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// formatResetDuration renders a duration compactly, like "2d3h", "1h5m" or "47m".
func formatResetDuration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	days := int64(d / (24 * time.Hour))
	hours := int64(d % (24 * time.Hour) / time.Hour)
	minutes := int64(d % time.Hour / time.Minute)
	seconds := int64(d % time.Minute / time.Second)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
