package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestCodexAutoExecutorTransportSelection pins which upstream transport an OAuth
// Codex credential actually uses. Only a downstream websocket combined with the
// auth file's "websockets": true may open an upstream websocket; everything else
// must stay on HTTP/SSE.
func TestCodexAutoExecutorTransportSelection(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`

	tests := []struct {
		name                string
		websockets          any
		downstreamWebsocket bool
		wantTransport       string
	}{
		{name: "metadata flag with downstream websocket", websockets: true, downstreamWebsocket: true, wantTransport: "websocket"},
		{name: "string metadata flag with downstream websocket", websockets: "true", downstreamWebsocket: true, wantTransport: "websocket"},
		{name: "missing flag with downstream websocket", websockets: nil, downstreamWebsocket: true, wantTransport: "http"},
		{name: "disabled flag with downstream websocket", websockets: false, downstreamWebsocket: true, wantTransport: "http"},
		{name: "metadata flag with downstream http", websockets: true, downstreamWebsocket: false, wantTransport: "http"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var transports []string
			record := func(transport string) {
				mu.Lock()
				transports = append(transports, transport)
				mu.Unlock()
			}

			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					record("websocket")
					conn, errUpgrade := upgrader.Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Errorf("upgrade websocket: %v", errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						t.Errorf("read upstream websocket message: %v", errRead)
						return
					}
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(completed)); errWrite != nil {
						t.Errorf("write completed websocket message: %v", errWrite)
					}
					return
				}
				record("http")
				if r.Method != http.MethodPost {
					t.Errorf("http fallback method = %s, want POST", r.Method)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + completed + "\n\n"))
			}))
			defer server.Close()

			metadata := map[string]any{"type": "codex", "access_token": "oauth-access-token"}
			if tt.websockets != nil {
				metadata["websockets"] = tt.websockets
			}
			auth := &cliproxyauth.Auth{
				ID:         "codex-oauth-" + tt.wantTransport,
				Provider:   "codex",
				Attributes: map[string]string{"base_url": server.URL},
				Metadata:   metadata,
			}

			ctx := context.Background()
			if tt.downstreamWebsocket {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			}
			exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			result, errExecute := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "gpt-5-codex",
				Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FromString("openai-response"),
				ResponseFormat: sdktranslator.FromString("openai-response"),
			})
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}

			timeout := time.After(5 * time.Second)
		drain:
			for {
				select {
				case chunk, ok := <-result.Chunks:
					if !ok {
						break drain
					}
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
				case <-timeout:
					t.Fatal("timed out draining stream")
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(transports) != 1 || transports[0] != tt.wantTransport {
				t.Fatalf("upstream transports = %v, want exactly [%s]", transports, tt.wantTransport)
			}
		})
	}
}
