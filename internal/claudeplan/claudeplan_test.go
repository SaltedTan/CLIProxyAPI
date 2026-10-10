package claudeplan

import "testing"

func TestProUnitsAreWeeklyAndSessionProUnitsPerSession(t *testing.T) {
	cases := []struct {
		plan            string
		weekly, session float64
	}{
		{Pro, 1, 1},
		{Team, 1.25, 1.25},
		{Max5x, 5, 5},
		{Max20x, 10, 20},
	}
	for _, tc := range cases {
		if weekly, ok := ProUnits(tc.plan); !ok || weekly != tc.weekly {
			t.Errorf("ProUnits(%q) = %v, %v, want %v", tc.plan, weekly, ok, tc.weekly)
		}
		if session, ok := SessionProUnits(tc.plan); !ok || session != tc.session {
			t.Errorf("SessionProUnits(%q) = %v, %v, want %v", tc.plan, session, ok, tc.session)
		}
	}
	for _, plan := range []string{"", Unknown, "enterprise"} {
		if _, ok := ProUnits(plan); ok {
			t.Errorf("ProUnits(%q) must be unknown", plan)
		}
		if _, ok := SessionProUnits(plan); ok {
			t.Errorf("SessionProUnits(%q) must be unknown", plan)
		}
	}
}
