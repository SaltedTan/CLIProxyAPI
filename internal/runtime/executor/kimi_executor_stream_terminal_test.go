package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	kimiChatTerminalContent = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"k2","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	kimiChatTerminalFinish  = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"k2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	kimiChatTerminalUsage   = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"k2","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}` + "\n\n"
	kimiChatTerminalDone    = "data: [DONE]\n\n"
	kimiChatTerminalError   = `data: {"error":{"message":"boom","type":"server_error"}}` + "\n\n"
	kimiChatTerminalMessage = "upstream stream closed before [DONE]"

	kimiResponsesTerminalCreated   = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_t\",\"status\":\"in_progress\",\"model\":\"k2\"}}\n\n"
	kimiResponsesTerminalDelta     = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"
	kimiResponsesTerminalCompleted = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_t\",\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":5,\"output_tokens\":7}}}\n\n"
	kimiResponsesTerminalFailed    = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_t\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n"
	kimiResponsesTerminalError     = "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n"
	kimiResponsesTerminalMessage   = "upstream stream closed before response.completed"
)

func runKimiTerminalStream(t *testing.T, source, response sdktranslator.Format, stream string, readErr error) (string, []error, <-chan usage.Record) {
	t.Helper()
	baseURL := "https://kimi-terminal.test/" + strings.ReplaceAll(t.Name(), "/", "_")
	records := captureTerminalUsage(t, baseURL)
	exec := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "kimi", Attributes: map[string]string{"api_key": "test", "base_url": baseURL}}
	payload := []byte(`{"model":"kimi-k2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if source == sdktranslator.FormatOpenAIResponse {
		payload = []byte(`{"model":"kimi-k2.5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	}
	result, err := exec.ExecuteStream(terminalStreamContext(stream, readErr), auth, cliproxyexecutor.Request{
		Model:   "kimi-k2.5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: source, ResponseFormat: response, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	output, errs := drainTerminalStream(t, result)
	return output, errs, records
}

func TestKimiExecutorChatStreamCleanEOFBeforeDone(t *testing.T) {
	stream := kimiChatTerminalContent + kimiChatTerminalFinish + kimiChatTerminalUsage
	t.Run("responses client fails", func(t *testing.T) {
		output, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, stream, nil)
		assertTerminalStreamError(t, errs, kimiChatTerminalMessage)
		if strings.Contains(output, "response.completed") {
			t.Fatalf("truncated stream was completed: %s", output)
		}
		// Usage observed before the truncation is still counted, on a failed record.
		record := waitTerminalUsage(t, records)
		if !record.Failed || record.Fail.StatusCode != http.StatusBadGateway {
			t.Fatalf("usage record failed=%v status=%d, want failed with %d", record.Failed, record.Fail.StatusCode, http.StatusBadGateway)
		}
		if record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 {
			t.Fatalf("usage tokens input=%d output=%d, want 5/7", record.Detail.InputTokens, record.Detail.OutputTokens)
		}
	})
	t.Run("chat client keeps compatibility", func(t *testing.T) {
		output, errs, _ := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, stream, nil)
		if len(errs) != 0 {
			t.Fatalf("stream errors = %v, want none", errs)
		}
		if !strings.Contains(output, "hello") {
			t.Fatalf("stream output missing content: %s", output)
		}
	})
}

// A stream that ends mid-generation, with neither a finish_reason nor [DONE], is
// truncated for every client format, not only for Responses clients.
func TestKimiExecutorChatStreamCleanEOFMidGenerationFails(t *testing.T) {
	for _, tc := range []struct {
		response sdktranslator.Format
		message  string
	}{
		{response: sdktranslator.FormatOpenAI, message: "upstream stream closed before finish_reason"},
		{response: sdktranslator.FormatOpenAIResponse, message: kimiChatTerminalMessage},
	} {
		t.Run(tc.response.String(), func(t *testing.T) {
			_, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, tc.response, kimiChatTerminalContent, nil)
			assertTerminalStreamError(t, errs, tc.message)
			if record := waitTerminalUsage(t, records); !record.Failed || record.Fail.StatusCode != http.StatusBadGateway {
				t.Fatalf("usage record failed=%v status=%d, want failed with %d", record.Failed, record.Fail.StatusCode, http.StatusBadGateway)
			}
		})
	}
}

func TestKimiExecutorChatStreamCompleteHasNoError(t *testing.T) {
	stream := kimiChatTerminalContent + kimiChatTerminalFinish + kimiChatTerminalUsage + kimiChatTerminalDone
	for _, response := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		t.Run(response.String(), func(t *testing.T) {
			output, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, response, stream, nil)
			if len(errs) != 0 {
				t.Fatalf("stream errors = %v, want none", errs)
			}
			if response == sdktranslator.FormatOpenAIResponse && strings.Count(output, `"type":"response.completed"`) != 1 {
				t.Fatalf("response.completed count != 1: %s", output)
			}
			if record := waitTerminalUsage(t, records); record.Failed {
				t.Fatalf("usage record failed with %d %s, want success", record.Fail.StatusCode, record.Fail.Body)
			}
		})
	}
}

func TestKimiExecutorChatStreamUpstreamErrorAddsNoSecondError(t *testing.T) {
	stream := kimiChatTerminalContent + kimiChatTerminalError
	t.Run("chat client", func(t *testing.T) {
		output, errs, _ := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, stream, nil)
		if len(errs) != 0 {
			t.Fatalf("stream errors = %v, want only the forwarded upstream error", errs)
		}
		if strings.Count(output, "boom") != 1 {
			t.Fatalf("forwarded upstream error count != 1: %s", output)
		}
	})
	// The Responses translation drops the error chunk, so the 502 is the only error.
	t.Run("responses client", func(t *testing.T) {
		output, errs, _ := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, stream, nil)
		if strings.Contains(output, "boom") {
			t.Fatalf("Responses translation now forwards upstream errors; revisit this case: %s", output)
		}
		assertTerminalStreamError(t, errs, kimiChatTerminalMessage)
	})
}

func TestKimiExecutorChatStreamScannerError(t *testing.T) {
	readErr := errors.New("upstream connection reset")
	for _, response := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		t.Run(response.String(), func(t *testing.T) {
			output, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAI, response, kimiChatTerminalContent+kimiChatTerminalUsage, readErr)
			if len(errs) != 1 || !errors.Is(errs[0], readErr) {
				t.Fatalf("stream errors = %v, want only %v", errs, readErr)
			}
			// No terminal marker is synthesized ahead of the transport error.
			if strings.Contains(output, "response.completed") {
				t.Fatalf("failed stream was completed: %s", output)
			}
			record := waitTerminalUsage(t, records)
			if !record.Failed || record.Detail.InputTokens != 5 {
				t.Fatalf("usage record failed=%v input=%d, want failed with 5 input tokens", record.Failed, record.Detail.InputTokens)
			}
		})
	}
}

func TestKimiExecutorResponsesStreamCleanEOFBeforeTerminalFails(t *testing.T) {
	output, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, kimiResponsesTerminalCreated+kimiResponsesTerminalDelta, nil)
	assertTerminalStreamError(t, errs, kimiResponsesTerminalMessage)
	if !strings.Contains(output, "hello") {
		t.Fatalf("stream output missing forwarded content: %s", output)
	}
	if record := waitTerminalUsage(t, records); !record.Failed || record.Fail.StatusCode != http.StatusBadGateway {
		t.Fatalf("usage record failed=%v status=%d, want failed with %d", record.Failed, record.Fail.StatusCode, http.StatusBadGateway)
	}
}

func TestKimiExecutorResponsesStreamCompleteHasNoError(t *testing.T) {
	_, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, kimiResponsesTerminalCreated+kimiResponsesTerminalDelta+kimiResponsesTerminalCompleted+kimiChatTerminalDone, nil)
	if len(errs) != 0 {
		t.Fatalf("stream errors = %v, want none", errs)
	}
	record := waitTerminalUsage(t, records)
	if record.Failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 {
		t.Fatalf("usage record failed=%v input=%d output=%d, want success with 5/7", record.Failed, record.Detail.InputTokens, record.Detail.OutputTokens)
	}
}

func TestKimiExecutorResponsesStreamUpstreamFailureAddsNoSyntheticError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
	}{
		{name: "response.failed", event: kimiResponsesTerminalFailed},
		{name: "error", event: kimiResponsesTerminalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, errs, _ := runKimiTerminalStream(t, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, kimiResponsesTerminalCreated+kimiResponsesTerminalDelta+tc.event, nil)
			if len(errs) != 0 {
				t.Fatalf("stream errors = %v, want only the forwarded upstream failure", errs)
			}
			if strings.Count(output, "boom") != 1 {
				t.Fatalf("forwarded upstream failure count != 1: %s", output)
			}
		})
	}
}

func TestKimiExecutorResponsesStreamScannerError(t *testing.T) {
	readErr := errors.New("upstream connection reset")
	_, errs, records := runKimiTerminalStream(t, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, kimiResponsesTerminalCreated+kimiResponsesTerminalDelta, readErr)
	if len(errs) != 1 || !errors.Is(errs[0], readErr) {
		t.Fatalf("stream errors = %v, want only %v", errs, readErr)
	}
	if record := waitTerminalUsage(t, records); !record.Failed {
		t.Fatal("usage record not failed, want failed")
	}
}
