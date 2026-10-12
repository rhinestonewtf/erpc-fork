package clients

// Fork patch (RHI-7827): sign every outgoing request body for upstreams that
// set jsonRpc.flashbotsSigningKey, as Flashbots relays and builders require.
// See PATCH_LIST.md.

import (
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

const flashbotsSignatureHeader = "X-Flashbots-Signature"

type flashbotsSigner struct {
	key *ecdsa.PrivateKey
	// address is the checksummed address of key, which Flashbots uses to
	// track reputation and credit refunds.
	address string
}

func newFlashbotsSigner(hexKey string) (*flashbotsSigner, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(hexKey), "0x"))
	if err != nil {
		// Never wrap err: depending on the failure it can quote part of the key.
		return nil, fmt.Errorf("jsonRpc.flashbotsSigningKey is not a valid secp256k1 private key")
	}
	return &flashbotsSigner{
		key:     key,
		address: crypto.PubkeyToAddress(key.PublicKey).Hex(),
	}, nil
}

// headerValue returns "<address>:<signature>", where signature is the EIP-191
// personal-message signature of the hex-encoded keccak256 of body. This is the
// scheme in Flashbots' Go reference client.
func (s *flashbotsSigner) headerValue(body []byte) (string, error) {
	digest := accounts.TextHash([]byte(hexutil.Encode(crypto.Keccak256(body))))
	sig, err := crypto.Sign(digest, s.key)
	if err != nil {
		return "", err
	}
	return s.address + ":" + hexutil.Encode(sig), nil
}

// configureFlashbotsSigner builds the signer for an upstream that carries a
// signing key. It refuses the transport settings that would make the signed
// bytes differ from the bytes sent, even if config validation was bypassed.
func (c *GenericHttpJsonRpcClient) configureFlashbotsSigner(cfg *common.JsonRpcUpstreamConfig) error {
	if cfg == nil || cfg.FlashbotsSigningKey == "" {
		return nil
	}
	if c.enableGzip || c.supportsBatch {
		return fmt.Errorf("jsonRpc.flashbotsSigningKey requires enableGzip and supportsBatch to be off")
	}
	signer, err := newFlashbotsSigner(string(cfg.FlashbotsSigningKey))
	if err != nil {
		return err
	}
	c.flashbotsSigner = signer
	return nil
}

// signRequest sets the signature header over body, which must be exactly the
// bytes httpReq sends. No-op for upstreams without a signing key.
func (c *GenericHttpJsonRpcClient) signRequest(httpReq *http.Request, body []byte) error {
	if c.flashbotsSigner == nil {
		return nil
	}
	value, err := c.flashbotsSigner.headerValue(body)
	if err != nil {
		return fmt.Errorf("failed to compute %s: %w", flashbotsSignatureHeader, err)
	}
	httpReq.Header.Set(flashbotsSignatureHeader, value)
	return nil
}
