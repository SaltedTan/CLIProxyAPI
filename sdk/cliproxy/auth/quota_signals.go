package auth

import (
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxQuotaSignalHeaders = 64
	maxQuotaSignalValue   = 512
)

// ProviderSupportsQuotaObservation reports whether the named provider emits a
// passive credential-level quota snapshot understood by collectQuotaSignals.
func ProviderSupportsQuotaObservation(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "devin":
		return true
	default:
		return false
	}
}

// ObserveResponseHeadersForProvider replaces the passive quota snapshot with the
// signals carried by the current upstream response.
//
// The snapshot is replaced rather than merged: a watermark such as Retry-After
// or a "limit reached" flag only appears on the response that produced it, so
// accumulating signals across responses would leave an expired value visible
// indefinitely. Responses that carry no quota signal at all (transport
// failures, 5xx, unrelated endpoints) leave the previous snapshot untouched.
//
// This function only ever touches ObservedAt and Signals. Cooldown and
// scheduling fields are never read or written here.
func (q *QuotaState) ObserveResponseHeadersForProvider(provider string, headers http.Header, observedAt time.Time) bool {
	if q == nil {
		return false
	}
	if !ProviderSupportsQuotaObservation(provider) {
		return q.ClearObservationSignals()
	}
	next := collectQuotaSignals(provider, headers)
	if len(next) == 0 {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	q.Signals = next
	q.ObservedAt = observedAt
	return true
}

// ClearObservationSignals removes only passive observation data. It leaves
// cooldown and scheduler state untouched.
func (q *QuotaState) ClearObservationSignals() bool {
	if q == nil || (len(q.Signals) == 0 && q.ObservedAt.IsZero()) {
		return false
	}
	q.Signals = nil
	q.ObservedAt = time.Time{}
	return true
}

// cooldownFieldsOf copies only scheduler cooldown fields. Observation data is
// omitted so cooldown persistence and restore cannot replace a live snapshot.
func cooldownFieldsOf(q QuotaState) QuotaState {
	return QuotaState{
		Exceeded:      q.Exceeded,
		Reason:        q.Reason,
		NextRecoverAt: q.NextRecoverAt,
		BackoffLevel:  q.BackoffLevel,
	}
}

// applyCooldownFields writes only scheduler cooldown fields. ObservedAt and
// Signals are left untouched so a cooldown transition cannot erase the last
// upstream watermark.
func applyCooldownFields(dst *QuotaState, cooldown QuotaState) {
	if dst == nil {
		return
	}
	dst.Exceeded = cooldown.Exceeded
	dst.Reason = cooldown.Reason
	dst.NextRecoverAt = cooldown.NextRecoverAt
	dst.BackoffLevel = cooldown.BackoffLevel
}

// collectQuotaSignals builds the bounded snapshot for a single response.
func collectQuotaSignals(provider string, headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	values := make(map[string]string, len(headers))
	for key, headerValues := range headers {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if !isQuotaSignalHeaderForProvider(provider, canonicalKey) || len(headerValues) == 0 {
			continue
		}
		value := strings.TrimSpace(headerValues[len(headerValues)-1])
		if !validQuotaSignalValue(value) {
			continue
		}
		if _, exists := values[canonicalKey]; !exists {
			names = append(names, canonicalKey)
		}
		values[canonicalKey] = value
	}
	if len(names) == 0 {
		return nil
	}
	// Rank then sort so truncation is deterministic and keeps credential-level
	// watermarks (plan, credits, primary) ahead of additional-limit namespaces.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := quotaSignalRetentionRank(names[i]), quotaSignalRetentionRank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	if len(names) > maxQuotaSignalHeaders {
		names = names[:maxQuotaSignalHeaders]
	}
	signals := make(map[string]string, len(names))
	for _, name := range names {
		signals[name] = values[name]
	}
	return signals
}

// validQuotaSignalValue rejects empty, oversized, and control-character values.
// Observed values reach the plain-text upstream request log, so a value
// containing CR/LF could otherwise forge a header line there.
func validQuotaSignalValue(value string) bool {
	if value == "" || len(value) > maxQuotaSignalValue {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func quotaSignalRetentionRank(name string) int {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == "retry-after", strings.HasPrefix(lower, "anthropic-ratelimit-unified-"):
		return 0
	case lower == "x-codex-plan-type", lower == "x-codex-active-limit", strings.HasPrefix(lower, "x-codex-credits-"):
		return 1
	case lower == "x-codex-allowed", lower == "x-codex-limit-reached",
		strings.HasPrefix(lower, "x-codex-primary-"), strings.HasPrefix(lower, "x-codex-secondary-"):
		return 2
	case strings.HasPrefix(lower, "x-codex-code-review-"):
		return 3
	case strings.HasPrefix(lower, "x-codex-additional-"):
		return 5
	case strings.HasPrefix(lower, "x-codex-"):
		return 4
	default:
		return 6
	}
}

func isQuotaSignalHeaderForProvider(provider, name string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "retry-after" {
		return provider == "claude" || provider == "codex"
	}
	if strings.HasPrefix(name, "anthropic-ratelimit-unified-") {
		return provider == "claude"
	}
	if strings.HasPrefix(name, "x-ratelimit-") {
		// Observed Codex responses do not carry x-ratelimit-* headers; the only
		// upstream seen emitting them is Grok, which is excluded from quota
		// observation. The rule is kept so a future Codex rollout is captured
		// without another change, but it is expected to be inert today.
		return provider == "codex"
	}
	if !strings.HasPrefix(name, "x-codex-") {
		return false
	}
	if provider != "codex" {
		return false
	}
	if name == "x-codex-active-limit" || name == "x-codex-plan-type" ||
		strings.HasPrefix(name, "x-codex-credits-") {
		return true
	}
	// Codex namespaces each additional limit by a short name on the HTTP path
	// (x-codex-bengalfox-primary-used-percent) and by limit name on the
	// websocket path (x-codex-additional-<limit>-primary-used-percent), so the
	// suffix is matched instead of an exhaustive header list.
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// mergeQuotaObservation keeps the newest observation snapshot instead of
// unioning signals captured at different times, so merging an older snapshot
// can never resurrect a stale watermark.
func mergeQuotaObservation(target, source QuotaState) QuotaState {
	if source.ObservedAt.IsZero() || source.ObservedAt.Before(target.ObservedAt) {
		return target
	}
	target.ObservedAt = source.ObservedAt
	target.Signals = source.Clone().Signals
	return target
}

// withoutQuotaObservation returns quota without its observation snapshot. The cooldown
// fields are kept.
func withoutQuotaObservation(quota QuotaState) QuotaState {
	quota.ObservedAt = time.Time{}
	quota.Signals = nil
	return quota
}

// quotaAccountMetadataKeys identify the upstream account a credential draws quota from.
// Claude quota is per organization, so the organization is compared as well as the account.
var quotaAccountMetadataKeys = []string{"account_uuid", "organization_uuid", "account_id", "org_id", "user_id", "email"}

// sameQuotaAccount reports whether incoming draws quota from the same upstream account as
// existing, so quota data taken for existing (its passive quota snapshot, or a usage
// endpoint reading) still describes incoming. Providers must match, and identity metadata
// on both sides that disagrees always means another account (see quotaAccountMetadata).
// Otherwise either proves the same account:
//   - sameQuotaAccountByIdentity, which decides from the auths' contents;
//   - the same quota lineage: since existing was cloned, the Manager changed the auth only
//     by its own token refreshes and request preparations, and by replaces that
//     sameQuotaAccountByIdentity proved (see Auth.quotaLineage). Only a conflict disproves
//     it: a replace that dropped an identity key kept the lineage only when that rule
//     proved the account. Different lineages prove nothing either way.
func sameQuotaAccount(existing, incoming *Auth) bool {
	if sameQuotaAccountByIdentity(existing, incoming) {
		return true
	}
	if existing == nil || incoming == nil || existing.quotaLineage == 0 || existing.quotaLineage != incoming.quotaLineage {
		return false
	}
	if !sameQuotaProvider(existing, incoming) {
		return false
	}
	_, conflict := quotaAccountMetadata(existing, incoming)
	return !conflict
}

// sameQuotaAccountByIdentity is sameQuotaAccount without the quota lineage: it decides from
// the auths' contents alone. A token refresh changes the credentials but not the account,
// so identity metadata decides first (see quotaAccountMetadata). Without proof, including
// when incoming lacks an identity key that existing has, the credentials must be present
// and unchanged, including a Devin session_token, which CredentialsChanged does not compare.
func sameQuotaAccountByIdentity(existing, incoming *Auth) bool {
	if !sameQuotaProvider(existing, incoming) {
		return false
	}
	proven, conflict := quotaAccountMetadata(existing, incoming)
	if conflict {
		return false
	}
	if proven {
		return true
	}
	return quotaCredentialPresent(existing) && !CredentialsChanged(existing, incoming) &&
		authMetadataString(existing, "session_token") == authMetadataString(incoming, "session_token")
}

// sameQuotaProvider reports whether both auths exist and have the same provider.
func sameQuotaProvider(existing, incoming *Auth) bool {
	if existing == nil || incoming == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(existing.Provider), strings.TrimSpace(incoming.Provider))
}

// quotaAccountMetadata compares the identity metadata of existing with that of incoming,
// which follows it:
//   - a key present on both sides with different values is a conflict: another account;
//   - a matching key other than account_uuid proves the same account. account_uuid is no
//     proof, because it is synthesized from the auth ID when no profile is available;
//   - a proof key (any key but account_uuid) that existing has and incoming lacks voids the
//     proof, without a conflict: a file that dropped its organization_uuid may now hold
//     another organization of the same user, whose quota differs. A config credential
//     loses the identity it gained at runtime on a config reload, but keeps its
//     credentials, so the unchanged-credentials rule of sameQuotaAccountByIdentity still
//     proves it;
//   - a key only incoming has is ignored: an older file gains identity when the proxy fills
//     it from the profile.
func quotaAccountMetadata(existing, incoming *Auth) (proven, conflict bool) {
	dropped := false
	for _, key := range quotaAccountMetadataKeys {
		existingValue := authMetadataString(existing, key)
		incomingValue := authMetadataString(incoming, key)
		if existingValue == "" || incomingValue == "" {
			if existingValue != "" && key != "account_uuid" {
				dropped = true
			}
			continue
		}
		if !strings.EqualFold(existingValue, incomingValue) {
			return false, true
		}
		if key != "account_uuid" {
			proven = true
		}
	}
	return proven && !dropped, false
}

// quotaLineageCounter issues quota lineages (see Auth.quotaLineage).
var quotaLineageCounter atomic.Uint64

// nextQuotaLineage returns a new quota lineage, never zero.
func nextQuotaLineage() uint64 {
	return quotaLineageCounter.Add(1)
}

// keptQuotaLineage returns the quota lineage of existing for an auth that follows it, or a
// new one when existing has none.
func keptQuotaLineage(existing *Auth) uint64 {
	if existing != nil && existing.quotaLineage != 0 {
		return existing.quotaLineage
	}
	return nextQuotaLineage()
}

// replacedQuotaLineage returns the quota lineage of incoming, which replaces existing (nil
// for a new auth ID): existing's when sameQuotaAccountByIdentity proves the same account,
// else a new one. incoming's own lineage is ignored: it may be a clone of existing taken
// before an edit that changed the account.
func replacedQuotaLineage(existing, incoming *Auth) uint64 {
	if existing != nil && sameQuotaAccountByIdentity(existing, incoming) {
		return keptQuotaLineage(existing)
	}
	return nextQuotaLineage()
}

// quotaCredentialPresent reports whether auth carries a credential that
// sameQuotaAccountByIdentity compares, so an unchanged result means the same credential
// rather than none at all.
func quotaCredentialPresent(auth *Auth) bool {
	return authAccessToken(auth) != "" || authRefreshToken(auth) != "" ||
		authMetadataString(auth, "id_token") != "" || authMetadataString(auth, "idToken") != "" ||
		authAttribute(auth, AttributeAPIKey) != "" || authMetadataString(auth, "api_key") != "" ||
		authMetadataString(auth, "session_token") != ""
}
