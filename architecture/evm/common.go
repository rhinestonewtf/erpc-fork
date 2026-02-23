package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// upstreamPostForward_markUnexpectedEmpty converts empty results for point-lookups
// (blocks, transactions, receipts, traces, etc.) to missing-data so network retry can rotate.
func upstreamPostForward_markUnexpectedEmpty(
	ctx context.Context,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil || rs.IsObjectNull() || !rs.IsResultEmptyish() {
		return rs, re
	}

	if rq != nil {
		if rd := rq.Directives(); rd != nil && !rd.RetryEmpty {
			return rs, re
		}
	}

	// Build a simple message and include raw result in details for diagnostics.
	method, _ := rq.Method()
	details := map[string]interface{}{"method": method}
	if jrr, jerr := rs.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
		details["rawResult"] = jrr.GetResultString()
	}

	missingErr := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			"upstream returned unexpected empty data",
			nil,
			details,
		),
		u,
	)
	// Mark retryable if the requested block is near the upstream's head (transient propagation race)
	if mdErr, ok := missingErr.(*common.ErrEndpointMissingData); ok && u != nil {
		if evmUps, ok := u.(common.EvmUpstream); ok {
			if sp := evmUps.EvmStatePoller(); sp != nil {
				latestBlock := sp.LatestBlock()
				if bn := rq.EvmBlockNumber(); bn != nil {
					if blockNum, ok := bn.(int64); ok && latestBlock > 0 && blockNum > latestBlock {
						if blockNum-latestBlock <= 4 {
							mdErr.MarkRetryableTowardsUpstream()
						}
					}
				}
			}
		}
	}
	return rs, missingErr
}
