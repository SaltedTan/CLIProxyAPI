package util

import (
	"strings"
	"testing"
)

func TestMaskSensitiveQueryMasksOAuthCallbackValues(t *testing.T) {
	const code = "ac_9f3b2c1d8e7a6b5c4d3e2f1a0b9c8d7e"
	const state = "st_0a1b2c3d4e5f6a7b8c9d"
	masked := MaskSensitiveQuery("code=" + code + "&state=" + state + "&scope=user%3Aprofile")

	if strings.Contains(masked, code) || strings.Contains(masked, state) {
		t.Fatalf("masked query still holds the callback values: %s", masked)
	}
	if !strings.Contains(masked, "scope=user%3Aprofile") {
		t.Fatalf("masked query changed a parameter that is not sensitive: %s", masked)
	}
}

func TestMaskSensitiveQueryKeepsUnrelatedCodeParameters(t *testing.T) {
	raw := "error_code=429&country_code=SG"
	if masked := MaskSensitiveQuery(raw); masked != raw {
		t.Fatalf("MaskSensitiveQuery(%q) = %q, want it unchanged", raw, masked)
	}
}
