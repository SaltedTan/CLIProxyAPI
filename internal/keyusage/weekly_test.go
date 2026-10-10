package keyusage

import (
	"testing"
	"time"
)

// weekly is a reading of a weekly window used percent, resetting in resetIn unless it
// is zero.
func weekly(used float64, resetIn time.Duration) usageReading {
	reading := usageReading{at: testNow.Add(-time.Minute), weekly: windowReading{ok: true, used: used}}
	if resetIn != 0 {
		reading.weekly.resetAt = testNow.Add(resetIn)
	}
	return reading
}

func TestCombineWeeklyWeighsAccountsByWeeklyAllowance(t *testing.T) {
	weight := NewPool(nil).weight
	summary := combineWeekly(testNow, []poolAccount{
		{units: weight(planAuth("pro", "pro")), reading: weekly(70, 72*time.Hour), ok: true},
		// Used up for 28 hours, when all of it returns.
		{units: weight(planAuth("max5", "max_5x")), reading: weekly(100, 28*time.Hour), ok: true},
		// Max 20x has twice the weekly allowance of Max 5x, though four times its 5-hour limit.
		{units: weight(planAuth("max20", "max_20x")), reading: weekly(10, 120*time.Hour), ok: true},
		// Reports no weekly window: left out without making the figure partial.
		{units: 5, reading: usageReading{at: testNow}, ok: true},
	})
	// Capacity 1 + 5 + 10 Pro units, 0.3 + 0 + 9 left.
	if !summary.Available || summary.Partial || summary.CapacityProUnits != 16 || summary.RemainingProUnits != 9.3 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(28*time.Hour)) || summary.NextResetRestoresProUnits != 5 {
		t.Fatalf("next reset = %v +%v", summary.NextResetAt, summary.NextResetRestoresProUnits)
	}
	if summary.UpdatedAt == nil || !summary.UpdatedAt.Equal(testNow.Add(-time.Minute)) {
		t.Fatalf("updated at = %v", summary.UpdatedAt)
	}
}

func TestCombineWeeklyEdgeCases(t *testing.T) {
	summary := combineWeekly(testNow, []poolAccount{
		// A window that reset since it was read is all left.
		{units: 5, reading: weekly(100, -time.Minute), ok: true},
		// So is one that nothing used and has no reset; it tops up nothing.
		{units: 1, reading: weekly(0, 0), ok: true},
		// A window used up with its reset unknown has nothing left and never tops up.
		{units: 10, reading: weekly(100, 0), ok: true},
		// Usage over 100% counts as 100%; resets within a minute are one top-up.
		{units: 1.25, reading: weekly(104, 48*time.Hour), ok: true},
		{units: 1, reading: weekly(50, 48*time.Hour+30*time.Second), ok: true},
		// An account without a reading is skipped and makes the figure partial.
		{units: 10, ok: false},
	})
	if !summary.Partial || summary.CapacityProUnits != 18.25 || summary.RemainingProUnits != 6.5 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.NextResetAt == nil || !summary.NextResetAt.Equal(testNow.Add(48*time.Hour)) || summary.NextResetRestoresProUnits != 1.75 {
		t.Fatalf("next reset = %v +%v", summary.NextResetAt, summary.NextResetRestoresProUnits)
	}

	none := combineWeekly(testNow, []poolAccount{{units: 1, reading: fiveHour(20, time.Hour), ok: true}, {units: 1, ok: false}})
	if none.Available || !none.Partial || none.UpdatedAt != nil {
		t.Fatalf("no weekly window must be unavailable: %+v", none)
	}
}
