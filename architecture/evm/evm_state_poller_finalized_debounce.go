package evm

import (
	"context"
	"math"
	"time"

	"github.com/erpc/erpc/common"
)

// finalityDebounceMultiplier scales a chain's finality period the way
// DefaultDynamicBlockTimeDebounceMultiplier scales its block time: poll a little
// more often than the value can change, so a step is picked up promptly.
const finalityDebounceMultiplier = 0.7

// finalityPeriodByChain is how often each chain's consensus ADVANCES the finalized
// pointer. It lives in code, not config, for two reasons. The values are protocol
// constants — Ethereum's epoch is 32 slots of 12s and will not be retuned by an
// operator — and eRPC parses config with KnownFields(true), so a config key that
// only this fork understands turns a lost patch into a start-up failure rather than
// a silent revert to upstream behaviour.
//
// A chain absent from this table falls through to the block-time debounce, which is
// already correct for the large group that finalises every block: there, finality
// period and block time are the same quantity. Absence is therefore not an omission
// and the table needs no entry for a new chain to behave sanely.
//
// Measured 2026-10-07 against public nodes, NOT through rpc-proxy: eRPC rewrites a
// client's "finalized" tag into its own state-poller value (architecture/evm/json_rpc.go),
// so the proxy can only ever report its own pointer back.
var finalityPeriodByChain = map[int64]time.Duration{
	// Casper FFG finalises at epoch boundaries: 32 slots x 12s. Measured exactly,
	// 32 blocks per step on mainnet and 31 on Sepolia (one missed slot).
	1:        384 * time.Second, // Ethereum
	11155111: 384 * time.Second, // Sepolia

	// `finalized` derived from L1 finality, so these inherit Ethereum's epoch while
	// producing blocks every 1-2s — the widest gap between the two clocks, and most
	// of the saving. Corroborated where the public endpoint was stable: Base 374s,
	// Unichain 383s, Base Sepolia 378s, Arbitrum Sepolia 378s, Robinhood 385s,
	// Katana 420s. OP Mainnet, Soneium, OP Sepolia and X Layer sat behind load
	// balancers whose nodes disagreed, so they rest on the shared mechanism.
	10:       384 * time.Second, // OP Mainnet
	130:      384 * time.Second, // Unichain
	196:      384 * time.Second, // X Layer
	480:      384 * time.Second, // World Chain
	1868:     384 * time.Second, // Soneium
	4663:     384 * time.Second, // Robinhood
	8453:     384 * time.Second, // Base
	42161:    384 * time.Second, // Arbitrum One
	57073:    384 * time.Second, // Ink
	84532:    384 * time.Second, // Base Sepolia
	421614:   384 * time.Second, // Arbitrum Sepolia
	747474:   384 * time.Second, // Katana
	11155420: 384 * time.Second, // OP Sepolia

	// Same FFG mechanism, its own parameters: 16 slots x 5s. Measured 14 blocks /
	// 75s minimum, 80s median, no divergence between samples.
	100: 80 * time.Second, // Gnosis

	// Ronin is the one entry measured rather than derived. Its documented fast
	// finality is ~6s, but the `finalized` TAG advances in ~480s steps and trails
	// head by ~18 minutes (40/40 successful samples, so not a throttled endpoint).
	// With no sourced explanation for the gap, this is declared at half the observed
	// cadence: too short only costs polls, too long leaves the pointer stale.
	2020: 240 * time.Second, // Ronin
}

// resolveFinalizedDebounce returns the debounce for the SCHEDULED finalized poll.
//
//	upstream statePollerDebounce (explicit operator override, governs both polls)
//	  → finalityPeriodByChain * finalityDebounceMultiplier
//	  → resolveDebounce (the head debounce — correct whenever finality is per-block)
//
// This is resolveDebounce's own shape applied to the right quantity. The head
// debounce is blockTime*0.7 because the head advances once per block; the finalized
// pointer advances once per FINALITY period, which on most chains is a different
// clock entirely. Polling the second at the first's rate is the whole waste.
//
// Lagging here is one-directional. A finalized pointer behind reality makes eRPC
// treat a block as not-yet-finalized for longer, which delays cache promotion; it can
// never make eRPC treat an unfinalized block as final, which is the direction that
// would serve wrong data.
func (e *EvmStatePoller) resolveFinalizedDebounce(cfg *common.EvmNetworkConfig) time.Duration {
	e.stateMu.RLock()
	dbi := e.debounceInterval
	e.stateMu.RUnlock()
	if dbi != 0 {
		return dbi
	}
	if cfg != nil {
		if period, ok := finalityPeriodByChain[cfg.ChainId]; ok {
			// Rounded, not truncated: a finality period is minutes, so the float
			// product lands a nanosecond short and that artifact is worth not having.
			return time.Duration(math.Round(float64(period) * finalityDebounceMultiplier))
		}
	}
	return e.resolveDebounce(cfg)
}

// PollFinalizedBlockNumber is the on-demand entry point, reached from
// Upstream.EvmIsBlockFinalized with forceFreshIfStale — i.e. a caller that has
// already seen a stale answer and wants it re-checked before rejecting an upstream
// for a finality-confidence request. It keeps resolveDebounce so that path stays
// exactly as responsive as it was before this patch; the finality debounce governs
// standing background traffic only.
func (e *EvmStatePoller) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return e.pollFinalizedBlockNumber(ctx, e.resolveDebounce)
}

// pollFinalizedBlockNumberScheduled is the background Poll() entry point.
func (e *EvmStatePoller) pollFinalizedBlockNumberScheduled(ctx context.Context) (int64, error) {
	return e.pollFinalizedBlockNumber(ctx, e.resolveFinalizedDebounce)
}
