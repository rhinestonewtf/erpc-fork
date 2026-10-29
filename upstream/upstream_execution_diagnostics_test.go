package upstream

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/execdiag"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func diagBreakerExecutor(t *testing.T) *upstreamExecutor {
	t.Helper()
	ex, err := NewUpstreamExecutor(&common.UpstreamFailsafeConfig{
		MatchMethod:   "eth_*",
		MatchFinality: []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateUnfinalized},
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    1,
			FailureThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(50 * time.Millisecond),
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
		},
	}, &log.Logger)
	require.NoError(t, err)
	return ex
}

func TestExecDiag_BreakerScope(t *testing.T) {
	assert.Equal(t, "method=eth_*;finality=finalized|unfinalized", breakerScope(diagBreakerExecutor(t)))
	noBreaker, _ := NewUpstreamExecutor(nil, &log.Logger)
	assert.Equal(t, "", breakerScope(noBreaker))
}

// Mirrors tryForward: admitted calls begin/end an invocation and dial.
func diagInner(release <-chan struct{}, fail bool) func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
	return func(ctx context.Context, _ bool) (resp *common.NormalizedResponse, err error) {
		req := ctx.Value(diagReqKey{}).(*common.NormalizedRequest)
		ctx, inv := execdiag.BeginInvocation(ctx, req, common.NewFakeUpstream("rpc1"), false)
		defer func() { inv.End(err) }()
		common.MarkDispatchAttempted(ctx)
		if release != nil {
			<-release
		}
		if fail {
			return nil, common.NewErrEndpointServerSideException(nil, nil, 503)
		}
		return common.NewNormalizedResponse(), nil
	}
}

type diagReqKey struct{}

func TestExecDiag_BreakerAdmissionAndRejection(t *testing.T) {
	execdiag.ConfigureForTest(t, nil)
	ex := diagBreakerExecutor(t)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	ctx, scope := execdiag.BeginRequest(context.Background(), req, "main", "evm:1")
	require.NotNil(t, scope)
	ctx = context.WithValue(ctx, diagReqKey{}, req)
	call := func(inner func(context.Context, bool) (*common.NormalizedResponse, error)) error {
		cctx, uc := execdiag.BeginUpstreamCall(ctx, req, common.NewFakeUpstream("rpc1"), false)
		_, err := ex.callBreakerWithTimeout(cctx, req, inner, false)
		uc.End(err)
		return err
	}

	// Closed → failure opens the breaker.
	require.Error(t, call(diagInner(nil, true)))
	// Open → rejected locally, inner never runs.
	err := call(diagInner(nil, false))
	require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))

	// Half-open: the trial invocation holds the only permit, a concurrent
	// call is rejected while half-open.
	time.Sleep(60 * time.Millisecond)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- call(diagInner(release, false)) }()
	require.Eventually(t, func() bool { return req.ExecState().InvocationsInflight() == 1 }, time.Second, 5*time.Millisecond)
	err = call(diagInner(nil, false))
	require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))
	close(release)
	require.NoError(t, <-done)

	recs := req.ExecState().Invocations()
	require.Len(t, recs, 4)
	type view struct{ id, dispatch, breaker, scope string }
	got := make([]view, len(recs))
	for i, r := range recs {
		got[i] = view{r.UpstreamId, r.Dispatch, r.BreakerState, r.BreakerScope}
	}
	scopeStr := "method=eth_*;finality=finalized|unfinalized"
	assert.Equal(t, []view{
		{"rpc1", common.DispatchAttempted, "closed", scopeStr},
		{"rpc1", common.DispatchNone, "open", scopeStr},
		{"rpc1", common.DispatchNone, "half_open", scopeStr},
		{"rpc1", common.DispatchAttempted, "half_open", scopeStr},
	}, got)
	class, _, _ := execdiag.Classify(recs[2].Err, recs[2].Dispatch, true)
	assert.Equal(t, execdiag.ClassRejected, class)
}

func TestExecDiag_BreakerBypassedForHedges(t *testing.T) {
	execdiag.ConfigureForTest(t, nil)
	ex := diagBreakerExecutor(t)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	ctx, _ := execdiag.BeginRequest(context.Background(), req, "main", "evm:1")
	ctx = context.WithValue(ctx, diagReqKey{}, req)
	_, err := ex.callBreakerWithTimeout(ctx, req, diagInner(nil, false), true)
	require.NoError(t, err)
	recs := req.ExecState().Invocations()
	require.Len(t, recs, 1)
	assert.Equal(t, execdiag.BreakerBypassed, recs[0].BreakerState)
}

func TestExecDiag_BreakerTransitionEvents(t *testing.T) {
	buf := execdiag.ConfigureForTest(t, nil)
	ex := diagBreakerExecutor(t)
	prevHook := make(chan string, 4)
	ex.Breaker().OnTransition = func(from, to failsafe.State, _ string) { prevHook <- from.String() + "_to_" + to.String() }
	u := &Upstream{ProjectId: "main", config: &common.UpstreamConfig{Id: "rpc1", VendorName: "vendorx"}, failsafeExecutors: []*upstreamExecutor{ex}}
	u.networkId.Store("evm:1")
	wireBreakerDiagnostics(u)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	ctx := context.WithValue(context.Background(), diagReqKey{}, req)
	_, _ = ex.callBreakerWithTimeout(ctx, req, diagInner(nil, true), false)

	require.Eventually(t, func() bool { return len(buf.Lines()) >= 1 }, time.Second, 5*time.Millisecond)
	var ev map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(buf.Lines()[0]), &ev))
	assert.Equal(t, "rpc.breaker.transition", ev["event"])
	assert.Equal(t, "closed", ev["from"])
	assert.Equal(t, "open", ev["to"])
	assert.Equal(t, "rpc1", ev["upstream_id"])
	assert.Equal(t, "vendorx", ev["vendor"])
	assert.Equal(t, "evm:1", ev["network"])
	assert.Equal(t, "main", ev["project"])
	assert.Equal(t, "method=eth_*;finality=finalized|unfinalized", ev["breaker_scope"])
	assert.Equal(t, "warn", ev["level"])
	assert.NotEmpty(t, ev["reason"])
	assert.NotContains(t, ev, "operational_request_id")
	assert.Equal(t, "closed_to_open", <-prevHook, "the existing metric hook keeps firing")
}
