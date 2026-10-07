package common

// Fork patch (rhinestonewtf/erpc-fork, RHI-8079) — registered in PATCH_LIST.md.

// CorroboratedHeadLeadTolerance is how far the highest head may lead the next
// one and still be served as the network head. It is the block lag at which
// the default selection policy excludes an upstream as behind
// (`blockNumberLagAbove(16)` in internal/policy/default_policy.js; a test pins
// the two together). Within it, serving the leader cannot push its partner
// out of rotation; beyond it, it would — the failure upstream's second-highest
// head exists to prevent — so the second-highest is served instead.
const CorroboratedHeadLeadTolerance int64 = 16

// CorroboratedHeadIndex picks the network head from a ballot of upstream heads
// sorted DESCENDING (ServedTipPick.Sorted): the highest head when it leads the
// next by at most CorroboratedHeadLeadTolerance, otherwise the second-highest.
// It returns an index into `sorted`; 0 for a ballot of one.
//
// Upstream's corroborated head is the plain second-highest. On a network with
// two upstreams that is always the LOWER head: `latest` is interpolated to
// whichever upstream is behind while eth_blockNumber reports the fresher one,
// so a client that waits for block N and then reads `latest` reads N-1 until
// the slower poller catches up — the steady state, since honest upstreams poll
// out of phase and routinely sit a block apart.
//
// BOUNDARIES:
//   - A lone bogus head within the tolerance is served, so it can move `latest`
//     ahead by at most that many blocks; requests interpolated to it still
//     route only to upstreams that report the block (enforceBlockAvailability).
//   - Where honest heads routinely sit further apart than the tolerance (a fast
//     chain whose slowest poller refreshes less often than ~16 blocks), this
//     falls back to upstream's second-highest head.
//   - A selection policy that overrides the default lag threshold does not move
//     this tolerance.
func CorroboratedHeadIndex(sorted []ServedTipInput) int {
	if len(sorted) < 2 {
		return 0
	}
	if sorted[0].BlockNumber-sorted[1].BlockNumber <= CorroboratedHeadLeadTolerance {
		return 0
	}
	return 1
}
