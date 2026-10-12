package data

// Fork patch RHI-8244: the block timestamp travels with the head through shared
// state.
//
// Upstream #1208 moved the EVM "latest" poll from the shared staleness clock to a
// replica-local one, because a replica that only ever RECEIVED a head through the
// shared counter had no block timestamp to feed its block-time estimate. The
// fix here is the other way round: the counter carries the timestamp next to the
// value (CounterInt64State.BlockTimestamp) and hands both to OnValue, so a
// receiving replica samples block time from the propagated observation and the
// fetch can stay on the shared clock — one upstream call per cluster per
// debounce window, not one per pod.
//
// adoptBlockTimestamp is the one piece of logic beyond "store it beside the
// value": a head can reach the counter WITHOUT a timestamp (SuggestLatestBlock
// from request traffic, or a peer still on the previous wire format) and the
// same head can arrive again WITH one (the fetching replica's poll). The second
// observation fills the gap and fires the callback so the tracker can attach the
// timestamp to the head it already holds; it never overwrites a timestamp that
// is already set, so a value and its timestamp always come from one observation.
//
// The value and its timestamp are two atomics, written value-first. Callbacks
// get both as arguments from the writer, so they always see a matched pair; the
// only reader that loads them separately is localState, in the background push,
// which can in principle publish a head with the previous head's timestamp. The
// cost of that window is at most one mis-attributed sample in a ~19-sample EMA
// on the receiving replicas, from a push that runs off the request path; not
// worth a lock on the counter's hot path.
func (c *counterInt64) adoptBlockTimestamp(value int64, blockTimestamp int64) bool {
	if blockTimestamp <= 0 {
		return false
	}
	if c.value.Load() != value {
		return false
	}
	if !c.blockTimestamp.CompareAndSwap(0, blockTimestamp) {
		return false
	}
	c.triggerValueCallback(value, blockTimestamp)
	return true
}
