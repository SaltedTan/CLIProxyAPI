// Package claudeplan resolves the plan of a Claude OAuth credential and its allowances
// relative to Claude Pro, shared by usage reports and quota-aware routing: ProUnits is
// a plan's weekly allowance and SessionProUnits its 5-hour (session) limit.
package claudeplan

import "strings"

// Claude plan identifiers. The allowances are approximate. Anthropic sells Max 5x and
// Max 20x as five and twenty times the usage of Pro per 5-hour session; per week, Max
// 5x has about five times the usage of Pro, and Max 20x about twice that of Max 5x.
const (
	Pro     = "pro"
	Team    = "team"
	Max5x   = "max_5x"
	Max20x  = "max_20x"
	Unknown = "unknown"
)

// Sources a plan is resolved from.
const (
	SourcePlanType         = "plan_type"
	SourceRateLimitTier    = "rate_limit_tier"
	SourceOrganizationType = "organization_type"
)

// proUnits holds each plan's weekly allowance.
var proUnits = map[string]float64{
	Pro:    1,
	Team:   1.25,
	Max5x:  5,
	Max20x: 10,
}

// sessionProUnits holds each plan's 5-hour limit. Max 20x has twice the weekly
// allowance of Max 5x but four times its 5-hour limit.
var sessionProUnits = map[string]float64{
	Pro:    1,
	Team:   1.25,
	Max5x:  5,
	Max20x: 20,
}

// ProUnits returns the weekly allowance of a known plan in Claude Pro units.
func ProUnits(plan string) (float64, bool) {
	units, ok := proUnits[plan]
	return units, ok
}

// SessionProUnits returns the 5-hour (session) limit of a known plan in Claude Pro
// units: 1 is one Pro plan's 5-hour limit.
func SessionProUnits(plan string) (float64, bool) {
	units, ok := sessionProUnits[plan]
	return units, ok
}

// Resolve returns the plan recorded in a Claude credential's metadata and its source,
// or empty strings when none is recorded. An explicit plan_type value (pro, team,
// max_5x, max_20x) overrides the plan recorded from the Claude OAuth profile
// (rate_limit_tier and organization_type).
func Resolve(metadata map[string]any) (string, string) {
	if plan := normalize(metadataString(metadata, "plan_type")); plan != "" {
		return plan, SourcePlanType
	}
	organizationType := strings.ToLower(metadataString(metadata, "organization_type"))
	// The tier only sizes Max plans; a stale Max tier must not override a newer plan.
	if organizationType == "" || organizationType == "claude_max" {
		tier := strings.ToLower(metadataString(metadata, "rate_limit_tier"))
		switch {
		case strings.Contains(tier, "max_20x"):
			return Max20x, SourceRateLimitTier
		case strings.Contains(tier, "max_5x"):
			return Max5x, SourceRateLimitTier
		}
	}
	if organizationType == "claude_pro" {
		return Pro, SourceOrganizationType
	}
	if plan := strings.TrimPrefix(organizationType, "claude_"); plan != "" {
		return plan, SourceOrganizationType
	}
	return "", ""
}

func normalize(raw string) string {
	plan := strings.ToLower(strings.TrimSpace(raw))
	plan = strings.NewReplacer("-", "_", " ", "_").Replace(plan)
	plan = strings.TrimPrefix(plan, "claude_")
	switch plan {
	case "max5x":
		return Max5x
	case "max20x":
		return Max20x
	default:
		return plan
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
