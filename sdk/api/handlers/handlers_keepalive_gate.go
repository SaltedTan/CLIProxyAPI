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
// conductor, such as plugin executors, which admission never refuses.
func openUpstreamCommitGate(ctx context.Context) {
	upstreamCommitGateFrom(ctx).open()
}

// withoutUpstreamCommitNotice detaches a nested model call from the client
// request's keepalive: it is a request of its own, and its upstream attempt
// does not commit the client's response.
func withoutUpstreamCommitNotice(ctx context.Context) context.Context {
	return coreauth.WithUpstreamCommitNotice(ctx, nil)
}
