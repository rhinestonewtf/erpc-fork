package common

import (
	"context"
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
	nilSt.InvocationEnded()
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
			st.RecordInvocation(InvocationRecord{UpstreamId: "u"})
			st.InvocationEnded()
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
}
