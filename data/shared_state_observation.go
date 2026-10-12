package data

// Fork patches RHI-8244 and RHI-8178: a head travels through shared state as an
// observation, not a bare number.
//
// Upstream #1208 moved the EVM "latest" poll from the shared staleness clock to a
// replica-local one, because a replica that only ever RECEIVED a head through the
// shared counter had no block timestamp to feed its block-time estimate. The
// fix here is the other way round: the counter carries what the writer saw
// (CounterInt64State.BlockTimestamp, BlockHash) next to the value and hands all
// of it to OnValue, so a receiving replica samples block time from the peer's
// observation and can find the block in the shared cache by hash, while the
// fetch stays on the shared clock — one upstream call per cluster per debounce
// window, not one per pod.
//
// adoptObservation is the one piece of logic beyond "store it beside the value":
// a head can reach the counter WITHOUT its block fields (SuggestLatestBlock from
// request traffic, or a peer on an older wire format) and the same head can
// arrive again WITH them (the fetching replica's poll). The second observation
// fills the gaps and fires the callback so the tracker and the stream hub can
// attach them to the head they already hold. A fetched observation that names a
// DIFFERENT hash at the same height supersedes the held one (the chain
// reorganised under the tip); an older record never supersedes.
//
// The value and its block fields are separate atomics, written value-first.
// Callbacks get the observation as an argument from the writer, so they always
// see a matched set; the only reader that loads them separately is localState,
// in the background push, which can in principle publish a head with the
// previous head's fields. The cost of that window is at most one mis-attributed
// block-time sample in a ~19-sample EMA on the receiving replicas, from a push
// that runs off the request path; not worth a lock on the counter's hot path.

// CounterInt64Observation is one writer's reading of the counter: the value, and
// whatever the writer kept of the block it read the value from. A zero field
// means unknown, never "none".
type CounterInt64Observation struct {
	Value          int64
	BlockTimestamp int64  // on-chain timestamp, unix seconds
	BlockHash      string // 0x-prefixed block hash
}

func observationOf(st CounterInt64State) CounterInt64Observation {
	return CounterInt64Observation{Value: st.Value, BlockTimestamp: st.BlockTimestamp, BlockHash: st.BlockHash}
}

// storeObservation records the block fields of an observation whose value was
// just accepted. Called right after the value CAS; never on an equal value.
func (c *counterInt64) storeObservation(obs CounterInt64Observation) {
	c.blockTimestamp.Store(obs.BlockTimestamp)
	hash := obs.BlockHash
	c.blockHash.Store(&hash)
}

// observation is the counter's current reading.
func (c *counterInt64) observation() CounterInt64Observation {
	obs := CounterInt64Observation{Value: c.value.Load(), BlockTimestamp: c.blockTimestamp.Load()}
	if h := c.blockHash.Load(); h != nil {
		obs.BlockHash = *h
	}
	return obs
}

// adoptObservation merges obs into the held observation of the SAME value and
// fires the callback when anything changed. Missing fields are filled in from
// obs. With supersede, a different hash in obs replaces the held hash and
// timestamp together (one block, one set of fields); without it the held block
// wins, which is what an older record gets. Returns whether anything changed.
func (c *counterInt64) adoptObservation(obs CounterInt64Observation, supersede bool) bool {
	if c.value.Load() != obs.Value {
		return false
	}
	held := c.observation()
	if held.Value != obs.Value {
		return false
	}

	changed := false
	if obs.BlockHash != "" && held.BlockHash != "" && obs.BlockHash != held.BlockHash {
		if !supersede {
			return false
		}
		// A different block at the same height: take its fields as a set.
		c.storeObservation(obs)
		changed = true
	} else {
		if obs.BlockTimestamp > 0 && held.BlockTimestamp == 0 && c.blockTimestamp.CompareAndSwap(0, obs.BlockTimestamp) {
			changed = true
		}
		if obs.BlockHash != "" && held.BlockHash == "" {
			hash := obs.BlockHash
			if prev := c.blockHash.Load(); prev == nil || *prev == "" {
				if c.blockHash.CompareAndSwap(prev, &hash) {
					changed = true
				}
			}
		}
	}
	if !changed {
		return false
	}
	merged := c.observation()
	if merged.Value != obs.Value {
		// The value moved on while we were filling in the old one; the newer
		// observation already fired with its own fields.
		return false
	}
	c.triggerValueCallback(merged)
	return true
}
