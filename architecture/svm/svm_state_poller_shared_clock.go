package svm

import (
	"context"
	"errors"
	"time"

	"github.com/erpc/erpc/data"
)

var errNoSlotInResponse = errors.New("getSlot returned no slot")

// maxLagSampleAge bounds how old the processed slot may be when it is
// subtracted from a fresh getMaxShredInsertSlot sample. On the shared clock the
// processed slot can be a full debounce old, and statePollerDebounce has no
// upper limit (devnet runs 30s, ~75 slots), so an unbounded age would read as
// ingestion lag and cordon a healthy upstream. 10s is ~25 slots at 400ms, a
// quarter of MaxShredInsertSlotLagThreshold. Above the default debounces, so
// it costs nothing there.
const maxLagSampleAge = 10 * time.Second

type slotPoll int

const (
	slotShared slotPoll = iota // fetch unless a replica refreshed the counter inside the window
	slotSkip                   // traffic-gated: no fetch this tick
	slotForce                  // fetch directly, bypassing the shared clock
)

// slotPollFor is the traffic gate for one slot view. Traffic that keeps a view
// fresh also keeps its shared clock fresh, so the skip cap must count every
// traffic-fresh tick of that view, not only ticks where both views are fresh,
// or one busy method starves the view's own observation indefinitely. A view
// whose shared clock is fresh from a peer's poll is not counted: that was an
// observation, and the peer's own cap bounds any traffic it saw. No debounce
// means no gate. Callers hold pollMu.
func slotPollFor(skips *int, externalAt int64, d time.Duration) slotPoll {
	if d <= 0 {
		return slotForce
	}
	if externalAt > 0 && time.Since(time.UnixMilli(externalAt)) < d {
		if *skips < maxConsecutiveSlotPollSkips {
			*skips++
			return slotSkip
		}
		*skips = 0
		return slotForce
	}
	*skips = 0
	return slotShared
}

// slotFreshForLag reports whether the processed slot was accepted recently
// enough to subtract from a shred sample taken now (see maxLagSampleAge).
func (e *SvmStatePoller) slotFreshForLag() bool {
	at := e.latestSlotSeenAt.Load()
	return at > 0 && time.Since(time.UnixMilli(at)) <= maxLagSampleAge
}

// pollSlot debounces a getSlot fetch on the counter's SHARED staleness clock,
// so a slot any replica accepted inside the window (its own poll, a peer's, or
// live traffic) suppresses this replica's fetch: one call per cluster per
// window instead of one per pod. Mirrors the EVM latest poll
// (latestBlockShared.TryUpdateIfStale).
//
// force bypasses the clock and waits for the answer: the traffic gate's
// skip-cap tick, which must observe the upstream itself, and a processed slot
// too old to compute shred lag against.
//
// A failed or empty fetch marks only this replica's clock fresh and pushes
// nothing, so peers still fetch on their own next window.
func (e *SvmStatePoller) pollSlot(ctx context.Context, v data.CounterInt64SharedVariable, payload []byte, staleness time.Duration, force bool) {
	if force {
		if slot, err := e.fetchSlot(ctx, payload); err == nil && slot > 0 {
			v.TryUpdate(e.appCtx, slot)
		}
		return
	}
	_, _ = v.TryUpdateIfStale(ctx, staleness, func(ctx context.Context) (data.CounterInt64Observation, error) {
		slot, err := e.fetchSlot(ctx, payload)
		if err == nil && slot <= 0 {
			// A zero would read as a whole-chain rollback; see processNewValue.
			err = errNoSlotInResponse
		}
		return data.CounterInt64Observation{Value: slot}, err
	})
}
