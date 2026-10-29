package upstream

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/execdiag"
	"github.com/erpc/erpc/failsafe"
)

// breakerScope names the failsafe entry a breaker belongs to, since breakers
// are per (upstream, method pattern, finality) rather than per upstream.
func breakerScope(e *upstreamExecutor) string {
	if e == nil || e.breaker == nil {
		return ""
	}
	scope := "method=" + e.method
	if len(e.finalities) > 0 {
		fs := make([]string, len(e.finalities))
		for i, f := range e.finalities {
			fs[i] = f.String()
		}
		scope += ";finality=" + strings.Join(fs, "|")
	}
	return scope
}

// withBreakerAdmission tags ctx with the breaker state the invocation was
// admitted under. Must run after TryAcquirePermit so a permit that moved the
// breaker Open → HalfOpen reports half_open.
func withBreakerAdmission(ctx context.Context, req *common.NormalizedRequest, e *upstreamExecutor, isHedge bool) context.Context {
	if !execdiag.Enabled() {
		return ctx
	}
	switch {
	case e.breaker == nil:
		return execdiag.WithBreakerAdmission(ctx, execdiag.BreakerNone, "")
	case !upstreamBreakerEligible(req, isHedge):
		return execdiag.WithBreakerAdmission(ctx, execdiag.BreakerBypassed, breakerScope(e))
	default:
		return execdiag.WithBreakerAdmission(ctx, e.breaker.State().String(), breakerScope(e))
	}
}

func recordBreakerRejection(ctx context.Context, req *common.NormalizedRequest, e *upstreamExecutor, isHedge bool, err error) {
	if !execdiag.Enabled() {
		return
	}
	execdiag.RecordLocal(ctx, req, "", "", isHedge, err, e.breaker.State().String(), breakerScope(e))
}

// wireBreakerDiagnostics chains a diagnostics emitter onto every breaker's
// existing transition hook (which keeps emitting the state-change metric).
func wireBreakerDiagnostics(u *Upstream) {
	for _, ex := range u.failsafeExecutors {
		b := ex.Breaker()
		if b == nil {
			continue
		}
		prev := b.OnTransition
		scope := breakerScope(ex)
		b.OnTransition = func(from, to failsafe.State, reason string) {
			if prev != nil {
				prev(from, to, reason)
			}
			execdiag.BreakerTransition(u.ProjectId, u.NetworkId(), u.Id(), u.VendorName(), scope, from.String(), to.String(), reason)
		}
	}
}
