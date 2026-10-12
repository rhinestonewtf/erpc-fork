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

// Fork patch RHI-8244. These fail if the block timestamp stops travelling with
// the head through shared state, which is what lets the latest poll stay on the
// shared staleness clock (one fetch per cluster) without starving the block-time
// estimate on the replicas that did not fetch.

type tsObservation struct{ value, blockTimestamp int64 }

func newTimestampTestCounter(t *testing.T) (*counterInt64, context.Context) {
	t.Helper()
	logger := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	connector, err := NewMemoryConnector(ctx, &logger, "ts", &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"})
	require.NoError(t, err)
	c := &counterInt64{
		registry: &sharedStateRegistry{
			appCtx: ctx, logger: &logger, connector: connector, instanceId: "me",
			fallbackTimeout: time.Second, updateMaxWait: time.Second, lockMaxWait: time.Second, lockTtl: time.Second,
		},
		key:              "ts/latest",
		ignoreRollbackOf: 1024,
	}
	return c, ctx
}

func TestBlockTimestamp_ReceivingReplicaSamplesBlockTime(t *testing.T) {
	// The replica under test never fetches. Every head it sees comes from a
	// peer through the shared record, with the peer's block timestamp. Its
	// tracker must still settle a block-time estimate — the whole reason
	// upstream #1208 moved the poll to a local clock was that it could not.
	logger := zerolog.Nop()
	tr := health.NewTracker(&logger, "receiver", time.Minute)
	up := common.NewFakeUpstream("a")
	c, _ := newTimestampTestCounter(t)
	c.OnValue(func(n int64, ts int64) { tr.SetLatestBlockNumber(up, n, ts) })

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

func TestBlockTimestamp_TravelsWithTheValue(t *testing.T) {
	c, ctx := newTimestampTestCounter(t)
	var seen []tsObservation
	c.OnValue(func(v, ts int64) { seen = append(seen, tsObservation{v, ts}) })

	// A local fetch stores the pair and publishes it as one record.
	_, err := c.TryUpdateIfStale(ctx, -time.Second, func(context.Context) (int64, int64, error) { return 10_000, 1_800_000_000, nil })
	require.NoError(t, err)
	assert.Equal(t, CounterInt64State{Value: 10_000, BlockTimestamp: 1_800_000_000, UpdatedBy: "me", UpdatedAt: c.updatedAtMs()}, c.localState())

	// A head accepted without a timestamp clears the previous one: the pair
	// always describes a single observation.
	c.TryUpdate(ctx, 10_001)
	assert.Zero(t, c.localState().BlockTimestamp)

	// A large rollback carries its own timestamp too.
	c.processNewState("peer", CounterInt64State{Value: 10_001 - 2000, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 7})
	assert.Equal(t, int64(7), c.localState().BlockTimestamp)

	assert.Equal(t, []tsObservation{{10_000, 1_800_000_000}, {10_001, 0}, {10_001 - 2000, 7}}, seen)
}

func TestBlockTimestamp_AdoptedForAHeadAlreadyHeld(t *testing.T) {
	// SuggestLatestBlock learns a head from request traffic with no timestamp;
	// the fetching replica then publishes the same head WITH one. The second
	// observation must reach the tracker, and only once.
	c, ctx := newTimestampTestCounter(t)
	var seen []tsObservation
	c.OnValue(func(v, ts int64) { seen = append(seen, tsObservation{v, ts}) })

	c.TryUpdate(ctx, 100)
	require.Equal(t, []tsObservation{{100, 0}}, seen)

	// Remote, equal value, carries a timestamp: adopt and fire.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 1_800_000_000})
	assert.Equal(t, []tsObservation{{100, 0}, {100, 1_800_000_000}}, seen)
	assert.Equal(t, int64(1_800_000_000), c.localState().BlockTimestamp)

	// A second timestamp for the same head is not an observation of anything
	// new: never overwrite, never fire again.
	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: c.updatedAtMs() + 1, BlockTimestamp: 1_800_000_099})
	assert.Equal(t, int64(1_800_000_000), c.localState().BlockTimestamp)
	assert.Len(t, seen, 2)

	// The local fetch path adopts the same way, and reports a change so the
	// pair is pushed to peers.
	c.TryUpdate(ctx, 101)
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, 101, 1_800_000_002)
	assert.True(t, updated, "filling in a missing timestamp is a state change worth publishing")
	assert.Equal(t, tsObservation{101, 1_800_000_002}, seen[len(seen)-1])

	// A timestamp for a head we no longer hold is ignored.
	c.TryUpdate(ctx, 102)
	assert.False(t, c.processNewValue(UpdateSourceTryUpdateIfStale, 101, 1_800_000_003))
	assert.Zero(t, c.localState().BlockTimestamp)
}

func TestBlockTimestamp_AdoptedFromAnOlderPeerRecord(t *testing.T) {
	// SuggestLatestBlock learns head N from request traffic and stamps it with
	// a fresh UpdatedAt. The peer that actually fetched N then publishes its
	// record, which is OLDER by UpdatedAt and would be rejected as stale. The
	// value gate is right to reject it; the timestamp must still be adopted,
	// or this replica sits on the fallback debounce until the next block.
	c, ctx := newTimestampTestCounter(t)
	var seen []tsObservation
	c.OnValue(func(v, ts int64) { seen = append(seen, tsObservation{v, ts}) })

	c.TryUpdate(ctx, 100)
	require.Equal(t, []tsObservation{{100, 0}}, seen)
	localTs := c.updatedAtMs()

	c.processNewState("peer", CounterInt64State{Value: 100, UpdatedAt: localTs - 1000, UpdatedBy: "peer", BlockTimestamp: 1_800_000_000})
	assert.Equal(t, []tsObservation{{100, 0}, {100, 1_800_000_000}}, seen)
	assert.Equal(t, int64(1_800_000_000), c.localState().BlockTimestamp)
	assert.Equal(t, localTs, c.updatedAtMs(), "an older record must not move the freshness clock")

	// An older record for a DIFFERENT head is still rejected outright.
	c.processNewState("peer", CounterInt64State{Value: 99, UpdatedAt: localTs - 500, UpdatedBy: "peer", BlockTimestamp: 1_799_999_998})
	assert.Equal(t, int64(100), c.GetValue())
	assert.Len(t, seen, 2)
}

func TestBlockTimestamp_WireFormatIsOptional(t *testing.T) {
	// Old writers omit the field, old readers ignore it: no schema bump.
	withTs, err := common.SonicCfg.Marshal(CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x", BlockTimestamp: 3})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"t":2,"b":"x","bt":3}`, string(withTs))

	withoutTs, err := common.SonicCfg.Marshal(CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"t":2,"b":"x"}`, string(withoutTs), "a zero timestamp must not appear on the wire")

	var st CounterInt64State
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`{"v":1,"t":2,"b":"x"}`), &st))
	assert.Equal(t, CounterInt64State{Value: 1, UpdatedAt: 2, UpdatedBy: "x"}, st, "a record from before the field reads as timestamp unknown")
}
