package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fork patch RHI-8079 — see PATCH_LIST.md.

func headBallot(heads ...int64) []ServedTipInput {
	out := make([]ServedTipInput, 0, len(heads))
	for _, h := range heads {
		out = append(out, ServedTipInput{UpstreamID: "u", BlockNumber: h})
	}
	return out
}

func TestCorroboratedHead(t *testing.T) {
	const tol = CorroboratedHeadLeadTolerance
	cases := []struct {
		name       string
		ballot     []ServedTipInput
		wantHead   int64
		wantSample int
	}{
		{"empty ballot", nil, 0, -1},
		{"sole reporter", headBallot(100), 100, 0},
		{"two honest upstreams a block apart take the fresher", headBallot(100, 99), 100, 0},
		{"equal heads", headBallot(100, 100), 100, 0},
		{"lead at the tolerance is served whole", headBallot(100+tol, 100), 100 + tol, 0},
		{"lead past the tolerance is capped, sampled from the leader", headBallot(100+tol+1, 100), 100 + tol, 0},
		{"lead at the outlier bound is still capped", headBallot(100+DefaultToleratedBlockHeadRollback, 100), 100 + tol, 0},
		{"lead past the outlier bound is ignored", headBallot(100+DefaultToleratedBlockHeadRollback+1, 100), 100, 1},
		{"wrong-chain head over two honest ones", headBallot(62_381_379, 32_610_710, 32_610_710), 32_610_710, 1},
		{"honest leader over a stale third", headBallot(1000, 999, 100), 1000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, sample := CorroboratedHead(tc.ballot)
			assert.Equal(t, tc.wantHead, head)
			assert.Equal(t, tc.wantSample, sample)
		})
	}
}

// The 2026-10-07 dev regression: a fast chain's leader outrunning a slower
// poller dropped the head back by the whole gap each time the lead crossed the
// tolerance. The head must never move back while the leader only advances.
func TestCorroboratedHead_LeaderRunningAwayNeverMovesHeadBack(t *testing.T) {
	const partner = int64(1000)
	prev := int64(0)
	for leader := partner; leader <= partner+DefaultToleratedBlockHeadRollback; leader++ {
		head, _ := CorroboratedHead(headBallot(leader, partner))
		assert.GreaterOrEqual(t, head, prev, "leader %d", leader)
		assert.LessOrEqual(t, head, leader, "never past the leader")
		prev = head
	}
}
