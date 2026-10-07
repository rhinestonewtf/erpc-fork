package common

// Fork patch (rhinestonewtf/erpc-fork, RHI-8079) — registered in PATCH_LIST.md.
//
// CorroboratedHeadIndex picks the network head from a ballot of upstream heads
// sorted DESCENDING (ServedTipPick.Sorted): the highest head when the next one
// is within DefaultToleratedBlockHeadRollback of it, otherwise the
// second-highest. It returns an index into `sorted`; 0 for a ballot of one.
//
// Upstream's corroborated head is the plain second-highest, so that one
// wrong-chain upstream cannot make every honest upstream read as far behind.
// On a network with two upstreams that is always the LOWER head: `latest` is
// interpolated to whichever upstream is behind while eth_blockNumber reports
// the fresher one, so a client that waits for block N and then reads `latest`
// reads N-1 until the slower poller catches up. Honest upstreams routinely sit
// a block or two apart, which made that the steady state rather than an edge.
//
// The tolerance is the one the state poller, the health tracker and the
// served-tip regression guard already use to tell lagging-provider noise from
// a bogus or corrected head, so this adds no new threshold: a highest head
// within that noise of the next is a fresher honest view, and one beyond it is
// still treated as the uncorroborated outlier upstream guards against.
//
// BOUNDARY: a lone bogus head that lands within the tolerance of the honest
// ones is served, as it was before 0.3.0 (when the head was the plain max);
// requests interpolated to it still route only to upstreams that report the
// block, under enforceBlockAvailability.
func CorroboratedHeadIndex(sorted []ServedTipInput) int {
	if len(sorted) < 2 {
		return 0
	}
	if sorted[0].BlockNumber-sorted[1].BlockNumber <= DefaultToleratedBlockHeadRollback {
		return 0
	}
	return 1
}
