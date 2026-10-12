package evm

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// Fork patch RHI-8244. The latest poll must debounce on the SHARED staleness
// clock (TryUpdateIfStale), so that a head a peer fetched inside the debounce
// window suppresses this replica's fetch: one upstream call per cluster per
// window. Upstream #1208 put it on a replica-local clock (TryUpdateIfLocallyStale),
// under which every replica fetches every window. This fails if a rebase moves
// the call site back.

type fetchCountingUpstream struct {
	*suggestGateUpstream
	forwards atomic.Int32
}

func (u *fetchCountingUpstream) Forward(ctx context.Context, req *common.NormalizedRequest, a, b bool) (*common.NormalizedResponse, error) {
	u.forwards.Add(1)
	return u.suggestGateUpstream.Forward(ctx, req, a, b)
}

func TestPollLatestBlockNumberUsesTheSharedClock(t *testing.T) {
	up := &fetchCountingUpstream{suggestGateUpstream: newSuggestGateUpstream(123, "0x7b", nil)}
	p := newGateTestPoller(t, up)

	// A head arrives through the shared counter without this replica fetching
	// (a peer's poll, or request traffic). That marks the SHARED clock fresh and
	// leaves the replica-local one untouched.
	p.latestBlockShared.TryUpdate(t.Context(), 1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	// Debounce resolves to the 1s floor here (no config, no EMA), so a poll
	// right now is inside the window: on the shared clock it must not fetch.
	_, err := p.PollLatestBlockNumber(t.Context())
	require.NoError(t, err)
	require.Zero(t, up.forwards.Load(),
		"a head fresh in shared state must suppress the fetch; the latest poll is back on the replica-local clock")
}
