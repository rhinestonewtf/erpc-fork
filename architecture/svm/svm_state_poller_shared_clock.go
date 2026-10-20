package svm

import (
	"context"
	"errors"
	"time"

	"github.com/erpc/erpc/data"
)

var errNoSlotInResponse = errors.New("getSlot returned no slot")

// pollSlot debounces a getSlot fetch on the counter's SHARED staleness clock,
// so a slot any replica accepted inside the window (its own poll, a peer's, or
// live traffic) suppresses this replica's fetch: one call per cluster per
// window instead of one per pod. Mirrors the EVM latest poll
// (latestBlockShared.TryUpdateIfStale).
//
// force bypasses the clock. It is the traffic gate's skip-cap tick, which must
// observe the upstream directly however fresh traffic has kept the counter. A
// zero staleness bypasses it too: like the rest of Poll, no debounce means no
// gate, and a millisecond clock would otherwise skip back-to-back polls.
//
// A failed or empty fetch marks only this replica's clock fresh and pushes
// nothing, so peers still fetch on their own next window.
func (e *SvmStatePoller) pollSlot(ctx context.Context, v data.CounterInt64SharedVariable, payload []byte, staleness time.Duration, force bool) {
	if force || staleness <= 0 {
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
