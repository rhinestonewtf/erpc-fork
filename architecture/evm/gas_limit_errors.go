package evm

import "strings"

// FORK PATCH (RHI-6277). Kept in its own file so an upstream rebase touches only the
// five-line call site in error_normalizer.go, not this logic. See PATCH_LIST.md.
//
// A rejection of the transaction's GAS LIMIT is terminal: the limit is fixed in the
// signed bytes, and both bounds on it are CONSENSUS rules — the per-transaction cap
// (EIP-7825, 2^24 on most chains) and the intrinsic-gas floor. Every upstream for a
// chain enforces the same numbers, so no failover and no fee bump can change the
// verdict; retrying only burns the caller's timeout budget.
//
// Why this is not what upstream already does: these messages currently land in the
// generic -32003 / "out of gas" branch, which marks eth_sendRawTransaction retryable
// toward other upstreams on the rationale that "different providers may have different
// balance-checking or gas-estimation logic". True for pool admission and min-fee policy
// — node-local, so another upstream may accept the same broadcast — but not for a
// consensus gas bound. This carve-out is therefore deliberately narrow and runs BEFORE
// that branch, leaving bare -32003 "transaction rejected" and underpriced/mempool
// rejections retryable exactly as upstream intends (see upstream #1094).
//
// Cost of getting it wrong, measured: a Base 8453 lane signed a limit 15,981 gas over
// 2^24, the node answered "gas limit too high" in ~100ms, erpc retried it across
// upstreams for ~17s and returned ErrUpstreamsExhausted. The caller's 5s submit timeout
// fired first, so it only ever saw a TimeoutError, never learned the transaction was
// permanently invalid, and re-broadcast the same bytes 686 times over 7h while the
// stuck nonce head-of-line blocked the lane.
var terminalGasLimitRejections = []string{
	// Over the per-transaction cap. Base says the first for cap+1 and the second for
	// 30M; Optimism says the first for both — so neither string alone is sufficient.
	// All measured by bisecting eth_sendRawTransaction against public nodes.
	"gas limit too high",
	"exceeds max transaction gas limit",
	// Over the block gas limit: never admissible either, same reasoning.
	"exceeds block gas limit",
	// Under the intrinsic-gas floor. Note the pre-existing "gas too low" substring in
	// error_normalizer.go already matches this and makes it RETRYABLE, which is the
	// half of the bug a new pattern alone would not fix.
	"intrinsic gas too low",
	// geth's error identifier, as some clients echo it verbatim.
	"intrinsicgas",
}

// isTerminalGasLimitRejection reports whether msg is a gas-limit verdict that no
// upstream can answer differently. msg is matched case-insensitively; callers may pass
// an already-lowercased string.
func isTerminalGasLimitRejection(msg string) bool {
	ml := strings.ToLower(msg)
	for _, pattern := range terminalGasLimitRejections {
		if strings.Contains(ml, pattern) {
			return true
		}
	}
	return false
}
