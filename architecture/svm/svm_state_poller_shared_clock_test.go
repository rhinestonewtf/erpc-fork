package svm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The getSlot polls must debounce on the counters' SHARED staleness clock, so a
// slot a peer fetched inside the window suppresses this replica's fetch: one
// upstream call per cluster per window. This fails if the call sites drift back
// to a fetch gated only by the pod-local lastPollAt.
func TestPollSlotUsesTheSharedClock(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(5 * time.Second)

	// Slots arrive through the shared counters without this replica fetching
	// (a peer's poll). TryUpdate, not Suggest*, so the traffic gate stays closed
	// and only the shared clock can suppress the fetch.
	p.latestSlotShared.TryUpdate(t.Context(), 1000)
	p.finalizedSlotShared.TryUpdate(t.Context(), 990)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, up.callCount("getSlot:processed"),
		"a slot fresh in shared state must suppress getSlot(processed); the poll is back on a pod-local clock")
	require.Zero(t, up.callCount("getSlot:finalized"),
		"a slot fresh in shared state must suppress getSlot(finalized); the poll is back on a pod-local clock")
	require.Equal(t, 1, up.callCount("getMaxShredInsertSlot"), "getMaxShredInsertSlot stays per replica")
	require.Equal(t, 1, up.callCount("getHealth"), "getHealth stays per replica")
}

// A failed fetch must not satisfy the clock for longer than one window, or a
// transient upstream error would freeze the slot.
func TestPollSlotRetriesAfterAFailedFetch(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	up.scriptError("getSlot:processed", -32005, "node is behind")
	p := newPollerWithUpstream(t, up)

	const debounce = 100 * time.Millisecond
	p.SetDebounceInterval(debounce)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, p.LatestSlot())

	up.script("getSlot:processed", []byte(`1000`))
	time.Sleep(debounce + 40*time.Millisecond)
	require.NoError(t, p.Poll(context.Background()))
	require.Equal(t, 2, up.callCount("getSlot:processed"))
	require.Equal(t, int64(1000), p.LatestSlot())
}
