package common

import (
	"context"
	"sync/atomic"
)

// DispatchProbe lets a transport client report whether it reached network
// I/O for one upstream invocation. Clients that never mark it leave the
// state "unknown" rather than implying no dispatch.
type DispatchProbe struct {
	observed  atomic.Bool
	attempted atomic.Bool
}

type dispatchProbeCtxKey struct{}

func WithDispatchProbe(ctx context.Context, p *DispatchProbe) context.Context {
	return context.WithValue(ctx, dispatchProbeCtxKey{}, p)
}

func dispatchProbeFrom(ctx context.Context) *DispatchProbe {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(dispatchProbeCtxKey{}).(*DispatchProbe)
	return p
}

// MarkDispatchObserved records that a reporting client handled the call.
func MarkDispatchObserved(ctx context.Context) {
	if p := dispatchProbeFrom(ctx); p != nil {
		p.observed.Store(true)
	}
}

// MarkDispatchAttempted records that the client is about to perform network I/O.
func MarkDispatchAttempted(ctx context.Context) {
	if p := dispatchProbeFrom(ctx); p != nil {
		p.observed.Store(true)
		p.attempted.Store(true)
	}
}

func (p *DispatchProbe) State() string {
	switch {
	case p == nil || !p.observed.Load():
		return DispatchUnknown
	case p.attempted.Load():
		return DispatchAttempted
	default:
		return DispatchNone
	}
}
