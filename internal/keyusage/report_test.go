package keyusage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
)

func ptr(value float64) *float64 { return &value }

func timePtr(value time.Time) *time.Time { return &value }

func TestReportText(t *testing.T) {
	report := Report{
		GeneratedAt: testNow,
		Key:         Key{ID: "3f2a9c0d1e2b4a5c", Name: "Laptop"},
		Claude: clientusage.KeyAllowance{
			Limited:           true,
			LimitProUnits:     ptr(5),
			UsedProUnits:      1.84,
			RemainingProUnits: ptr(3.16),
			RemainingPercent:  ptr(63.2),
			WindowStartedAt:   timePtr(testNow.Add(-50 * time.Hour)),
			WindowResetsAt:    timePtr(testNow.Add(118 * time.Hour)),
		},
		Fable: FableSummary{
			Available:                true,
			RemainingPercent:         58.3,
			UsedPercent:              41.7,
			NextResetAt:              timePtr(testNow.Add(19*time.Hour + 30*time.Minute)),
			NextResetRestoresPercent: 22,
			Partial:                  true,
		},
	}
	want := `Client key: Laptop (3f2a9c0d1e2b4a5c)

Claude allowance: 63.2% left, 1.84 of 5.00 Pro units used
  Window resets in 4d22h (Tue 2026-10-13 10:00 UTC)

Fable (shared by all keys): 58.3% left
  Next top-up in 19h30m (Fri 2026-10-09 07:30 UTC): +22%
  Some accounts could not be read; the figure may be off.
`
	if got := report.Text(); got != want {
		t.Fatalf("text =\n%s\nwant\n%s", got, want)
	}

	report.Claude = clientusage.KeyAllowance{Limited: true, LimitProUnits: ptr(5), RemainingProUnits: ptr(5), RemainingPercent: ptr(100)}
	report.Fable = FableSummary{}
	want = `Client key: Laptop (3f2a9c0d1e2b4a5c)

Claude allowance: 100% left, 0.00 of 5.00 Pro units used
  No window running: your next Claude request starts a 7-day window.

Fable (shared): usage unavailable right now
`
	if got := report.Text(); got != want {
		t.Fatalf("text =\n%s\nwant\n%s", got, want)
	}
}

func TestKeyName(t *testing.T) {
	names := map[string]string{"full-key": "Laptop", clientusage.KeyID("other"): "Desktop"}
	if KeyName(names, "full-key") != "Laptop" || KeyName(names, "other") != "Desktop" || KeyName(names, "") != "" {
		t.Fatal("key names must resolve by full key, then by key ID")
	}
}
