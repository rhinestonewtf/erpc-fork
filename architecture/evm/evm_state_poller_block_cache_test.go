package evm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patch RHI-8178. Fails if the poller stops writing the block it fetched
// into the cache by hash, stops carrying the hash in the head observation, or
// publishes the head before the cache write has landed (a subscriber that wakes
// on the head must find the block).

const blockCacheTestBody = `{"number":"0x64","hash":"0xabc123","parentHash":"0xdef456","timestamp":"0x6b49d200","logsBloom":"0x00","transactions":["0x11","0x22"]}`

type blockServingUpstream struct {
	*suggestGateUpstream
	body []byte
}

func (u *blockServingUpstream) Forward(_ context.Context, req *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), u.body, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr).SetUpstream(u), nil
}

type cacheSetRecord struct {
	method string
	params []interface{}
	result string
	seq    int
}

type recordingCache struct {
	mu   sync.Mutex
	sets []cacheSetRecord
	seq  int
}

func (c *recordingCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	rq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return err
	}
	rs, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.sets = append(c.sets, cacheSetRecord{method: rq.Method, params: rq.Params, result: string(rs.GetResultBytes()), seq: c.seq})
	return nil
}
func (c *recordingCache) Get(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (c *recordingCache) IsObjectNull() bool { return c == nil }
func (c *recordingCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sets)
}

type blockCacheTestNetwork struct{}

func (blockCacheTestNetwork) Id() string                                    { return "evm:123" }
func (blockCacheTestNetwork) Label() string                                 { return "evm:123" }
func (blockCacheTestNetwork) ProjectId() string                             { return "test" }
func (blockCacheTestNetwork) Architecture() common.NetworkArchitecture      { return common.ArchitectureEvm }
func (blockCacheTestNetwork) Config() *common.NetworkConfig                 { return nil }
func (blockCacheTestNetwork) Logger() *zerolog.Logger                       { l := zerolog.Nop(); return &l }
func (blockCacheTestNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (blockCacheTestNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (blockCacheTestNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnfinalized
}

func TestPollLatestBlock_CachesTheBlockByHashBeforePublishingTheHead(t *testing.T) {
	up := &blockServingUpstream{suggestGateUpstream: newSuggestGateUpstream(123, "0x7b", nil), body: []byte(blockCacheTestBody)}
	p := newGateTestPoller(t, up)
	cache := &recordingCache{}
	p.SetBlockCache(blockCacheTestNetwork{}, cache)

	// Observe the head the way the stream hub does, and note how many cache
	// writes had landed when it fired.
	var mu sync.Mutex
	var heads []data.CounterInt64Observation
	var writesAtHead []int
	p.OnLatestBlock(func(o data.CounterInt64Observation) {
		mu.Lock()
		defer mu.Unlock()
		heads = append(heads, o)
		writesAtHead = append(writesAtHead, cache.count())
	})

	n, err := p.PollLatestBlockNumber(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(100), n)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(heads) > 0 }, 2*time.Second, 10*time.Millisecond)

	// The block went into the cache under its hash, verbatim.
	require.Equal(t, 1, cache.count(), "exactly one cache write for one fetched block")
	set := cache.sets[0]
	assert.Equal(t, "eth_getBlockByHash", set.method)
	assert.Equal(t, []interface{}{"0xabc123", false}, set.params)
	assert.JSONEq(t, blockCacheTestBody, set.result, "the cached body must be exactly what the upstream returned, transaction hashes included")

	// The head observation carries the hash, and the cache write preceded it.
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, data.CounterInt64Observation{Value: 100, BlockTimestamp: 0x6b49d200, BlockHash: "0xabc123"}, heads[0])
	assert.Equal(t, 1, writesAtHead[0], "the head must be published only after the block is in the cache")
}

func TestPollLatestBlock_NoCacheMeansNoWrite(t *testing.T) {
	// A poller that was never wired to a network and cache (tests, SVM, a
	// network without evmJsonRpcCache) must behave exactly as before.
	up := &blockServingUpstream{suggestGateUpstream: newSuggestGateUpstream(123, "0x7b", nil), body: []byte(blockCacheTestBody)}
	p := newGateTestPoller(t, up)

	n, err := p.PollLatestBlockNumber(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(100), n)

	cache := &recordingCache{}
	p.SetBlockCache(nil, cache)
	p.latestBlockShared.TryUpdate(t.Context(), 0) // no-op; just proves nothing panics without a network
	assert.Zero(t, cache.count())
}

func TestPollFinalizedBlock_IsNotCachedByHash(t *testing.T) {
	// The poller fetches a finalized block every tick too. Nothing reads it by
	// hash, and the cache would file it under the unfinalized policy (the head
	// it names is not stored yet when the write happens), so it must not be
	// written at all. RHI-8260.
	up := &blockServingUpstream{suggestGateUpstream: newSuggestGateUpstream(123, "0x7b", nil), body: []byte(blockCacheTestBody)}
	p := newGateTestPoller(t, up)
	cache := &recordingCache{}
	p.SetBlockCache(blockCacheTestNetwork{}, cache)

	n, err := p.PollFinalizedBlockNumber(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(100), n)
	assert.Zero(t, cache.count(), "a finalized-tag fetch must not be cached by hash")

	// The latest fetch of the same block still is.
	_, err = p.PollLatestBlockNumber(t.Context())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cache.count() == 1 }, 2*time.Second, 10*time.Millisecond)
}
