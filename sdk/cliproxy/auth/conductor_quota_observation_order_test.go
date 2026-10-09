package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func codexUsageHeadersContext(percent string, observedAt time.Time) context.Context {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeadersAt(ctx, http.Header{
		"X-Codex-Plan-Type":                   []string{"pro"},
		"X-Codex-Primary-Used-Percent":        []string{percent},
		"X-Codex-Primary-Window-Minutes":      []string{"10080"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
	}, observedAt)
	return ctx
}

// A long stream that received its headers before a later request reported newer usage, and
// finishes last, must not move the quota reading back. The order is the one of the headers,
// not of the results, whether or not the account's tokens rotated in between.
func TestMarkResultKeepsTheNewestQuotaObservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rotate bool
	}{
		{name: "same credential version"},
		{name: "after a token refresh", rotate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			base := registerForLineage(t, manager, &Auth{
				ID:       "quota-observation-order-" + tc.name,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"type": "codex", "access_token": "access-1", "refresh_token": "refresh-1"},
			})
			resultFor := func(auth *Auth) Result {
				return Result{
					AuthID:            auth.ID,
					Provider:          "codex",
					Success:           true,
					CredentialVersion: auth.CredentialVersion,
					RegistrationEpoch: auth.RegistrationEpoch,
					quotaLineage:      auth.quotaLineage,
				}
			}
			clock := time.Now().Add(-time.Hour)

			// Stream A receives its headers first.
			streamA := codexUsageHeadersContext("10", clock)
			resultA := resultFor(base)

			requestAuth := base
			if tc.rotate {
				if _, errUpdate := manager.UpdateRefreshedAuth(ctx, base, withTokens(base, "2")); errUpdate != nil {
					t.Fatalf("UpdateRefreshedAuth() error = %v", errUpdate)
				}
				requestAuth = currentForLineage(t, manager, base.ID)
				if requestAuth.CredentialVersion <= base.CredentialVersion {
					t.Fatalf("credential version = %d, want above %d", requestAuth.CredentialVersion, base.CredentialVersion)
				}
			}

			// Request B reads newer usage and finishes before stream A.
			clock = clock.Add(time.Minute)
			manager.MarkResult(codexUsageHeadersContext("95", clock), resultFor(requestAuth))
			if got := currentForLineage(t, manager, base.ID).Quota.Signals["X-Codex-Primary-Used-Percent"]; got != "95" {
				t.Fatalf("quota observation after request B = %q, want 95", got)
			}

			manager.MarkResult(streamA, resultA)
			current := currentForLineage(t, manager, base.ID)
			if got := current.Quota.Signals["X-Codex-Primary-Used-Percent"]; got != "95" {
				t.Fatalf("quota observation after stream A = %q, want 95: an older reading replaced a newer one", got)
			}
			if !current.Quota.ObservedAt.Equal(clock) {
				t.Fatalf("quota observed at %v, want %v", current.Quota.ObservedAt, clock)
			}
		})
	}
}
