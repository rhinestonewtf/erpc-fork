package evm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patch (RHI-7827) tests. They go through HandleProjectPreForward, so
// they also fail if the hooks.go call site is dropped by an upstream sync.

// sendRawTxFixture is a type-2 transaction for chain 1 with nonce 10.
const sendRawTxFixtureNonce = 10

type bundleTestNetwork struct {
	cfg atomic.Pointer[common.NetworkConfig] // swapped by tests while keep-alive loops read it

	latest       atomic.Int64
	onChainNonce atomic.Int64 // what eth_getTransactionCount returns; < 0 makes it fail

	// forwardFn overrides how eth_sendBundle is answered; nil accepts.
	forwardFn func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)

	mu    sync.Mutex
	calls []bundleTestCall
}

type bundleTestCall struct {
	method      string
	useUpstream string
	bundle      map[string]interface{}
}

func (n *bundleTestNetwork) Id() string                               { return "evm:1" }
func (n *bundleTestNetwork) Label() string                            { return "evm:1" }
func (n *bundleTestNetwork) ProjectId() string                        { return "test-project" }
func (n *bundleTestNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureEvm }
func (n *bundleTestNetwork) Config() *common.NetworkConfig            { return n.cfg.Load() }
func (n *bundleTestNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (n *bundleTestNetwork) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (n *bundleTestNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *bundleTestNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return n.latest.Load()
}
func (n *bundleTestNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 { return 0 }
func (n *bundleTestNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream    { return nil }

func (n *bundleTestNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	method, _ := req.Method()
	call := bundleTestCall{method: method}
	if d := req.Directives(); d != nil {
		call.useUpstream = d.UseUpstream
	}
	if jrq, err := req.JsonRpcRequest(); err == nil && len(jrq.Params) == 1 {
		call.bundle, _ = jrq.Params[0].(map[string]interface{})
	}
	n.mu.Lock()
	n.calls = append(n.calls, call)
	n.mu.Unlock()

	if method == "eth_getTransactionCount" {
		nonce := n.onChainNonce.Load()
		if nonce < 0 {
			return nil, errors.New("nonce read failed")
		}
		return jsonRpcResult(req, fmt.Sprintf("0x%x", nonce))
	}
	if n.forwardFn != nil {
		return n.forwardFn(ctx, req)
	}
	return acceptBundle(ctx, req)
}

// bundles returns the eth_sendBundle calls made so far.
func (n *bundleTestNetwork) bundles() []bundleTestCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []bundleTestCall
	for _, c := range n.calls {
		if c.method == "eth_sendBundle" {
			out = append(out, c)
		}
	}
	return out
}

func (n *bundleTestNetwork) bundleBlocks() []string {
	var blocks []string
	for _, c := range n.bundles() {
		blocks = append(blocks, c.bundle["blockNumber"].(string))
	}
	sort.Strings(blocks)
	return blocks
}

func (n *bundleTestNetwork) keptCount() int {
	k := keepAliveFor(n)
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.kept)
}

func newBundleTestNetwork(latest int64, bs *common.BundleSubmissionConfig) *bundleTestNetwork {
	n := &bundleTestNetwork{}
	n.setChain(1, bs)
	n.latest.Store(latest)
	n.onChainNonce.Store(sendRawTxFixtureNonce) // not yet included
	return n
}

func (n *bundleTestNetwork) setChain(chainId int64, bs *common.BundleSubmissionConfig) {
	n.cfg.Store(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: chainId, BundleSubmission: bs},
	})
}

// bundleCfg is a bundle submission config with a short keep-alive window, so
// tests that are not about keep-alive do not leave loops running.
func bundleCfg(targetBlocks int) *common.BundleSubmissionConfig {
	return &common.BundleSubmissionConfig{
		UseUpstream:  "bundle-relay",
		TargetBlocks: targetBlocks,
		ResubmitFor:  common.Duration(50 * time.Millisecond),
	}
}

// fastKeepAlive makes the keep-alive loop look for new heads every few
// milliseconds for the duration of a test.
func fastKeepAlive(t *testing.T) {
	t.Helper()
	bundleKeepAliveTickOverride.Store(int64(5 * time.Millisecond))
	t.Cleanup(func() { bundleKeepAliveTickOverride.Store(0) })
}

func jsonRpcResult(req *common.NormalizedRequest, result interface{}) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponse(req.ID(), result, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func acceptBundle(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return jsonRpcResult(req, map[string]interface{}{"bundleHash": "0xb0"})
}

// bundleBlock returns the target block of a sub-request, or "" if absent.
func bundleBlock(req *common.NormalizedRequest) string {
	jrq, err := req.JsonRpcRequest()
	if err != nil || len(jrq.Params) != 1 {
		return ""
	}
	b, _ := jrq.Params[0].(map[string]interface{})
	s, _ := b["blockNumber"].(string)
	return s
}

func sendRawTxRequest(param string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":7,"method":"eth_sendRawTransaction","params":[` + param + `]}`))
}

func waitForBundles(t *testing.T, n *bundleTestNetwork, want int) []bundleTestCall {
	t.Helper()
	require.Eventually(t, func() bool { return len(n.bundles()) >= want }, 2*time.Second, 2*time.Millisecond)
	return n.bundles()
}

func keepAliveOutcomes(outcome string) float64 {
	return testutil.ToFloat64(telemetry.MetricBundleKeepAliveTotal.WithLabelValues("test-project", "evm:1", outcome))
}

func TestBundleSubmission_NotConfigured_FallsThrough(t *testing.T) {
	n := newBundleTestNetwork(100, nil)

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	assert.False(t, handled, "without evm.bundleSubmission the request must take the normal path")
	assert.Nil(t, resp)
	assert.NoError(t, err)
	assert.Empty(t, n.bundles())
}

func TestBundleSubmission_SubmitsOneBundlePerTargetBlock(t *testing.T) {
	cfg := bundleCfg(3)
	cfg.BundleFields = map[string]interface{}{
		"builders":      []interface{}{"flashbots", "Titan"},
		"refundPercent": 10,
	}
	n := newBundleTestNetwork(100, cfg)

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	require.True(t, handled)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"`+sendRawTxFixtureHash+`"`, jrr.GetResultString(), "callers get the tx hash, never the bundle hash")
	assert.Equal(t, int64(7), jrr.ID(), "the caller's request id is preserved")

	calls := waitForBundles(t, n, 3)
	require.Len(t, calls, 3)
	for _, c := range calls {
		assert.Equal(t, "bundle-relay", c.useUpstream, "sub-requests are pinned to the bundle relays")
		require.NotNil(t, c.bundle)
		assert.Equal(t, []string{sendRawTxFixture}, c.bundle["txs"])
		assert.Equal(t, []interface{}{"flashbots", "Titan"}, c.bundle["builders"], "bundleFields pass through verbatim")
		assert.Equal(t, 10, c.bundle["refundPercent"])
		assert.NotContains(t, c.bundle, "revertingTxHashes", "nothing may be allowed to revert unless configured")
	}
	assert.Equal(t, []string{"0x65", "0x66", "0x67"}, n.bundleBlocks(), "one bundle for each of head+1 … head+targetBlocks")
}

func TestBundleSubmission_AddsMissingHexPrefix(t *testing.T) {
	n := newBundleTestNetwork(100, bundleCfg(1))

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture[2:]+`"`))

	require.NoError(t, err)
	calls := waitForBundles(t, n, 1)
	assert.Equal(t, []string{sendRawTxFixture}, calls[0].bundle["txs"])
}

func TestBundleSubmission_AnswersAtFirstAcceptedBlock(t *testing.T) {
	release := make(chan struct{})
	n := newBundleTestNetwork(100, bundleCfg(3))
	n.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		switch bundleBlock(req) {
		case "0x65":
			<-release // the relay is slow for this block
			return acceptBundle(ctx, req)
		case "0x66":
			return acceptBundle(ctx, req)
		default:
			return nil, errors.New("relay rejected block 0x67")
		}
	}
	defer close(release)

	done := make(chan struct{})
	var resp *common.NormalizedResponse
	var err error
	go func() {
		defer close(done)
		_, resp, err = HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the caller should be answered as soon as one target block is accepted")
	}
	require.NoError(t, err)
	jrr, jerr := resp.JsonRpcResponse()
	require.NoError(t, jerr)
	assert.Equal(t, `"`+sendRawTxFixtureHash+`"`, jrr.GetResultString())
}

func TestBundleSubmission_AllRejected_ReturnsEarliestBlockError(t *testing.T) {
	n := newBundleTestNetwork(100, bundleCfg(3))
	n.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		block := bundleBlock(req)
		if block == "0x65" {
			// The earliest block's error arrives last; it must still be the one reported.
			time.Sleep(50 * time.Millisecond)
		}
		return nil, fmt.Errorf("relay rejected block %s", block)
	}

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	require.True(t, handled)
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "relay rejected block 0x65")
	assert.Len(t, n.bundles(), 3)
	assert.Zero(t, n.keptCount(), "a transaction no relay accepted must not be kept alive")
}

func TestBundleSubmission_InvalidTransaction_NothingSent(t *testing.T) {
	for name, tc := range map[string]struct {
		param   string
		chainId int64
	}{
		"undecodable bytes":         {param: `"0x00"`, chainId: 1},
		"not hex":                   {param: `"0xzz"`, chainId: 1},
		"not a string":              {param: `123`, chainId: 1},
		"no params":                 {param: ``, chainId: 1},
		"signed for another chain":  {param: `"` + sendRawTxFixture + `"`, chainId: 5},
		"signed for another chain2": {param: `"` + sendRawTxFixture + `"`, chainId: 8453},
	} {
		t.Run(name, func(t *testing.T) {
			n := newBundleTestNetwork(100, nil)
			n.setChain(tc.chainId, bundleCfg(3))

			handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(tc.param))

			require.True(t, handled, "a configured network must answer even malformed sends, never fall through")
			assert.Nil(t, resp)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest), "got %v", err)
			assert.Empty(t, n.bundles(), "nothing may be forwarded anywhere")
		})
	}
}

func TestBundleSubmission_HeadUnknown_NothingSent(t *testing.T) {
	n := newBundleTestNetwork(0, bundleCfg(3))

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	require.True(t, handled)
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "a retryable, exhausted-class error; got %v", err)
	assert.Empty(t, n.bundles())
}

func TestBundleSubmission_SubmissionsOutliveTheCaller(t *testing.T) {
	release := make(chan struct{})
	subCtxErr := make(chan error, 1)
	n := newBundleTestNetwork(100, bundleCfg(1))
	n.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		<-release
		subCtxErr <- ctx.Err()
		return acceptBundle(ctx, req)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := HandleProjectPreForward(ctx, n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
		done <- err
	}()
	waitForBundles(t, n, 1)
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("a caller that goes away must not be held until the relay answers")
	}

	close(release)
	select {
	case err := <-subCtxErr:
		assert.NoError(t, err, "the submission must keep running after the caller is gone")
	case <-time.After(2 * time.Second):
		t.Fatal("submission never completed")
	}
	require.Eventually(t, func() bool { return n.keptCount() == 1 }, 2*time.Second, 2*time.Millisecond,
		"a transaction accepted after the caller left is still kept alive")
}

func TestBundleKeepAlive_ResubmitsEachNewBlockUntilTheNonceMoves(t *testing.T) {
	fastKeepAlive(t)
	cfg := bundleCfg(3)
	cfg.ResubmitFor = common.Duration(time.Minute)
	n := newBundleTestNetwork(100, cfg)
	settledBefore := keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeSettled)

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
	require.NoError(t, err)
	waitForBundles(t, n, 3)

	// A caller that never re-broadcasts: each new head brings one more block into reach.
	n.latest.Store(101)
	waitForBundles(t, n, 4)
	n.latest.Store(102)
	waitForBundles(t, n, 5)
	assert.Equal(t, []string{"0x65", "0x66", "0x67", "0x68", "0x69"}, n.bundleBlocks(),
		"each block is targeted once, always targetBlocks ahead of the head")

	// The transaction lands: the sender's nonce moves past it.
	n.onChainNonce.Store(sendRawTxFixtureNonce + 1)
	n.latest.Store(103)
	require.Eventually(t, func() bool { return n.keptCount() == 0 }, 2*time.Second, 2*time.Millisecond)
	assert.Equal(t, settledBefore+1, keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeSettled))
	n.latest.Store(104)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, n.bundles(), 5, "nothing is submitted once the transaction is settled")
}

func TestBundleKeepAlive_UnreadableNonceKeepsSubmitting(t *testing.T) {
	fastKeepAlive(t)
	cfg := bundleCfg(1)
	cfg.ResubmitFor = common.Duration(time.Minute)
	n := newBundleTestNetwork(100, cfg)
	n.onChainNonce.Store(-1) // every nonce read fails

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
	require.NoError(t, err)
	waitForBundles(t, n, 1)
	n.latest.Store(101)

	waitForBundles(t, n, 2)
	assert.Equal(t, 1, n.keptCount(), "a failed read must not settle the transaction")
	// A re-broadcast with a 1ms window lets the loop wind down.
	keepAliveFor(n).keep(&sendRawTx{hash: sendRawTxFixtureHash}, 0, time.Millisecond)
	require.Eventually(t, func() bool { return n.keptCount() == 0 }, 2*time.Second, 2*time.Millisecond)
}

func TestBundleKeepAlive_GivesUpWhenTheWindowEnds(t *testing.T) {
	fastKeepAlive(t)
	n := newBundleTestNetwork(100, bundleCfg(1)) // resubmitFor 50ms
	expiredBefore := keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeExpired)

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
	require.NoError(t, err)
	waitForBundles(t, n, 1)

	require.Eventually(t, func() bool { return n.keptCount() == 0 }, 2*time.Second, 2*time.Millisecond)
	assert.Equal(t, expiredBefore+1, keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeExpired),
		"a transaction dropped at the end of its window must be counted, not silent")
	submitted := len(n.bundles())
	n.latest.Store(101)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, n.bundles(), submitted, "nothing is submitted after the window")
}

func TestBundleKeepAlive_RebroadcastRestartsTheWindow(t *testing.T) {
	n := newBundleTestNetwork(100, bundleCfg(1))
	k := keepAliveFor(n)
	tx := &sendRawTx{raw: sendRawTxFixture, hash: sendRawTxFixtureHash, sender: "0xabc", nonce: 1}

	k.keep(tx, 101, time.Minute)
	k.keep(tx, 105, time.Hour)

	k.mu.Lock()
	defer k.mu.Unlock()
	require.Len(t, k.kept, 1, "a re-broadcast is the same kept transaction")
	kt := k.kept[sendRawTxFixtureHash]
	assert.Equal(t, int64(105), kt.coveredTo, "coverage only moves forward")
	assert.True(t, time.Until(kt.deadline) > 30*time.Minute, "the window restarts from the re-broadcast")
	kt.deadline = time.Now() // let the loop wind down
}

func TestBundleKeepAlive_StopsWhenBundleSubmissionIsRemoved(t *testing.T) {
	fastKeepAlive(t)
	cfg := bundleCfg(1)
	cfg.ResubmitFor = common.Duration(time.Minute)
	n := newBundleTestNetwork(100, cfg)
	disabledBefore := keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeDisabled)

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))
	require.NoError(t, err)
	waitForBundles(t, n, 1)
	n.setChain(1, nil)

	require.Eventually(t, func() bool { return n.keptCount() == 0 }, 2*time.Second, 2*time.Millisecond)
	assert.Equal(t, disabledBefore+1, keepAliveOutcomes(telemetry.BundleKeepAliveOutcomeDisabled))
}
