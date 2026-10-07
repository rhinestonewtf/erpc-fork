package common

// Fork patch (rhinestonewtf/erpc-fork, RHI-8079) — registered in PATCH_LIST.md.

// CorroboratedHeadLeadTolerance is how far the network head may lead the
// second-highest upstream head. It is the block lag at which the default
// selection policy excludes an upstream as behind
// (`blockNumberLagAbove(16)` in internal/policy/default_policy.js; a test pins
// the two together), so a head capped at it never pushes the second-highest
// upstream out of rotation — the failure upstream's second-highest head exists
// to prevent.
const CorroboratedHeadLeadTolerance int64 = 16

// CorroboratedHead derives the network head from a ballot of upstream heads
// sorted DESCENDING (ServedTipPick.Sorted), and the index in `sorted` of the
// upstream whose own head and timestamp are a real block to sample block time
// from. With gap = highest - second-highest:
//
//   - gap <= CorroboratedHeadLeadTolerance: the highest head.
//   - gap <= DefaultToleratedBlockHeadRollback: second-highest +
//     CorroboratedHeadLeadTolerance — a block the leader has, rising with the
//     second-highest, so a leader running further ahead never moves the head
//     back.
//   - otherwise the second-highest: a head that far off is a bogus report (the
//     same bound the poller and tracker use to call a head a correction), as
//     upstream treats it.
//
// The head is not upstream's plain second-highest. On a network with two
// upstreams that is always the LOWER head: `latest` is interpolated to
// whichever upstream is behind while eth_blockNumber reports the fresher one,
// so a client that waits for block N and then reads `latest` reads N-1 until
// the slower poller catches up. Nor is it "highest within the tolerance, else
// second-highest": that drops back by the whole gap each time a fast chain's
// leader outruns a slower poller, so `latest` sawtooths backwards.
//
// BOUNDARIES:
//   - A lone bogus head up to DefaultToleratedBlockHeadRollback ahead can move
//     the head at most CorroboratedHeadLeadTolerance blocks past the
//     second-highest; requests interpolated there route only to upstreams that
//     report the block (enforceBlockAvailability).
//   - A partner capped at exactly the tolerance never trips
//     blockNumberLagAbove(16), even when it is further behind; that is
//     upstream's two-upstream behaviour too.
//   - A selection policy that overrides the default lag threshold does not move
//     this tolerance.
func CorroboratedHead(sorted []ServedTipInput) (head int64, sampleIdx int) {
	if len(sorted) == 0 {
		return 0, -1
	}
	if len(sorted) == 1 {
		return sorted[0].BlockNumber, 0
	}
	leader, second := sorted[0].BlockNumber, sorted[1].BlockNumber
	switch gap := leader - second; {
	case gap <= CorroboratedHeadLeadTolerance:
		return leader, 0
	case gap <= DefaultToleratedBlockHeadRollback:
		return second + CorroboratedHeadLeadTolerance, 0
	default:
		return second, 1
	}
}
