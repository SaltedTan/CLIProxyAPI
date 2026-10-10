package keyusage

import "time"

// The pool combines the overall weekly window of every enabled Claude OAuth account,
// whatever models it serves. Each account's percentage is of its own plan's weekly
// allowance, so percentages are weighted by that allowance in Pro units
// (clientusage.CredentialInfo.PlanProUnits: Pro 1, Team 1.25, Max 5x 5, Max 20x 10,
// else the credential weight), as the Fable figure is. An account that used up its
// weekly window has none of it left until the window resets, when all of it returns.

// combineWeekly weighs each account's weekly window by its plan's weekly allowance.
func combineWeekly(now time.Time, accounts []poolAccount) LimitSummary {
	return combineLimit(now, accounts, func(reading usageReading) windowReading { return reading.weekly })
}
