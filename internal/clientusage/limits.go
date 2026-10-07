package clientusage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Per client key Claude allowance. Operators cap each key's Claude usage in Pro units
// per 7-day window (access.api-key-limits). The window is the key's own (see
// keyWindow): it opens at the key's first Claude request and ends 7 days later. The
// tracker compares the Pro units attributed to the key inside that window, from the
// weekly utilization headers, with the cap before a Claude credential is used, and
// refuses the request with a 429 once the cap is reached. The comparison is the
// report's (limitReached), so what the dashboard shows is what is enforced. The attribution is an estimate, requests in flight when the cap is
// reached still complete, and nothing is enforced in Home mode.

// clientQuotaUnknownReset is the recovery hint when no open window is known.
const clientQuotaUnknownReset = time.Minute

// SetLimits replaces the Claude allowance per client key, in Pro units per 7-day
// window, keyed by full API key or key ID. Keys are trimmed; negative and non-finite
// entries are dropped. A 0 means no limit and is kept, because a full-key 0 takes
// precedence over an ID entry for the same key. Limits take effect on the next
// admission check, so a raised or removed limit re-admits a key immediately.
func (t *Tracker) SetLimits(limits map[string]float64) {
	if t == nil {
		return
	}
	cleaned := make(map[string]float64, len(limits))
	for key, limit := range limits {
		key = strings.TrimSpace(key)
		if key == "" || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
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
// full key wins over one for its ID, including a full-key 0 that lifts the limit;
// requests without a key use the anonymous ID. t.mu must be held.
func (t *Tracker) limitForLocked(apiKey string) (float64, bool) {
	if len(t.limits) == 0 {
		return 0, false
	}
	apiKey = strings.TrimSpace(apiKey)
	limit, ok := 0.0, false
	if apiKey != "" {
		limit, ok = t.limits[apiKey]
	}
	if !ok {
		limit, ok = t.limits[KeyID(apiKey)]
	}
	if !ok || limit <= 0 {
		return 0, false
	}
	return limit, true
}

// limitedLocked reports whether the client key has a Claude allowance. t.mu must be held.
func (t *Tracker) limitedLocked(apiKey string) bool {
	_, ok := t.limitForLocked(apiKey)
	return ok
}

// keyLimit is a limit indexed by key ID for reports; APIKey is set when the limit
// was configured for the full key.
type keyLimit struct {
	Limit  float64
	APIKey string
}

// limitsByIDLocked indexes the configured limits by key ID for the report, applying
// each entry to every row admission would apply it to: as an ID to the row with that
// ID, and as a full key to the row of the key's ID. Full-key readings win, a full-key
// 0 lifting the limit. An entry that is neither is not listed (see appliesAsIDLocked).
// configured is the set of keys in access.api-keys. t.mu must be held.
func (t *Tracker) limitsByIDLocked(configured map[string]struct{}) map[string]keyLimit {
	configuredIDs := make(map[string]struct{}, len(configured))
	for apiKey := range configured {
		configuredIDs[KeyID(apiKey)] = struct{}{}
	}
	index := make(map[string]keyLimit, len(t.limits))
	for entry, limit := range t.limits {
		if limit > 0 && t.appliesAsIDLocked(entry, configuredIDs) {
			index[entry] = keyLimit{Limit: limit}
		}
	}
	for entry, limit := range t.limits {
		if !t.appliesAsFullKeyLocked(entry, configured) {
			continue
		}
		id := KeyID(entry)
		if limit > 0 {
			index[id] = keyLimit{Limit: limit, APIKey: entry}
		} else {
			delete(index, id)
		}
	}
	return index
}

// appliesAsIDLocked reports whether the report reads a limits entry as a key ID: it
// is the anonymous ID, or it has the shape of an ID and names a known one (a tracked
// key or a configured key's ID). An unknown entry of that shape is not listed until
// a key with that ID has usage: it may be a real key that happens to look like an
// ID, which its row would show unmasked. Admission applies it either way. t.mu must
// be held.
func (t *Tracker) appliesAsIDLocked(entry string, configuredIDs map[string]struct{}) bool {
	if entry == AnonymousKeyID {
		return true
	}
	if !isKeyID(entry) {
		return false
	}
	if _, ok := t.keys[entry]; ok {
		return true
	}
	_, ok := configuredIDs[entry]
	return ok
}

// appliesAsFullKeyLocked reports whether a limits entry is read as a full key: it is
// a known key (configured or with usage) or does not look like an ID. A real key that
// happens to look like an ID is therefore never reported as one. t.mu must be held.
func (t *Tracker) appliesAsFullKeyLocked(entry string, configured map[string]struct{}) bool {
	return t.knownFullKeyLocked(entry, configured) || !isKeyID(entry)
}

// knownFullKeyLocked reports whether entry is a client key the tracker knows: it is
// configured or its ID has usage. t.mu must be held.
func (t *Tracker) knownFullKeyLocked(entry string, configured map[string]struct{}) bool {
	if _, ok := configured[entry]; ok {
		return true
	}
	_, ok := t.keys[KeyID(entry)]
	return ok
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
// alike) are refused for a client key whose Pro units in its open allowance window
// reached its limit; other providers and keys without a limit are always admitted,
// as is a key with no open window. The refusal is a *coreauth.ClientQuotaError whose
// ResetIn is the time to the end of the key's window (one minute when unknown).
// Deciding does not count: the conductor may still serve the request through another
// provider and calls RecordRefusal only when it returns the refusal to the client.
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
	if state != nil && state.Window.open(now) {
		_, current, resetsAt = t.claudeUsageLocked(state, now, t.credentialRefLocked(t.resolve))
	}
	t.mu.Unlock()
	if !limitReached(current, limit) {
		if log.IsLevelEnabled(log.DebugLevel) {
			log.Debugf("client usage: key %s admitted to Claude with %s of %s Pro units used", keyID, formatProUnits(current), formatProUnits(limit))
		}
		return nil
	}

	resetIn := clientQuotaUnknownReset
	if !resetsAt.IsZero() {
		resetIn = resetsAt.Sub(now)
		if resetIn < time.Second {
			resetIn = time.Second
		}
	}
	if log.IsLevelEnabled(log.DebugLevel) {
		log.Debugf("client usage: key %s is over its Claude allowance (%s of %s Pro units), credential refused", keyID, formatProUnits(current), formatProUnits(limit))
	}
	return &coreauth.ClientQuotaError{
		Code:    coreauth.ErrorCodeClientKeyLimitReached,
		Message: fmt.Sprintf("client API key Claude allowance reached: %s of %s Pro units used in the current 7-day window; resets in %s", formatProUnits(current), formatProUnits(limit), formatResetDuration(resetIn)),
		ResetIn: resetIn,
	}
}

// MayRefuse implements coreauth.RefusalPredictor: only a client key with a Claude
// allowance can be refused.
func (t *Tracker) MayRefuse(ctx context.Context) bool {
	if t == nil {
		return false
	}
	apiKey := apiKeyFromContext(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limitedLocked(apiKey)
}

// RecordRefusal implements coreauth.RefusalRecorder. The conductor calls it once when
// a refusal from Admit becomes the request's result, so a request served by another
// provider is never counted. The refusal counts as blocked on the key's totals and on
// today's daily bucket, and is logged by key id.
func (t *Tracker) RecordRefusal(ctx context.Context, refusal error) {
	var quota *coreauth.ClientQuotaError
	if t == nil || !errors.As(refusal, &quota) || quota == nil {
		return
	}
	keyID := KeyID(apiKeyFromContext(ctx))
	now := t.now()
	t.mu.Lock()
	// The key was refused because it has attributed usage; it has no state only when
	// it was reset in the meantime, and then there is nothing to count against.
	if state := t.keys[keyID]; state != nil {
		state.Totals.Blocked++
		state.dayCounters(now.In(t.location)).Blocked++
		t.dirty = true
	}
	t.mu.Unlock()
	// The key id is part of the message: the server's log format prints no fields.
	log.WithFields(log.Fields{
		"key_id":      keyID,
		"retry_after": quota.RetryAfter().String(),
	}).Infof("client usage: key %s refused, Claude allowance reached: %s", keyID, quota.Error())
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

// maskKey masks a client API key for the report: four characters at each end of a
// long key, two of a medium one, and nothing of a short one.
func maskKey(apiKey string) string {
	switch {
	case len(apiKey) > 8:
		return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
	case len(apiKey) > 4:
		return apiKey[:2] + "..." + apiKey[len(apiKey)-2:]
	default:
		return "***"
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
