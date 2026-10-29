package execdiag

import (
	"context"
	"errors"
	"regexp"

	"github.com/erpc/erpc/common"
)

// Invocation classes. The set is closed; anything not matched by the table in
// classifyLinear falls through to ClassUnclassified, which is notable so new
// error shapes surface instead of being misfiled.
const (
	ClassResult           = "result"
	ClassCancelled        = "cancelled"
	ClassTimeout          = "timeout"
	ClassRejected         = "rejected"
	ClassSkipped          = "skipped"
	ClassApplicationError = "application_error"
	ClassDataUnavailable  = "data_unavailable"
	ClassTransportFailure = "transport_failure"
	ClassProviderError    = "provider_error"
	ClassUnclassified     = "unclassified"
	ClassMixed            = "mixed"
)

const (
	CauseUnknown  = "unknown_error"
	CauseMultiple = "multiple"
)

var (
	cancelledCodes = []common.ErrorCode{
		common.ErrCodeEndpointRequestCanceled,
		common.ErrCodeUpstreamHedgeCancelled,
	}
	timeoutCodes = []common.ErrorCode{
		common.ErrCodeFailsafeTimeoutExceeded,
		common.ErrCodeEndpointRequestTimeout,
		common.ErrCodeNetworkRequestTimeout,
	}
	rejectedCodes = []common.ErrorCode{
		common.ErrCodeFailsafeCircuitBreakerOpen,
		common.ErrCodeUpstreamRateLimitRuleExceeded,
		common.ErrCodeNetworkRateLimitRuleExceeded,
		common.ErrCodeProjectRateLimitRuleExceeded,
		common.ErrCodeAuthRateLimitRuleExceeded,
		common.ErrCodeNoUpstreamsLeftToSelect,
		"ErrNoUpstreamsFound",
	}
	skippedCodes = []common.ErrorCode{
		common.ErrCodeUpstreamRequestSkipped,
		common.ErrCodeUpstreamMethodIgnored,
		common.ErrCodeUpstreamBlockUnavailable,
		common.ErrCodeFinalizedBlockUnavailable,
		common.ErrCodeUpstreamSyncing,
		common.ErrCodeUpstreamShadowing,
		common.ErrCodeUpstreamExcludedByPolicy,
		common.ErrCodeUpstreamNotAllowed,
		common.ErrCodeUpstreamNodeTypeMismatch,
	}
	applicationCodes = []common.ErrorCode{
		common.ErrCodeEndpointExecutionException,
		common.ErrCodeEndpointClientSideException,
		common.ErrCodeEndpointNonceException,
		common.ErrCodeEndpointRequestTooLarge,
		common.ErrCodeGetLogsExceededMaxAllowedResponseSize,
		common.ErrCodeGetLogsExceededMaxAllowedRange,
		common.ErrCodeGetLogsExceededMaxAllowedAddresses,
		common.ErrCodeGetLogsExceededMaxAllowedTopics,
		common.ErrCodeInvalidRequest,
		common.ErrCodeJsonRpcRequestUnmarshal,
	}
	dataUnavailableCodes = []common.ErrorCode{
		common.ErrCodeEndpointMissingData,
	}
	transportCodes = []common.ErrorCode{
		common.ErrCodeEndpointTransportFailure,
	}
	providerCodes = []common.ErrorCode{
		common.ErrCodeEndpointServerSideException,
		common.ErrCodeEndpointCapacityExceeded,
		common.ErrCodeEndpointUnauthorized,
		common.ErrCodeEndpointBillingIssue,
		common.ErrCodeEndpointUnsupported,
		common.ErrCodeEndpointContentValidation,
		common.ErrCodeEndpointChainIdMismatch,
		"ErrUpstreamMalformedResponse",
		common.ErrCodeJsonRpcExceptionInternal,
	}

	// Wrapper codes only carry their cause; they never name the cause itself.
	wrapperCodes = []common.ErrorCode{
		common.ErrCodeUpstreamRequest,
		common.ErrCodeFailsafeRetryExceeded,
		common.ErrCodeUpstreamsExhausted,
	}
)

// chain is the linear view of an error up to (not including) the first
// multi-error join. Each node is classified by its own code only, so a joined
// error's leaves can be classified independently.
type chain struct {
	nodes  []error
	codes  []common.ErrorCode
	joined []error
	hasStd bool
}

func linearize(err error) chain {
	var c chain
	for cur := err; cur != nil; {
		if j, ok := cur.(interface{ Unwrap() []error }); ok {
			c.joined = j.Unwrap()
			return c
		}
		c.nodes = append(c.nodes, cur)
		if se, ok := cur.(common.StandardError); ok {
			c.hasStd = true
			if b := se.Base(); b != nil {
				c.codes = append(c.codes, b.Code)
			} else {
				c.codes = append(c.codes, "")
			}
			cur = se.GetCause()
			continue
		}
		c.codes = append(c.codes, "")
		cur = errors.Unwrap(cur)
	}
	return c
}

func (c chain) has(codes []common.ErrorCode) bool {
	for _, have := range c.codes {
		if have == "" {
			continue
		}
		for _, want := range codes {
			if have == want {
				return true
			}
		}
	}
	return false
}

func (c chain) is(targets ...error) bool {
	for i, n := range c.nodes {
		// Match only this node; recursive errors.Is would cross a join and
		// hide mixed causes behind cancellation or timeout.
		if c.codes[i] != "" {
			continue
		}
		for _, t := range targets {
			if n == t {
				return true
			}
		}
	}
	return false
}

// classifyChain applies the ordered table to one linear chain. ok=false when
// nothing in the chain matched (the caller decides the fallthrough).
func classifyChain(c chain, dispatch string, isInvocation bool) (string, bool) {
	switch {
	case c.has(cancelledCodes) || c.is(context.Canceled):
		return ClassCancelled, true
	case c.has(timeoutCodes) || c.is(common.ErrDynamicTimeoutExceeded, context.DeadlineExceeded):
		return ClassTimeout, true
	case c.has(rejectedCodes):
		return ClassRejected, true
	case c.has(skippedCodes):
		return ClassSkipped, true
	case isInvocation && dispatch == common.DispatchNone:
		return ClassRejected, true
	case c.has(applicationCodes):
		return ClassApplicationError, true
	case c.has(dataUnavailableCodes):
		return ClassDataUnavailable, true
	case c.has(transportCodes):
		return ClassTransportFailure, true
	case c.has(providerCodes):
		return ClassProviderError, true
	}
	return "", false
}

// Classify maps an invocation's (err, dispatch) to a bounded class, the
// contract-level attempt outcome, and whether it makes the request notable.
func Classify(err error, dispatch string, isInvocation bool) (class, outcome string, notable bool) {
	class = classOf(err, dispatch, isInvocation)
	return class, attemptOutcome(class), IsNotable(class)
}

func classOf(err error, dispatch string, isInvocation bool) string {
	if err == nil {
		return ClassResult
	}
	c := linearize(err)
	if cl, ok := classifyChain(c, dispatch, isInvocation); ok {
		return cl
	}
	if len(c.joined) == 0 {
		return ClassUnclassified
	}
	seen := ""
	for _, leaf := range c.joined {
		if leaf == nil {
			continue
		}
		lc := classOf(leaf, dispatch, isInvocation)
		if seen == "" {
			seen = lc
		} else if seen != lc {
			return ClassMixed
		}
	}
	if seen == "" {
		return ClassUnclassified
	}
	return seen
}

func attemptOutcome(class string) string {
	switch class {
	case ClassResult, ClassApplicationError:
		return "success"
	case ClassCancelled:
		return "cancelled"
	case ClassRejected:
		return "rejected"
	case ClassSkipped:
		return "skipped"
	case ClassDataUnavailable:
		return "degraded"
	default:
		return "failed"
	}
}

// IsNotable reports whether a class indicates a local rejection or an
// unhealthy execution worth emitting evidence for.
func IsNotable(class string) bool {
	switch class {
	case ClassResult, ClassCancelled, ClassSkipped, ClassApplicationError, ClassDataUnavailable:
		return false
	default:
		return true
	}
}

func isWrapper(c chain, i int) bool {
	code := c.codes[i]
	for _, w := range wrapperCodes {
		if code == w {
			return true
		}
	}
	if code == common.ErrCodeUpstreamRequestSkipped && i+1 < len(c.codes) && c.codes[i+1] != "" {
		return true
	}
	return false
}

// Cause returns the first non-wrapper eRPC error code in a linear chain.
func Cause(err error) string {
	if err == nil {
		return ""
	}
	return causeOf(linearize(err))
}

func causeOf(c chain) string {
	last := ""
	for i, code := range c.codes {
		if code == "" {
			continue
		}
		if !isWrapper(c, i) {
			return string(code)
		}
		last = string(code)
	}
	if len(c.joined) > 0 {
		seen := ""
		for _, leaf := range c.joined {
			if leaf == nil {
				continue
			}
			lc := causeOf(linearize(leaf))
			if seen == "" {
				seen = lc
			} else if seen != lc {
				return CauseMultiple
			}
		}
		if seen != "" {
			return seen
		}
	}
	if last != "" {
		return last
	}
	return CauseUnknown
}

// FinalError describes the error returned to the client.
func FinalError(err error) (finalClass, finalErrorCode, finalCause string) {
	if err == nil {
		return ClassResult, "", ""
	}
	c := linearize(err)
	finalErrorCode = CauseUnknown
	for _, code := range c.codes {
		if code != "" {
			finalErrorCode = string(code)
			break
		}
	}
	return classOf(err, "", false), finalErrorCode, causeOf(c)
}

// HTTPStatus returns the provider HTTP status carried in the error details.
func HTTPStatus(err error) int {
	for _, n := range linearize(err).nodes {
		se, ok := n.(common.StandardError)
		if !ok {
			continue
		}
		if v, ok := se.DeepSearch("statusCode").(int); ok && v > 0 {
			return v
		}
		return 0
	}
	return 0
}

// JSONRPCCode returns the normalized JSON-RPC code of the first internal
// JSON-RPC exception in the chain.
func JSONRPCCode(err error) (int, bool) {
	var jre *common.ErrJsonRpcExceptionInternal
	if err != nil && errors.As(err, &jre) && jre != nil {
		if _, ok := jre.Details["normalizedCode"]; ok {
			return int(jre.NormalizedCode()), true
		}
	}
	return 0, false
}

var safeTokenRe = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,64}$`)

// SafeToken passes caller-influenced identifiers through only when they are
// short, syntactically plain tokens.
func SafeToken(s string) string {
	if safeTokenRe.MatchString(s) {
		return s
	}
	return "invalid"
}
