package execdiag

import (
	"sort"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// requestSnapshot holds every request-scoped scalar needed for emission, so a
// deferred (late-invocation) emission never touches the request itself.
type requestSnapshot struct {
	hasError                                 bool
	attemptBudget                            int
	project, network, method, finality, user string
	opID, parentID, leaderID                 string
	trace                                    traceIDs
	path                                     string
	finalClass, finalCode, finalCause        string
	startedAt                                time.Time
	duration                                 time.Duration
	logicalAttempts, networkAttempts         int
	retries, hedges                          int
	consensusSlots, subrequests              int
	wonUpstream                              string
}

type classified struct {
	rec     common.InvocationRecord
	no      int
	class   string
	outcome string
	notable bool
	won     bool
}

// priority orders attempts for the per-request cap: won, notable, rest.
func (c classified) priority() int {
	switch {
	case c.won:
		return 0
	case c.notable:
		return 1
	default:
		return 2
	}
}

// Unknown vendor names are synthesized from endpoint hosts upstream.
func safeVendor(v string) string {
	if strings.HasPrefix(v, "unknown-") {
		return "unknown"
	}
	return v
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func classifyRecords(records []common.InvocationRecord, wonUpstream string) []classified {
	sorted := make([]common.InvocationRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt.Before(sorted[j].StartedAt) })

	out := make([]classified, len(sorted))
	wonIdx := -1
	for i, r := range sorted {
		class, outcome, notable := Classify(r.Err, r.Dispatch, true)
		out[i] = classified{rec: r, no: i + 1, class: class, outcome: outcome, notable: notable}
		if wonUpstream != "" && r.UpstreamId == wonUpstream && (class == ClassResult || class == ClassApplicationError) {
			wonIdx = i
		}
	}
	if wonIdx >= 0 {
		out[wonIdx].won = true
	}
	return out
}

// selectAttempts keeps at most max records — won first, then notable, then
// the rest — and returns them in start order.
func selectAttempts(all []classified, max int) []classified {
	if len(all) <= max {
		return all
	}
	idx := make([]int, len(all))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return all[idx[a]].priority() < all[idx[b]].priority() })
	idx = idx[:max]
	sort.Ints(idx)
	out := make([]classified, len(idx))
	for i, j := range idx {
		out[i] = all[j]
	}
	return out
}

func (e *emitter) writeRequest(s requestSnapshot, records []common.InvocationRecord, inflight int) {
	all := classifyRecords(records, s.wonUpstream)
	selected := selectAttempts(all, min(e.maxAttempts, s.attemptBudget))
	// Computed here, not at Finish, because late invocations can dispatch.
	outcome := requestOutcome(s.hasError, s.finalClass, s.path, records)

	dispatches, unknown := 0, 0
	counts := map[string]int{}
	for _, c := range all {
		switch c.rec.Dispatch {
		case common.DispatchAttempted:
			dispatches++
		case common.DispatchUnknown:
			unknown++
		}
		counts[c.class]++
	}
	classes := make([]string, 0, len(counts))
	for k := range counts {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	cc := zerolog.Dict()
	for _, k := range classes {
		cc.Int(k, counts[k])
	}

	level := zerolog.InfoLevel
	if outcome == "failed" || outcome == "rejected" {
		level = zerolog.WarnLevel
	}
	omitted := len(all) - len(selected)

	ev := e.log.WithLevel(level).
		Str("event", "rpc.request.completed").
		Str("project", s.project).
		Str("operational_request_id", s.opID)
	ev = withTrace(ev, s.trace, true)
	if s.parentID != "" {
		ev = ev.Str("parent_request_id", s.parentID)
	}
	if s.leaderID != "" {
		ev = ev.Str("leader_request_id", s.leaderID)
	}
	ev = ev.Str("network", s.network).
		Str("rpc_method", s.method).
		Str("finality", s.finality)
	if s.user != "" {
		ev = ev.Str("user_id", s.user)
	}
	ev = ev.Str("path", s.path).
		Str("outcome", outcome)
	if s.finalCode != "" {
		ev = ev.Str("final_class", s.finalClass).
			Str("final_error_code", s.finalCode).
			Str("final_cause", s.finalCause)
	}
	ev = ev.Float64("duration_ms", ms(s.duration)).
		Int("invocations", len(all)).
		Int("dispatches", dispatches)
	if unknown > 0 {
		ev = ev.Int("dispatch_unknown", unknown)
	}
	ev = ev.Int("logical_attempts", s.logicalAttempts).
		Int("network_attempts", s.networkAttempts).
		Int("retries", s.retries).
		Int("hedges", s.hedges)
	if s.consensusSlots > 0 {
		ev = ev.Int("consensus_slots", s.consensusSlots)
	}
	if s.subrequests > 0 {
		ev = ev.Int("subrequests", s.subrequests)
	}
	ev.Dict("class_counts", cc).
		Int("attempts_emitted", len(selected)).
		Int("attempts_omitted", omitted).
		Int("invocations_inflight", inflight).
		Bool("evidence_complete", omitted == 0 && inflight == 0).
		Msg("request completed with notable upstream execution")

	for _, c := range selected {
		e.writeAttempt(s, c)
	}
}

func (e *emitter) writeAttempt(s requestSnapshot, c classified) {
	level := zerolog.InfoLevel
	if c.notable {
		level = zerolog.WarnLevel
	}
	r := c.rec
	ev := e.log.WithLevel(level).
		Str("event", "rpc.attempt.completed").
		Str("project", s.project).
		Str("operational_request_id", s.opID)
	ev = withTrace(ev, s.trace, false)
	ev = ev.Int("attempt_no", c.no).
		Str("upstream_id", r.UpstreamId).
		Str("vendor", safeVendor(r.VendorName)).
		Str("network", s.network).
		Str("rpc_method", s.method).
		Str("finality", s.finality).
		Str("dispatch", r.Dispatch).
		Str("class", c.class).
		Str("outcome", c.outcome)
	if c.class != ClassResult {
		ev = ev.Str("cause", Cause(r.Err))
		if st := HTTPStatus(r.Err); st > 0 {
			ev = ev.Int("http_status", st)
		}
		if code, ok := JSONRPCCode(r.Err); ok {
			ev = ev.Int("jsonrpc_code", code)
		}
	}
	ev = ev.Str("selection_reason", r.SelectionReason).
		Bool("hedge", r.IsHedge).
		Bool("retry", r.IsRetry).
		Int("network_attempt", r.NetworkAttempt).
		Str("breaker_state", r.BreakerState)
	if r.BreakerScope != "" {
		ev = ev.Str("breaker_scope", r.BreakerScope)
	}
	ev.Bool("won", c.won).
		Float64("started_offset_ms", startedOffset(r.StartedAt, s)).
		Float64("duration_ms", ms(r.Duration)).
		Msg("upstream invocation completed")
}

func startedOffset(t time.Time, s requestSnapshot) float64 {
	if s.startedAt.IsZero() || t.Before(s.startedAt) {
		return 0
	}
	return ms(t.Sub(s.startedAt))
}

func withTrace(ev *zerolog.Event, t traceIDs, withSpans bool) *zerolog.Event {
	if t.traceID == "" {
		return ev
	}
	ev = ev.Str("trace_id", t.traceID)
	if !withSpans {
		return ev
	}
	if t.spanID != "" {
		ev = ev.Str("span_id", t.spanID)
	}
	if t.parentSpanID != "" {
		ev = ev.Str("parent_span_id", t.parentSpanID)
	}
	return ev
}
