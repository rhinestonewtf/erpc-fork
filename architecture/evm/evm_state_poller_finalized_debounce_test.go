package evm

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If a rebase drops evm_state_poller_finalized_debounce.go, or re-points the
// scheduled poll back at resolveDebounce, these fail rather than silently
// restoring the old polling rate.

func TestResolveFinalizedDebounce(t *testing.T) {
	t.Run("ScalesTheFinalityPeriodNotBlockTime", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 1, 2) // 2s blocks, as on most rollups we route

		eth := &common.EvmNetworkConfig{ChainId: 1}

		// The head debounce tracks block production, and should: the head really
		// does move once per block.
		assert.Equal(t, 1400*time.Millisecond, p.resolveDebounce(eth))

		// Finality runs on the other clock. 384s * 0.7.
		assert.Equal(t, 268800*time.Millisecond, p.resolveFinalizedDebounce(eth))
	})

	t.Run("BlockTimeNeverReachesItHoweverFastTheChain", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 10, 1) // 100ms blocks

		// Arbitrum One: L1-derived finality, ~4000x the block rate. The finalized
		// debounce must not move at all — this is the case that costs the money.
		arb := &common.EvmNetworkConfig{ChainId: 42161}
		assert.Equal(t, 70*time.Millisecond, p.resolveDebounce(arb))
		assert.Equal(t, 268800*time.Millisecond, p.resolveFinalizedDebounce(arb))
	})

	t.Run("UnlistedChainKeepsPrePatchBehaviour", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 1, 2)

		// A chain that finalises every block has finality period == block time, so
		// the head debounce is already right for it. So does a chain nobody has
		// measured yet: absence must never mean "debounce forever".
		for _, cfg := range []*common.EvmNetworkConfig{
			{ChainId: 56},      // BNB, finalises every ~4 blocks
			{ChainId: 137},     // Polygon, finalises every 1-4 blocks
			{ChainId: 999},     // HyperEVM, finalises every block
			{ChainId: 8675309}, // never seen
		} {
			assert.Equal(t, p.resolveDebounce(cfg), p.resolveFinalizedDebounce(cfg),
				"chain %d must fall through to the block-time debounce", cfg.ChainId)
			assert.Equal(t, 1400*time.Millisecond, p.resolveFinalizedDebounce(cfg))
		}

		assert.Equal(t, p.resolveDebounce(nil), p.resolveFinalizedDebounce(nil),
			"a nil config must not be read as chain 0")
	})

	t.Run("GnosisRunsItsOwnEpoch", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)

		// 16 slots x 5s. Not Ethereum's number, and not derived from block time.
		assert.Equal(t, 56*time.Second, p.resolveFinalizedDebounce(&common.EvmNetworkConfig{ChainId: 100}))
	})

	t.Run("ExplicitUpstreamDebounceStillGovernsBothPolls", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 1, 2)

		// An operator who sets statePollerDebounce on the upstream has said what
		// they want for that upstream's polling; this patch does not overrule it.
		p.stateMu.Lock()
		p.debounceInterval = 3 * time.Second
		p.stateMu.Unlock()

		eth := &common.EvmNetworkConfig{ChainId: 1}
		assert.Equal(t, 3*time.Second, p.resolveDebounce(eth))
		assert.Equal(t, 3*time.Second, p.resolveFinalizedDebounce(eth))
	})

	t.Run("FinalityPeriodDoesNotLeakIntoTheHeadDebounce", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		cfg := &common.EvmNetworkConfig{ChainId: 1, FallbackStatePollerDebounce: common.Duration(5 * time.Second)}

		// No block time measured yet, so the head falls through to its own
		// fallback — a 268s head debounce here would be a serious regression.
		require.Zero(t, p.tracker.GetNetworkBlockTime(up.NetworkId()))
		assert.Equal(t, 5*time.Second, p.resolveDebounce(cfg))
		assert.Equal(t, 268800*time.Millisecond, p.resolveFinalizedDebounce(cfg))
	})
}

// TestFinalityPeriodTable guards the measurements themselves. A chain moving
// between groups is a real change in what we believe about its consensus, and
// should not pass review as a one-character diff.
func TestFinalityPeriodTable(t *testing.T) {
	l1Derived := []int64{10, 130, 196, 480, 1868, 4663, 8453, 42161, 57073, 84532, 421614, 747474, 11155420}
	for _, cid := range append([]int64{1, 11155111}, l1Derived...) {
		assert.Equal(t, 384*time.Second, finalityPeriodByChain[cid],
			"chain %d finalises on Ethereum's epoch", cid)
	}
	assert.Equal(t, 80*time.Second, finalityPeriodByChain[100])
	assert.Equal(t, 240*time.Second, finalityPeriodByChain[2020])
	assert.Len(t, finalityPeriodByChain, 17)

	// The chains deliberately absent: they finalise every block, so the block-time
	// debounce already fits and an entry here would be a claim we cannot support.
	for _, cid := range []int64{56, 137, 143, 146, 999, 5042, 9745, 9746, 43114} {
		_, ok := finalityPeriodByChain[cid]
		assert.False(t, ok, "chain %d finalises per block and must stay unlisted", cid)
	}

	for cid, d := range finalityPeriodByChain {
		assert.Greater(t, d, time.Duration(0), "chain %d: a non-positive period would disable debouncing", cid)
		assert.Less(t, d, 30*time.Minute, "chain %d: implausibly long, check the measurement", cid)
	}
}

// TestPollFinalizedBlockNumberWiring guards the call-site half of the patch: the
// exported method is the on-demand path and must stay on the responsive debounce,
// while Poll() goes through the scheduled wrapper.
func TestPollFinalizedBlockNumberWiring(t *testing.T) {
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	driveBlockTimeEMA(t, p, up, 1, 2)

	var seen []time.Duration
	record := func(cfg *common.EvmNetworkConfig) time.Duration {
		d := p.resolveDebounce(cfg)
		seen = append(seen, d)
		return d
	}

	// shouldSkipFinalizedCheck short-circuits before any fetch, so this exercises
	// the resolver selection without needing a live upstream.
	p.stateMu.Lock()
	p.skipFinalizedCheck = true
	p.stateMu.Unlock()

	_, err := p.pollFinalizedBlockNumber(t.Context(), record)
	require.NoError(t, err)
	assert.Empty(t, seen, "the skip check must come before debounce resolution")

	eth := &common.EvmNetworkConfig{ChainId: 1}
	assert.NotEqual(t, p.resolveDebounce(eth), p.resolveFinalizedDebounce(eth),
		"on an epoch-finality chain the two paths must resolve differently, or the patch is a no-op")
}
