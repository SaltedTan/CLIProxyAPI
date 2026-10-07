package auth

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// ErrorCodeClientKeyLimitReached identifies a request refused because the client
// API key spent its configured Claude allowance.
const ErrorCodeClientKeyLimitReached = "client_key_limit_reached"

// clientQuotaDefaultRetryAfter is the Retry-After hint when no reset time is known.
const clientQuotaDefaultRetryAfter = time.Minute

// AdmissionPolicy decides, after a credential was picked and before any upstream
// call, whether the current request may use it. A non-nil error refuses the
// credential; the conductor then tries the next candidate and returns the refusal
// only when no upstream attempt was made. Implementations must be safe for
// concurrent use and must not block on network I/O.
type AdmissionPolicy interface {
	Admit(ctx context.Context, auth *Auth) error
}

type admissionPolicyHolder struct {
	policy AdmissionPolicy
}

// SetAdmissionPolicy installs the policy consulted in the credential pick loop of
// Execute and ExecuteStream. ExecuteCount and Home mode never consult it. A nil
// policy disables admission checks.
func (m *Manager) SetAdmissionPolicy(policy AdmissionPolicy) {
	if m == nil {
		return
	}
	if policy == nil {
		m.admissionPolicy.Store(nil)
		return
	}
	m.admissionPolicy.Store(&admissionPolicyHolder{policy: policy})
}

// AdmissionPolicy returns the installed admission policy, or nil when none is set.
func (m *Manager) AdmissionPolicy() AdmissionPolicy {
	if m == nil {
		return nil
	}
	holder := m.admissionPolicy.Load()
	if holder == nil {
		return nil
	}
	return holder.policy
}

// ClientQuotaError refuses a request because the client API key spent its
// configured allowance. It is a request-stop error: the conductor runs no retry
// rounds and records no credential cooldown for it.
type ClientQuotaError struct {
	// Code is a short machine readable identifier, normally ErrorCodeClientKeyLimitReached.
	Code string
	// Message is the client-facing description. It never contains the raw key.
	Message string
	// ResetIn is how long until the allowance is expected to recover.
	ResetIn time.Duration
}

// Error returns the client-facing message so the standard error envelope carries it.
func (e *ClientQuotaError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return "client API key allowance reached"
	}
	return e.Message
}

// StatusCode implements the status accessor used by the handlers and the conductor.
func (e *ClientQuotaError) StatusCode() int {
	return http.StatusTooManyRequests
}

// RetryAfter returns the recovery hint, defaulting to one minute when unknown.
func (e *ClientQuotaError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	value := e.retryAfter()
	return &value
}

func (e *ClientQuotaError) retryAfter() time.Duration {
	if e == nil || e.ResetIn <= 0 {
		return clientQuotaDefaultRetryAfter
	}
	if e.ResetIn < time.Second {
		return time.Second
	}
	return e.ResetIn
}

// Headers returns the locally computed Retry-After hint and the JSON content type.
func (e *ClientQuotaError) Headers() http.Header {
	if e == nil {
		return nil
	}
	headers := make(http.Header, 2)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", strconv.FormatInt(retryAfterSeconds(e.retryAfter()), 10))
	return headers
}

// IsRequestStop marks the refusal as final for the current request.
func (e *ClientQuotaError) IsRequestStop() bool {
	return true
}

// retryAfterSeconds rounds a duration up to whole seconds, at least one.
func retryAfterSeconds(retryAfter time.Duration) int64 {
	seconds := int64(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// admissionCache evaluates the admission policy once per request and provider.
// The decision depends only on the client key and the credential's provider, so the
// result is reused for every other candidate of that provider, including across
// retry rounds, and a refusal is counted once per request.
type admissionCache struct {
	policy    AdmissionPolicy
	decisions map[string]error
	refusal   error
}

// newAdmissionCache returns nil when no policy is installed or Home mode is on,
// in which case the pick loops never consult admission.
func (m *Manager) newAdmissionCache() *admissionCache {
	if m == nil || m.HomeEnabled() {
		return nil
	}
	policy := m.AdmissionPolicy()
	if policy == nil {
		return nil
	}
	return &admissionCache{policy: policy, decisions: make(map[string]error)}
}

// admit returns the refusal for auth, evaluating the policy on first sight of its provider.
func (c *admissionCache) admit(ctx context.Context, auth *Auth) error {
	if c == nil || auth == nil {
		return nil
	}
	provider := canonicalSchedulingProvider(auth.Provider)
	if decision, ok := c.decisions[provider]; ok {
		return decision
	}
	decision := c.policy.Admit(ctx, auth)
	c.decisions[provider] = decision
	if decision != nil && c.refusal == nil {
		c.refusal = decision
	}
	return decision
}

// refused returns the first refusal recorded for this request, if any.
func (c *admissionCache) refused() error {
	if c == nil {
		return nil
	}
	return c.refusal
}
