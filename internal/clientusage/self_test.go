package clientusage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestKeyAllowanceMatchesTheReportAndAdmission(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-a": 1})

	allowance := tracker.KeyAllowance("key-a")
	if !allowance.Limited || allowance.LimitReached {
		t.Fatalf("allowance = %+v, want limited and under the limit", allowance)
	}
	approx(t, "used", allowance.UsedProUnits, 0.25)
	approx(t, "remaining", *allowance.RemainingProUnits, 0.75)
	approx(t, "remaining percent", *allowance.RemainingPercent, 75)
	if allowance.WindowResetsAt == nil || !allowance.WindowResetsAt.Equal(testWindowEnd) {
		t.Fatalf("window resets at = %v, want %v", allowance.WindowResetsAt, testWindowEnd)
	}
	report := findKey(t, tracker.Snapshot(SnapshotOptions{}), KeyID("key-a")).Claude
	if *report.RemainingProUnits != *allowance.RemainingProUnits || !report.WindowResetsAt.Equal(*allowance.WindowResetsAt) {
		t.Fatalf("allowance %+v differs from the report %+v", allowance, report)
	}

	tracker.SetLimits(map[string]float64{"key-a": 0.25})
	allowance = tracker.KeyAllowance("key-a")
	if !allowance.LimitReached || *allowance.RemainingProUnits != 0 || *allowance.RemainingPercent != 0 {
		t.Fatalf("allowance = %+v, want the limit reached", allowance)
	}
	if errAdmit := tracker.Admit(requestContext("key-a"), claudeAuth); errAdmit == nil {
		t.Fatal("a key reported at its limit must be refused")
	}
}

func TestKeyAllowanceOfIdleUnlimitedAndOtherKeys(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetLimits(map[string]float64{"key-b": 2})

	// key-b has a limit but no usage: its whole allowance is left and no window runs.
	idle := tracker.KeyAllowance("key-b")
	if !idle.Limited || idle.UsedProUnits != 0 || *idle.RemainingProUnits != 2 || idle.WindowResetsAt != nil {
		t.Fatalf("idle allowance = %+v", idle)
	}
	// key-a has usage but no limit.
	unlimited := tracker.KeyAllowance("key-a")
	if unlimited.Limited || unlimited.RemainingProUnits != nil || unlimited.WindowResetsAt == nil {
		t.Fatalf("unlimited allowance = %+v", unlimited)
	}
	approx(t, "used", unlimited.UsedProUnits, 0.25)

	// After the key's window ends, nothing is used until the next request.
	now = testWindowEnd.Add(time.Minute)
	ended := tracker.KeyAllowance("key-a")
	if ended.UsedProUnits != 0 || ended.WindowResetsAt != nil {
		t.Fatalf("allowance after the window = %+v", ended)
	}
}

func TestKeyAllowanceNamesNoCredential(t *testing.T) {
	now := testNow
	tracker, _ := limitedKeyTracker(t, &now)
	tracker.SetCredentialResolver(maxPlan)
	tracker.SetLimits(map[string]float64{"key-a": 5})
	raw, errMarshal := json.Marshal(tracker.KeyAllowance("key-a"))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, leak := range []string{"claude-1", "idx-claude-1", "Max", "key-a", "auth"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("allowance %s contains %q", raw, leak)
		}
	}
}
