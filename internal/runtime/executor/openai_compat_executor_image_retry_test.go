package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// A rate-limited streamed image request keeps the upstream Retry-After, like the other
// request paths, so the credential cools down for the time the upstream asked.
func TestOpenAICompatExecutorImageStreamPropagatesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit","message":"try later"}}`))
	}))
	t.Cleanup(server.Close)

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	_, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: []byte(`{"model":"compat-image","prompt":"draw","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-image"),
		Stream:       true,
		Headers:      http.Header{"Content-Type": []string{"application/json"}},
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations",
		},
	})
	if errExecute == nil {
		t.Fatal("expected rate-limit error")
	}
	retryable, ok := errExecute.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil || *retryable.RetryAfter() != 7*time.Second {
		t.Fatalf("error = %v, retry-after missing or not 7s", errExecute)
	}
	if status, ok := errExecute.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status 429", errExecute)
	}
}
