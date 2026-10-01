package evm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patch (RHI-7827) tests. They go through HandleProjectPreForward, so
// they also fail if the hooks.go call site is dropped by an upstream sync.

type bundleTestNetwork struct {
	cfg       *common.NetworkConfig
	latest    int64
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
func (n *bundleTestNetwork) Config() *common.NetworkConfig            { return n.cfg }
func (n *bundleTestNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (n *bundleTestNetwork) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (n *bundleTestNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *bundleTestNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64    { return n.latest }
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
	return n.forwardFn(ctx, req)
}

func (n *bundleTestNetwork) recorded() []bundleTestCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]bundleTestCall(nil), n.calls...)
}

func newBundleTestNetwork(latest int64, bs *common.BundleSubmissionConfig) *bundleTestNetwork {
	return &bundleTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 1, BundleSubmission: bs},
		},
		latest:    latest,
		forwardFn: acceptBundle,
	}
}

func acceptBundle(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponse(req.ID(), map[string]interface{}{"bundleHash": "0xb0"}, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
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

func waitForCalls(t *testing.T, n *bundleTestNetwork, want int) []bundleTestCall {
	t.Helper()
	require.Eventually(t, func() bool { return len(n.recorded()) >= want }, 2*time.Second, 5*time.Millisecond)
	return n.recorded()
}

func TestBundleSubmission_NotConfigured_FallsThrough(t *testing.T) {
	n := newBundleTestNetwork(100, nil)

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	assert.False(t, handled, "without evm.bundleSubmission the request must take the normal path")
	assert.Nil(t, resp)
	assert.NoError(t, err)
	assert.Empty(t, n.recorded())
}

func TestBundleSubmission_SubmitsOneBundlePerTargetBlock(t *testing.T) {
	n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{
		UseUpstream:  "bundle-relay",
		TargetBlocks: 3,
		BundleFields: map[string]interface{}{
			"builders":      []interface{}{"flashbots", "Titan"},
			"refundPercent": 10,
		},
	})

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	require.True(t, handled)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"`+sendRawTxFixtureHash+`"`, jrr.GetResultString(), "callers get the tx hash, never the bundle hash")
	assert.Equal(t, int64(7), jrr.ID(), "the caller's request id is preserved")

	calls := waitForCalls(t, n, 3)
	require.Len(t, calls, 3)
	var blocks []string
	for _, c := range calls {
		assert.Equal(t, "eth_sendBundle", c.method)
		assert.Equal(t, "bundle-relay", c.useUpstream, "sub-requests are pinned to the bundle relays")
		require.NotNil(t, c.bundle)
		assert.Equal(t, []string{sendRawTxFixture}, c.bundle["txs"])
		assert.Equal(t, []interface{}{"flashbots", "Titan"}, c.bundle["builders"], "bundleFields pass through verbatim")
		assert.Equal(t, 10, c.bundle["refundPercent"])
		assert.NotContains(t, c.bundle, "revertingTxHashes", "nothing may be allowed to revert unless configured")
		blocks = append(blocks, c.bundle["blockNumber"].(string))
	}
	sort.Strings(blocks)
	assert.Equal(t, []string{"0x65", "0x66", "0x67"}, blocks, "one bundle for each of head+1 … head+targetBlocks")
}

func TestBundleSubmission_AddsMissingHexPrefix(t *testing.T) {
	n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 1})

	_, _, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture[2:]+`"`))

	require.NoError(t, err)
	calls := waitForCalls(t, n, 1)
	assert.Equal(t, []string{sendRawTxFixture}, calls[0].bundle["txs"])
}

func TestBundleSubmission_AnswersAtFirstAcceptedBlock(t *testing.T) {
	release := make(chan struct{})
	n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 3})
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
	n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 3})
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
	assert.Len(t, n.recorded(), 3)
}

func TestBundleSubmission_InvalidTransaction_NothingSent(t *testing.T) {
	for name, param := range map[string]string{
		"undecodable bytes": `"0x00"`,
		"not hex":           `"0xzz"`,
		"not a string":      `123`,
		"no params":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 3})

			handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(param))

			require.True(t, handled, "a configured network must answer even malformed sends, never fall through")
			assert.Nil(t, resp)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest), "got %v", err)
			assert.Empty(t, n.recorded(), "nothing may be forwarded anywhere")
		})
	}
}

func TestBundleSubmission_HeadUnknown_NothingSent(t *testing.T) {
	n := newBundleTestNetwork(0, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 3})

	handled, resp, err := HandleProjectPreForward(context.Background(), n, sendRawTxRequest(`"`+sendRawTxFixture+`"`))

	require.True(t, handled)
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "a retryable, exhausted-class error; got %v", err)
	assert.Empty(t, n.recorded())
}

func TestBundleSubmission_SubmissionsOutliveTheCaller(t *testing.T) {
	release := make(chan struct{})
	subCtxErr := make(chan error, 1)
	n := newBundleTestNetwork(100, &common.BundleSubmissionConfig{UseUpstream: "bundle-relay", TargetBlocks: 1})
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
	waitForCalls(t, n, 1)
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
}
