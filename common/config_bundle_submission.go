package common

// Fork patch (RHI-7827): submit eth_sendRawTransaction as Flashbots-style
// eth_sendBundle requests, and sign requests for upstreams that need an
// X-Flashbots-Signature. The logic lives in files of its own so an upstream
// sync can only conflict on the field declarations and one-line call sites.
// See PATCH_LIST.md.

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// DefaultBundleSubmissionTargetBlocks is how many consecutive blocks, starting
// at the network head + 1, one broadcast is submitted for when the network
// config does not say.
const DefaultBundleSubmissionTargetBlocks = 3

// DefaultBundleSubmissionResubmitFor is how long eRPC keeps an accepted
// transaction alive when the network config does not say. Flashbots Protect
// keeps transactions for 25 blocks, about five minutes on mainnet.
const DefaultBundleSubmissionResubmitFor = Duration(5 * time.Minute)

// BundleSubmissionConfig makes an EVM network answer every
// eth_sendRawTransaction by sending the transaction as a single-tx
// eth_sendBundle to the upstreams matching UseUpstream, once for each of the
// next TargetBlocks blocks. The transaction never enters normal upstream
// selection, so no retry, hedge or failover can deliver it to a
// public-mempool upstream. Callers still receive the transaction hash.
//
// A bundle is only valid for the block it targets, so once a broadcast is
// accepted eRPC keeps submitting the transaction for each new block until the
// sender's nonce moves past it, or ResubmitFor runs out. Callers that send once
// and wait for a receipt therefore get mempool-like retention.
type BundleSubmissionConfig struct {
	// UseUpstream selects the bundle relays: an upstream id or tag, with the
	// same syntax as the use-upstream directive. Required.
	UseUpstream string `yaml:"useUpstream" json:"useUpstream"`

	// TargetBlocks is how many consecutive blocks, starting at the network
	// head + 1, each broadcast is submitted for (one eth_sendBundle per block).
	TargetBlocks int `yaml:"targetBlocks,omitempty" json:"targetBlocks"`

	// ResubmitFor is how long after its latest broadcast an accepted
	// transaction keeps being submitted for each new block. Submission stops
	// early once the sender's on-chain nonce passes the transaction's nonce
	// (included, or replaced). A re-broadcast restarts the window.
	ResubmitFor Duration `yaml:"resubmitFor,omitempty" json:"resubmitFor" tstype:"Duration"`

	// BundleFields is merged verbatim into every eth_sendBundle params object,
	// e.g. `builders`. eRPC owns `txs` and `blockNumber`; setting either is a
	// config error.
	BundleFields map[string]interface{} `yaml:"bundleFields,omitempty" json:"bundleFields,omitempty"`
}

// bundleSubmissionOwnedFields are the eth_sendBundle params eRPC fills for
// every submission, so config may not set them.
var bundleSubmissionOwnedFields = []string{"txs", "blockNumber"}

func (c *BundleSubmissionConfig) SetDefaults() {
	if c.TargetBlocks == 0 {
		c.TargetBlocks = DefaultBundleSubmissionTargetBlocks
	}
	if c.ResubmitFor == 0 {
		c.ResubmitFor = DefaultBundleSubmissionResubmitFor
	}
}

func (c *BundleSubmissionConfig) Validate() error {
	if strings.TrimSpace(c.UseUpstream) == "" {
		return fmt.Errorf("network.*.evm.bundleSubmission.useUpstream is required (an upstream id or tag)")
	}
	if err := ValidatePattern(c.UseUpstream); err != nil {
		return fmt.Errorf("network.*.evm.bundleSubmission.useUpstream has invalid selector %q: %w", c.UseUpstream, err)
	}
	if c.TargetBlocks < 1 {
		return fmt.Errorf("network.*.evm.bundleSubmission.targetBlocks must be at least 1, got %d", c.TargetBlocks)
	}
	if c.ResubmitFor < 0 {
		return fmt.Errorf("network.*.evm.bundleSubmission.resubmitFor must not be negative, got %s", c.ResubmitFor.Duration())
	}
	for _, owned := range bundleSubmissionOwnedFields {
		if _, ok := c.BundleFields[owned]; ok {
			return fmt.Errorf("network.*.evm.bundleSubmission.bundleFields must not set %q: eRPC fills it for every submission", owned)
		}
	}
	return nil
}

// validateBundleSubmissionNotDefaulted rejects bundleSubmission under
// networkDefaults.evm. Networks created on demand copy networkDefaults.evm
// wholesale, so a default would route every chain's writes to the bundle
// relays, including chains that have none.
func validateBundleSubmissionNotDefaulted(d *NetworkDefaults) error {
	if d != nil && d.Evm != nil && d.Evm.BundleSubmission != nil {
		return fmt.Errorf("networkDefaults.evm.bundleSubmission is not allowed: set evm.bundleSubmission on each network that has a bundle relay")
	}
	return nil
}

// SecretString holds a credential read from config, usually via ${ENV}
// expansion. It marshals as "REDACTED", so the value never appears in the
// startup config log, the admin erpc_config method or a config dump.
type SecretString string

const redactedSecret = "REDACTED"

func (s SecretString) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return []byte(`"` + redactedSecret + `"`), nil
}

func (s SecretString) MarshalYAML() (interface{}, error) {
	if s == "" {
		return "", nil
	}
	return redactedSecret, nil
}

// String keeps the value out of %v / %s formatting as well.
func (s SecretString) String() string {
	if s == "" {
		return ""
	}
	return redactedSecret
}

// validateFlashbotsSigning checks the signing key's shape and the transport
// settings signing depends on. The signature covers the exact JSON body sent,
// so the body must not be gzip-compressed or merged into a batch. The key
// itself is parsed (and rejected if it is not a valid secp256k1 key) when the
// upstream's client is built.
func (j *JsonRpcUpstreamConfig) validateFlashbotsSigning() error {
	if j.FlashbotsSigningKey == "" {
		return nil
	}
	key := strings.TrimPrefix(strings.TrimSpace(string(j.FlashbotsSigningKey)), "0x")
	if b, err := hex.DecodeString(key); err != nil || len(b) != 32 {
		// Never echo the value: it is a private key.
		return fmt.Errorf("jsonRpc.flashbotsSigningKey must be a 32-byte hex private key")
	}
	if j.EnableGzip != nil && *j.EnableGzip {
		return fmt.Errorf("jsonRpc.flashbotsSigningKey cannot be combined with jsonRpc.enableGzip: the signature covers the uncompressed body")
	}
	if j.SupportsBatch != nil && *j.SupportsBatch {
		return fmt.Errorf("jsonRpc.flashbotsSigningKey cannot be combined with jsonRpc.supportsBatch: each request must be signed on its own")
	}
	return nil
}
