// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets.
//
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		cliproxyauth.NoteRoutingTransport(ctx, cliproxyauth.RoutingTransportWebsocket)
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	cliproxyauth.NoteRoutingTransport(ctx, cliproxyauth.RoutingTransportHTTP)
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		cliproxyauth.NoteRoutingTransport(ctx, cliproxyauth.RoutingTransportWebsocket)
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	cliproxyauth.NoteRoutingTransport(ctx, cliproxyauth.RoutingTransportHTTP)
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

// SupportsApplyPatch requires both selectable transports to support the tool.
func (e *CodexAutoExecutor) SupportsApplyPatch() bool {
	return e != nil && e.httpExec != nil && e.wsExec != nil && e.httpExec.SupportsApplyPatch() && e.wsExec.SupportsApplyPatch()
}

// InterruptExecutionSession forwards a response.interrupt control frame on the
// current upstream socket. It does not dial, replay, or select another credential.
func (e *CodexAutoExecutor) InterruptExecutionSession(ctx context.Context, sessionID string, payload []byte) error {
	if e == nil || e.wsExec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	return e.wsExec.InterruptExecutionSession(ctx, sessionID, payload)
}

// interruptPayloadRules returns the final payload barrier for response.interrupt frames
// sent during the turn of req: user payload rules, matched with the turn's model, protocol
// and request context, applied once to the interrupt itself. Like response.steer, the frame
// does not inherit response.create defaults and keeps its type as transport framing.
func (e *CodexWebsocketsExecutor) interruptPayloadRules(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) func([]byte) []byte {
	var cfg *config.Config
	if e != nil && e.CodexExecutor != nil {
		cfg = e.cfg
	}
	model := req.Model
	baseModel := thinking.ParseSuffix(model).ModelName
	return func(payload []byte) []byte {
		interruptReq := cliproxyexecutor.Request{Model: model, Payload: payload}
		payload = helps.NewPayloadFinalizer(cfg, "codex-websockets", baseModel, "codex", "", payload, interruptReq, opts)(payload)
		payload, _ = sjson.SetBytes(payload, "type", "response.interrupt")
		return payload
	}
}

// InterruptExecutionSession writes the interrupt payload to the session socket captured
// for this execution, after the active turn's payload rules. response.create defaults
// must not rewrite response_id, mode, or extension fields.
func (e *CodexWebsocketsExecutor) InterruptExecutionSession(ctx context.Context, sessionID string, payload []byte) error {
	if e == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	sess := store.sessions[sessionID]
	store.mu.Unlock()
	if sess == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sess.connMu.Lock()
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sess.connMu.Unlock()
	if conn == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	// A retained socket from an earlier turn is not the current upstream.
	// HTTP turns must fall through to local cancellation instead.
	readCh, _ := sess.activeForConn(conn)
	if readCh == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if !cliproxyexecutor.WebsocketAuthEnabled(ctx, authID) {
		return fmt.Errorf("websocket credential is no longer enabled")
	}
	// The turn may have ended, and another started, since readCh was captured: its rules
	// are looked up and applied under the write lock, and the interrupt is rejected rather
	// than forwarded unfiltered when the turn changed.
	if errWrite := sess.writeTurnInterrupt(conn, readCh, payload); errWrite != nil {
		return errWrite
	}
	log.Infof("codex websockets: request forwarded session=%s auth=%s url=%s event=response.interrupt", sessionID, authID, wsURL)
	return nil
}
