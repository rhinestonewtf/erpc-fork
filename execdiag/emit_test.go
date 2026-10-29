package execdiag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func cfgOf(rate float64, burst, maxAttempts int) *common.ExecutionDiagnosticsConfig {
	return &common.ExecutionDiagnosticsConfig{
		Enabled:                    util.BoolPtr(true),
		MaxEventsPerSecond:         rate,
		Burst:                      burst,
		MaxAttemptEventsPerRequest: maxAttempts,
	}
}

func newReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":"canaryidEEE","method":"eth_getBalance","params":["canaryparamDDD","latest"]}`))
}

func parseLines(t *testing.T, buf *CaptureBuffer) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, l := range buf.Lines() {
		m := map[string]interface{}{}
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		out = append(out, m)
	}
	return out
}

func byEvent(lines []map[string]interface{}, event string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, l := range lines {
		if l["event"] == event {
			out = append(out, l)
		}
	}
	return out
}

// assertEventContract fails on any key outside the documented allowlist.
func assertEventContract(t *testing.T, lines []map[string]interface{}) {
	t.Helper()
	for _, l := range lines {
		assert.Empty(t, UnexpectedEventKeys(l), "unexpected diagnostics keys in %v", l)
		assert.Equal(t, componentName, l["component"])
	}
}

func record(st *common.ExecState, at time.Time, id, dispatch string, err error) {
	st.RecordInvocation(common.InvocationRecord{
		UpstreamId:      id,
		VendorName:      "vendor",
		StartedAt:       at,
		Duration:        time.Millisecond,
		Dispatch:        dispatch,
		Err:             err,
		SelectionReason: "primary",
		BreakerState:    BreakerNone,
	})
}

func okResp(req *common.NormalizedRequest, upstreamId string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithRequest(req).SetUpstream(common.NewFakeUpstream(upstreamId))
}

func breakerErr() error {
	return common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, failsafe.ErrCircuitOpen, nil)
}

func serverErr(status int) error {
	return upReq(common.NewErrEndpointServerSideException(nil, map[string]interface{}{"statusCode": status}, status))
}

func TestFinish_RecoveredBreakerRejection(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	require.NotNil(t, d)
	require.True(t, req.ExecState().InvocationLedgerEnabled())

	now := time.Now()
	st := req.ExecState()
	st.RecordInvocation(common.InvocationRecord{
		UpstreamId: "rpc1", StartedAt: now, Dispatch: common.DispatchNone, Err: breakerErr(),
		SelectionReason: "primary", BreakerState: "open", BreakerScope: "method=*",
	})
	record(st, now.Add(time.Millisecond), "rpc2", common.DispatchAttempted, nil)

	d.Finish(ctx, okResp(req, "rpc2"), nil)

	lines := parseLines(t, buf)
	assertEventContract(t, lines)
	sum := byEvent(lines, "rpc.request.completed")
	require.Len(t, sum, 1)
	assert.Equal(t, "degraded", sum[0]["outcome"])
	assert.Equal(t, "info", sum[0]["level"])
	assert.Equal(t, "forward", sum[0]["path"])
	assert.Equal(t, "eth_getBalance", sum[0]["rpc_method"])
	assert.Equal(t, float64(2), sum[0]["invocations"])
	assert.Equal(t, float64(1), sum[0]["dispatches"])
	assert.Equal(t, true, sum[0]["evidence_complete"])
	assert.Equal(t, map[string]interface{}{"rejected": float64(1), "result": float64(1)}, sum[0]["class_counts"])
	assert.Len(t, sum[0]["operational_request_id"], 32)
	assert.NotContains(t, sum[0], "final_class")

	att := byEvent(lines, "rpc.attempt.completed")
	require.Len(t, att, 2)
	assert.Equal(t, "rpc1", att[0]["upstream_id"])
	assert.Equal(t, "rejected", att[0]["class"])
	assert.Equal(t, "none", att[0]["dispatch"])
	assert.Equal(t, "open", att[0]["breaker_state"])
	assert.Equal(t, "ErrFailsafeCircuitBreakerOpen", att[0]["cause"])
	assert.Equal(t, "warn", att[0]["level"])
	assert.Equal(t, false, att[0]["won"])
	assert.Equal(t, "rpc2", att[1]["upstream_id"])
	assert.Equal(t, "result", att[1]["class"])
	assert.Equal(t, true, att[1]["won"])
	assert.Equal(t, "info", att[1]["level"])
	assert.NotContains(t, att[1], "cause")
	for _, a := range att {
		assert.Equal(t, sum[0]["operational_request_id"], a["operational_request_id"])
	}

	for _, canary := range []string{"canaryidEEE", "canaryparamDDD"} {
		assert.NotContains(t, buf.String(), canary)
	}
}

func TestFinish_BenignOnlyEmitsNothing(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	jre := common.NewErrJsonRpcExceptionInternal(3, common.JsonRpcErrorEvmReverted, "reverted", nil, nil)
	revert := upReq(common.NewErrEndpointExecutionException(jre))

	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	now := time.Now()
	record(req.ExecState(), now, "rpc1", common.DispatchAttempted, upReq(common.NewErrEndpointRequestCanceled(context.Canceled)))
	record(req.ExecState(), now.Add(time.Millisecond), "rpc2", common.DispatchAttempted, revert)
	d.Finish(ctx, nil, revert)

	req2 := newReq()
	ctx2, d2 := BeginRequest(context.Background(), req2, "main", "evm:1")
	record(req2.ExecState(), now, "rpc1", common.DispatchNone, common.NewErrUpstreamRequestSkipped(errors.New("x"), "rpc1"))
	record(req2.ExecState(), now.Add(time.Millisecond), "rpc2", common.DispatchAttempted, nil)
	d2.Finish(ctx2, okResp(req2, "rpc2"), nil)

	assert.Empty(t, buf.Lines())
}

func TestFinish_RequestOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		records func(st *common.ExecState)
		err     error
		outcome string
		level   string
	}{
		{"RejectedWithoutDispatch", func(st *common.ExecState) {
			record(st, time.Now(), "rpc1", common.DispatchNone, breakerErr())
		}, exhausted(breakerErr()), "rejected", "warn"},
		{"FailedAfterDispatch", func(st *common.ExecState) {
			record(st, time.Now(), "rpc1", common.DispatchAttempted, serverErr(503))
		}, exhausted(serverErr(503)), "failed", "warn"},
		{"UnknownDispatchIsNotRejected", func(st *common.ExecState) {
			record(st, time.Now(), "rpc1", common.DispatchUnknown, serverErr(503))
		}, exhausted(serverErr(503)), "failed", "warn"},
		{"Cancelled", func(st *common.ExecState) {}, common.NewErrEndpointRequestCanceled(context.Canceled), "cancelled", "info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := ConfigureForTest(t, nil)
			req := newReq()
			ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
			tc.records(req.ExecState())
			d.Finish(ctx, nil, tc.err)
			sum := byEvent(parseLines(t, buf), "rpc.request.completed")
			require.Len(t, sum, 1)
			assert.Equal(t, tc.outcome, sum[0]["outcome"])
			assert.Equal(t, tc.level, sum[0]["level"])
		})
	}
}

func TestBudget_RequestsTransitionsAndSuppressionReport(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	buf := configureForTest(t, cfgOf(1, 3, 2), clock.Now)
	e := active.Load()

	notable := func() {
		req := newReq()
		ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
		record(req.ExecState(), time.Now(), "rpc1", common.DispatchAttempted, serverErr(500))
		record(req.ExecState(), time.Now().Add(time.Millisecond), "rpc2", common.DispatchAttempted, serverErr(500))
		record(req.ExecState(), time.Now().Add(2*time.Millisecond), "rpc3", common.DispatchAttempted, serverErr(500))
		d.Finish(ctx, nil, exhausted(serverErr(500)))
	}

	// cost = 1 + min(3, 2) = 3 lines: the first request drains the burst.
	notable()
	notable()
	notable()
	assert.Len(t, byEvent(parseLines(t, buf), "rpc.request.completed"), 1)
	assert.Len(t, byEvent(parseLines(t, buf), "rpc.attempt.completed"), 2)

	// Transitions have their own bucket.
	for i := 0; i < 4; i++ {
		BreakerTransition("main", "evm:1", "rpc1", "vendor", "method=*", "closed", "open", "failure_threshold")
	}
	tr := byEvent(parseLines(t, buf), "rpc.breaker.transition")
	assert.Len(t, tr, 3)
	assert.Equal(t, "warn", tr[0]["level"])
	assert.NotContains(t, tr[0], "operational_request_id")

	clock.Advance(3 * time.Second)
	notable()
	assert.Len(t, byEvent(parseLines(t, buf), "rpc.request.completed"), 2)

	e.flushSuppressed()
	sup := byEvent(parseLines(t, buf), "rpc.diagnostics.suppressed")
	require.Len(t, sup, 1)
	assert.Equal(t, float64(2), sup[0]["requests_suppressed"])
	assert.Equal(t, float64(6), sup[0]["request_events_suppressed"])
	assert.Equal(t, float64(1), sup[0]["transitions_suppressed"])
	assert.Equal(t, float64(3000), sup[0]["window_ms"])

	// Nothing suppressed since: no second report.
	e.flushSuppressed()
	assert.Len(t, byEvent(parseLines(t, buf), "rpc.diagnostics.suppressed"), 1)
	assertEventContract(t, parseLines(t, buf))
}

func TestAttemptCap_SelectsWonThenNotable(t *testing.T) {
	buf := ConfigureForTest(t, cfgOf(10, 100, 2))
	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	st := req.ExecState()
	now := time.Now()
	record(st, now, "rpc1", common.DispatchNone, common.NewErrUpstreamRequestSkipped(errors.New("x"), "rpc1"))
	record(st, now.Add(1*time.Millisecond), "rpc2", common.DispatchAttempted, upReq(common.NewErrEndpointTransportFailure(testURL, errors.New("refused"))))
	record(st, now.Add(2*time.Millisecond), "rpc3", common.DispatchAttempted, upReq(common.NewErrEndpointRequestCanceled(context.Canceled)))
	record(st, now.Add(3*time.Millisecond), "rpc4", common.DispatchAttempted, upReq(common.NewErrEndpointMissingData(nil, nil)))
	record(st, now.Add(4*time.Millisecond), "rpc5", common.DispatchAttempted, nil)
	d.Finish(ctx, okResp(req, "rpc5"), nil)

	lines := parseLines(t, buf)
	sum := byEvent(lines, "rpc.request.completed")
	require.Len(t, sum, 1)
	assert.Equal(t, float64(2), sum[0]["attempts_emitted"])
	assert.Equal(t, float64(3), sum[0]["attempts_omitted"])
	assert.Equal(t, false, sum[0]["evidence_complete"])
	assert.Equal(t, float64(5), sum[0]["invocations"])

	att := byEvent(lines, "rpc.attempt.completed")
	require.Len(t, att, 2)
	assert.Equal(t, "rpc2", att[0]["upstream_id"])
	assert.Equal(t, float64(2), att[0]["attempt_no"])
	assert.Equal(t, "rpc5", att[1]["upstream_id"])
	assert.Equal(t, float64(5), att[1]["attempt_no"])
	assert.Equal(t, true, att[1]["won"])
}

func TestLateInvocations(t *testing.T) {
	prev := lateInvocationGrace
	lateInvocationGrace = 150 * time.Millisecond
	t.Cleanup(func() { lateInvocationGrace = prev })

	t.Run("DrainsWithinGrace", func(t *testing.T) {
		buf := ConfigureForTest(t, nil)
		req := newReq()
		ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
		up := common.NewFakeUpstream("rpc1")
		_, inv := BeginInvocation(ctx, req, up, true)
		record(req.ExecState(), time.Now(), "rpc2", common.DispatchAttempted, serverErr(503))
		d.Finish(ctx, nil, exhausted(serverErr(503)))
		assert.Empty(t, buf.Lines(), "emission must wait for the in-flight invocation")
		inv.End(upReq(common.NewErrEndpointRequestCanceled(context.Canceled)))

		require.Eventually(t, func() bool { return len(buf.Lines()) == 3 }, time.Second, 10*time.Millisecond)
		lines := parseLines(t, buf)
		sum := byEvent(lines, "rpc.request.completed")[0]
		assert.Equal(t, float64(0), sum["invocations_inflight"])
		assert.Equal(t, true, sum["evidence_complete"])
		att := byEvent(lines, "rpc.attempt.completed")
		assert.Equal(t, "cancelled", att[0]["class"])
		assert.Equal(t, true, att[0]["hedge"])
		assert.Equal(t, "hedge", att[0]["selection_reason"])
		assert.Equal(t, "unknown", att[0]["dispatch"], "no client marked the probe")
	})

	t.Run("GraceExpires", func(t *testing.T) {
		buf := ConfigureForTest(t, nil)
		req := newReq()
		ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
		_, _ = BeginInvocation(ctx, req, common.NewFakeUpstream("rpc1"), true)
		record(req.ExecState(), time.Now(), "rpc2", common.DispatchAttempted, serverErr(503))
		d.Finish(ctx, nil, exhausted(serverErr(503)))

		require.Eventually(t, func() bool { return len(buf.Lines()) == 2 }, 2*time.Second, 10*time.Millisecond)
		sum := byEvent(parseLines(t, buf), "rpc.request.completed")[0]
		assert.Equal(t, float64(1), sum["invocations_inflight"])
		assert.Equal(t, false, sum["evidence_complete"])
	})
}

func TestInvocation_DispatchProbeAndBreakerAdmission(t *testing.T) {
	ConfigureForTest(t, nil)
	req := newReq()
	ctx, _ := BeginRequest(context.Background(), req, "main", "evm:1")
	ctx = WithBreakerAdmission(ctx, "half_open", "method=*")
	ictx, inv := BeginInvocation(ctx, req, common.NewFakeUpstream("rpc1"), false)
	assert.Equal(t, 1, req.ExecState().InvocationsInflight())
	common.MarkDispatchAttempted(ictx)
	inv.End(nil)

	recs := req.ExecState().Invocations()
	require.Len(t, recs, 1)
	assert.Equal(t, common.DispatchAttempted, recs[0].Dispatch)
	assert.Equal(t, "half_open", recs[0].BreakerState)
	assert.Equal(t, "method=*", recs[0].BreakerScope)
	assert.Equal(t, 0, req.ExecState().InvocationsInflight())
}

func TestUpstreamCall_RecordsEarlyReturnOnce(t *testing.T) {
	ConfigureForTest(t, nil)
	req := newReq()
	ctx, _ := BeginRequest(context.Background(), req, "main", "evm:1")
	up := common.NewFakeUpstream("rpc1")

	// Early return before any invocation: recorded as a local rejection.
	_, uc := BeginUpstreamCall(ctx, req, up, false)
	uc.End(common.NewErrUpstreamRateLimitRuleExceeded("rpc1", "b", "r"))

	// Breaker rejection inside the call: identity comes from the call.
	cctx, uc2 := BeginUpstreamCall(ctx, req, up, true)
	RecordLocal(cctx, req, "", "", false, breakerErr(), "open", "method=*")
	uc2.End(breakerErr())

	recs := req.ExecState().Invocations()
	require.Len(t, recs, 2)
	assert.Equal(t, "rpc1", recs[0].UpstreamId)
	assert.Equal(t, common.DispatchNone, recs[0].Dispatch)
	assert.Equal(t, "rpc1", recs[1].UpstreamId)
	assert.Equal(t, "open", recs[1].BreakerState)
	assert.True(t, recs[1].IsHedge)
}

func TestScopes_SubrequestsAndFollowers(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	parentReq := newReq()
	pctx, parent := BeginRequest(context.Background(), parentReq, "main", "evm:1")

	// Same request re-entering Forward is a no-op scope.
	_, again := BeginRequest(pctx, parentReq, "main", "evm:1")
	assert.Nil(t, again)

	subReq := newReq()
	sctx, sub := BeginRequest(pctx, subReq, "main", "evm:1")
	require.NotNil(t, sub)
	record(subReq.ExecState(), time.Now(), "rpc1", common.DispatchAttempted, serverErr(500))
	sub.Finish(sctx, nil, exhausted(serverErr(500)))

	followerReq := newReq()
	fctx, follower := BeginRequest(context.Background(), followerReq, "main", "evm:1")
	MarkFollower(fctx, parent)
	follower.Finish(fctx, nil, serverErr(500))

	parent.Finish(pctx, nil, exhausted(serverErr(500)))

	sum := byEvent(parseLines(t, buf), "rpc.request.completed")
	require.Len(t, sum, 3)
	assert.Equal(t, parent.ID(), sum[0]["parent_request_id"])
	assert.Equal(t, parent.ID(), sum[1]["leader_request_id"])
	assert.Equal(t, "multiplexed_follower", sum[1]["path"])
	assert.Equal(t, "failed", sum[1]["outcome"])
	assert.Equal(t, float64(1), sum[2]["subrequests"])
	assert.Equal(t, parent.ID(), sum[2]["operational_request_id"])
}

func TestDisabled_NoLedgerNoEvents(t *testing.T) {
	buf := ConfigureForTest(t, &common.ExecutionDiagnosticsConfig{Enabled: util.BoolPtr(false)})
	assert.False(t, Enabled())
	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	assert.Nil(t, d)
	assert.False(t, req.ExecState().InvocationLedgerEnabled())
	_, inv := BeginInvocation(ctx, req, common.NewFakeUpstream("rpc1"), false)
	assert.Nil(t, inv)
	inv.End(errors.New("x"))
	d.Finish(ctx, nil, errors.New("x"))
	BreakerTransition("main", "evm:1", "rpc1", "v", "", "closed", "open", "r")
	assert.Empty(t, buf.Lines())
}

func TestInternalRequestsAreOutOfScope(t *testing.T) {
	ConfigureForTest(t, nil)
	req := newReq()
	req.SetDirectives(&common.RequestDirectives{IsInternal: true})
	_, d := BeginRequest(context.Background(), req, "main", "evm:1")
	assert.Nil(t, d)
}

func TestInboundTrace_LogOnly(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := WithInboundTraceContext(context.Background(), h)
	assert.False(t, trace.SpanContextFromContext(ctx).IsValid(), "inbound trace must not become the active span context")

	req := newReq()
	ctx, d := BeginRequest(ctx, req, "main", "evm:1")
	record(req.ExecState(), time.Now(), "rpc1", common.DispatchAttempted, serverErr(500))
	d.Finish(ctx, nil, exhausted(serverErr(500)))

	lines := parseLines(t, buf)
	sum := byEvent(lines, "rpc.request.completed")[0]
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", sum["trace_id"])
	assert.Equal(t, "00f067aa0ba902b7", sum["parent_span_id"])
	assert.NotContains(t, sum, "span_id")
	att := byEvent(lines, "rpc.attempt.completed")[0]
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", att["trace_id"])

	bad := http.Header{}
	bad.Set("traceparent", "garbage")
	assert.Nil(t, WithInboundTraceContext(context.Background(), bad).Value(inboundTraceKey{}))
}

func TestFinish_UserIdSanitized(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	for _, id := range []string{"user-1", "bad user/../x"} {
		req := newReq()
		req.SetUser(&common.User{Id: id})
		ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
		d.Finish(ctx, nil, exhausted(serverErr(500)))
	}
	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	d.Finish(ctx, nil, exhausted(serverErr(500)))

	sum := byEvent(parseLines(t, buf), "rpc.request.completed")
	require.Len(t, sum, 3)
	assert.Equal(t, "user-1", sum[0]["user_id"])
	assert.Equal(t, "invalid", sum[1]["user_id"])
	assert.NotContains(t, sum[2], "user_id")
}
