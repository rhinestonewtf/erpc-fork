package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// Fork patch RHI-8178: the block the poller fetched goes into the network's
// JSON-RPC cache under its hash.
//
// The poller fetches a full block (eth_getBlockByNumber, hashes-only) on every
// tick and, before this patch, kept only the number: its request goes straight
// to the upstream, so nothing it observes ever reaches the cache. The
// StreamBlocks subscriber loop then fetched the same block again by number, and
// chain-indexer's reorg walk (eth_getBlockByHash back to the common ancestor)
// went upstream for blocks the proxy had just held in its hands.
//
// A by-hash entry is content-addressed, so caching it while the block is still
// unfinalized is never wrong, unlike a by-number entry whose mapping changes on
// reorg. The write happens synchronously, before the head is published through
// shared state, so a subscriber that wakes on the new head finds the block in
// the cache (shared Redis, any replica) rather than racing the write. Whether
// anything is stored is the cache policy's call: this is an ordinary Set for
// eth_getBlockByHash at the block's finality, so an environment with no
// unfinalized by-hash policy stores nothing, exactly as a client request would.

// fetchedBlock is what fetchBlock read off the upstream: the number, the
// on-chain timestamp (0 when unparseable) and the hash ("" when absent).
type fetchedBlock struct {
	number    int64
	timestamp int64
	hash      string
}

// SetBlockCache gives the poller the network it polls for and that network's
// cache, so fetched blocks can be written by hash. Either may be nil, in which
// case fetched blocks are not cached. Called once per upstream when the network
// is wired to it.
func (e *EvmStatePoller) SetBlockCache(network common.Network, cache common.CacheDAL) {
	if e == nil {
		return
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.blockCacheNetwork = network
	e.blockCache = cache
}

// cacheBlockByHash stores resp, a block fetched by the poller, as the response
// to eth_getBlockByHash(hash, false) on the poller's network. Best effort: a
// failure is logged at debug and the poll proceeds.
func (e *EvmStatePoller) cacheBlockByHash(ctx context.Context, resp *common.NormalizedResponse, hash string) {
	if hash == "" || resp == nil {
		return
	}
	e.stateMu.RLock()
	network, cache := e.blockCacheNetwork, e.blockCache
	e.stateMu.RUnlock()
	if network == nil || cache == nil || cache.IsObjectNull() {
		return
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}

	req := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByHash","params":["%s",false]}`, util.RandomID(), hash),
	))
	req.SetNetwork(network)
	// A response of its own so the finality the cache derives is for the by-hash
	// request, not the poller's by-tag one; it shares the parsed body, which the
	// poller releases only after this returns.
	byHash := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr).SetUpstream(resp.Upstream())
	if err := cache.Set(ctx, req, byHash); err != nil {
		e.logger.Debug().Err(err).Str("blockHash", hash).Msg("could not cache fetched block by hash")
	}
}
