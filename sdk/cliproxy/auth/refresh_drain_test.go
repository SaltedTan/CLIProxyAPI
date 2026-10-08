package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestDrainRefreshesWaitsForInFlightRefreshToPersist(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "drain-in-flight"
		manager, store, executor := newRotationManager(t, id)
		registerRotationAuth(t, manager, id, true)

		refreshDone := make(chan error, 1)
		go func() {
			_, errRefresh := manager.refreshAuthForRequest(context.Background(), id, "")
			refreshDone <- errRefresh
		}()
		<-executor.started

		drainCtx, cancelDrain := context.WithCancel(context.Background())
		defer cancelDrain()
		drainDone := make(chan error, 1)
		go func() { drainDone <- manager.DrainRefreshes(drainCtx) }()
		synctest.Wait()
		select {
		case errDrain := <-drainDone:
			t.Fatalf("DrainRefreshes() returned %v while a refresh was still in flight", errDrain)
		default:
		}

		close(executor.release)
		if errDrain := <-drainDone; errDrain != nil {
			t.Fatalf("DrainRefreshes() error = %v, want nil", errDrain)
		}
		// DrainRefreshes returns only after the persist, so the store already has the tokens.
		assertRotatedTokensKept(t, manager, store, id)
		if errRefresh := <-refreshDone; errRefresh != nil {
			t.Fatalf("refreshAuthForRequest() error = %v, want nil", errRefresh)
		}
	})
}

func TestDrainRefreshesRefusesNewExchangesWithoutChangingState(t *testing.T) {
	const id = "drain-refused"
	manager, _, executor := newRotationManager(t, "")
	registerRotationAuth(t, manager, id, false)
	before, _ := manager.GetByID(id)

	if errDrain := manager.DrainRefreshes(context.Background()); errDrain != nil {
		t.Fatalf("DrainRefreshes() with nothing in flight = %v, want nil", errDrain)
	}
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), id, ""); !errors.Is(errRefresh, errRefreshDraining) {
		t.Fatalf("refresh while draining error = %v, want %v", errRefresh, errRefreshDraining)
	}
	if _, errForce := manager.ForceRefreshAuth(context.Background(), id); !errors.Is(errForce, errRefreshDraining) {
		t.Fatalf("force refresh while draining error = %v, want %v", errForce, errRefreshDraining)
	}
	if calls := executor.refreshCallsFor(id); calls != 0 {
		t.Fatalf("executor Refresh calls while draining = %d, want 0", calls)
	}
	after, _ := manager.GetByID(id)
	if after.LastError != nil || after.Unavailable != before.Unavailable || after.Status != before.Status ||
		!after.NextRefreshAfter.Equal(before.NextRefreshAfter) || after.RefreshFailures != before.RefreshFailures ||
		after.StatusMessage != before.StatusMessage || authRefreshToken(after) != authRefreshToken(before) {
		t.Fatalf("refused refresh changed the auth state:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// Restarting the manager lifts the drain.
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	manager.StartAutoRefresh(ctx, time.Hour)
	manager.StopAutoRefresh()
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), id, ""); errRefresh != nil {
		t.Fatalf("refresh after StartAutoRefresh error = %v, want nil", errRefresh)
	}
	if calls := executor.refreshCallsFor(id); calls != 1 {
		t.Fatalf("executor Refresh calls after StartAutoRefresh = %d, want 1", calls)
	}
}

// An embedder that restarts the manager during a drain lifts it, so a new
// exchange can begin right after the last tracked one ended. The drain must
// wait for that exchange too instead of returning on the stale idle signal.
func TestDrainRefreshesWaitsForRefreshStartedAfterConcurrentRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		flights := &manager.refreshFlights
		if !flights.begin("refresh-a") {
			t.Fatal("refresh-a refused before any drain")
		}

		drainCtx, cancelDrain := context.WithCancel(context.Background())
		defer cancelDrain()
		drainDone := make(chan error, 1)
		go func() { drainDone <- manager.DrainRefreshes(drainCtx) }()
		synctest.Wait()

		loopCtx, cancelLoop := context.WithCancel(context.Background())
		defer cancelLoop()
		defer manager.StopAutoRefresh()
		manager.StartAutoRefresh(loopCtx, time.Hour)

		// refresh-a ends and refresh-b begins before the drain sees refresh-a's end.
		flights.mu.Lock()
		flights.endLocked("refresh-a")
		startedB := flights.beginLocked("refresh-b")
		flights.mu.Unlock()
		if !startedB {
			t.Fatal("refresh-b refused after StartAutoRefresh lifted the drain")
		}
		synctest.Wait()
		select {
		case errDrain := <-drainDone:
			t.Fatalf("DrainRefreshes() returned %v while refresh-b was still in flight", errDrain)
		default:
		}

		flights.end("refresh-b")
		if errDrain := <-drainDone; errDrain != nil {
			t.Fatalf("DrainRefreshes() error = %v, want nil", errDrain)
		}
	})
}

func TestDrainRefreshesReportsInFlightAuthWhenContextEnds(t *testing.T) {
	const id = "drain-budget-spent"
	manager, store, executor := newRotationManager(t, id)
	registerRotationAuth(t, manager, id, true)

	refreshDone := make(chan error, 1)
	go func() {
		_, errRefresh := manager.refreshAuthForRequest(context.Background(), id, "")
		refreshDone <- errRefresh
	}()
	<-executor.started

	spent, cancelSpent := context.WithCancel(context.Background())
	cancelSpent()
	errDrain := manager.DrainRefreshes(spent)
	if errDrain == nil || !strings.Contains(errDrain.Error(), id) || !errors.Is(errDrain, context.Canceled) {
		t.Fatalf("DrainRefreshes(spent ctx) error = %v, want a context.Canceled error naming %q", errDrain, id)
	}

	close(executor.release)
	if errRefresh := <-refreshDone; errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v, want nil", errRefresh)
	}
	assertRotatedTokensKept(t, manager, store, id)
}
