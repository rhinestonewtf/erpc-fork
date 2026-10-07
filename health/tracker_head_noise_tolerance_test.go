package health

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// Fork patch RHI-8079 — see PATCH_LIST.md. Fails if the tracker stops calling
// common.CorroboratedHead.

func TestTrackerHeadFollowsFresherOfTwoHonestUpstreams(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-head-noise-two")
	fresh := common.NewFakeUpstream("fresh")
	slow := common.NewFakeUpstream("slow")
	net := fresh.NetworkId()

	// The 2026-10-06 shape: one upstream has the new block, the other has not
	// polled it yet.
	tracker.SetLatestBlockNumber(slow, 26_132_913, 0)
	tracker.SetLatestBlockNumber(fresh, 26_132_914, 0)

	assert.Equal(t, int64(26_132_914), networkLatest(tracker, net))
	assert.Equal(t, int64(0), blockHeadLag(tracker, fresh))
	assert.Equal(t, int64(1), blockHeadLag(tracker, slow),
		"a slower upstream reads as behind, so selection can prefer the fresher one")
}

func TestTrackerHeadStillIgnoresFarAheadPartner(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-head-noise-outlier")
	honest := common.NewFakeUpstream("honest")
	rogue := common.NewFakeUpstream("rogue")
	net := honest.NetworkId()

	tracker.SetLatestBlockNumber(honest, 100, 0)
	tracker.SetLatestBlockNumber(rogue, 100+common.DefaultToleratedBlockHeadRollback+1, 0)

	assert.Equal(t, int64(100), networkLatest(tracker, net))
	assert.Equal(t, int64(0), blockHeadLag(tracker, honest))
}

// The 2026-10-07 dev regression: a leader outrunning a slower poller made the
// network head drop back by the whole gap, logging "re-derived network latest
// block" every few seconds. Capped, the head only moves forward, the partner
// stays at the policy's lag tolerance, and block time keeps being sampled.
func TestTrackerHeadLeaderRunningAwayIsCappedNotDropped(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-head-noise-runaway")
	leader := common.NewFakeUpstream("leader")
	slow := common.NewFakeUpstream("slow")
	net := leader.NetworkId()
	const base = int64(1_000)
	const baseTs = int64(1_800_000_000)
	tol := common.CorroboratedHeadLeadTolerance

	tracker.SetLatestBlockNumber(slow, base, baseTs)
	prev := int64(0)
	for i := int64(1); i <= 3*tol; i++ {
		tracker.SetLatestBlockNumber(leader, base+i, baseTs+i)
		head := networkLatest(tracker, net)
		assert.GreaterOrEqual(t, head, prev, "leader at +%d", i)
		prev = head
	}

	assert.Equal(t, base+tol, networkLatest(tracker, net), "capped at the partner plus the tolerance")
	assert.Equal(t, tol, blockHeadLag(tracker, slow), "partner sits at the policy's lag tolerance")
	assert.Equal(t, int64(0), blockHeadLag(tracker, leader))
	assert.Equal(t, time.Second, tracker.GetNetworkBlockTime(net),
		"block time is sampled from the leader's real blocks while the head is capped")
}
