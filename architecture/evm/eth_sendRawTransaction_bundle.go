package evm

// Fork patch (RHI-7827): on networks with evm.bundleSubmission, answer
// eth_sendRawTransaction by submitting the transaction as single-tx
// eth_sendBundle requests to the configured relays, one per target block, and
// keep resubmitting it for each new block until it is included or its window
// ends. See PATCH_LIST.md and common/config_bundle_submission.go.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"go.opentelemetry.io/otel/attribute"
)

// bundleSubmissionDetachedTimeout caps one batch of submissions, or one nonce
// read, that runs detached from any caller. The network's failsafe policy is
// the real bound; this only stops a network without one from leaking
// goroutines.
const bundleSubmissionDetachedTimeout = 30 * time.Second

// bundleKeepAliveMaxKept bounds the memory and relay traffic of one network's
// kept transactions in one process.
const bundleKeepAliveMaxKept = 10_000

// bundleKeepAliveTick is how often the keep-alive loop looks for a new network
// head.
const bundleKeepAliveTick = time.Second

// bundleKeepAliveTickOverride, when non-zero, replaces bundleKeepAliveTick in
// tests. It is atomic because loops started by earlier tests may still be
// reading it.
var bundleKeepAliveTickOverride atomic.Int64

type bundleTargetResult struct {
	block int64
	err   error
}

// sendRawTx is a decoded eth_sendRawTransaction.
type sendRawTx struct {
	raw    string // 0x-prefixed signed transaction, as the caller sent it
	hash   string
	sender string
	nonce  uint64
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

	tx, err := decodeSendRawTx(nq, n)
	if err != nil {
		record(telemetry.BundleSubmissionOutcomeInvalidTx)
		return nil, common.NewErrInvalidRequest(err)
	}
	span.SetAttributes(attribute.String("tx_hash", tx.hash))

	head := common.EvmHighestLatestBlockNumber(n, ctx)
	if head <= 0 {
		record(telemetry.BundleSubmissionOutcomeHeadUnknown)
		return nil, common.NewErrUpstreamsExhaustedWithCause(
			fmt.Errorf("bundle submission: the network head is not known yet, so there is no block to target"),
		)
	}
	span.SetAttributes(attribute.Int64("head", head), attribute.Int("target_blocks", cfg.TargetBlocks))

	targets := make([]int64, 0, cfg.TargetBlocks)
	for b := head + 1; b <= head+int64(cfg.TargetBlocks); b++ {
		targets = append(targets, b)
	}
	results := submitBundles(context.WithoutCancel(ctx), n, nq.ID(), cfg, tx.raw, targets)

	// The collector deliberately outlives this request: the caller is answered
	// at the first accepted target block, and an accepted transaction is kept
	// alive even if the caller has already gone. Keep-alive learns the outcome
	// of every target block, so one that fails here is retried there.
	answer := make(chan error, 1)
	go func() {
		ka := keepAliveFor(n)
		pending := make(map[int64]struct{}, len(targets))
		for _, b := range targets {
			pending[b] = struct{}{}
		}
		accepted := false
		var firstErr error
		var firstErrBlock int64
		for r := range results {
			delete(pending, r.block)
			switch {
			case r.err == nil && !accepted:
				accepted = true
				ka.keep(tx, r.block, keys(pending), cfg.ResubmitFor.Duration())
				answer <- nil
			case accepted:
				ka.recordTarget(tx.hash, r.block, r.err == nil)
			}
			if r.err == nil {
				continue
			}
			lg.Debug().Err(r.err).Str("txHash", tx.hash).Int64("targetBlock", r.block).Msg("bundle submission for target block failed")
			// Report the earliest target block's error: it is the one the
			// caller most needs, and it keeps the answer deterministic.
			if firstErr == nil || r.block < firstErrBlock {
				firstErr, firstErrBlock = r.err, r.block
			}
		}
		if !accepted {
			answer <- firstErr
		}
	}()

	select {
	case err := <-answer:
		if err != nil {
			record(telemetry.BundleSubmissionOutcomeRejected)
			lg.Warn().Err(err).
				Str("txHash", tx.hash).
				Int64("head", head).
				Int("targetBlocks", cfg.TargetBlocks).
				Msg("bundle submission failed for every target block")
			return nil, err
		}
		record(telemetry.BundleSubmissionOutcomeAccepted)
		return createSyntheticSuccessResponse(ctx, nq, tx.hash)
	case <-ctx.Done():
		record(telemetry.BundleSubmissionOutcomeAbandoned)
		return nil, ctx.Err()
	}
}

// decodeSendRawTx decodes the signed transaction a caller sent. A transaction
// eRPC cannot decode, whose signature yields no sender, or that is signed for
// another chain can never be included on this network, so it is rejected
// instead of submitted, as a node would reject it.
func decodeSendRawTx(nq *common.NormalizedRequest, n common.Network) (*sendRawTx, error) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	jrq.RLock()
	var param interface{}
	if len(jrq.Params) > 0 {
		param = jrq.Params[0]
	}
	jrq.RUnlock()

	raw, ok := param.(string)
	if !ok {
		return nil, fmt.Errorf("eth_sendRawTransaction: params[0] must be the signed transaction as a hex string")
	}
	if !strings.HasPrefix(raw, "0x") {
		raw = "0x" + raw
	}
	b, err := hex.DecodeString(raw[2:])
	if err != nil {
		return nil, fmt.Errorf("eth_sendRawTransaction: transaction is not valid hex: %w", err)
	}
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("eth_sendRawTransaction: failed to decode transaction: %w", err)
	}

	var signer ethtypes.Signer = ethtypes.HomesteadSigner{}
	if tx.Protected() {
		if nc := n.Config(); nc != nil && nc.Evm != nil && nc.Evm.ChainId != 0 &&
			tx.ChainId().Cmp(big.NewInt(nc.Evm.ChainId)) != 0 {
			return nil, fmt.Errorf("eth_sendRawTransaction: transaction is signed for chain %s, not %d", tx.ChainId(), nc.Evm.ChainId)
		}
		signer = ethtypes.LatestSignerForChainID(tx.ChainId())
	}
	from, err := ethtypes.Sender(signer, tx)
	if err != nil {
		return nil, fmt.Errorf("eth_sendRawTransaction: invalid sender: %w", err)
	}
	return &sendRawTx{raw: raw, hash: tx.Hash().Hex(), sender: from.Hex(), nonce: tx.Nonce()}, nil
}

// submitBundles submits rawTx as one eth_sendBundle per block, concurrently,
// pinned to the configured relays. It returns the results on a channel that is
// closed once every submission has finished.
func submitBundles(ctx context.Context, n common.Network, parentID interface{}, cfg *common.BundleSubmissionConfig, rawTx string, blocks []int64) <-chan bundleTargetResult {
	ctx, cancel := context.WithTimeout(ctx, bundleSubmissionDetachedTimeout)
	results := make(chan bundleTargetResult, len(blocks))
	var wg sync.WaitGroup
	for _, block := range blocks {
		params := []interface{}{bundleParams(rawTx, block, cfg.BundleFields)}
		wg.Add(1)
		go func(block int64) {
			defer wg.Done()
			_, err := forwardSubRequest(ctx, n, parentID, cfg.UseUpstream, "eth_sendBundle", params)
			results <- bundleTargetResult{block: block, err: err}
		}(block)
	}
	go func() {
		wg.Wait()
		cancel()
		close(results)
	}()
	return results
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

func keys(m map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// bundleKeepAlive keeps a network's accepted transactions submitted for each
// new block until the sender's nonce passes them or their window ends. A
// bundle is only valid for the block it targets, so without this a caller that
// sends once and waits for a receipt would lose its transaction after
// targetBlocks blocks. State is in memory, one per network: a restart forgets
// kept transactions, as a node restart forgets its mempool. The loop runs
// only while something is kept.
type bundleKeepAlive struct {
	network common.Network

	mu      sync.Mutex
	kept    map[string]*keptTx // by tx hash
	running bool
}

type keptTx struct {
	tx *sendRawTx
	// Target blocks a relay accepted a bundle for, and target blocks whose
	// submission is still running. A future block in neither is due, which
	// is how a target block whose submission failed gets retried.
	accepted map[int64]struct{}
	inflight map[int64]struct{}
	deadline time.Time // no submissions after this
}

var bundleKeepAlives sync.Map // common.Network -> *bundleKeepAlive

func keepAliveFor(n common.Network) *bundleKeepAlive {
	if v, ok := bundleKeepAlives.Load(n); ok {
		return v.(*bundleKeepAlive)
	}
	v, _ := bundleKeepAlives.LoadOrStore(n, &bundleKeepAlive{network: n, kept: make(map[string]*keptTx)})
	return v.(*bundleKeepAlive)
}

// keep starts keeping tx alive for window, given the target block a relay just
// accepted and the target blocks whose submission is still running. A
// re-broadcast of a kept transaction adds to what it knows and restarts its
// window.
func (k *bundleKeepAlive) keep(tx *sendRawTx, acceptedBlock int64, inflight []int64, window time.Duration) {
	deadline := time.Now().Add(window)
	k.mu.Lock()
	defer k.mu.Unlock()
	kt, ok := k.kept[tx.hash]
	if !ok {
		if len(k.kept) >= bundleKeepAliveMaxKept {
			k.record(telemetry.BundleKeepAliveOutcomeOverflow)
			return
		}
		kt = &keptTx{tx: tx, accepted: make(map[int64]struct{}), inflight: make(map[int64]struct{})}
		k.kept[tx.hash] = kt
		k.updateGauge()
		if !k.running {
			k.running = true
			go k.run()
		}
	}
	kt.deadline = deadline
	kt.accepted[acceptedBlock] = struct{}{}
	for _, b := range inflight {
		kt.inflight[b] = struct{}{}
	}
}

// recordTarget records how a kept transaction's submission for block ended. A
// block that was not accepted becomes due again while it is still ahead.
func (k *bundleKeepAlive) recordTarget(hash string, block int64, accepted bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	kt, ok := k.kept[hash]
	if !ok {
		return
	}
	delete(kt.inflight, block)
	if accepted {
		kt.accepted[block] = struct{}{}
	}
}

func (k *bundleKeepAlive) run() {
	tick := bundleKeepAliveTick
	if o := bundleKeepAliveTickOverride.Load(); o > 0 {
		tick = time.Duration(o)
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var lastHead int64
	for range ticker.C {
		cfg := bundleSubmissionConfig(k.network)
		if !k.prune(cfg, time.Now()) {
			return
		}
		head := common.EvmHighestLatestBlockNumber(k.network, context.Background())
		if head <= lastHead {
			continue
		}
		lastHead = head
		k.step(cfg, head)
	}
}

// prune drops transactions whose window has ended, or all of them if bundle
// submission was removed from the network, and reports whether any are left.
// When none are, it marks the loop stopped under the lock keep takes, so a
// transaction kept concurrently starts a new loop.
func (k *bundleKeepAlive) prune(cfg *common.BundleSubmissionConfig, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for hash, kt := range k.kept {
		switch {
		case cfg == nil:
			delete(k.kept, hash)
			k.record(telemetry.BundleKeepAliveOutcomeDisabled)
		case now.After(kt.deadline):
			delete(k.kept, hash)
			k.record(telemetry.BundleKeepAliveOutcomeExpired)
			k.network.Logger().Warn().
				Str("txHash", hash).
				Str("sender", kt.tx.sender).
				Uint64("nonce", kt.tx.nonce).
				Msg("bundle submission gave up: the transaction was not included before resubmitFor ran out")
		}
	}
	k.updateGauge()
	if len(k.kept) == 0 {
		k.running = false
		return false
	}
	return true
}

// step settles transactions whose sender's nonce has moved past them, then
// submits the rest for every target block (head+1 … head+targetBlocks) that no
// relay has accepted and no submission is running for: the block the new head
// brings into reach, and any earlier one whose submission failed.
func (k *bundleKeepAlive) step(cfg *common.BundleSubmissionConfig, head int64) {
	k.mu.Lock()
	senders := make(map[string]struct{}, len(k.kept))
	for _, kt := range k.kept {
		senders[kt.tx.sender] = struct{}{}
	}
	k.mu.Unlock()
	nonces := k.latestNonces(senders)

	type resubmission struct {
		hash   string
		raw    string
		blocks []int64
	}
	var due []resubmission
	upTo := head + int64(cfg.TargetBlocks)
	k.mu.Lock()
	for hash, kt := range k.kept {
		if onChain, ok := nonces[kt.tx.sender]; ok && onChain > kt.tx.nonce {
			delete(k.kept, hash)
			k.record(telemetry.BundleKeepAliveOutcomeSettled)
			continue
		}
		for b := range kt.accepted {
			if b <= head {
				delete(kt.accepted, b)
			}
		}
		for b := range kt.inflight {
			if b <= head {
				delete(kt.inflight, b)
			}
		}
		var blocks []int64
		for b := head + 1; b <= upTo; b++ {
			_, isAccepted := kt.accepted[b]
			_, isInflight := kt.inflight[b]
			if !isAccepted && !isInflight {
				blocks = append(blocks, b)
				kt.inflight[b] = struct{}{}
			}
		}
		if len(blocks) > 0 {
			due = append(due, resubmission{hash: hash, raw: kt.tx.raw, blocks: blocks})
		}
	}
	k.updateGauge()
	k.mu.Unlock()

	lg := k.network.Logger().With().Str("hook", "bundleSubmission").Logger()
	for _, r := range due {
		results := submitBundles(context.Background(), k.network, nil, cfg, r.raw, r.blocks)
		go func(r resubmission) {
			for res := range results {
				k.recordTarget(r.hash, res.block, res.err == nil)
				if res.err != nil {
					lg.Debug().Err(res.err).Str("txHash", r.hash).Int64("targetBlock", res.block).Msg("bundle resubmission for target block failed")
				}
			}
		}(r)
	}
}

// latestNonces reads each sender's on-chain nonce (its transaction count at
// latest). A sender whose read fails is left out, so its transactions simply
// stay kept until a later read succeeds.
func (k *bundleKeepAlive) latestNonces(senders map[string]struct{}) map[string]uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), bundleSubmissionDetachedTimeout)
	defer cancel()
	var mu sync.Mutex
	out := make(map[string]uint64, len(senders))
	var wg sync.WaitGroup
	for sender := range senders {
		wg.Add(1)
		go func(sender string) {
			defer wg.Done()
			result, err := forwardSubRequest(ctx, k.network, nil, "", "eth_getTransactionCount", []interface{}{sender, "latest"})
			if err != nil {
				return
			}
			nonce, err := common.HexToUint64(strings.Trim(string(result), `"`))
			if err != nil {
				return
			}
			mu.Lock()
			out[sender] = nonce
			mu.Unlock()
		}(sender)
	}
	wg.Wait()
	return out
}

// record and updateGauge are called with k.mu held.
func (k *bundleKeepAlive) record(outcome string) {
	telemetry.MetricBundleKeepAliveTotal.WithLabelValues(k.network.ProjectId(), k.network.Label(), outcome).Inc()
}

func (k *bundleKeepAlive) updateGauge() {
	telemetry.MetricBundleKeepAliveTracked.WithLabelValues(k.network.ProjectId(), k.network.Label()).Set(float64(len(k.kept)))
}
