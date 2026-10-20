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

// Traffic that keeps only ONE view fresh also keeps that view's shared clock
// fresh, so the skip cap must apply per view: the busy view is still fetched on
// the fifth poll.
func TestTrafficGate_OneBusyViewIsStillObservedEveryFifthPoll(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)

	const debounce = 100 * time.Millisecond
	p.SetDebounceInterval(debounce)

	wantProcessed := []int{0, 0, 0, 0, 1, 1}
	for i, want := range wantProcessed {
		if i > 0 {
			time.Sleep(debounce + 40*time.Millisecond)
		}
		p.SuggestLatestSlot(int64(2000 + i))
		require.NoError(t, p.Poll(context.Background()))
		require.Equal(t, i+1, p.pollCount, "poll %d did not run", i+1)
		require.Equal(t, want, up.callCount("getSlot:processed"), "after poll %d", i+1)
		require.Equal(t, i+1, up.callCount("getSlot:finalized"), "the quiet view polls every time (poll %d)", i+1)
	}
}

func backdateLatestSlot(p *SvmStatePoller, age time.Duration) {
	p.latestSlotSeenAt.Store(time.Now().Add(-age).UnixMilli())
}

// With a long debounce the shared processed slot can be many slots old. It
// must be re-fetched, synchronously, before it is subtracted from a fresh
// shred sample; otherwise its age reads as ingestion lag and cordons a
// healthy upstream.
func TestShredLag_RefetchesAProcessedSlotTooOldToCompare(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	up.script("getSlot:processed", []byte(`1195`))
	up.script("getMaxShredInsertSlot", []byte(`1200`))
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(60 * time.Second)

	p.latestSlotShared.TryUpdate(t.Context(), 1000) // fresh shared clock, old value
	backdateLatestSlot(p, time.Minute)

	require.NoError(t, p.Poll(context.Background()))
	require.Equal(t, 1, up.callCount("getSlot:processed"))
	require.Equal(t, int64(5), p.MaxShredInsertSlotLag())
	require.True(t, p.IsHealthy())
	require.Empty(t, up.cordons())
}

// If that re-fetch fails, the lag is left alone rather than computed against
// the old slot.
func TestShredLag_SkippedWhenTheProcessedSlotStaysTooOld(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	up.scriptError("getSlot:processed", -32005, "node is behind")
	up.script("getMaxShredInsertSlot", []byte(`1200`))
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(60 * time.Second)

	p.latestSlotShared.TryUpdate(t.Context(), 1000)
	backdateLatestSlot(p, time.Minute)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, p.MaxShredInsertSlotLag())
	require.True(t, p.IsHealthy())
	require.Empty(t, up.cordons())
}

// A node whose replay has stalled keeps answering getSlot(processed) with the
// same slot while shreds keep arriving. The counter fires no OnValue for an
// unchanged slot, so the poller's own successful fetch must still count as
// fresh, or the detector never sees the lag it exists to catch.
func TestShredLag_StalledProcessedSlotStillCordons(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	up.script("getSlot:processed", []byte(`1000`))
	up.script("getMaxShredInsertSlot", []byte(`1200`))
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(30 * time.Second)

	p.latestSlotShared.TryUpdate(t.Context(), 1000)
	backdateLatestSlot(p, time.Minute) // the stall began long ago

	require.NoError(t, p.Poll(context.Background()))
	require.Equal(t, 1, up.callCount("getSlot:processed"))
	require.Equal(t, int64(200), p.MaxShredInsertSlotLag())
	require.False(t, p.IsHealthy())
	require.Len(t, up.cordons(), 1)
}
