package handlers

import (
	"context"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// upstreamCommitGate holds the non-streaming keepalive back until the request
// is committed to an upstream attempt. Before that point an admission refusal
// must still become a 429 with Retry-After, which the first keepalive byte on
// the wire would make impossible.
type upstreamCommitGate struct {
	once sync.Once
	done chan struct{}
}

func newUpstreamCommitGate() *upstreamCommitGate {
	return &upstreamCommitGate{done: make(chan struct{})}
}

func (g *upstreamCommitGate) open() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.done) })
}

func (g *upstreamCommitGate) opened() <-chan struct{} {
	return g.done
}

type upstreamCommitGateKey struct{}

// withUpstreamCommitGate attaches a fresh gate to ctx and has the conductor
// open it once the request can no longer be refused before upstream.
func withUpstreamCommitGate(ctx context.Context) context.Context {
	gate := newUpstreamCommitGate()
	ctx = context.WithValue(ctx, upstreamCommitGateKey{}, gate)
	return coreauth.WithUpstreamCommitNotice(ctx, gate.open)
}

func upstreamCommitGateFrom(ctx context.Context) *upstreamCommitGate {
	if ctx == nil {
		return nil
	}
	gate, _ := ctx.Value(upstreamCommitGateKey{}).(*upstreamCommitGate)
	return gate
}

// openUpstreamCommitGate releases the keepalive on paths that bypass the
// conductor, such as plugin executors, which admission never refuses. It goes
// through the same notice the conductor uses, so a nested call detached from
// the client request's notice cannot reach the client request's gate.
func openUpstreamCommitGate(ctx context.Context) {
	coreauth.NotifyUpstreamCommit(ctx)
}

// withoutUpstreamCommitNotice detaches a nested model call from the client
// request's keepalive: it is a request of its own, and its upstream attempt
// does not commit the client's response. Both the notice and the gate are
// shadowed, so neither the conductor nor a handler path can find them.
func withoutUpstreamCommitNotice(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, upstreamCommitGateKey{}, (*upstreamCommitGate)(nil))
	return coreauth.WithUpstreamCommitNotice(ctx, nil)
}

// UpstreamCommitted returns a channel that is closed once the request in ctx
// is committed to an upstream attempt, or at once when ctx carries no gate.
// Handlers that write anything to a response before their execution call
// returns, such as a bootstrap heartbeat, wait on it first so that an
// admission refusal can still be answered with a plain 429.
func UpstreamCommitted(ctx context.Context) <-chan struct{} {
	if gate := upstreamCommitGateFrom(ctx); gate != nil {
		return gate.opened()
	}
	return closedUpstreamCommit
}

var closedUpstreamCommit = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
