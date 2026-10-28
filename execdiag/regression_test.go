package execdiag

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify_WrappedJoins(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		err := fmt.Errorf("outer: %w", errors.Join(sentinel, serverErr(503)))
		class, _, cause := FinalError(err)
		assert.Equal(t, ClassMixed, class)
		assert.Equal(t, CauseMultiple, cause)
	}
	class, _, notable := Classify(&common.BaseError{Code: common.ErrCodeGetLogsExceededMaxAllowedTopics}, common.DispatchAttempted, true)
	assert.Equal(t, ClassApplicationError, class)
	assert.False(t, notable)
}

func TestLateDispatchChangesRejectedOutcome(t *testing.T) {
	buf := ConfigureForTest(t, nil)
	req := newReq()
	ctx, scope := BeginRequest(context.Background(), req, "main", "evm:1")
	ictx, inv := BeginInvocation(ctx, req, common.NewFakeUpstream("rpc1"), false)
	common.MarkDispatchAttempted(ictx)
	scope.Finish(ctx, nil, context.DeadlineExceeded)
	inv.End(context.DeadlineExceeded)
	require.Eventually(t, func() bool { return len(buf.Lines()) == 2 }, time.Second, time.Millisecond)
	sum := byEvent(parseLines(t, buf), "rpc.request.completed")[0]
	assert.Equal(t, "failed", sum["outcome"])
	assert.Equal(t, float64(1), sum["dispatches"])
	assert.Equal(t, true, sum["evidence_complete"])
}
