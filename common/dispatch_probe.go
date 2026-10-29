package common

import (
	"context"
	"sync"
	"sync/atomic"
)

// DispatchProbe lets a transport client report whether it reached network
// I/O for one upstream invocation. Clients that never mark it leave the
// state "unknown" rather than implying no dispatch.
type DispatchProbe struct {
	observed  atomic.Bool
	attempted atomic.Bool

	// A batched call is pending from the moment a batch takes it until the
	// batch sends or drops it, which can happen after the caller stopped
	// waiting. Settle defers its callback until then.
	mu       sync.Mutex
	pending  bool
	onSettle func(state string)
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
		p.resolve()
	}
}

// MarkDispatchPending records that a batch took the call; the batch must
// later call MarkDispatchAttempted or MarkDispatchDropped.
func MarkDispatchPending(ctx context.Context) {
	if p := dispatchProbeFrom(ctx); p != nil {
		p.mu.Lock()
		p.pending = true
		p.mu.Unlock()
	}
}

// MarkDispatchDropped records that a pending call was removed before network I/O.
func MarkDispatchDropped(ctx context.Context) {
	if p := dispatchProbeFrom(ctx); p != nil {
		p.resolve()
	}
}

func (p *DispatchProbe) resolve() {
	p.mu.Lock()
	p.pending = false
	fn := p.onSettle
	p.onSettle = nil
	p.mu.Unlock()
	if fn != nil {
		fn(p.State())
	}
}

// Settle calls fn with the final state: now, or once a pending batch decides.
func (p *DispatchProbe) Settle(fn func(state string)) {
	if p != nil {
		p.mu.Lock()
		if p.pending {
			p.onSettle = fn
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
	fn(p.State())
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
