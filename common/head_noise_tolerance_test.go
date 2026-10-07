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

func TestCorroboratedHeadIndex(t *testing.T) {
	cases := []struct {
		name   string
		ballot []ServedTipInput
		want   int
	}{
		{"empty ballot", nil, 0},
		{"sole reporter", headBallot(100), 0},
		{"two honest upstreams a block apart take the fresher", headBallot(100, 99), 0},
		{"equal heads", headBallot(100, 100), 0},
		{"gap at the tolerance is still noise", headBallot(DefaultToleratedBlockHeadRollback+1, 1), 0},
		{"gap past the tolerance is an outlier", headBallot(DefaultToleratedBlockHeadRollback+2, 1), 1},
		{"wrong-chain head over two honest ones", headBallot(62_381_379, 32_610_710, 32_610_710), 1},
		{"honest leader over a stale third", headBallot(1000, 999, 100), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CorroboratedHeadIndex(tc.ballot))
		})
	}
}
