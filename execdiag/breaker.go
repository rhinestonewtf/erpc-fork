package execdiag

import "github.com/rs/zerolog"

// BreakerTransition emits a circuit-breaker state change. Transitions are
// asynchronous to requests, so no request id is attached. They use their own
// limiter so request bursts cannot starve them.
func BreakerTransition(projectId, networkId, upstreamId, vendor, scope, from, to, reason string) {
	e := active.Load()
	if e == nil {
		return
	}
	if !e.trLim.AllowN(e.now(), 1) {
		e.trSuppressed.Add(1)
		return
	}
	level := zerolog.InfoLevel
	if to == "open" {
		level = zerolog.WarnLevel
	}
	ev := e.event(level).
		Str("event", "rpc.breaker.transition").
		Str("project", projectId).
		Str("network", networkId).
		Str("upstream_id", upstreamId).
		Str("vendor", safeVendor(vendor))
	if scope != "" {
		ev = ev.Str("breaker_scope", scope)
	}
	ev.Str("from", from).
		Str("to", to).
		Str("reason", SafeToken(reason)).
		Msg("circuit breaker transition")
}
