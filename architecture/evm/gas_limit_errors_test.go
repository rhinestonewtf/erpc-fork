package evm

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
)

// FORK PATCH (RHI-6277). See gas_limit_errors.go and PATCH_LIST.md.
//
// TestExtractJsonRpcError_GasLimitRejections_NotRetryable pins that a gas-limit
// verdict is not retried toward other upstreams, and — via the controls — that the
// carve-out stays narrow enough to leave upstream's node-local rejection handling
// (#1094) intact.
func TestExtractJsonRpcError_GasLimitRejections_NotRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		method        string
		message       string
		wantRetryable bool
	}{
		{
			// Measured on Base mainnet for a limit of cap+1 (2^24 + 1).
			name:          "base: gas limit too high is terminal",
			method:        "eth_sendRawTransaction",
			message:       "gas limit too high",
			wantRetryable: false,
		},
		{
			// Measured on Base mainnet for a limit of 30M. Same cap, different wording;
			// Optimism says "gas limit too high" for both.
			name:          "base: exceeds max transaction gas limit is terminal",
			method:        "eth_sendRawTransaction",
			message:       "exceeds max transaction gas limit",
			wantRetryable: false,
		},
		{
			// The floor rather than the ceiling, and the case the pre-existing
			// "gas too low" substring match already caught and made RETRYABLE.
			name:          "geth: intrinsic gas too low is terminal",
			method:        "eth_sendRawTransaction",
			message:       "intrinsic gas too low: have 20000, want 21000",
			wantRetryable: false,
		},
		{
			name:          "geth: exceeds block gas limit is terminal",
			method:        "eth_sendRawTransaction",
			message:       "exceeds block gas limit",
			wantRetryable: false,
		},
		{
			// Clients capitalise differently, so matching is case-insensitive.
			name:          "mixed case is matched",
			method:        "eth_sendRawTransaction",
			message:       "Gas Limit Too High",
			wantRetryable: false,
		},
		{
			// CONTROL. A bare -32003 stays retryable: pool admission is node-local.
			name:          "control: bare transaction rejected stays retryable",
			method:        "eth_sendRawTransaction",
			message:       "transaction rejected",
			wantRetryable: true,
		},
		{
			// CONTROL. Also node-local — another pool may accept the same fee.
			name:          "control: underpriced stays retryable",
			method:        "eth_sendRawTransaction",
			message:       "transaction underpriced",
			wantRetryable: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","method":"` + tc.method + `","params":[],"id":1}`))
			nr := common.NewNormalizedResponse().WithRequest(req)

			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			// -32003 is what Base actually returns for these, and it is the code the
			// generic retryable branch keys on — so this is the shape that matters.
			jrErr := common.NewErrJsonRpcExceptionExternal(
				int(common.JsonRpcErrorTransactionRejected),
				tc.message,
				"",
			)
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			// ExecutionException either way: deterministic client-side rejections must
			// not count toward an upstream's circuit breaker.
			if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryable {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v (message=%q)",
					got, tc.wantRetryable, tc.message)
			}
		})
	}
}

// A read method must be unaffected: gas-limit verdicts on eth_call were already
// non-retryable, so the carve-out must not be the thing that makes them so.
func TestExtractJsonRpcError_GasLimitRejections_ReadMethodsUnchanged(t *testing.T) {
	t.Parallel()

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
	nr := common.NewNormalizedResponse().WithRequest(req)

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorTransactionRejected),
		"gas limit too high",
		"",
	)
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	err := ExtractJsonRpcError(r, nr, jr, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatalf("eth_call gas-limit rejection must not be retryable toward network")
	}
}
