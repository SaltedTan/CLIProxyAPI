package util

import "strings"

// credentialSubprotocolMarkers name the part of a WebSocket subprotocol entry
// after which a credential follows, such as OpenAI realtime's
// "openai-insecure-api-key.<key>" entry used by browser clients.
var credentialSubprotocolMarkers = []string{"api-key", "apikey", "token", "secret", "bearer"}

// maskWebsocketSubprotocols masks the credential in each Sec-WebSocket-Protocol
// entry that carries one and leaves the other entries readable.
func maskWebsocketSubprotocols(value string) string {
	entries := strings.Split(value, ",")
	for i, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		masked := maskCredentialSubprotocol(trimmed)
		if masked != trimmed {
			entries[i] = strings.Replace(entry, trimmed, masked, 1)
		}
	}
	return strings.Join(entries, ",")
}

func maskCredentialSubprotocol(entry string) string {
	lower := strings.ToLower(entry)
	markerEnd := -1
	for _, marker := range credentialSubprotocolMarkers {
		if idx := strings.Index(lower, marker); idx >= 0 && (markerEnd < 0 || idx+len(marker) < markerEnd) {
			markerEnd = idx + len(marker)
		}
	}
	if markerEnd < 0 {
		return entry
	}
	// Keep the readable prefix up to the separator after the marker.
	if dot := strings.Index(entry[markerEnd:], "."); dot >= 0 {
		split := markerEnd + dot + 1
		return entry[:split] + HideAPIKey(entry[split:])
	}
	return HideAPIKey(entry)
}
