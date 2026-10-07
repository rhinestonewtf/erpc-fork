package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

// Fork patch RHI-8079 — see PATCH_LIST.md. Fails if evmHeadReference stops
// calling common.CorroboratedHeadIndex: the head behind `latest`/`finalized`
// interpolation and the eth_blockNumber floor must be the fresher of two
// honest upstreams, not the lagging one.

func TestNetworkHead_TwoHonestUpstreams_ServesTheFresher(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "fresh", latest: 1000},
		{id: "slow", latest: 999},
	})

	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
	// The fixture sets each finalized head 10 below its latest.
	assert.Equal(t, int64(990), network.EvmHighestFinalizedBlockNumber(ctx))
}

func TestNetworkHead_FarAheadPartner_StillNotServed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "honest", latest: 1000},
		{id: "rogue", latest: 1000 + common.DefaultToleratedBlockHeadRollback + 1},
	})

	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
}
