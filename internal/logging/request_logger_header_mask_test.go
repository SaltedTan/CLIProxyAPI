package logging

import (
	"strings"
	"testing"
)

func TestFormatRequestInfoMasksCookieManagementKeyAndSubprotocolKey(t *testing.T) {
	logger := NewFileRequestLogger(true, t.TempDir(), "", 10)
	headers := map[string][]string{
		"Cookie":                 {"session=cookie-secret-0123456789"},
		"X-Management-Key":       {"management-secret-0123456789"},
		"Sec-Websocket-Protocol": {"realtime, openai-insecure-api-key.sk-secret-0123456789"},
	}

	content := logger.formatRequestInfo("/v1/realtime", "GET", headers, nil, "", "", false)

	if strings.Contains(content, "0123456789") {
		t.Fatalf("request log contains an unmasked credential:\n%s", content)
	}
	if !strings.Contains(content, "Sec-Websocket-Protocol: realtime, openai-insecure-api-key.") {
		t.Fatalf("request log lost the readable subprotocol entries:\n%s", content)
	}
}
