package execdiag

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func upReq(cause error) error {
	return common.NewErrUpstreamRequest(cause, nil, "evm:1", "eth_call", time.Millisecond, 1, 0, 0)
}

func exhausted(errs ...error) error {
	m := &sync.Map{}
	for i, e := range errs {
		m.Store(fmt.Sprintf("up%d", i), e)
	}
	return common.NewErrUpstreamsExhausted(nil, m, "p", "evm:1", "eth_call", time.Millisecond, len(errs), 0, 0, len(errs))
}

var testURL, _ = url.Parse("http://provider.localhost")

func TestClassify_FallthroughFirst(t *testing.T) {
	t.Run("PlainGoError", func(t *testing.T) {
		class, outcome, notable := Classify(errors.New("boom"), common.DispatchAttempted, true)
		assert.Equal(t, ClassUnclassified, class)
		assert.Equal(t, "failed", outcome)
		assert.True(t, notable)
		assert.Equal(t, CauseUnknown, Cause(errors.New("boom")))
	})
	t.Run("UnknownErpcCode", func(t *testing.T) {
		err := upReq(&common.BaseError{Code: "ErrSomethingNew", Message: "x"})
		class, _, notable := Classify(err, common.DispatchAttempted, true)
		assert.Equal(t, ClassUnclassified, class)
		assert.True(t, notable)
		assert.Equal(t, "ErrSomethingNew", Cause(err))
	})
	t.Run("UnknownDispatchStaysUnclassified", func(t *testing.T) {
		class, _, _ := Classify(errors.New("boom"), common.DispatchUnknown, true)
		assert.Equal(t, ClassUnclassified, class)
	})
	t.Run("FinalUnknown", func(t *testing.T) {
		fc, code, cause := FinalError(errors.New("boom"))
		assert.Equal(t, ClassUnclassified, fc)
		assert.Equal(t, CauseUnknown, code)
		assert.Equal(t, CauseUnknown, cause)
	})
}

func TestClassify_Table(t *testing.T) {
	jre := common.NewErrJsonRpcExceptionInternal(-32603, common.JsonRpcErrorServerSideException, "internal", nil, nil)
	cases := []struct {
		name     string
		err      error
		dispatch string
		class    string
		outcome  string
		notable  bool
		cause    string
	}{
		{"nil", nil, common.DispatchAttempted, ClassResult, "success", false, ""},
		{"endpointCanceled", upReq(common.NewErrEndpointRequestCanceled(context.Canceled)), common.DispatchAttempted, ClassCancelled, "cancelled", false, "ErrEndpointRequestCanceled"},
		{"ctxCanceled", context.Canceled, common.DispatchAttempted, ClassCancelled, "cancelled", false, CauseUnknown},
		{"hedgeCancelled", common.NewErrUpstreamHedgeCancelled("u", context.Canceled), common.DispatchAttempted, ClassCancelled, "cancelled", false, "ErrUpstreamHedgeCancelled"},
		{"failsafeTimeout", common.NewErrFailsafeTimeoutExceeded(common.ScopeUpstream, common.ErrDynamicTimeoutExceeded, nil), common.DispatchAttempted, ClassTimeout, "failed", true, "ErrFailsafeTimeoutExceeded"},
		{"dynamicTimeoutPlain", fmt.Errorf("wrapped: %w", common.ErrDynamicTimeoutExceeded), common.DispatchAttempted, ClassTimeout, "failed", true, CauseUnknown},
		{"deadline", context.DeadlineExceeded, common.DispatchAttempted, ClassTimeout, "failed", true, CauseUnknown},
		{"breakerOpen", common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, failsafe.ErrCircuitOpen, nil), common.DispatchNone, ClassRejected, "rejected", true, "ErrFailsafeCircuitBreakerOpen"},
		{"upstreamRateLimit", common.NewErrUpstreamRateLimitRuleExceeded("u", "b", "r"), common.DispatchNone, ClassRejected, "rejected", true, "ErrUpstreamRateLimitRuleExceeded"},
		{"skipped", common.NewErrUpstreamRequestSkipped(common.NewErrUpstreamMethodIgnored("eth_call", "u"), "u"), common.DispatchNone, ClassSkipped, "skipped", false, "ErrUpstreamMethodIgnored"},
		{"dispatchNoneOther", upReq(&common.BaseError{Code: "ErrHttp", Message: "prep"}), common.DispatchNone, ClassRejected, "rejected", true, "ErrHttp"},
		{"dispatchNoneBeatsProvider", upReq(common.NewErrEndpointServerSideException(nil, nil, 500)), common.DispatchNone, ClassRejected, "rejected", true, "ErrEndpointServerSideException"},
		{"revert", upReq(common.NewErrEndpointExecutionException(jre)), common.DispatchAttempted, ClassApplicationError, "success", false, "ErrEndpointExecutionException"},
		{"missingData", upReq(common.NewErrEndpointMissingData(nil, nil)), common.DispatchAttempted, ClassDataUnavailable, "degraded", false, "ErrEndpointMissingData"},
		{"transport", upReq(common.NewErrEndpointTransportFailure(testURL, errors.New("connection refused"))), common.DispatchAttempted, ClassTransportFailure, "failed", true, "ErrEndpointTransportFailure"},
		{"serverSide", upReq(common.NewErrEndpointServerSideException(jre, map[string]interface{}{"statusCode": 503}, 503)), common.DispatchAttempted, ClassProviderError, "failed", true, "ErrEndpointServerSideException"},
		{"capacity", upReq(common.NewErrEndpointCapacityExceeded(nil)), common.DispatchAttempted, ClassProviderError, "failed", true, "ErrEndpointCapacityExceeded"},
		{"bareJsonRpc", upReq(jre), common.DispatchAttempted, ClassProviderError, "failed", true, "ErrJsonRpcExceptionInternal"},
		{"unknownDispatchProvider", upReq(common.NewErrEndpointServerSideException(nil, nil, 500)), common.DispatchUnknown, ClassProviderError, "failed", true, "ErrEndpointServerSideException"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, outcome, notable := Classify(tc.err, tc.dispatch, true)
			assert.Equal(t, tc.class, class)
			assert.Equal(t, tc.outcome, outcome)
			assert.Equal(t, tc.notable, notable)
			assert.Equal(t, tc.cause, Cause(tc.err))
		})
	}
}

func TestClassify_FinalJoined(t *testing.T) {
	transport := upReq(common.NewErrEndpointTransportFailure(testURL, errors.New("refused")))
	server := upReq(common.NewErrEndpointServerSideException(nil, map[string]interface{}{"statusCode": 503}, 503))

	t.Run("Mixed", func(t *testing.T) {
		err := common.NewErrFailsafeRetryExceeded(common.ScopeNetwork, exhausted(transport, server), nil)
		fc, code, cause := FinalError(err)
		assert.Equal(t, ClassMixed, fc)
		assert.Equal(t, "ErrFailsafeRetryExceeded", code)
		assert.Equal(t, CauseMultiple, cause)
	})
	t.Run("SingleClass", func(t *testing.T) {
		fc, code, cause := FinalError(exhausted(server, server))
		assert.Equal(t, ClassProviderError, fc)
		assert.Equal(t, "ErrUpstreamsExhausted", code)
		assert.Equal(t, "ErrEndpointServerSideException", cause)
	})
	t.Run("OuterTimeoutWins", func(t *testing.T) {
		err := common.NewErrFailsafeTimeoutExceeded(common.ScopeNetwork, exhausted(transport, server), nil)
		fc, _, cause := FinalError(err)
		assert.Equal(t, ClassTimeout, fc)
		assert.Equal(t, "ErrFailsafeTimeoutExceeded", cause)
	})
	t.Run("LeafCancelDoesNotHideOthers", func(t *testing.T) {
		cancelled := upReq(common.NewErrEndpointRequestCanceled(context.Canceled))
		fc, _, _ := FinalError(exhausted(cancelled, server))
		assert.Equal(t, ClassMixed, fc)
	})
	t.Run("EmptyExhausted", func(t *testing.T) {
		fc, code, cause := FinalError(exhausted())
		assert.Equal(t, ClassRejected, fc)
		assert.Equal(t, "ErrUpstreamsExhausted", code)
		assert.Equal(t, "ErrUpstreamsExhausted", cause)
	})
	t.Run("ExhaustedWithUnknownCause", func(t *testing.T) {
		fc, _, _ := FinalError(common.NewErrUpstreamsExhaustedWithCause(errors.New("boom")))
		assert.Equal(t, ClassUnclassified, fc)
	})
	t.Run("EmptyExhaustedUnderTimeout", func(t *testing.T) {
		fc, _, _ := FinalError(common.NewErrFailsafeTimeoutExceeded(common.ScopeNetwork, exhausted(), nil))
		assert.Equal(t, ClassTimeout, fc)
	})
	t.Run("EmptyExhaustedUnderCancellation", func(t *testing.T) {
		fc, _, _ := FinalError(common.NewErrEndpointRequestCanceled(exhausted()))
		assert.Equal(t, ClassCancelled, fc)
	})
	t.Run("EmptyExhaustedUnderRetry", func(t *testing.T) {
		fc, code, cause := FinalError(common.NewErrFailsafeRetryExceeded(common.ScopeNetwork, exhausted(), nil))
		assert.Equal(t, ClassRejected, fc)
		assert.Equal(t, "ErrFailsafeRetryExceeded", code)
		assert.Equal(t, "ErrUpstreamsExhausted", cause)
	})
}

func TestClassify_Extraction(t *testing.T) {
	jre := common.NewErrJsonRpcExceptionInternal(-32099, common.JsonRpcErrorServerSideException, "canary", nil, nil)
	err := upReq(common.NewErrEndpointServerSideException(jre, map[string]interface{}{"statusCode": 502}, 502))
	assert.Equal(t, 502, HTTPStatus(err))
	code, ok := JSONRPCCode(err)
	assert.True(t, ok)
	assert.Equal(t, int(common.JsonRpcErrorServerSideException), code)

	assert.Equal(t, 0, HTTPStatus(errors.New("x")))
	_, ok = JSONRPCCode(errors.New("x"))
	assert.False(t, ok)
}

func TestSafeToken(t *testing.T) {
	assert.Equal(t, "eth_getBalance", SafeToken("eth_getBalance"))
	assert.Equal(t, "user-1:a.b", SafeToken("user-1:a.b"))
	assert.Equal(t, "invalid", SafeToken(""))
	assert.Equal(t, "invalid", SafeToken("eth_call canary"))
	assert.Equal(t, "invalid", SafeToken("x\"y"))
	assert.Equal(t, "invalid", SafeToken("a/b?key=secret"))
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	assert.Equal(t, "invalid", SafeToken(string(long)))
}
