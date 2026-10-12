package data

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patches RHI-8244 and RHI-8178. These fail if the block timestamp or hash
// stop travelling with the head through shared state, which is what lets the
// latest poll stay on the shared staleness clock (one fetch per cluster) while
// the replicas that did not fetch still sample block time and find the block in
// the cache by hash.

func newObservationTestCounter(t *testing.T) (*counterInt64, context.Context) {
	t.Helper()
	logger := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	connector, err := NewMemoryConnector(ctx, &logger, "obs", &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"})
	require.NoError(t, err)
	c := &counterInt64{
		registry: &sharedStateRegistry{
			appCtx: ctx, logger: &logger, connector: connector, instanceId: "me",
			fallbackTimeout: time.Second, updateMaxWait: time.Second, lockMaxWait: time.Second, lockTtl: time.Second,
		},
		key:              "obs/latest",
		ignoreRollbackOf: 1024,
	}
	return c, ctx
}

func obs(v, ts int64, hash string) CounterInt64Observation {
	return CounterInt64Observation{Value: v, BlockTimestamp: ts, BlockHash: hash}
}

func TestObservation_ReceivingReplicaSamplesBlockTime(t *testing.T) {
	// The replica under test never fetches. Every head it sees comes from a
	// peer through the shared record, with the peer's block timestamp. Its
	// tracker must still settle a block-time estimate — the whole reason
	// upstream #1208 moved the poll to a local clock was that it could not.
	logger := zerolog.Nop()
	tr := health.NewTracker(&logger, "receiver", time.Minute)
	up := common.NewFakeUpstream("a")
	c, _ := newObservationTestCounter(t)
	c.OnValue(func(o CounterInt64Observation) { tr.SetLatestBlockNumber(up, o.Value, o.BlockTimestamp) })

	base := time.Now().UnixMilli()
	for n := int64(100); n < 110; n++ {
		accepted := c.processNewState("peer", CounterInt64State{
			Value: n, UpdatedAt: base + n, UpdatedBy: "peer", BlockTimestamp: 1_800_000_000 + 2*(n-100),
		})
		require.True(t, accepted, "forward head from a peer must be accepted")
	}
	assert.Equal(t, 2*time.Second, tr.GetNetworkBlockTime(up.NetworkId()),
		"a replica that only ever received heads must still estimate block time from the propagated timestamps")
}

func TestObservation_TravelsWithTheValue(t *testing.T) {
	c, ctx := newObservationTestCounter(t)
	var seen []CounterInt64Observation
	c.OnValue(func(o CounterInt64Observation) { seen = append(seen, o) })

	// A local fetch stores the whole observation and publishes it as one record.
	_, err := c.TryUpdateIfStale(ctx, -time.Second, func(context.Context) (CounterInt64Observation, error) { return obs(10_000, 1_800_000_000, "0xaa"), nil })
	require.NoError(t, err)
	assert.Equal(t, CounterInt64State{Value: 10_000, BlockTimestamp: 1_800_000_000, BlockHash: "0xaa", UpdatedBy: "me", UpdatedAt: c.updatedAtMs()}, c.localState())

	// A head accepted without block fields clears the previous ones: the
	// record always describes a single observation.
	c.TryUpdate(ctx, 10_001)
	assert.Equal(t, obs(10_001, 0, ""), c.observation())

	// A large rollback carries its own fields too.
	c.processNewState("peer", CounterInt64State{Value: 10_001 - 2000, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 7, BlockHash: "0xbb"})
	assert.Equal(t, obs(10_001-2000, 7, "0xbb"), c.observation())

	assert.Equal(t, []CounterInt64Observation{obs(10_000, 1_800_000_000, "0xaa"), obs(10_001, 0, ""), obs(10_001-2000, 7, "0xbb")}, seen)
}

func TestObservation_FillsInAHeadAlreadyHeld(t *testing.T) {
	// SuggestLatestBlock learns a head from request traffic with no block
	// fields; the fetching replica then publishes the same head WITH them. The
	// second observation must reach the listeners, and only once.
	c, ctx := newObservationTestCounter(t)
	var seen []CounterInt64Observation
	c.OnValue(func(o CounterInt64Observation) { seen = append(seen, o) })

	c.TryUpdate(ctx, 100)
	require.Equal(t, []CounterInt64Observation{obs(100, 0, "")}, seen)

	// Remote, equal value, newer, carries the fields: fill and fire once.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 1_800_000_000, BlockHash: "0xaa"})
	assert.Equal(t, []CounterInt64Observation{obs(100, 0, ""), obs(100, 1_800_000_000, "0xaa")}, seen)

	// The same block again is not an observation of anything new.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 1_800_000_000, BlockHash: "0xaa"})
	assert.Len(t, seen, 2)

	// The local fetch path fills in the same way, and reports a change so the
	// record is pushed to peers.
	c.TryUpdate(ctx, 101)
	assert.True(t, c.processNewValue(UpdateSourceTryUpdateIfStale, obs(101, 1_800_000_002, "0xcc")),
		"filling in missing block fields is a state change worth publishing")
	assert.Equal(t, obs(101, 1_800_000_002, "0xcc"), seen[len(seen)-1])

	// Fields for a head we no longer hold are ignored.
	c.TryUpdate(ctx, 102)
	assert.False(t, c.processNewValue(UpdateSourceTryUpdateIfStale, obs(101, 1_800_000_003, "0xdd")))
	assert.Equal(t, obs(102, 0, ""), c.observation())
}

func TestObservation_ADifferentBlockAtTheSameHeightSupersedes(t *testing.T) {
	// The chain reorganised under the tip: a later FETCH of the same height
	// sees a different hash. The stream must not hand out the orphan, so the
	// newer fetch replaces hash and timestamp together.
	c, ctx := newObservationTestCounter(t)
	var seen []CounterInt64Observation
	c.OnValue(func(o CounterInt64Observation) { seen = append(seen, o) })

	require.True(t, c.processNewValue(UpdateSourceTryUpdateIfStale, obs(100, 1_800_000_000, "0xaa")))
	require.True(t, c.processNewValue(UpdateSourceTryUpdateIfStale, obs(100, 1_800_000_001, "0xbb")), "a different block at the same height is a change")
	assert.Equal(t, obs(100, 1_800_000_001, "0xbb"), c.observation())
	assert.Equal(t, obs(100, 1_800_000_001, "0xbb"), seen[len(seen)-1])

	// A newer remote record does the same.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 1_800_000_002, BlockHash: "0xcc"})
	assert.Equal(t, obs(100, 1_800_000_002, "0xcc"), c.observation())

	// An OLDER remote record never supersedes, even with a different hash.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() - 1000, BlockTimestamp: 1_800_000_000, BlockHash: "0xaa"})
	assert.Equal(t, obs(100, 1_800_000_002, "0xcc"), c.observation())
	assert.Equal(t, obs(100, 1_800_000_002, "0xcc"), seen[len(seen)-1])
	_ = ctx
}

func TestObservation_FilledFromAnOlderPeerRecord(t *testing.T) {
	// SuggestLatestBlock learns head N from request traffic and stamps it with
	// a fresh UpdatedAt. The peer that actually fetched N then publishes its
	// record, which is OLDER by UpdatedAt and would be rejected as stale. The
	// value gate is right to reject it; the block fields must still be filled,
	// or this replica sits on the fallback debounce and fetches by number
	// until the next block.
	c, ctx := newObservationTestCounter(t)
	var seen []CounterInt64Observation
	c.OnValue(func(o CounterInt64Observation) { seen = append(seen, o) })

	c.TryUpdate(ctx, 100)
	require.Equal(t, []CounterInt64Observation{obs(100, 0, "")}, seen)
	localTs := c.updatedAtMs()

	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: localTs - 1000, UpdatedBy: "peer", BlockTimestamp: 1_800_000_000, BlockHash: "0xaa"})
	assert.Equal(t, []CounterInt64Observation{obs(100, 0, ""), obs(100, 1_800_000_000, "0xaa")}, seen)
	assert.Equal(t, obs(100, 1_800_000_000, "0xaa"), c.observation())
	assert.Equal(t, localTs, c.updatedAtMs(), "an older record must not move the freshness clock")

	// An older record for a DIFFERENT head is still rejected outright.
	c.processNewState("peer", CounterInt64State{Value: 99, UpdatedAt: localTs - 500, UpdatedBy: "peer", BlockTimestamp: 1_799_999_998, BlockHash: "0x99"})
	assert.Equal(t, int64(100), c.GetValue())
	assert.Len(t, seen, 2)
}

func TestObservation_WireFormatIsOptional(t *testing.T) {
	// Old writers omit the fields, old readers ignore them: no schema bump.
	full, err := common.SonicCfg.Marshal(CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x", BlockTimestamp: 3, BlockHash: "0xaa"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"t":2,"b":"x","bt":3,"bh":"0xaa"}`, string(full))

	bare, err := common.SonicCfg.Marshal(CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"t":2,"b":"x"}`, string(bare), "unknown fields must not appear on the wire")

	var st CounterInt64State
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`{"v":1,"t":2,"b":"x"}`), &st))
	assert.Equal(t, CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x"}, st, "a record from before the fields reads as unknown")
}
