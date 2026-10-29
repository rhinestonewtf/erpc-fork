package common

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInvocationLedger_DisabledByDefault(t *testing.T) {
	st := &ExecState{}
	st.RecordInvocation(InvocationRecord{UpstreamId: "a"})
	assert.False(t, st.InvocationLedgerEnabled())
	assert.Empty(t, st.Invocations())

	var nilSt *ExecState
	nilSt.RecordInvocation(InvocationRecord{})
	nilSt.InvocationStarted()
	assert.Equal(t, 0, nilSt.InvocationsInflight())
	assert.Nil(t, nilSt.Invocations())
}

func TestInvocationLedger_Concurrent(t *testing.T) {
	st := &ExecState{}
	st.EnableInvocationLedger()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.InvocationStarted()
			st.CompleteInvocation(InvocationRecord{UpstreamId: "u"})
		}()
	}
	wg.Wait()
	assert.Len(t, st.Invocations(), 50)
	assert.Equal(t, 0, st.InvocationsInflight())

	cp := st.Invocations()
	cp[0].UpstreamId = "mutated"
	assert.Equal(t, "u", st.Invocations()[0].UpstreamId, "Invocations returns a copy")
}

func TestDispatchProbe(t *testing.T) {
	var nilProbe *DispatchProbe
	assert.Equal(t, DispatchUnknown, nilProbe.State())

	// No probe in ctx: marks are no-ops.
	MarkDispatchObserved(context.Background())
	MarkDispatchAttempted(context.Background())

	p := &DispatchProbe{}
	ctx := WithDispatchProbe(context.Background(), p)
	assert.Equal(t, DispatchUnknown, p.State())
	MarkDispatchObserved(ctx)
	assert.Equal(t, DispatchNone, p.State())
	MarkDispatchAttempted(ctx)
	assert.Equal(t, DispatchAttempted, p.State())

	p2 := &DispatchProbe{}
	MarkDispatchAttempted(WithDispatchProbe(context.Background(), p2))
	assert.Equal(t, DispatchAttempted, p2.State())

	// Settle reports a batch-held call only once the batch sends or drops it.
	var got []string
	record := func(state string) { got = append(got, state) }
	p3 := &DispatchProbe{}
	ctx3 := WithDispatchProbe(context.Background(), p3)
	MarkDispatchObserved(ctx3)
	MarkDispatchPending(ctx3)
	p3.Settle(record)
	assert.Empty(t, got)
	MarkDispatchAttempted(ctx3)
	assert.Equal(t, []string{DispatchAttempted}, got)

	got = nil
	p4 := &DispatchProbe{}
	ctx4 := WithDispatchProbe(context.Background(), p4)
	MarkDispatchObserved(ctx4)
	MarkDispatchPending(ctx4)
	p4.Settle(record)
	MarkDispatchDropped(ctx4)
	assert.Equal(t, []string{DispatchNone}, got)

	got = nil
	(&DispatchProbe{}).Settle(record)
	assert.Equal(t, []string{DispatchUnknown}, got, "a probe that is not pending settles immediately")
}

func TestInvocationLedger_AmendError(t *testing.T) {
	st := &ExecState{}
	st.EnableInvocationLedger()
	st.InvocationStarted()
	idx := st.CompleteInvocation(InvocationRecord{UpstreamId: "u"})
	first := errors.New("post-hook rejection")
	st.AmendInvocationError(idx, first)
	st.AmendInvocationError(idx, errors.New("later"))
	st.AmendInvocationError(idx+1, first)
	recs := st.Invocations()
	assert.Len(t, recs, 1)
	assert.Equal(t, first, recs[0].Err, "only an error-free record is amended, once")

	off := &ExecState{}
	assert.Equal(t, -1, off.CompleteInvocation(InvocationRecord{}))
}
