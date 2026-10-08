package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// errRefreshDraining is returned instead of starting a token exchange once
// DrainRefreshes has run. The auth's state is left untouched.
var errRefreshDraining = errors.New("auth refresh refused: auth manager is draining refreshes for shutdown")

// refreshFlightTracker counts token exchanges that have started, so shutdown can
// wait for them to persist their result. The zero value is ready to use.
type refreshFlightTracker struct {
	mu       sync.Mutex
	draining bool
	count    int
	byAuth   map[string]int
	// idle is closed when count drops to zero; begin replaces it on 0 -> 1.
	idle chan struct{}
}

// begin registers an exchange for authID. It returns false while draining.
func (t *refreshFlightTracker) begin(authID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.beginLocked(authID)
}

// end releases an exchange registered by begin.
func (t *refreshFlightTracker) end(authID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked(authID)
}

// beginLocked is begin for callers that hold t.mu.
func (t *refreshFlightTracker) beginLocked(authID string) bool {
	if t.draining {
		return false
	}
	if t.count == 0 {
		t.idle = make(chan struct{})
	}
	t.count++
	if t.byAuth == nil {
		t.byAuth = make(map[string]int)
	}
	t.byAuth[authID]++
	return true
}

// endLocked is end for callers that hold t.mu.
func (t *refreshFlightTracker) endLocked(authID string) {
	if t.byAuth[authID] <= 1 {
		delete(t.byAuth, authID)
	} else {
		t.byAuth[authID]--
	}
	t.count--
	if t.count == 0 {
		close(t.idle)
	}
}

func (t *refreshFlightTracker) setDraining(draining bool) {
	t.mu.Lock()
	t.draining = draining
	t.mu.Unlock()
}

// authIDsLocked returns the in-flight auth IDs in a stable order. Callers hold t.mu.
func (t *refreshFlightTracker) authIDsLocked() []string {
	ids := make([]string, 0, len(t.byAuth))
	for id := range t.byAuth {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DrainRefreshes stops new token exchanges from starting and waits until the
// running ones have finished and persisted their result, or until ctx is done.
// Once an exchange is sent, a provider that rotates refresh tokens has already
// invalidated the stored one, so its result must reach the store before the
// process exits. On ctx expiry the returned error names the auths still
// refreshing. StartAutoRefresh allows new exchanges again.
func (m *Manager) DrainRefreshes(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t := &m.refreshFlights
	t.mu.Lock()
	t.draining = true
	if t.count == 0 {
		t.mu.Unlock()
		return nil
	}
	idle := t.idle
	ids := t.authIDsLocked()
	t.mu.Unlock()

	start := time.Now()
	log.Infof("waiting for %d in-flight auth refresh(es) to finish: %s", len(ids), strings.Join(ids, ", "))
	for {
		select {
		case <-idle:
		case <-ctx.Done():
			t.mu.Lock()
			ids = t.authIDsLocked()
			t.mu.Unlock()
			if len(ids) == 0 {
				return nil
			}
			return fmt.Errorf("%d auth refresh(es) still in flight, their rotated tokens may be lost: %s: %w", len(ids), strings.Join(ids, ", "), ctx.Err())
		}
		// A concurrent StartAutoRefresh lifts the drain, so an exchange may have
		// begun after the captured channel closed. Wait for that one as well.
		t.mu.Lock()
		if t.count == 0 {
			t.mu.Unlock()
			log.Infof("in-flight auth refreshes finished after %s", time.Since(start).Round(time.Millisecond))
			return nil
		}
		idle = t.idle
		t.mu.Unlock()
	}
}
