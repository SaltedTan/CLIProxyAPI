package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type routingObserverTestExecutor struct {
	provider  string
	failFirst bool
	transport string
	mu        sync.Mutex
	calls     []string
}

func (e *routingObserverTestExecutor) Identifier() string { return e.provider }

func (e *routingObserverTestExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	first := len(e.calls) == 1
	e.mu.Unlock()
	if e.transport != "" {
		NoteRoutingTransport(ctx, e.transport)
	}
	if e.failFirst && first {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "unavailable"}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *routingObserverTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *routingObserverTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *routingObserverTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *routingObserverTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func registerRoutingObserverAuths(t *testing.T, manager *Manager, provider, model string, ids ...string) map[string]string {
	t.Helper()
	indexes := make(map[string]string, len(ids))
	reg := registry.GetGlobalRegistry()
	for _, id := range ids {
		registered, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
			ID:       id,
			Provider: provider,
			Status:   StatusActive,
			Metadata: map[string]any{"disable_cooling": true},
		})
		if errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		indexes[id] = registered.EnsureIndex()
	}
	return indexes
}

func TestRoutingObservabilityRecordsFailoverWithCandidates(t *testing.T) {
	provider := "routing-observer-failover"
	model := provider + "-model"
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	executor := &routingObserverTestExecutor{provider: provider, failFirst: true}
	manager.RegisterExecutor(executor)
	indexes := registerRoutingObserverAuths(t, manager, provider, model, provider+"-a", provider+"-b")

	if _, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}

	snapshot := manager.RoutingObservability()
	if snapshot.Mode != "local" || snapshot.Strategy != "round-robin" {
		t.Fatalf("mode/strategy = %q/%q, want local/round-robin", snapshot.Mode, snapshot.Strategy)
	}
	if snapshot.Since.IsZero() {
		t.Fatal("since should be set by NewManager")
	}
	if len(snapshot.Recent) != 2 {
		t.Fatalf("recent decisions = %d, want 2 (%+v)", len(snapshot.Recent), snapshot.Recent)
	}
	if len(executor.calls) != 2 || executor.calls[0] == executor.calls[1] {
		t.Fatalf("executor calls = %v, want two distinct credentials", executor.calls)
	}
	failedID, servedID := executor.calls[0], executor.calls[1]
	initial, failover := snapshot.Recent[1], snapshot.Recent[0]
	if initial.AuthIndex != indexes[failedID] || initial.Attempt != 1 || initial.AttemptKind != RoutingAttemptInitial {
		t.Fatalf("initial decision = %+v", initial)
	}
	if initial.Candidates == nil || *initial.Candidates != 2 {
		t.Fatalf("initial candidates = %v, want 2", initial.Candidates)
	}
	if initial.Selection != RoutingSelectionStrategy {
		t.Fatalf("initial selection = %q, want strategy", initial.Selection)
	}
	if failover.AuthIndex != indexes[servedID] || failover.Attempt != 2 || failover.AttemptKind != RoutingAttemptFailover {
		t.Fatalf("failover decision = %+v", failover)
	}
	if failover.PreviousAuthIndex != indexes[failedID] {
		t.Fatalf("failover previous = %q, want %q", failover.PreviousAuthIndex, indexes[failedID])
	}
	if failover.Candidates == nil || *failover.Candidates != 1 {
		t.Fatalf("failover candidates = %v, want 1 after excluding the failed credential", failover.Candidates)
	}
	if snapshot.Counters.Selections != 2 || snapshot.Counters.Failovers != 1 || snapshot.Counters.Retries != 0 {
		t.Fatalf("counters = %+v", snapshot.Counters)
	}
}

func TestRoutingObservabilityTracksSessionAffinityLifecycle(t *testing.T) {
	ctx := context.Background()
	provider := "routing-observer-affinity"
	model := provider + "-model"
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	})
	defer affinity.Stop()
	manager.SetSelector(affinity)
	manager.RegisterExecutor(&routingObserverTestExecutor{provider: provider})
	indexes := registerRoutingObserverAuths(t, manager, provider, model, provider+"-a", provider+"-b")

	session := "observer-session"
	pick := func() *Auth {
		t.Helper()
		opts := cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.DerivedSessionIDMetadataKey: session,
		}}
		auth, _, _, errPick := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
		if errPick != nil {
			t.Fatalf("pick: %v", errPick)
		}
		return auth
	}

	bound := pick()
	if again := pick(); again.ID != bound.ID {
		t.Fatalf("affinity hit picked %q, want %q", again.ID, bound.ID)
	}
	disabled := bound.Clone()
	disabled.Disabled = true
	disabled.Status = StatusDisabled
	if _, errUpdate := manager.Update(WithSkipPersist(ctx), disabled); errUpdate != nil {
		t.Fatalf("disable bound credential: %v", errUpdate)
	}
	moved := pick()
	if moved.ID == bound.ID {
		t.Fatalf("rebind kept disabled credential %q", moved.ID)
	}

	snapshot := manager.RoutingObservability()
	if !snapshot.SessionAffinity.Enabled || snapshot.SessionAffinity.TTLSeconds != 3600 {
		t.Fatalf("session affinity = %+v", snapshot.SessionAffinity)
	}
	if snapshot.Strategy != "fill-first" {
		t.Fatalf("strategy = %q, want the wrapped fill-first selector", snapshot.Strategy)
	}
	if len(snapshot.Recent) != 3 {
		t.Fatalf("recent decisions = %d, want 3", len(snapshot.Recent))
	}
	wantSelections := []string{RoutingSelectionAffinityRebind, RoutingSelectionAffinityHit, RoutingSelectionAffinityNew}
	for i, want := range wantSelections {
		if got := snapshot.Recent[i].Selection; got != want {
			t.Fatalf("recent[%d].selection = %q, want %q", i, got, want)
		}
	}
	rebind := snapshot.Recent[0]
	if rebind.PreviousAuthIndex != indexes[bound.ID] || rebind.AuthIndex != indexes[moved.ID] {
		t.Fatalf("rebind = %+v, want move from %q to %q", rebind, indexes[bound.ID], indexes[moved.ID])
	}
	if rebind.Session == "" || strings.Contains(rebind.Session, session) || rebind.Session != snapshot.Recent[2].Session {
		t.Fatalf("session fingerprint = %q, want a stable hash that hides %q", rebind.Session, session)
	}
	if snapshot.Counters.AffinityNew != 1 || snapshot.Counters.AffinityHits != 1 || snapshot.Counters.AffinityRebinds != 1 {
		t.Fatalf("affinity counters = %+v", snapshot.Counters)
	}
	if snapshot.SessionAffinity.ActiveSessions != 1 || snapshot.SessionAffinity.SessionsByAuthIndex[indexes[moved.ID]] != 1 {
		t.Fatalf("active sessions = %+v, want one session bound to %q", snapshot.SessionAffinity, indexes[moved.ID])
	}
}

func TestRoutingObservabilityRecordsTransportFromExecutor(t *testing.T) {
	provider := "routing-observer-transport"
	model := provider + "-model"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&routingObserverTestExecutor{provider: provider, transport: RoutingTransportWebsocket})
	registerRoutingObserverAuths(t, manager, provider, model, provider+"-a")

	if _, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}
	snapshot := manager.RoutingObservability()
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].Transport != RoutingTransportWebsocket {
		t.Fatalf("recent = %+v, want one websocket decision", snapshot.Recent)
	}
	if snapshot.Counters.TransportWebsocket != 1 || snapshot.Counters.TransportHTTP != 0 {
		t.Fatalf("transport counters = %+v", snapshot.Counters)
	}

	// Without a request trace the note is ignored instead of touching unrelated decisions.
	NoteRoutingTransport(context.Background(), RoutingTransportHTTP)
	if got := manager.RoutingObservability().Counters.TransportHTTP; got != 0 {
		t.Fatalf("untraced transport note counted: %d", got)
	}
}

func TestRoutingObserverHistoryIsBoundedNewestFirst(t *testing.T) {
	var observer routingObserver
	total := routingDecisionHistoryLimit + 5
	for i := 0; i < total; i++ {
		observer.record(context.Background(), nil, RoutingDecision{Provider: "p", AuthIndex: string(rune('a' + i%26))}, "id", nil)
	}
	_, counters, recent := observer.snapshot()
	if len(recent) != routingDecisionHistoryLimit {
		t.Fatalf("recent = %d, want %d", len(recent), routingDecisionHistoryLimit)
	}
	if counters.Selections != int64(total) {
		t.Fatalf("selections = %d, want %d", counters.Selections, total)
	}
	if recent[0].seq != uint64(total) || recent[len(recent)-1].seq != uint64(total-routingDecisionHistoryLimit+1) {
		t.Fatalf("history order = newest %d oldest %d", recent[0].seq, recent[len(recent)-1].seq)
	}
	if recent[0].Attempt != 1 || recent[0].AttemptKind != RoutingAttemptInitial || recent[0].Selection != RoutingSelectionStrategy {
		t.Fatalf("untraced decision defaults = %+v", recent[0])
	}
}

func TestRoutingStrategyNameReflectsRunningSelector(t *testing.T) {
	for _, testCase := range []struct {
		selector Selector
		want     string
	}{
		{&RoundRobinSelector{}, "round-robin"},
		{&WeightedRoundRobinSelector{}, "weighted-round-robin"},
		{&FillFirstSelector{}, "fill-first"},
		{NewQuotaAwareSelector(&RoundRobinSelector{}), "quota-aware"},
		{&SessionAffinitySelector{fallback: NewQuotaAwareSelector(&RoundRobinSelector{})}, "quota-aware"},
		{lastAuthSelector{}, "custom"},
	} {
		if got := routingStrategyName(testCase.selector); got != testCase.want {
			t.Fatalf("routingStrategyName(%T) = %q, want %q", testCase.selector, got, testCase.want)
		}
	}
}

func TestRoutingPickNoteCarriesQuotaAwareProbeReason(t *testing.T) {
	now := quotaAwareTestBase()
	selector := newTestQuotaAwareSelector(now, nil)
	auths := []*Auth{
		{ID: "a", Provider: "codex", Status: StatusActive},
		codexQuotaAuth("b", now, 40, 10*time.Hour),
	}
	pick := func() (*Auth, *routingPickNote) {
		t.Helper()
		note := &routingPickNote{candidates: -1}
		auth, errPick := selector.Pick(withRoutingPickNote(context.Background(), note), "codex", "", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatalf("Pick: %v", errPick)
		}
		return auth, note
	}

	// The credential without weekly data is probed once, then normal pace ranking resumes.
	probed, note := pick()
	if probed.ID != "a" || note.strategyReason != "probe_no_weekly_data" || note.candidates != 2 {
		t.Fatalf("probe pick = %s %+v, want a with probe_no_weekly_data over 2 candidates", probed.ID, note)
	}
	ranked, note := pick()
	if ranked.ID != "b" || note.strategyReason != "weekly_pace" {
		t.Fatalf("ranked pick = %s %+v, want b with weekly_pace", ranked.ID, note)
	}
}
