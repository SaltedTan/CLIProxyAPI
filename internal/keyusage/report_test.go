package keyusage

import (
	"encoding/json"
	"strings"
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
		FiveHour: FiveHourSummary{
			Available:                 true,
			CapacityProUnits:          26,
			RemainingProUnits:         18.3,
			NextResetAt:               timePtr(testNow.Add(2*time.Hour + 13*time.Minute)),
			NextResetRestoresProUnits: 5,
			Partial:                   true,
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

Claude 5-hour limit (shared by all keys): 1830% of 2600% left
  100% is one Claude Pro plan's 5-hour limit (Max 5x 500%, Max 20x 2000%).
  Next top-up in 2h13m (Thu 2026-10-08 14:13 UTC): +500%
  Some accounts could not be read; the figure may be off.

Fable (shared by all keys): 58.3% left
  Next top-up in 19h30m (Fri 2026-10-09 07:30 UTC): +22%
  Some accounts could not be read; the figure may be off.
`
	if got := report.Text(); got != want {
		t.Fatalf("text =\n%s\nwant\n%s", got, want)
	}

	// Fractions of a percent show; a figure without a top-up has no top-up line.
	report.FiveHour = FiveHourSummary{Available: true, CapacityProUnits: 6.25, RemainingProUnits: 6.004}
	if got, want := report.Text(), "Claude 5-hour limit (shared by all keys): 600.4% of 625% left\n  100% is one Claude Pro plan's 5-hour limit (Max 5x 500%, Max 20x 2000%).\n\nFable"; !strings.Contains(got, want) {
		t.Fatalf("text =\n%s\nwant it to contain\n%s", got, want)
	}

	report.Claude = clientusage.KeyAllowance{Limited: true, LimitProUnits: ptr(5), RemainingProUnits: ptr(5), RemainingPercent: ptr(100)}
	report.FiveHour = FiveHourSummary{Partial: true}
	report.Fable = FableSummary{}
	want = `Client key: Laptop (3f2a9c0d1e2b4a5c)

Claude allowance: 100% left, 0.00 of 5.00 Pro units used
  No window running: your next Claude request starts a 7-day window.

Claude 5-hour limit (shared): usage unavailable right now

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

func TestReportLine(t *testing.T) {
	report := Report{
		GeneratedAt: testNow,
		Claude: clientusage.KeyAllowance{
			Limited:           true,
			LimitProUnits:     ptr(5),
			RemainingProUnits: ptr(3.16),
			RemainingPercent:  ptr(63.2),
			WindowResetsAt:    timePtr(testNow.Add(118 * time.Hour)),
		},
		FiveHour: FiveHourSummary{
			Available:                 true,
			CapacityProUnits:          26,
			RemainingProUnits:         18.3,
			NextResetAt:               timePtr(testNow.Add(2*time.Hour + 13*time.Minute)),
			NextResetRestoresProUnits: 5,
		},
		Fable: FableSummary{
			Available:                true,
			RemainingPercent:         12.4,
			NextResetAt:              timePtr(testNow.Add(19*time.Hour + 30*time.Minute)),
			NextResetRestoresPercent: 0.4,
		},
	}
	if got, want := report.Line(false), "Claude 63% left · resets in 4d22h │ 5h 1830% of 2600% left · +500% in 2h13m │ Fable 12% left · +<1% in 19h30m"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	// The 5-hour figure is colored by the share of its capacity left, 70% here.
	if got, want := report.Line(true), "\x1b[32mClaude 63% left\x1b[0m · resets in 4d22h │ \x1b[32m5h 1830% of 2600% left\x1b[0m · +500% in 2h13m │ \x1b[31mFable 12% left\x1b[0m · +<1% in 19h30m"; got != want {
		t.Fatalf("colored line = %q, want %q", got, want)
	}

	report.FiveHour.Partial = true
	report.Fable.Partial = true
	if got, want := report.Line(false), "Claude 63% left · resets in 4d22h │ 5h 1830% of 2600% left (partial) · +500% in 2h13m │ Fable 12% left (partial) · +<1% in 19h30m"; got != want {
		t.Fatalf("partial line = %q, want %q", got, want)
	}

	report.FiveHour = FiveHourSummary{Available: true, CapacityProUnits: 26, RemainingProUnits: 7.8}
	if got, want := report.Line(true), "\x1b[32mClaude 63% left\x1b[0m · resets in 4d22h │ \x1b[33m5h 780% of 2600% left\x1b[0m │ \x1b[31mFable 12% left\x1b[0m (partial) · +<1% in 19h30m"; got != want {
		t.Fatalf("colored line = %q, want %q", got, want)
	}

	report.Claude = clientusage.KeyAllowance{Limited: true, LimitProUnits: ptr(5), RemainingProUnits: ptr(0), RemainingPercent: ptr(0), LimitReached: true, WindowResetsAt: timePtr(testNow.Add(26 * time.Hour))}
	report.FiveHour = FiveHourSummary{Available: true, CapacityProUnits: 1, RemainingProUnits: 0.004, NextResetAt: timePtr(testNow.Add(47 * time.Minute)), NextResetRestoresProUnits: 0.996}
	report.Fable = FableSummary{}
	if got, want := report.Line(true), "\x1b[31mClaude: limit reached\x1b[0m · back in 1d2h │ \x1b[31m5h <1% of 100% left\x1b[0m · +100% in 47m │ Fable: n/a"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	report.Claude = clientusage.KeyAllowance{Limited: true, LimitProUnits: ptr(5), RemainingProUnits: ptr(5), RemainingPercent: ptr(100)}
	report.FiveHour = FiveHourSummary{Partial: true}
	if got, want := report.Line(false), "Claude 100% left · 7d window starts on next use │ 5h: n/a │ Fable: n/a"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// The 5-hour figure adds a five_hour object; the Fable object keeps its encoding.
func TestReportJSON(t *testing.T) {
	report := Report{
		GeneratedAt: testNow,
		Key:         Key{ID: "3f2a9c0d1e2b4a5c"},
		FiveHour: FiveHourSummary{
			Available:                 true,
			CapacityProUnits:          26,
			RemainingProUnits:         18.3,
			NextResetAt:               timePtr(testNow.Add(2*time.Hour + 13*time.Minute)),
			NextResetRestoresProUnits: 5,
			UpdatedAt:                 timePtr(testNow.Add(-time.Minute)),
		},
		Fable: FableSummary{
			Available:                true,
			RemainingPercent:         58.3,
			UsedPercent:              41.7,
			NextResetAt:              timePtr(testNow.Add(19*time.Hour + 30*time.Minute)),
			NextResetRestoresPercent: 22,
			UpdatedAt:                timePtr(testNow.Add(-time.Minute)),
			Partial:                  true,
		},
	}
	body, errMarshal := json.Marshal(report)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, want := range []string{
		`"five_hour":{"available":true,"capacity_pro_units":26,"remaining_pro_units":18.3,"next_reset_at":"2026-10-08T14:13:00Z","next_reset_restores_pro_units":5,"updated_at":"2026-10-08T11:59:00Z","partial":false}`,
		`"fable":{"available":true,"remaining_percent":58.3,"used_percent":41.7,"next_reset_at":"2026-10-09T07:30:00Z","next_reset_restores_percent":22,"updated_at":"2026-10-08T11:59:00Z","partial":true}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("report = %s, want it to contain %s", body, want)
		}
	}
	empty, errMarshal := json.Marshal(FiveHourSummary{})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if got, want := string(empty), `{"available":false,"capacity_pro_units":0,"remaining_pro_units":0,"partial":false}`; got != want {
		t.Fatalf("unavailable five_hour = %s, want %s", got, want)
	}
}
