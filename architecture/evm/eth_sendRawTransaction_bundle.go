package evm

// Fork patch (RHI-7827): on networks with evm.bundleSubmission, answer
// eth_sendRawTransaction by submitting the transaction as single-tx
// eth_sendBundle requests to the configured relays, one per target block.
// See PATCH_LIST.md and common/config_bundle_submission.go.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// bundleSubmissionDetachedTimeout caps how long one broadcast's submissions
// may keep running after the caller already has its answer. The network's
// failsafe policy for eth_sendBundle is the real bound; this only stops a
// network without one from leaking goroutines.
const bundleSubmissionDetachedTimeout = 30 * time.Second

type bundleTargetResult struct {
	block int64
	err   error
}

// projectPreForward_eth_sendRawTransaction hands the request to bundle
// submission when the network is configured for it. Once it is, every
// eth_sendRawTransaction is answered here, malformed ones included: falling
// through would send the transaction to the network's ordinary upstreams.
func projectPreForward_eth_sendRawTransaction(ctx context.Context, n common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	cfg := bundleSubmissionConfig(n)
	if cfg == nil {
		return false, nil, nil
	}
	resp, err = submitRawTransactionAsBundles(ctx, n, nq, cfg)
	return true, resp, err
}

func bundleSubmissionConfig(n common.Network) *common.BundleSubmissionConfig {
	if n == nil {
		return nil
	}
	nc := n.Config()
	if nc == nil || nc.Evm == nil {
		return nil
	}
	return nc.Evm.BundleSubmission
}

func submitRawTransactionAsBundles(ctx context.Context, n common.Network, nq *common.NormalizedRequest, cfg *common.BundleSubmissionConfig) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Project.PreForwardHook.eth_sendRawTransaction.bundleSubmission")
	defer span.End()

	lg := n.Logger().With().Str("hook", "bundleSubmission").Logger()
	record := func(outcome string) {
		telemetry.MetricBundleSubmissionTotal.WithLabelValues(n.ProjectId(), n.Label(), outcome).Inc()
		span.SetAttributes(attribute.String("bundle_submission.outcome", outcome))
	}

	rawTx, txHash, err := rawTransactionForBundle(ctx, nq)
	if err != nil {
		record(telemetry.BundleSubmissionOutcomeInvalidTx)
		return nil, common.NewErrInvalidRequest(err)
	}
	span.SetAttributes(attribute.String("tx_hash", txHash))

	head := common.EvmHighestLatestBlockNumber(n, ctx)
	if head <= 0 {
		record(telemetry.BundleSubmissionOutcomeHeadUnknown)
		return nil, common.NewErrUpstreamsExhaustedWithCause(
			fmt.Errorf("bundle submission: the network head is not known yet, so there is no block to target"),
		)
	}
	span.SetAttributes(attribute.Int64("head", head), attribute.Int("target_blocks", cfg.TargetBlocks))

	// The submissions deliberately outlive this request: the caller gets its
	// answer at the first accepted target block, and the rest keep going.
	subCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bundleSubmissionDetachedTimeout)
	parentID := nq.ID()
	results := make(chan bundleTargetResult, cfg.TargetBlocks)
	var wg sync.WaitGroup
	for i := 1; i <= cfg.TargetBlocks; i++ {
		block := head + int64(i)
		params := []interface{}{bundleParams(rawTx, block, cfg.BundleFields)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := forwardSubRequest(subCtx, n, parentID, cfg.UseUpstream, "eth_sendBundle", params)
			if err != nil {
				lg.Debug().Err(err).Str("txHash", txHash).Int64("targetBlock", block).Msg("bundle submission for target block failed")
			}
			results <- bundleTargetResult{block: block, err: err}
		}()
	}
	go func() {
		wg.Wait()
		cancel()
	}()

	var firstErr error
	var firstErrBlock int64
	for received := 0; received < cfg.TargetBlocks; received++ {
		select {
		case r := <-results:
			if r.err == nil {
				record(telemetry.BundleSubmissionOutcomeAccepted)
				return createSyntheticSuccessResponse(ctx, nq, txHash)
			}
			// Report the earliest target block's error: it is the one the
			// caller most needs, and it keeps the answer deterministic.
			if firstErr == nil || r.block < firstErrBlock {
				firstErr, firstErrBlock = r.err, r.block
			}
		case <-ctx.Done():
			record(telemetry.BundleSubmissionOutcomeAbandoned)
			return nil, ctx.Err()
		}
	}

	record(telemetry.BundleSubmissionOutcomeRejected)
	lg.Warn().Err(firstErr).
		Str("txHash", txHash).
		Int64("head", head).
		Int("targetBlocks", cfg.TargetBlocks).
		Msg("bundle submission failed for every target block")
	return nil, firstErr
}

// rawTransactionForBundle returns the signed transaction as the caller sent it
// (0x-prefixed) and its hash. A transaction eRPC cannot decode is rejected
// rather than submitted: without the hash there is no correct answer to give
// the caller.
func rawTransactionForBundle(ctx context.Context, nq *common.NormalizedRequest) (string, string, error) {
	txHash, err := extractTxHashFromSendRawTransaction(ctx, nq)
	if err != nil {
		return "", "", fmt.Errorf("eth_sendRawTransaction: %w", err)
	}
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return "", "", err
	}
	jrq.RLock()
	rawTx, _ := jrq.Params[0].(string)
	jrq.RUnlock()
	if !strings.HasPrefix(rawTx, "0x") {
		rawTx = "0x" + rawTx
	}
	return rawTx, txHash, nil
}

// bundleParams builds one eth_sendBundle params object: the configured fields
// verbatim, plus the two fields eRPC owns.
func bundleParams(rawTx string, block int64, fields map[string]interface{}) map[string]interface{} {
	p := make(map[string]interface{}, len(fields)+2)
	for k, v := range fields {
		p[k] = v
	}
	p["txs"] = []string{rawTx}
	p["blockNumber"] = fmt.Sprintf("0x%x", block)
	return p
}
