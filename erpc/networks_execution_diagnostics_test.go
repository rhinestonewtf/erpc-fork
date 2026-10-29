package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/execdiag"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type diagEvent map[string]interface{}

func (e diagEvent) str(k string) string {
	s, _ := e[k].(string)
	return s
}

func (e diagEvent) num(k string) int {
	f, _ := e[k].(float64)
	return int(f)
}

func diagEvents(t *testing.T, buf *execdiag.CaptureBuffer) []diagEvent {
	t.Helper()
	var out []diagEvent
	for _, l := range buf.Lines() {
		ev := diagEvent{}
		require.NoError(t, json.Unmarshal([]byte(l), &ev), l)
		out = append(out, ev)
	}
	return out
}

func diagFilter(evs []diagEvent, event string) []diagEvent {
	var out []diagEvent
	for _, e := range evs {
		if e.str("event") == event {
			out = append(out, e)
		}
	}
	return out
}

func diagAttemptsOf(evs []diagEvent, opID string) []diagEvent {
	var out []diagEvent
	for _, e := range diagFilter(evs, "rpc.attempt.completed") {
		if e.str("operational_request_id") == opID {
			out = append(out, e)
		}
	}
	return out
}

func diagAttemptFor(t *testing.T, attempts []diagEvent, upstreamId string) diagEvent {
	t.Helper()
	for _, a := range attempts {
		if a.str("upstream_id") == upstreamId {
			return a
		}
	}
	t.Fatalf("no attempt for %s in %v", upstreamId, attempts)
	return nil
}

const diagBalanceReq = `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`

func diagIsBalance(r *http.Request) bool {
	return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
}

func diagUpstreams(rpc1Failsafe ...*common.FailsafeConfig) []*common.UpstreamConfig {
	return []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			Failsafe: rpc1Failsafe,
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
	}
}

func diagNetworkConfig(fs *common.FailsafeConfig) *common.NetworkConfig {
	cfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	if fs != nil {
		cfg.Failsafe = []*common.FailsafeConfig{fs}
	}
	return cfg
}

func diagRetry(attempts int) *common.FailsafeConfig {
	return &common.FailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: attempts}}
}

func diagBreaker(halfOpenAfter time.Duration) *common.FailsafeConfig {
	return &common.FailsafeConfig{
		MatchMethod: "*",
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    1,
			FailureThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(halfOpenAfter),
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
		},
	}
}

func diagCounter(c *atomic.Int32) func(*http.Response) *http.Response {
	return func(r *http.Response) *http.Response { c.Add(1); return r }
}

// diagMock registers an eth_getBalance mock replying with status and
// counting served calls; times == 0 persists it.
func diagMock(host string, times int, status int, calls *atomic.Int32) *gock.Response {
	r := gock.New(host).Post("").Filter(diagIsBalance)
	if times == 0 {
		r = r.Persist()
	} else {
		r = r.Times(times)
	}
	resp := r.Reply(status)
	if calls != nil {
		resp = resp.Map(diagCounter(calls))
	}
	return resp
}

func diagOk(host string, times int, calls *atomic.Int32) {
	diagMock(host, times, 200, calls).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
}

func dumpOnFailure(t *testing.T, buf *execdiag.CaptureBuffer) {
	t.Cleanup(func() {
		if t.Failed() {
			for _, l := range buf.Lines() {
				t.Log(l)
			}
		}
	})
}

func TestExecutionDiagnostics_OpenBreakerZeroClientCalls(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var rpc1Calls, rpc2Calls atomic.Int32
	diagMock("http://rpc1.localhost", 1, 503, &rpc1Calls).
		JSON(map[string]interface{}{"error": map[string]interface{}{"message": "provider issue"}})
	diagOk("http://rpc2.localhost", 0, &rpc2Calls)

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(diagBreaker(30*time.Second)), diagNetworkConfig(diagRetry(2)))

	_, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
	require.NoError(t, err)
	require.Equal(t, int32(1), rpc1Calls.Load())

	_, err = network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
	require.NoError(t, err)

	assert.Equal(t, int32(1), rpc1Calls.Load(), "an open breaker must not reach the client")
	assert.Equal(t, int32(2), rpc2Calls.Load())

	require.Eventually(t, func() bool {
		return len(diagFilter(diagEvents(t, buf), "rpc.breaker.transition")) == 1
	}, time.Second, 10*time.Millisecond)

	evs := diagEvents(t, buf)
	sums := diagFilter(evs, "rpc.request.completed")
	require.Len(t, sums, 2)

	second := sums[1]
	assert.Equal(t, "degraded", second.str("outcome"))
	assert.Equal(t, "forward", second.str("path"))
	assert.Greater(t, second.num("logical_attempts"), second.num("dispatches"))
	assert.Equal(t, 1, second.num("dispatches"))
	assert.Equal(t, 2, second.num("invocations"), "a breaker rejection is recorded exactly once")

	attempts := diagAttemptsOf(evs, second.str("operational_request_id"))
	rejected := diagAttemptFor(t, attempts, "rpc1")
	assert.Equal(t, "rejected", rejected.str("class"))
	assert.Equal(t, "none", rejected.str("dispatch"))
	assert.Equal(t, "open", rejected.str("breaker_state"))
	assert.Equal(t, "method=*", rejected.str("breaker_scope"))
	assert.Equal(t, "ErrFailsafeCircuitBreakerOpen", rejected.str("cause"))
	served := diagAttemptFor(t, attempts, "rpc2")
	assert.Equal(t, "result", served.str("class"))
	assert.Equal(t, "attempted", served.str("dispatch"))
	assert.Equal(t, true, served["won"])

	tr := diagFilter(evs, "rpc.breaker.transition")[0]
	assert.Equal(t, "closed", tr.str("from"))
	assert.Equal(t, "open", tr.str("to"))
	assert.Equal(t, "rpc1", tr.str("upstream_id"))
	assert.NotContains(t, tr, "operational_request_id")
}

func diagJsonRpcError(code int, msg string, data string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]interface{}{"code": code, "message": msg, "data": data},
	}
}

func TestExecutionDiagnostics_TransportVsProviderVsApplication(t *testing.T) {
	cases := []struct {
		name     string
		rpc1     func()
		class    string
		status   int
		jsonrpc  bool
		dispatch string
	}{
		{
			name: "TransportFailure",
			rpc1: func() {
				gock.New("http://rpc1.localhost").Post("").Filter(diagIsBalance).Times(1).ReplyError(errors.New("connection refused"))
			},
			class:    "transport_failure",
			dispatch: "attempted",
		},
		{
			name: "ProviderHttp503",
			rpc1: func() {
				diagMock("http://rpc1.localhost", 1, 503, nil).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})
			},
			class:    "provider_error",
			status:   503,
			dispatch: "attempted",
		},
		{
			name: "ProviderUnknownJsonRpcCode",
			rpc1: func() {
				diagMock("http://rpc1.localhost", 1, 200, nil).JSON(diagJsonRpcError(-32099, "never seen before", ""))
			},
			class:    "provider_error",
			status:   200,
			jsonrpc:  true,
			dispatch: "attempted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			tc.rpc1()
			diagOk("http://rpc2.localhost", 0, nil)

			buf := execdiag.ConfigureForTest(t, nil)
			dumpOnFailure(t, buf)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(2)))

			resp, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
			require.NoError(t, err)
			require.Equal(t, "rpc2", resp.Upstream().Id())

			evs := diagEvents(t, buf)
			sums := diagFilter(evs, "rpc.request.completed")
			require.Len(t, sums, 1)
			assert.Equal(t, "degraded", sums[0].str("outcome"))
			assert.Equal(t, true, sums[0]["evidence_complete"])

			attempts := diagAttemptsOf(evs, sums[0].str("operational_request_id"))
			require.Len(t, attempts, 2)
			// Failed provider first, fallback second, in start order.
			assert.Equal(t, "rpc1", attempts[0].str("upstream_id"))
			assert.Equal(t, 1, attempts[0].num("attempt_no"))
			assert.Equal(t, tc.class, attempts[0].str("class"))
			assert.Equal(t, tc.dispatch, attempts[0].str("dispatch"))
			assert.Equal(t, "failed", attempts[0].str("outcome"))
			assert.Equal(t, false, attempts[0]["won"])
			if tc.status > 0 {
				assert.Equal(t, tc.status, attempts[0].num("http_status"))
			} else {
				assert.NotContains(t, attempts[0], "http_status")
			}
			if tc.jsonrpc {
				assert.Contains(t, attempts[0], "jsonrpc_code")
			}
			assert.Equal(t, "rpc2", attempts[1].str("upstream_id"))
			assert.Equal(t, 2, attempts[1].num("attempt_no"))
			assert.Equal(t, "result", attempts[1].str("class"))
			assert.Equal(t, true, attempts[1]["won"])
		})
	}

	t.Run("RevertOnlyEmitsNothing", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		diagMock("http://rpc1.localhost", 0, 200, nil).JSON(diagJsonRpcError(3, "execution reverted", "0x08c379a0"))
		diagMock("http://rpc2.localhost", 0, 200, nil).JSON(diagJsonRpcError(3, "execution reverted", "0x08c379a0"))

		buf := execdiag.ConfigureForTest(t, nil)
		dumpOnFailure(t, buf)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(2)))

		req := common.NewNormalizedRequest([]byte(diagBalanceReq))
		_, err := network.Forward(ctx, req)
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException))

		recs := req.ExecState().Invocations()
		require.NotEmpty(t, recs)
		class, _, _ := execdiag.Classify(recs[0].Err, recs[0].Dispatch, true)
		assert.Equal(t, execdiag.ClassApplicationError, class)
		assert.Empty(t, buf.Lines())
	})
}

func TestExecutionDiagnostics_RetryExhaustionMixedCauses(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").Post("").Filter(diagIsBalance).Persist().ReplyError(errors.New("connection refused"))
	diagMock("http://rpc2.localhost", 0, 503, nil).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(2)))

	_, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
	require.Error(t, err)

	evs := diagEvents(t, buf)
	sums := diagFilter(evs, "rpc.request.completed")
	require.Len(t, sums, 1)
	sum := sums[0]
	assert.Equal(t, "failed", sum.str("outcome"))
	assert.Equal(t, "warn", sum.str("level"))
	assert.Equal(t, "mixed", sum.str("final_class"))
	assert.Equal(t, "multiple", sum.str("final_cause"))
	assert.Equal(t, string(common.BaseError{Code: err.(common.StandardError).Base().Code}.Code), sum.str("final_error_code"))
	counts, _ := sum["class_counts"].(map[string]interface{})
	assert.Contains(t, counts, "transport_failure")
	assert.Contains(t, counts, "provider_error")
	assert.GreaterOrEqual(t, sum.num("dispatches"), 2)
}

func TestExecutionDiagnostics_HedgeCancellation(t *testing.T) {
	hedgeNetwork := func() *common.NetworkConfig {
		return diagNetworkConfig(&common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{Delay: common.NewStaticDuration(50 * time.Millisecond), MaxCount: 1},
		})
	}

	t.Run("LoserIsCancelledAndNotNotable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		diagMock("http://rpc1.localhost", 1, 200, nil).Delay(500 * time.Millisecond).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		diagOk("http://rpc2.localhost", 0, nil)

		buf := execdiag.ConfigureForTest(t, nil)
		dumpOnFailure(t, buf)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, diagUpstreams(), hedgeNetwork())

		req := common.NewNormalizedRequest([]byte(diagBalanceReq))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.Equal(t, "rpc2", resp.Upstream().Id())

		st := req.ExecState()
		require.Eventually(t, func() bool { return st.InvocationsInflight() == 0 }, 2*time.Second, 10*time.Millisecond)
		var loser *common.InvocationRecord
		for _, r := range st.Invocations() {
			if r.UpstreamId == "rpc1" {
				r := r
				loser = &r
			}
		}
		require.NotNil(t, loser)
		class, _, _ := execdiag.Classify(loser.Err, loser.Dispatch, true)
		assert.Equal(t, execdiag.ClassCancelled, class)
		assert.Equal(t, common.DispatchAttempted, loser.Dispatch)

		time.Sleep(50 * time.Millisecond)
		assert.Empty(t, buf.Lines(), "a hedge loser alone is benign")
	})

	t.Run("NotableHedgeLegIsReported", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		diagMock("http://rpc1.localhost", 1, 200, nil).Delay(300 * time.Millisecond).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		diagMock("http://rpc2.localhost", 0, 503, nil).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})

		buf := execdiag.ConfigureForTest(t, nil)
		dumpOnFailure(t, buf)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := setupTestNetwork(t, ctx, diagUpstreams(), hedgeNetwork())

		resp, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
		require.NoError(t, err)
		require.Equal(t, "rpc1", resp.Upstream().Id())

		require.Eventually(t, func() bool {
			return len(diagFilter(diagEvents(t, buf), "rpc.request.completed")) == 1
		}, 2*time.Second, 10*time.Millisecond)
		evs := diagEvents(t, buf)
		sum := diagFilter(evs, "rpc.request.completed")[0]
		assert.Equal(t, "degraded", sum.str("outcome"))
		assert.Equal(t, true, sum["evidence_complete"])
		attempts := diagAttemptsOf(evs, sum.str("operational_request_id"))
		hedgeLeg := diagAttemptFor(t, attempts, "rpc2")
		assert.Equal(t, "provider_error", hedgeLeg.str("class"))
		assert.Equal(t, true, hedgeLeg["hedge"])
		assert.Equal(t, "hedge", hedgeLeg.str("selection_reason"))
		assert.Equal(t, true, diagAttemptFor(t, attempts, "rpc1")["won"])
	})
}

func TestExecutionDiagnostics_HalfOpenRecovery(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var rpc1Calls atomic.Int32
	diagMock("http://rpc1.localhost", 1, 503, &rpc1Calls).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})
	diagOk("http://rpc1.localhost", 0, &rpc1Calls)
	diagOk("http://rpc2.localhost", 0, nil)

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(diagBreaker(200*time.Millisecond)), diagNetworkConfig(diagRetry(2)))

	for i := 0; i < 2; i++ {
		_, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), rpc1Calls.Load())

	time.Sleep(250 * time.Millisecond)
	req := common.NewNormalizedRequest([]byte(diagBalanceReq))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "rpc1", resp.Upstream().Id())

	recs := req.ExecState().Invocations()
	require.Len(t, recs, 1)
	assert.Equal(t, "half_open", recs[0].BreakerState)
	assert.Equal(t, common.DispatchAttempted, recs[0].Dispatch)
	assert.NoError(t, recs[0].Err)

	want := []string{"closed>open", "open>half_open", "half_open>closed"}
	require.Eventually(t, func() bool {
		return len(diagFilter(diagEvents(t, buf), "rpc.breaker.transition")) == len(want)
	}, time.Second, 10*time.Millisecond)
	trs := diagFilter(diagEvents(t, buf), "rpc.breaker.transition")
	got := map[string]bool{}
	for _, tr := range trs {
		got[tr.str("from")+">"+tr.str("to")] = true
		assert.NotEmpty(t, tr.str("reason"))
		assert.Equal(t, "rpc1", tr.str("upstream_id"))
		assert.NotEmpty(t, tr.str("replica"))
	}
	for _, w := range want {
		assert.True(t, got[w], "missing transition %s", w)
	}
}

func TestExecutionDiagnostics_SecretCanariesAndFieldContract(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	diagMock("http://rpc1.localhost", 0, 200, nil).JSON(diagJsonRpcError(-32099, "canarymsgFFF", "canarybodyGGG"))
	diagMock("http://rpc2.localhost", 0, 500, nil).BodyString(`{"error":{"code":-32603,"message":"canarymsgFFF","data":"canarybodyGGG"}}`)

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ups := diagUpstreams(diagBreaker(30 * time.Second))
	ups[0].Endpoint = "http://rpc1.localhost/canarypathBBB?key=canaryqueryCCC"
	ups[0].VendorName = "unknown-canaryhostAAA.localhost"
	network := setupTestNetwork(t, ctx, ups, diagNetworkConfig(diagRetry(2)))

	body := `{"jsonrpc":"2.0","id":"canaryidEEE","method":"eth_getBalance","params":["0xcanaryparamDDD","latest"]}`
	for i := 0; i < 2; i++ {
		req := common.NewNormalizedRequest([]byte(body))
		_, err := network.Forward(ctx, req)
		require.Error(t, err)
	}
	time.Sleep(50 * time.Millisecond)

	out := buf.String()
	require.NotEmpty(t, out)
	for _, canary := range []string{"canaryhostAAA", "canarypathBBB", "canaryqueryCCC", "canaryparamDDD", "canaryidEEE", "canarymsgFFF", "canarybodyGGG"} {
		assert.NotContains(t, out, canary)
	}
	evs := diagEvents(t, buf)
	for _, ev := range evs {
		assert.Empty(t, execdiag.UnexpectedEventKeys(ev), "event %v", ev)
	}
	attempts := diagFilter(evs, "rpc.attempt.completed")
	require.NotEmpty(t, attempts)
	unknown := diagAttemptFor(t, attempts, "rpc1")
	assert.Equal(t, "provider_error", unknown.str("class"))
	assert.Contains(t, unknown, "jsonrpc_code")
}

func TestExecutionDiagnostics_Disabled(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	diagMock("http://rpc1.localhost", 1, 503, nil).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})
	diagOk("http://rpc2.localhost", 0, nil)

	buf := execdiag.ConfigureForTest(t, &common.ExecutionDiagnosticsConfig{Enabled: util.BoolPtr(false)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(2)))

	req := common.NewNormalizedRequest([]byte(diagBalanceReq))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "rpc2", resp.Upstream().Id())
	assert.False(t, req.ExecState().InvocationLedgerEnabled())
	assert.Empty(t, req.ExecState().Invocations())
	assert.Empty(t, buf.Lines())
}

func TestExecutionDiagnostics_MultiplexedFollower(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	diagMock("http://rpc1.localhost", 0, 500, nil).Delay(300 * time.Millisecond).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "boom"}})
	diagMock("http://rpc2.localhost", 0, 500, nil).Delay(300 * time.Millisecond).JSON(map[string]interface{}{"error": map[string]interface{}{"message": "boom"}})

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(1)))

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(delay time.Duration) {
			defer wg.Done()
			time.Sleep(delay)
			_, _ = network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
		}(time.Duration(i) * 50 * time.Millisecond)
	}
	wg.Wait()

	sums := diagFilter(diagEvents(t, buf), "rpc.request.completed")
	require.Len(t, sums, 2)
	var leader, follower diagEvent
	for _, s := range sums {
		if s.str("path") == "multiplexed_follower" {
			follower = s
		} else {
			leader = s
		}
	}
	require.NotNil(t, leader)
	require.NotNil(t, follower)
	assert.Equal(t, leader.str("operational_request_id"), follower.str("leader_request_id"))
	assert.Equal(t, "failed", follower.str("outcome"))
	assert.Equal(t, 0, follower.num("invocations"))
}

// rejectingPreForward refuses every upstream in the upstream pre-hook, the
// way a block-range check does, so nothing reaches a client.
type rejectingPreForward struct {
	common.ArchitectureHandler
}

func (rejectingPreForward) HandleUpstreamPreForward(_ context.Context, _ common.Network, u common.Upstream, _ *common.NormalizedRequest, _ bool) (bool, *common.NormalizedResponse, error) {
	return true, nil, common.NewErrUpstreamRequestSkipped(errors.New("block range unavailable"), u.Id())
}

func TestExecutionDiagnostics_UpstreamPreHookRejection(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var rpc1Calls, rpc2Calls atomic.Int32
	diagOk("http://rpc1.localhost", 0, &rpc1Calls)
	diagOk("http://rpc2.localhost", 0, &rpc2Calls)

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, diagUpstreams(), diagNetworkConfig(diagRetry(1)))
	network.architectureHandler = rejectingPreForward{network.architectureHandler}

	_, err := network.Forward(ctx, common.NewNormalizedRequest([]byte(diagBalanceReq)))
	require.Error(t, err)
	assert.Zero(t, rpc1Calls.Load()+rpc2Calls.Load())

	evs := diagEvents(t, buf)
	sums := diagFilter(evs, "rpc.request.completed")
	require.Len(t, sums, 1)
	sum := sums[0]
	assert.Equal(t, 0, sum.num("dispatches"))
	assert.Equal(t, 2, sum.num("invocations"))
	assert.Equal(t, true, sum["evidence_complete"])
	for _, id := range []string{"rpc1", "rpc2"} {
		a := diagAttemptFor(t, diagAttemptsOf(evs, sum.str("operational_request_id")), id)
		assert.Equal(t, "none", a.str("dispatch"))
		assert.Equal(t, "skipped", a.str("class"))
	}
}
