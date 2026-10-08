package clientusage

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Claude plan identifiers and their weekly allowance relative to Claude Pro. The
// allowances are approximate: Max 5x has about five times the weekly usage of Pro,
// and Max 20x about twice that of Max 5x.
const (
	PlanPro     = "pro"
	PlanTeam    = "team"
	PlanMax5x   = "max_5x"
	PlanMax20x  = "max_20x"
	PlanUnknown = "unknown"
)

var claudePlanProUnits = map[string]float64{
	PlanPro:    1,
	PlanTeam:   1.25,
	PlanMax5x:  5,
	PlanMax20x: 10,
}

// Plan sources reported with CredentialInfo.
const (
	PlanSourcePlanType         = "plan_type"
	PlanSourceRateLimitTier    = "rate_limit_tier"
	PlanSourceOrganizationType = "organization_type"
	PlanSourceWeight           = "weight"
)

// CredentialInfo describes a Claude credential for usage reports.
type CredentialInfo struct {
	AuthIndex string `json:"auth_index,omitempty"`
	Label     string `json:"label,omitempty"`
	// Plan is a known plan (pro, team, max_5x, max_20x), another plan name, or unknown.
	Plan string `json:"plan"`
	// PlanProUnits is the weekly allowance in Claude Pro units
	// (Pro 1, Team 1.25, Max 5x 5, Max 20x 10).
	PlanProUnits float64 `json:"plan_pro_units"`
	// PlanSource names where the allowance came from. Plans without a known
	// allowance fall back to the credential weight, which also expresses relative
	// plan size for quota-aware routing.
	PlanSource string `json:"plan_source"`
}

// CredentialInfoFromAuth resolves the report details of a credential. An explicit
// plan_type metadata value (pro, team, max_5x, max_20x) overrides the plan recorded
// from the Claude OAuth profile (rate_limit_tier and organization_type).
func CredentialInfoFromAuth(auth *coreauth.Auth) CredentialInfo {
	if auth == nil {
		return CredentialInfo{Plan: PlanUnknown, PlanProUnits: 1, PlanSource: PlanSourceWeight}
	}
	info := CredentialInfo{AuthIndex: auth.Index, Label: strings.TrimSpace(auth.Label)}
	if info.Label == "" {
		info.Label = metadataString(auth.Metadata, "email")
	}
	plan, source := resolveClaudePlan(auth.Metadata)
	if units, ok := claudePlanProUnits[plan]; ok {
		info.Plan, info.PlanProUnits, info.PlanSource = plan, units, source
		return info
	}
	if plan == "" {
		plan = PlanUnknown
	}
	info.Plan, info.PlanSource = plan, PlanSourceWeight
	info.PlanProUnits = float64(coreauth.AuthWeight(auth))
	if info.PlanProUnits <= 0 {
		info.PlanProUnits = 1
	}
	return info
}

func resolveClaudePlan(metadata map[string]any) (string, string) {
	if plan := normalizeClaudePlan(metadataString(metadata, "plan_type")); plan != "" {
		return plan, PlanSourcePlanType
	}
	organizationType := strings.ToLower(metadataString(metadata, "organization_type"))
	// The tier only sizes Max plans; a stale Max tier must not override a newer plan.
	if organizationType == "" || organizationType == "claude_max" {
		tier := strings.ToLower(metadataString(metadata, "rate_limit_tier"))
		switch {
		case strings.Contains(tier, "max_20x"):
			return PlanMax20x, PlanSourceRateLimitTier
		case strings.Contains(tier, "max_5x"):
			return PlanMax5x, PlanSourceRateLimitTier
		}
	}
	if organizationType == "claude_pro" {
		return PlanPro, PlanSourceOrganizationType
	}
	if plan := strings.TrimPrefix(organizationType, "claude_"); plan != "" {
		return plan, PlanSourceOrganizationType
	}
	return "", ""
}

func normalizeClaudePlan(raw string) string {
	plan := strings.ToLower(strings.TrimSpace(raw))
	plan = strings.NewReplacer("-", "_", " ", "_").Replace(plan)
	plan = strings.TrimPrefix(plan, "claude_")
	switch plan {
	case "max5x":
		return PlanMax5x
	case "max20x":
		return PlanMax20x
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
