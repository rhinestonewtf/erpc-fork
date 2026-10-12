package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/data"
)

func newTestHub() *blockStreamHub {
	return &blockStreamHub{subs: make(map[*blockStreamSub]struct{}), hashes: make(map[int64]string)}
}

func TestBlockStreamHub_AdvanceMonotonicAndSignals(t *testing.T) {
	h := newTestHub()
	sub := h.subscribe()

	h.advance(data.CounterInt64Observation{Value: 100})
	if got := h.head.Load(); got != 100 {
		t.Fatalf("head = %d, want 100", got)
	}
	select {
	case <-sub.ch:
	default:
		t.Fatal("expected a wake signal after advance to 100")
	}

	// A lower value is ignored (head is monotonic) and produces no signal.
	h.advance(data.CounterInt64Observation{Value: 50})
	if got := h.head.Load(); got != 100 {
		t.Fatalf("head after rollback = %d, want 100 (monotonic)", got)
	}
	select {
	case <-sub.ch:
		t.Fatal("did not expect a signal for a non-advancing head")
	default:
	}

	h.advance(data.CounterInt64Observation{Value: 101})
	if got := h.head.Load(); got != 101 {
		t.Fatalf("head = %d, want 101", got)
	}
	select {
	case <-sub.ch:
	default:
		t.Fatal("expected a wake signal after forward advance to 101")
	}
}

func TestBlockStreamHub_AdvanceIsNonBlocking(t *testing.T) {
	h := newTestHub()
	_ = h.subscribe() // deliberately never drained

	// advance runs inside the poller's synchronous update path: buffered(1) +
	// non-blocking send means a burst against an undrained subscriber must never
	// block.
	done := make(chan struct{})
	go func() {
		for i := int64(1); i <= 1000; i++ {
			h.advance(data.CounterInt64Observation{Value: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("advance blocked with an undrained subscriber")
	}
	if got := h.head.Load(); got != 1000 {
		t.Fatalf("head = %d, want 1000", got)
	}
}

func TestBlockStreamHub_Unsubscribe(t *testing.T) {
	h := newTestHub()
	sub := h.subscribe()
	h.unsubscribe(sub)

	if n := len(h.subs); n != 0 {
		t.Fatalf("subs = %d after unsubscribe, want 0", n)
	}
	// Advancing after unsubscribe must not panic and must not signal a gone sub.
	h.advance(data.CounterInt64Observation{Value: 10})
	select {
	case <-sub.ch:
		t.Fatal("an unsubscribed subscriber should not be signaled")
	default:
	}
}

func TestBlockStreamHub_FansOutToAllSubscribers(t *testing.T) {
	h := newTestHub()
	a := h.subscribe()
	b := h.subscribe()

	h.advance(data.CounterInt64Observation{Value: 7})
	for i, s := range []*blockStreamSub{a, b} {
		select {
		case <-s.ch:
		default:
			t.Fatalf("subscriber %d was not signaled on advance", i)
		}
	}
}

// Fork patch RHI-8178: the hub keeps the hash a poller observed at each height
// so the subscriber loop can fetch by hash (a cache hit) instead of by number.
func TestBlockStreamHub_RecordsObservedHashes(t *testing.T) {
	h := newTestHub()
	sub := h.subscribe()

	h.advance(data.CounterInt64Observation{Value: 100, BlockHash: "0xaa"})
	if hash, ok := h.hashFor(100); !ok || hash != "0xaa" {
		t.Fatalf("hashFor(100) = %q,%v, want 0xaa", hash, ok)
	}
	<-sub.ch

	// The number arrives first (request-path suggestion), the hash later: the
	// later observation does not advance the head but must still be recorded.
	h.advance(data.CounterInt64Observation{Value: 101})
	<-sub.ch
	if _, ok := h.hashFor(101); ok {
		t.Fatal("a head learned without a hash must not have one")
	}
	h.advance(data.CounterInt64Observation{Value: 101, BlockHash: "0xbb"})
	if hash, _ := h.hashFor(101); hash != "0xbb" {
		t.Fatalf("hashFor(101) = %q, want 0xbb", hash)
	}
	select {
	case <-sub.ch:
		t.Fatal("filling in a hash is not a head advance")
	default:
	}

	// A different block observed at the same height replaces the entry.
	h.advance(data.CounterInt64Observation{Value: 101, BlockHash: "0xcc"})
	if hash, _ := h.hashFor(101); hash != "0xcc" {
		t.Fatalf("hashFor(101) after reorg = %q, want 0xcc", hash)
	}

	// Entries fall out of the backfill window behind the head.
	h.advance(data.CounterInt64Observation{Value: 101 + blockStreamMaxBackfill + 1, BlockHash: "0xdd"})
	if _, ok := h.hashFor(100); ok {
		t.Fatal("hashes behind the backfill window must be pruned")
	}
	if _, ok := h.hashFor(101 + blockStreamMaxBackfill + 1); !ok {
		t.Fatal("the head's own hash must be kept")
	}
}
