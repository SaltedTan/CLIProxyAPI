package util

import (
	"strings"
	"testing"
)

func TestMaskSensitiveHeaderValueMasksCookiesAndManagementKey(t *testing.T) {
	cases := map[string]string{
		"Cookie":           "session=abcdef0123456789secret; theme=dark",
		"Set-Cookie":       "session=abcdef0123456789secret; Path=/; HttpOnly",
		"X-Management-Key": "management-secret-0123456789",
	}
	for key, value := range cases {
		masked := MaskSensitiveHeaderValue(key, value)
		if masked != HideAPIKey(value) {
			t.Fatalf("%s masked = %q, want %q", key, masked, HideAPIKey(value))
		}
		if strings.Contains(masked, "0123456789") {
			t.Fatalf("%s masked value still contains the credential: %q", key, masked)
		}
	}
}

func TestMaskSensitiveHeaderValueMasksOnlyCredentialSubprotocols(t *testing.T) {
	key := "sk-proj-0123456789abcdefghij"
	value := "realtime, openai-insecure-api-key." + key + ", openai-beta.realtime-v1"

	masked := MaskSensitiveHeaderValue("Sec-WebSocket-Protocol", value)

	want := "realtime, openai-insecure-api-key." + HideAPIKey(key) + ", openai-beta.realtime-v1"
	if masked != want {
		t.Fatalf("masked = %q, want %q", masked, want)
	}
	if strings.Contains(masked, "0123456789") {
		t.Fatalf("masked subprotocols still contain the key: %q", masked)
	}
}

func TestMaskSensitiveHeaderValueKeepsPlainSubprotocols(t *testing.T) {
	value := "realtime, openai-beta.realtime-v1"
	if masked := MaskSensitiveHeaderValue("Sec-WebSocket-Protocol", value); masked != value {
		t.Fatalf("masked = %q, want unchanged %q", masked, value)
	}
}
