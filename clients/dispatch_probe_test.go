package clients

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func probeClient(t *testing.T, ctx context.Context, jsonRpc *common.JsonRpcUpstreamConfig) HttpJsonRpcClient {
	t.Helper()
	logger := log.Logger
	ups := common.NewFakeUpstream("rpc1")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = "http://rpc1.localhost:8545"
	ups.Config().JsonRpc = jsonRpc
	client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, jsonRpc, nil, &noopErrorExtractor{})
	require.NoError(t, err)
	return client
}

func probeReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
}

func TestDispatchProbe_HttpSingle(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("http://rpc1.localhost:8545").Post("/").Reply(200).BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := probeClient(t, ctx, nil)
		p := &common.DispatchProbe{}
		_, err := client.SendRequest(common.WithDispatchProbe(ctx, p), probeReq())
		require.NoError(t, err)
		assert.Equal(t, common.DispatchAttempted, p.State())
	})

	t.Run("TransportError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("http://rpc1.localhost:8545").Post("/").ReplyError(errors.New("connection refused"))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := probeClient(t, ctx, nil)
		p := &common.DispatchProbe{}
		_, err := client.SendRequest(common.WithDispatchProbe(ctx, p), probeReq())
		require.Error(t, err)
		assert.Equal(t, common.DispatchAttempted, p.State())
	})

	t.Run("NoProbeInContext", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("http://rpc1.localhost:8545").Post("/").Reply(200).BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := probeClient(t, ctx, nil).SendRequest(ctx, probeReq())
		require.NoError(t, err)
	})
}

func TestDispatchProbe_HttpBatch(t *testing.T) {
	batchCfg := func() *common.JsonRpcUpstreamConfig {
		return &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  10,
			BatchMaxWait:  common.Duration(100 * time.Millisecond),
		}
	}

	t.Run("Dispatched", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("http://rpc1.localhost:8545").Post("/").Reply(200).BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := probeClient(t, ctx, batchCfg())
		p := &common.DispatchProbe{}
		_, err := client.SendRequest(common.WithDispatchProbe(ctx, p), probeReq())
		require.NoError(t, err)
		assert.Equal(t, common.DispatchAttempted, p.State())
	})

	t.Run("CancelledWhileQueued", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		appCtx, appCancel := context.WithCancel(context.Background())
		defer appCancel()
		client := probeClient(t, appCtx, batchCfg())

		p := &common.DispatchProbe{}
		reqCtx, cancel := context.WithCancel(common.WithDispatchProbe(appCtx, p))
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		_, err := client.SendRequest(reqCtx, probeReq())
		require.Error(t, err)
		// Let the batch flush run; the cancelled member must be dropped unsent.
		time.Sleep(250 * time.Millisecond)
		assert.Equal(t, common.DispatchNone, p.State())
	})
}

func TestDispatchProbe_GrpcBoundedCall(t *testing.T) {
	p := &common.DispatchProbe{}
	ctx := common.WithDispatchProbe(context.Background(), p)
	_, err := callBoundedT(ctx, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)
	assert.Equal(t, common.DispatchAttempted, p.State())

	p2 := &common.DispatchProbe{}
	dead, cancel := context.WithCancel(common.WithDispatchProbe(context.Background(), p2))
	cancel()
	_, err = callBoundedT(dead, func(context.Context) (int, error) { return 1, nil })
	require.Error(t, err)
	assert.Equal(t, common.DispatchUnknown, p2.State(), "an already-dead context never reaches the RPC")
}
