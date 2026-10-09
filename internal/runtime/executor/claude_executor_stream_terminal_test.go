package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type captureClaudeUsagePlugin struct {
	records chan usage.Record
}

func (p *captureClaudeUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.Provider != "claude" {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

type noopClaudeUsagePlugin struct{}

func (noopClaudeUsagePlugin) HandleUsage(context.Context, usage.Record) {}

func TestClaudeExecutor_ExecuteStream_Translated_ClientDisconnectAfterTerminalEventIsNotFailed(t *testing.T) {
	const streamData = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstreamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(streamData))
			flusher.Flush()
		}
		// Hold the upstream body open until client disconnects or test ends,
		// reproducing upstream lag where body close happens after terminal event.
		select {
		case <-r.Context().Done():
		case <-upstreamClosed:
		}
	}))
	defer func() {
		close(upstreamClosed)
		server.Close()
	}()

	pluginName := "test-claude-translated-disconnect"
	plugin := &captureClaudeUsagePlugin{
		records: make(chan usage.Record, 4),
	}
	usage.RegisterNamedPlugin(pluginName, plugin)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(pluginName, noopClaudeUsagePlugin{})
	})

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	sawTerminalEvent := false
	// Read chunks until terminal event is seen, then immediately cancel downstream context
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error during stream read: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "response.completed") {
			// Client disconnects immediately upon receiving terminal event (e.g. Codex CLI >=0.153.4)
			sawTerminalEvent = true
			cancel()
			break
		}
	}
	if !sawTerminalEvent {
		t.Fatal("expected to observe terminal response.completed event before stream finished")
	}

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("expected usage record to not be marked failed, but got failed=true, fail status: %d body: %s", record.Fail.StatusCode, record.Fail.Body)
		}
		if record.Detail.InputTokens != 100 {
			t.Errorf("InputTokens = %d, want 100", record.Detail.InputTokens)
		}
		if record.Detail.OutputTokens != 15 {
			t.Errorf("OutputTokens = %d, want 15", record.Detail.OutputTokens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestClaudeExecutor_ExecuteStream_Passthrough_ClientDisconnectAfterTerminalEventIsNotFailed(t *testing.T) {
	const streamData = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstreamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(streamData))
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-upstreamClosed:
		}
	}))
	defer func() {
		close(upstreamClosed)
		server.Close()
	}()

	pluginName := "test-claude-passthrough-disconnect"
	plugin := &captureClaudeUsagePlugin{
		records: make(chan usage.Record, 4),
	}
	usage.RegisterNamedPlugin(pluginName, plugin)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(pluginName, noopClaudeUsagePlugin{})
	})

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	sawTerminalEvent := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "message_stop") {
			sawTerminalEvent = true
			cancel()
			break
		}
	}
	if !sawTerminalEvent {
		t.Fatal("expected to observe terminal message_stop event before stream finished")
	}

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("expected usage record to not be marked failed in passthrough, but got failed=true, fail status: %d body: %s", record.Fail.StatusCode, record.Fail.Body)
		}
		if record.Detail.InputTokens != 100 {
			t.Errorf("InputTokens = %d, want 100", record.Detail.InputTokens)
		}
		if record.Detail.OutputTokens != 15 {
			t.Errorf("OutputTokens = %d, want 15", record.Detail.OutputTokens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

// terminalStreamBody serves a canned stream, then fails the read with err or ends cleanly when err is nil.
type terminalStreamBody struct {
	reader io.Reader
	err    error
}

func (b *terminalStreamBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) && b.err != nil {
		return n, b.err
	}
	return n, err
}

func (b *terminalStreamBody) Close() error { return nil }

// terminalStreamContext answers every upstream request with the canned SSE stream.
func terminalStreamContext(stream string, readErr error) context.Context {
	return context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &terminalStreamBody{reader: strings.NewReader(stream), err: readErr},
			Request:    req,
		}, nil
	})))
}

// terminalUsagePlugin captures the usage records of one test credential, keyed by its base URL.
type terminalUsagePlugin struct {
	baseURL string
	records chan usage.Record
}

func (p *terminalUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if record.BaseURL != p.baseURL {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func captureTerminalUsage(t *testing.T, baseURL string) <-chan usage.Record {
	t.Helper()
	plugin := &terminalUsagePlugin{baseURL: baseURL, records: make(chan usage.Record, 4)}
	name := "test-terminal-" + baseURL
	usage.RegisterNamedPlugin(name, plugin)
	t.Cleanup(func() { usage.RegisterNamedPlugin(name, noopClaudeUsagePlugin{}) })
	return plugin.records
}

func waitTerminalUsage(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
	return usage.Record{}
}

func drainTerminalStream(t *testing.T, result *cliproxyexecutor.StreamResult) (string, []error) {
	t.Helper()
	var output strings.Builder
	var errs []error
	for chunk := range result.Chunks {
		output.Write(chunk.Payload)
		if chunk.Err != nil {
			errs = append(errs, chunk.Err)
		}
	}
	return output.String(), errs
}

func assertTerminalStreamError(t *testing.T, errs []error, wantMessage string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("stream errors = %v, want exactly one", errs)
	}
	status, ok := errs[0].(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("stream error = %#v, want status %d", errs[0], http.StatusBadGateway)
	}
	if errs[0].Error() != wantMessage {
		t.Fatalf("stream error = %q, want %q", errs[0].Error(), wantMessage)
	}
}

const (
	claudeTerminalIncompleteMessage = "claude executor: upstream stream response ended before message completion"
	claudeTerminalStart             = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_terminal","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n\n"
	claudeTerminalText = "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n"
	claudeTerminalDelta = "event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n"
	claudeTerminalStop  = "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	claudeTerminalError = "event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
)

var claudeTerminalFormats = []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse}

func runClaudeTerminalStream(t *testing.T, format sdktranslator.Format, stream string, readErr error) (string, []error, <-chan usage.Record) {
	t.Helper()
	baseURL := "https://claude-terminal.test/" + strings.ReplaceAll(t.Name(), "/", "_")
	records := captureTerminalUsage(t, baseURL)
	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": baseURL}}
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if format == sdktranslator.FormatOpenAIResponse {
		payload = []byte(`{"model":"claude-opus-5","input":"hi","stream":true}`)
	}
	result, err := exec.ExecuteStream(terminalStreamContext(stream, readErr), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: format, ResponseFormat: format, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	output, errs := drainTerminalStream(t, result)
	return output, errs, records
}

func TestClaudeExecutorStreamCleanEOFBeforeMessageStopFails(t *testing.T) {
	for _, format := range claudeTerminalFormats {
		t.Run(format.String(), func(t *testing.T) {
			_, errs, records := runClaudeTerminalStream(t, format, claudeTerminalStart+claudeTerminalText+claudeTerminalDelta, nil)
			assertTerminalStreamError(t, errs, claudeTerminalIncompleteMessage)
			// Usage observed before the truncation is still counted, on a failed record.
			record := waitTerminalUsage(t, records)
			if !record.Failed || record.Fail.StatusCode != http.StatusBadGateway {
				t.Fatalf("usage record failed=%v status=%d, want failed with %d", record.Failed, record.Fail.StatusCode, http.StatusBadGateway)
			}
			if record.Detail.InputTokens != 100 || record.Detail.OutputTokens != 15 {
				t.Fatalf("usage tokens input=%d output=%d, want 100/15", record.Detail.InputTokens, record.Detail.OutputTokens)
			}
		})
	}
	t.Run("empty body", func(t *testing.T) {
		_, errs, _ := runClaudeTerminalStream(t, sdktranslator.FormatClaude, "", nil)
		assertTerminalStreamError(t, errs, claudeTerminalIncompleteMessage)
	})
}

func TestClaudeExecutorStreamCompleteHasNoError(t *testing.T) {
	for _, format := range claudeTerminalFormats {
		t.Run(format.String(), func(t *testing.T) {
			_, errs, records := runClaudeTerminalStream(t, format, claudeTerminalStart+claudeTerminalText+claudeTerminalDelta+claudeTerminalStop, nil)
			if len(errs) != 0 {
				t.Fatalf("stream errors = %v, want none", errs)
			}
			if record := waitTerminalUsage(t, records); record.Failed {
				t.Fatalf("usage record failed with %d %s, want success", record.Fail.StatusCode, record.Fail.Body)
			}
		})
	}
}

func TestClaudeExecutorStreamUpstreamErrorEventAddsNoSyntheticError(t *testing.T) {
	stream := claudeTerminalStart + claudeTerminalText + claudeTerminalError
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		t.Run(format.String(), func(t *testing.T) {
			output, errs, _ := runClaudeTerminalStream(t, format, stream, nil)
			if len(errs) != 0 {
				t.Fatalf("stream errors = %v, want only the forwarded upstream error", errs)
			}
			if strings.Count(output, "overloaded_error") != 1 {
				t.Fatalf("forwarded upstream error count != 1: %s", output)
			}
		})
	}
	// The Responses translation drops upstream error events, so the client would
	// otherwise see no failure at all: it gets exactly one synthetic error.
	t.Run(sdktranslator.FormatOpenAIResponse.String(), func(t *testing.T) {
		output, errs, _ := runClaudeTerminalStream(t, sdktranslator.FormatOpenAIResponse, stream, nil)
		if strings.Contains(output, "overloaded_error") {
			t.Fatalf("Responses translation now forwards upstream error events; drop this case: %s", output)
		}
		assertTerminalStreamError(t, errs, claudeTerminalIncompleteMessage)
	})
}

func TestClaudeExecutorStreamScannerErrorUnchanged(t *testing.T) {
	readErr := errors.New("upstream connection reset")
	for _, format := range claudeTerminalFormats {
		t.Run(format.String(), func(t *testing.T) {
			_, errs, records := runClaudeTerminalStream(t, format, claudeTerminalStart+claudeTerminalText, readErr)
			if len(errs) != 1 || !errors.Is(errs[0], readErr) {
				t.Fatalf("stream errors = %v, want only %v", errs, readErr)
			}
			record := waitTerminalUsage(t, records)
			if !record.Failed || record.Detail.InputTokens != 100 {
				t.Fatalf("usage record failed=%v input=%d, want failed with 100 input tokens", record.Failed, record.Detail.InputTokens)
			}
		})
	}
}
