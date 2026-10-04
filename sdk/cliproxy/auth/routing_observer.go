package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// routingDecisionHistoryLimit bounds the in-memory routing decision history.
const routingDecisionHistoryLimit = 100

// Selection kinds recorded for each local credential pick.
const (
	RoutingSelectionStrategy       = "strategy"
	RoutingSelectionAffinityHit    = "affinity_hit"
	RoutingSelectionAffinityNew    = "affinity_new"
	RoutingSelectionAffinityRebind = "affinity_rebind"
	RoutingSelectionPinned         = "pinned"
	RoutingSelectionPlugin         = "plugin"
)

// Attempt kinds describe how a pick relates to earlier picks of the same request.
const (
	RoutingAttemptInitial  = "initial"
	RoutingAttemptRetry    = "retry"
	RoutingAttemptFailover = "failover"
)

// Codex upstream transports reported by executors.
const (
	RoutingTransportWebsocket = "websocket"
	RoutingTransportHTTP      = "http"
)

// RoutingDecision describes one local credential selection.
type RoutingDecision struct {
	Time              time.Time `json:"time"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model,omitempty"`
	AuthIndex         string    `json:"auth_index"`
	Selection         string    `json:"selection"`
	StrategyReason    string    `json:"strategy_reason,omitempty"`
	Candidates        *int      `json:"candidates,omitempty"`
	Attempt           int       `json:"attempt"`
	AttemptKind       string    `json:"attempt_kind"`
	PreviousAuthIndex string    `json:"previous_auth_index,omitempty"`
	Session           string    `json:"session,omitempty"`
	Transport         string    `json:"transport,omitempty"`

	seq uint64
}

// RoutingCounters aggregates local routing decisions since the process started.
type RoutingCounters struct {
	Selections         int64 `json:"selections"`
	Retries            int64 `json:"retries"`
	Failovers          int64 `json:"failovers"`
	AffinityHits       int64 `json:"affinity_hits"`
	AffinityNew        int64 `json:"affinity_new"`
	AffinityRebinds    int64 `json:"affinity_rebinds"`
	TransportWebsocket int64 `json:"transport_websocket"`
	TransportHTTP      int64 `json:"transport_http"`
}

// RoutingSessionAffinity reports the live state of the session affinity selector.
type RoutingSessionAffinity struct {
	Enabled             bool           `json:"enabled"`
	TTLSeconds          int64          `json:"ttl_seconds,omitempty"`
	Subagents           *bool          `json:"subagents,omitempty"`
	ActiveSessions      int            `json:"active_sessions"`
	SessionsByAuthIndex map[string]int `json:"sessions_by_auth_index,omitempty"`
}

// RoutingObservability is a point-in-time snapshot of local routing behavior.
type RoutingObservability struct {
	ObservedAt      time.Time              `json:"observed_at"`
	Since           time.Time              `json:"since"`
	Mode            string                 `json:"mode"`
	Strategy        string                 `json:"strategy"`
	PluginScheduler bool                   `json:"plugin_scheduler"`
	SessionAffinity RoutingSessionAffinity `json:"session_affinity"`
	Counters        RoutingCounters        `json:"counters"`
	Recent          []RoutingDecision      `json:"recent"`
}

// routingObserver records local selection decisions in a bounded ring.
// The zero value is ready to use.
type routingObserver struct {
	mu       sync.Mutex
	since    time.Time
	seq      uint64
	recent   []RoutingDecision
	next     int
	counters RoutingCounters
}

// routingRequestTrace links the picks made while serving one execution request.
type routingRequestTrace struct {
	observer   *routingObserver
	attempts   int
	lastAuthID string
	lastSeq    uint64
}

// routingPickNote collects selector annotations for a single pick.
type routingPickNote struct {
	selection      string
	strategyReason string
	candidates     int
	previousAuthID string
}

type routingRequestTraceKey struct{}

type routingPickNoteKey struct{}

// beginRequest attaches a fresh trace so picks of one execution share attempt numbering.
func (o *routingObserver) beginRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, routingRequestTraceKey{}, &routingRequestTrace{observer: o})
}

func withRoutingPickNote(ctx context.Context, note *routingPickNote) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, routingPickNoteKey{}, note)
}

func routingPickNoteFromContext(ctx context.Context) *routingPickNote {
	if ctx == nil {
		return nil
	}
	note, _ := ctx.Value(routingPickNoteKey{}).(*routingPickNote)
	return note
}

func (n *routingPickNote) setSelection(selection string) {
	if n != nil {
		n.selection = selection
	}
}

func (n *routingPickNote) setStrategyReason(reason string) {
	if n != nil {
		n.strategyReason = reason
	}
}

func (n *routingPickNote) setCandidates(count int) {
	if n != nil {
		n.candidates = count
	}
}

func (n *routingPickNote) setPreviousAuthID(authID string) {
	if n != nil {
		n.previousAuthID = authID
	}
}

// routingSessionFingerprint returns a short, non-reversible label that correlates
// decisions belonging to the same session without exposing the session identifier.
func routingSessionFingerprint(meta map[string]any) string {
	raw := strings.TrimSpace(sessionMetadataString(meta, cliproxyexecutor.CanonicalSessionIDMetadataKey))
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

func (o *routingObserver) record(ctx context.Context, note *routingPickNote, decision RoutingDecision, authID string, indexOf func(string) string) {
	if o == nil {
		return
	}
	if note != nil {
		decision.Selection = note.selection
		decision.StrategyReason = note.strategyReason
		if note.candidates >= 0 {
			candidates := note.candidates
			decision.Candidates = &candidates
		}
	}
	if decision.Selection == "" {
		decision.Selection = RoutingSelectionStrategy
	}

	var trace *routingRequestTrace
	if ctx != nil {
		trace, _ = ctx.Value(routingRequestTraceKey{}).(*routingRequestTrace)
		if trace != nil && trace.observer != o {
			trace = nil
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	decision.Attempt = 1
	decision.AttemptKind = RoutingAttemptInitial
	previousAuthID := ""
	if note != nil {
		previousAuthID = note.previousAuthID
	}
	if trace != nil {
		trace.attempts++
		decision.Attempt = trace.attempts
		if trace.attempts > 1 {
			if trace.lastAuthID == authID {
				decision.AttemptKind = RoutingAttemptRetry
			} else {
				decision.AttemptKind = RoutingAttemptFailover
				if previousAuthID == "" {
					previousAuthID = trace.lastAuthID
				}
			}
		}
	}
	if previousAuthID != "" && previousAuthID != authID && indexOf != nil {
		decision.PreviousAuthIndex = indexOf(previousAuthID)
	}

	o.seq++
	decision.seq = o.seq
	if trace != nil {
		trace.lastAuthID = authID
		trace.lastSeq = decision.seq
	}

	o.counters.Selections++
	switch decision.AttemptKind {
	case RoutingAttemptRetry:
		o.counters.Retries++
	case RoutingAttemptFailover:
		o.counters.Failovers++
	}
	switch decision.Selection {
	case RoutingSelectionAffinityHit:
		o.counters.AffinityHits++
	case RoutingSelectionAffinityNew:
		o.counters.AffinityNew++
	case RoutingSelectionAffinityRebind:
		o.counters.AffinityRebinds++
	}

	if len(o.recent) < routingDecisionHistoryLimit {
		o.recent = append(o.recent, decision)
		o.next = len(o.recent) % routingDecisionHistoryLimit
		return
	}
	o.recent[o.next] = decision
	o.next = (o.next + 1) % routingDecisionHistoryLimit
}

// NoteRoutingTransport records the upstream transport chosen for the current pick.
// Executors call it after deciding between websocket and HTTP transports.
func NoteRoutingTransport(ctx context.Context, transport string) {
	if ctx == nil {
		return
	}
	trace, _ := ctx.Value(routingRequestTraceKey{}).(*routingRequestTrace)
	if trace == nil || trace.observer == nil {
		return
	}
	o := trace.observer
	o.mu.Lock()
	defer o.mu.Unlock()
	if trace.lastSeq == 0 {
		return
	}
	for i := range o.recent {
		if o.recent[i].seq != trace.lastSeq {
			continue
		}
		if o.recent[i].Transport == transport {
			return
		}
		previous := o.recent[i].Transport
		o.recent[i].Transport = transport
		if previous == "" {
			switch transport {
			case RoutingTransportWebsocket:
				o.counters.TransportWebsocket++
			case RoutingTransportHTTP:
				o.counters.TransportHTTP++
			}
		}
		return
	}
}

// snapshot returns counters and decisions ordered newest first.
func (o *routingObserver) snapshot() (time.Time, RoutingCounters, []RoutingDecision) {
	o.mu.Lock()
	defer o.mu.Unlock()
	recent := make([]RoutingDecision, 0, len(o.recent))
	for i := 0; i < len(o.recent); i++ {
		idx := (o.next - 1 - i + len(o.recent)) % len(o.recent)
		recent = append(recent, o.recent[idx])
	}
	return o.since, o.counters, recent
}

// routingStrategyName reports the effective strategy of the running selector.
func routingStrategyName(selector Selector) string {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	switch selector.(type) {
	case *RoundRobinSelector:
		return "round-robin"
	case *WeightedRoundRobinSelector:
		return "weighted-round-robin"
	case *FillFirstSelector:
		return "fill-first"
	case *QuotaAwareSelector:
		return "quota-aware"
	case nil:
		return ""
	default:
		return "custom"
	}
}

func (s *SessionAffinitySelector) ttl() time.Duration {
	if s == nil || s.cache == nil {
		return 0
	}
	return s.cache.ttl
}

// activeBindingsByAuth merges explicit-session and prefix-matched bindings per auth ID.
func (s *SessionAffinitySelector) activeBindingsByAuth(now time.Time) map[string]int {
	if s == nil {
		return nil
	}
	out := s.cache.ActiveBindingsByAuth(now)
	for authID, count := range s.matcher.ActiveBindingsByAuth() {
		if out == nil {
			out = make(map[string]int)
		}
		out[authID] += count
	}
	return out
}

// RoutingObservability returns a snapshot of local routing state and recent decisions.
func (m *Manager) RoutingObservability() RoutingObservability {
	now := time.Now().UTC()
	if m == nil {
		return RoutingObservability{ObservedAt: now, Mode: "local", Recent: []RoutingDecision{}}
	}
	since, counters, recent := m.routingObs.snapshot()
	out := RoutingObservability{
		ObservedAt: now,
		Since:      since.UTC(),
		Mode:       "local",
		Counters:   counters,
		Recent:     recent,
	}
	if m.HomeEnabled() {
		out.Mode = "home"
	}

	out.PluginScheduler = m.hasPluginScheduler()
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	out.Strategy = routingStrategyName(selector)

	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		subagents := affinity.subagentAffinity
		out.SessionAffinity.Enabled = true
		out.SessionAffinity.Subagents = &subagents
		bindings := affinity.activeBindingsByAuth(now)
		if ttl := affinity.ttl(); ttl > 0 {
			out.SessionAffinity.TTLSeconds = int64(ttl / time.Second)
		}
		if len(bindings) > 0 {
			byIndex := make(map[string]int, len(bindings))
			total := 0
			m.mu.RLock()
			for authID, count := range bindings {
				total += count
				index := authID
				if auth := m.auths[authID]; auth != nil && strings.TrimSpace(auth.Index) != "" {
					index = auth.Index
				}
				byIndex[index] += count
			}
			m.mu.RUnlock()
			out.SessionAffinity.ActiveSessions = total
			out.SessionAffinity.SessionsByAuthIndex = byIndex
		}
	}
	return out
}

// authIndexForRouting resolves an auth ID to its stable index for routing records.
func (m *Manager) authIndexForRouting(authID string) string {
	if m == nil || authID == "" {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth := m.auths[authID]; auth != nil {
		if index := strings.TrimSpace(auth.Index); index != "" {
			return index
		}
	}
	return ""
}

// recordRoutingPick stores a successful local pick.
func (m *Manager) recordRoutingPick(ctx context.Context, note *routingPickNote, auth *Auth, provider, model string, opts cliproxyexecutor.Options) {
	if m == nil || auth == nil {
		return
	}
	if note != nil && note.selection == "" && pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		note.selection = RoutingSelectionPinned
	}
	decision := RoutingDecision{
		Time:      time.Now().UTC(),
		Provider:  provider,
		Model:     model,
		AuthIndex: auth.Index,
		Session:   routingSessionFingerprint(opts.Metadata),
	}
	m.routingObs.record(ctx, note, decision, auth.ID, m.authIndexForRouting)
}
