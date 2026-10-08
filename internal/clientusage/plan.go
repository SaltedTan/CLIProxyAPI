package clientusage

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudeplan"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Claude plan identifiers. The allowances are approximate: Max 5x has about five times
// the weekly usage of Pro, and Max 20x about twice that of Max 5x.
const (
	PlanPro     = claudeplan.Pro
	PlanTeam    = claudeplan.Team
	PlanMax5x   = claudeplan.Max5x
	PlanMax20x  = claudeplan.Max20x
	PlanUnknown = claudeplan.Unknown
)

// Plan sources reported with CredentialInfo.
const (
	PlanSourcePlanType         = claudeplan.SourcePlanType
	PlanSourceRateLimitTier    = claudeplan.SourceRateLimitTier
	PlanSourceOrganizationType = claudeplan.SourceOrganizationType
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
// from the Claude OAuth profile (rate_limit_tier and organization_type); see
// claudeplan.Resolve.
func CredentialInfoFromAuth(auth *coreauth.Auth) CredentialInfo {
	if auth == nil {
		return CredentialInfo{Plan: PlanUnknown, PlanProUnits: 1, PlanSource: PlanSourceWeight}
	}
	info := CredentialInfo{AuthIndex: auth.Index, Label: strings.TrimSpace(auth.Label)}
	if info.Label == "" {
		info.Label = metadataString(auth.Metadata, "email")
	}
	plan, source := claudeplan.Resolve(auth.Metadata)
	if units, ok := claudeplan.ProUnits(plan); ok {
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

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
