package health

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// Fork patch RHI-8079 — see PATCH_LIST.md. Fails if the tracker stops calling
// common.CorroboratedHeadIndex.

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
