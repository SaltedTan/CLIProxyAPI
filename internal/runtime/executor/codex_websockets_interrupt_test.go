package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInterruptExecutionSessionRequiresActiveRead(t *testing.T) {
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			return
		}
		received <- payload
	}))
	defer upstream.Close()

	client, _, errDial := websocket.DefaultDialer.Dial("ws"+upstream.URL[len("http"):], nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()

	const sessionID = "interrupt-requires-active-read"
	executor := NewCodexWebsocketsExecutor(nil)
	defer executor.CloseExecutionSession(sessionID)
	sess := executor.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = client
	sess.authID = "auth"
	sess.wsURL = upstream.URL
	sess.connMu.Unlock()

	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	errIdle := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt)
	if !errors.Is(errIdle, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
		t.Fatalf("idle socket error = %v, want ErrNoActiveUpstreamWebsocket", errIdle)
	}
	select {
	case payload := <-received:
		t.Fatalf("idle socket was written: %s", payload)
	case <-time.After(200 * time.Millisecond):
	}

	readCh := sess.activate(client)
	defer sess.clearActive(client, readCh)
	if errActive := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt); errActive != nil {
		t.Fatal(errActive)
	}
	select {
	case payload := <-received:
		if !bytes.Equal(payload, interrupt) {
			t.Fatalf("active interrupt = %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active socket did not receive the interrupt")
	}
}

// A response.interrupt sent during an active turn passes the turn's payload rules as its
// final barrier: a wildcard Codex filter removes the extension field, while the frame keeps
// its type and response_id.
func TestInterruptExecutionSessionAppliesPayloadRules(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Error(errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Error(errRead)
			return
		}
		_, body, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Error(errRead)
			return
		}
		captured <- body
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: "codex"}},
		Params: []string{"private_extension"},
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
	sessionID := t.Name()
	executor := NewCodexWebsocketsExecutor(cfg)
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	defer executor.CloseExecutionSession(sessionID)
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "websockets": "true"}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
	result, errStream := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":[]}`)}, opts)
	if errStream != nil {
		t.Fatal(errStream)
	}

	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","private_extension":{"secret":true}}`)
	if errInterrupt := executor.InterruptExecutionSession(ctx, sessionID, interrupt); errInterrupt != nil {
		t.Fatal(errInterrupt)
	}
	select {
	case body := <-captured:
		if gjson.GetBytes(body, "private_extension").Exists() {
			t.Fatalf("interrupt bypassed payload rules: %s", body)
		}
		if gjson.GetBytes(body, "type").String() != "response.interrupt" || gjson.GetBytes(body, "response_id").String() != "r1" || gjson.GetBytes(body, "mode").String() != "discard_partial_items" {
			t.Fatalf("interrupt framing changed: %s", body)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	for range result.Chunks {
	}
}
