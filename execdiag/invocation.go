package execdiag

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
)

// Breaker admission states recorded on an invocation.
const (
	BreakerNone     = "none"
	BreakerBypassed = "bypassed"
)

// UpstreamCall wraps one Upstream.Forward call made by the network layer. If
// that call fails before any invocation was recorded (eligibility skip,
// upstream rate-limit budget, method parse, ...), End records it as a local
// rejection so no early-return path is invisible.
type UpstreamCall struct {
	req        *common.NormalizedRequest
	upstreamId string
	vendor     string
	isHedge    bool
	reason     string
	isRetry    bool
	netAttempt int
	start      time.Time
	recorded   atomic.Bool

	// okByResp maps each successful invocation's response to its ledger
	// index, so a post-forward rejection is charged to the leg whose
	// response was actually returned, not to whichever finished last.
	okMu     sync.Mutex
	okByResp map[*common.NormalizedResponse]int
}

type upstreamCallKey struct{}

func ledgerFor(req *common.NormalizedRequest) *common.ExecState {
	if active.Load() == nil || req == nil {
		return nil
	}
	st := req.ExecState()
	if !st.InvocationLedgerEnabled() {
		return nil
	}
	return st
}

func upstreamCallFrom(ctx context.Context, req *common.NormalizedRequest) *UpstreamCall {
	uc, _ := ctx.Value(upstreamCallKey{}).(*UpstreamCall)
	if uc == nil || uc.req != req {
		return nil
	}
	return uc
}

func BeginUpstreamCall(ctx context.Context, req *common.NormalizedRequest, u common.Upstream, isHedgeAttempt bool) (context.Context, *UpstreamCall) {
	st := ledgerFor(req)
	if st == nil || u == nil {
		return ctx, nil
	}
	snap := st.Snapshot()
	uc := &UpstreamCall{
		req:        req,
		upstreamId: u.Id(),
		vendor:     u.VendorName(),
		isHedge:    isHedgeAttempt,
		reason:     SelectionReason(ctx, isHedgeAttempt, snap.Retries),
		isRetry:    snap.Retries > 0,
		netAttempt: snap.NetworkAttempts,
		start:      time.Now(),
	}
	return context.WithValue(ctx, upstreamCallKey{}, uc), uc
}

// PostForward attributes a post-forward hook's rejection of a successful
// response to the invocation that produced it.
func (uc *UpstreamCall) PostForward(resp *common.NormalizedResponse, forwardErr, postErr error) {
	if uc == nil || forwardErr != nil || postErr == nil {
		return
	}
	uc.okMu.Lock()
	idx, ok := uc.okByResp[resp]
	uc.okMu.Unlock()
	if ok {
		uc.req.ExecState().AmendInvocationError(idx, postErr)
		return
	}
	uc.End(postErr)
}

func (uc *UpstreamCall) recordOk(resp *common.NormalizedResponse, idx int) {
	uc.okMu.Lock()
	defer uc.okMu.Unlock()
	if uc.okByResp == nil {
		uc.okByResp = make(map[*common.NormalizedResponse]int)
	}
	uc.okByResp[resp] = idx
}

func (uc *UpstreamCall) End(err error) {
	if uc == nil || err == nil || uc.recorded.Load() {
		return
	}
	uc.req.ExecState().RecordInvocation(common.InvocationRecord{
		UpstreamId:      uc.upstreamId,
		VendorName:      uc.vendor,
		StartedAt:       uc.start,
		Duration:        time.Since(uc.start),
		Dispatch:        common.DispatchNone,
		Err:             err,
		IsHedge:         uc.isHedge,
		IsRetry:         uc.isRetry,
		SelectionReason: uc.reason,
		NetworkAttempt:  uc.netAttempt,
		BreakerState:    BreakerNone,
	})
}

// Invocation is one admitted call into an upstream's transport client.
type Invocation struct {
	st         *common.ExecState
	probe      *common.DispatchProbe
	upstreamId string
	vendor     string
	isHedge    bool
	reason     string
	isRetry    bool
	netAttempt int
	breaker    breakerAdmission
	start      time.Time
	call       *UpstreamCall
}

// BeginInvocation installs a dispatch probe for the transport client and
// marks the invocation in flight. Call End exactly once with the outcome.
func BeginInvocation(ctx context.Context, req *common.NormalizedRequest, u common.Upstream, isHedge bool) (context.Context, *Invocation) {
	st := ledgerFor(req)
	if st == nil || u == nil {
		return ctx, nil
	}
	uc := upstreamCallFrom(ctx, req)
	if uc != nil {
		uc.recorded.Store(true)
		isHedge = isHedge || uc.isHedge
	}
	snap := st.Snapshot()
	inv := &Invocation{
		call:       uc,
		st:         st,
		probe:      &common.DispatchProbe{},
		upstreamId: u.Id(),
		vendor:     u.VendorName(),
		isHedge:    isHedge,
		reason:     SelectionReason(ctx, isHedge, snap.Retries),
		isRetry:    snap.Retries > 0,
		netAttempt: snap.NetworkAttempts,
		breaker:    breakerAdmission{state: BreakerNone},
		start:      time.Now(),
	}
	if a, ok := ctx.Value(breakerAdmissionKey{}).(breakerAdmission); ok {
		inv.breaker = a
	}
	st.InvocationStarted()
	return common.WithDispatchProbe(ctx, inv.probe), inv
}

// End records the invocation's outcome. A batched call the batch has not yet
// sent or dropped stays in flight until it does, so its dispatch is final.
func (inv *Invocation) End(resp *common.NormalizedResponse, err error) {
	if inv == nil {
		return
	}
	rec := common.InvocationRecord{
		UpstreamId:      inv.upstreamId,
		VendorName:      inv.vendor,
		StartedAt:       inv.start,
		Duration:        time.Since(inv.start),
		Err:             err,
		IsHedge:         inv.isHedge,
		IsRetry:         inv.isRetry,
		SelectionReason: inv.reason,
		NetworkAttempt:  inv.netAttempt,
		BreakerState:    inv.breaker.state,
		BreakerScope:    inv.breaker.scope,
	}
	inv.probe.Settle(func(state string) {
		rec.Dispatch = state
		idx := inv.st.CompleteInvocation(rec)
		if err == nil && resp != nil && idx >= 0 && inv.call != nil {
			inv.call.recordOk(resp, idx)
		}
	})
}

// RecordLocal records an invocation that was refused before reaching the
// transport client (open breaker, block-availability skip). Identity falls
// back to the enclosing UpstreamCall when the caller does not know it.
func RecordLocal(ctx context.Context, req *common.NormalizedRequest, upstreamId, vendor string, isHedge bool, err error, breakerState, breakerScope string) {
	st := ledgerFor(req)
	if st == nil {
		return
	}
	if uc := upstreamCallFrom(ctx, req); uc != nil {
		uc.recorded.Store(true)
		if upstreamId == "" {
			upstreamId, vendor = uc.upstreamId, uc.vendor
		}
		isHedge = isHedge || uc.isHedge
	}
	if breakerState == "" {
		breakerState = BreakerNone
	}
	snap := st.Snapshot()
	st.RecordInvocation(common.InvocationRecord{
		UpstreamId:      upstreamId,
		VendorName:      vendor,
		StartedAt:       time.Now(),
		Dispatch:        common.DispatchNone,
		Err:             err,
		IsHedge:         isHedge,
		IsRetry:         snap.Retries > 0,
		SelectionReason: SelectionReason(ctx, isHedge, snap.Retries),
		NetworkAttempt:  snap.NetworkAttempts,
		BreakerState:    breakerState,
		BreakerScope:    breakerScope,
	})
}

type breakerAdmission struct {
	state string
	scope string
}

type breakerAdmissionKey struct{}

// WithBreakerAdmission records the breaker state an invocation was admitted
// under, for the invocation started from ctx.
func WithBreakerAdmission(ctx context.Context, state, scope string) context.Context {
	if active.Load() == nil {
		return ctx
	}
	return context.WithValue(ctx, breakerAdmissionKey{}, breakerAdmission{state: state, scope: scope})
}

// SelectionReason mirrors the upstream attempt log's selection reason.
func SelectionReason(ctx context.Context, isHedge bool, retries int) string {
	switch {
	case common.IsConsensusSlot(ctx):
		return string(common.SelectionReasonConsensusSlot)
	case isHedge:
		return string(common.SelectionReasonHedge)
	case common.IsSweepIteration(ctx):
		return string(common.SelectionReasonSweep)
	case retries > 0:
		return string(common.SelectionReasonRetry)
	default:
		return string(common.SelectionReasonPrimary)
	}
}
