package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync"
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

// RefusalRecorder is implemented by admission policies that account for a refusal
// only once the conductor returns it to the client. A request refused by one
// provider and served by another is not recorded.
type RefusalRecorder interface {
	RecordRefusal(ctx context.Context, refusal error)
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
// retry rounds. It also remembers whether the request reached an upstream, so a
// refusal is returned, and recorded with the policy, only for requests that never did.
type admissionCache struct {
	holder *admissionPolicyHolder
	policy AdmissionPolicy
	// mu guards the fields below: calls sharing a request scope are normally
	// sequential, but the cache must stay consistent if a caller overlaps them.
	mu        sync.Mutex
	decisions map[string]error
	refusal   error
	attempted bool
	recorded  bool
}

// admissionScope carries one client request's admission across the conductor
// calls a handler makes for it, such as a stream bootstrap retry. The request is
// admitted once, so a later call can neither consult the policy again nor turn an
// outcome the request already had upstream into a refusal.
type admissionScope struct {
	mu    sync.Mutex
	cache *admissionCache
}

type admissionScopeKey struct{}

// WithRequestAdmission returns a context whose conductor calls share one
// admission decision. Handlers call it once per request they execute. It always
// starts a new scope: a nested execution derives its context from the request
// that triggered it, yet is a request of its own and must not inherit that
// request's decision or its upstream attempt.
func WithRequestAdmission(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, admissionScopeKey{}, &admissionScope{})
}

// newAdmissionCache returns nil when no policy is installed or Home mode is on,
// in which case the pick loops never consult admission. With a request scope on
// ctx the cache is shared by every call made for that request, as long as the
// installed policy has not changed in between.
func (m *Manager) newAdmissionCache(ctx context.Context) *admissionCache {
	if m == nil || m.HomeEnabled() {
		return nil
	}
	holder := m.admissionPolicy.Load()
	if holder == nil || holder.policy == nil {
		return nil
	}
	fresh := func() *admissionCache {
		return &admissionCache{holder: holder, policy: holder.policy, decisions: make(map[string]error)}
	}
	scope, _ := ctx.Value(admissionScopeKey{}).(*admissionScope)
	if scope == nil {
		return fresh()
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.cache == nil || scope.cache.holder != holder {
		scope.cache = fresh()
	}
	return scope.cache
}

// admit returns the refusal for auth, evaluating the policy on first sight of its provider.
func (c *admissionCache) admit(ctx context.Context, auth *Auth) error {
	if c == nil || auth == nil {
		return nil
	}
	provider := canonicalSchedulingProvider(auth.Provider)
	c.mu.Lock()
	decision, ok := c.decisions[provider]
	c.mu.Unlock()
	if ok {
		return decision
	}
	decision = c.policy.Admit(ctx, auth)
	c.mu.Lock()
	defer c.mu.Unlock()
	if earlier, raced := c.decisions[provider]; raced {
		return earlier
	}
	c.decisions[provider] = decision
	if decision != nil && c.refusal == nil {
		c.refusal = decision
	}
	return decision
}

// markAttempted notes that the request went on to an upstream attempt. From then on
// the outcome of that attempt, not a refusal, is the request's result.
func (c *admissionCache) markAttempted() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.attempted = true
	c.mu.Unlock()
}

// finalRefusal returns the request's refusal when every candidate was refused and no
// upstream attempt was made in any round, recording it with the policy exactly once.
// It returns nil otherwise, so an earlier upstream outcome is reported instead.
func (c *admissionCache) finalRefusal(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.refusal == nil || c.attempted {
		c.mu.Unlock()
		return nil
	}
	refusal := c.refusal
	record := !c.recorded
	c.recorded = true
	c.mu.Unlock()
	if record {
		if recorder, ok := c.policy.(RefusalRecorder); ok {
			recorder.RecordRefusal(ctx, refusal)
		}
	}
	return refusal
}
