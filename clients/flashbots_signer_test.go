package clients

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patch (RHI-7827) tests. TestHttpJsonRpcClient_SignsExactBytesSent also
// fails if the prepareRequest call site is dropped by an upstream sync.

// recoverFlashbotsSigner checks a header value against the message Flashbots
// documents: an EIP-191 personal message whose text is the 0x-prefixed hex of
// keccak256(body). The prefix is rebuilt by hand rather than through
// accounts.TextHash, so the test does not share the code under test.
func recoverFlashbotsSigner(t *testing.T, header string, body []byte) string {
	t.Helper()
	addr, sigHex, ok := strings.Cut(header, ":")
	require.True(t, ok, "header must be <address>:<signature>, got %q", header)
	sig, err := hexutil.Decode(sigHex)
	require.NoError(t, err)
	require.Len(t, sig, 65)

	text := hexutil.Encode(crypto.Keccak256(body))
	require.Len(t, text, 66)
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(text), text)
	pub, err := crypto.SigToPub(crypto.Keccak256([]byte(msg)), sig)
	require.NoError(t, err)
	recovered := crypto.PubkeyToAddress(*pub).Hex()
	assert.Equal(t, addr, recovered, "the address in the header must be the one that signed")
	return recovered
}

func newTestSigningKey(t *testing.T) (*ecdsa.PrivateKey, common.SecretString) {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return key, common.SecretString(hexutil.Encode(crypto.FromECDSA(key)))
}

func TestFlashbotsSigner_SignsTheDocumentedMessage(t *testing.T) {
	key, hexKey := newTestSigningKey(t)
	signer, err := newFlashbotsSigner(string(hexKey))
	require.NoError(t, err)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendBundle","params":[{"txs":["0x02"],"blockNumber":"0x1"}]}`)

	value, err := signer.headerValue(body)

	require.NoError(t, err)
	assert.Equal(t, crypto.PubkeyToAddress(key.PublicKey).Hex(), recoverFlashbotsSigner(t, value, body))
}

func TestFlashbotsSigner_AcceptsKeyWithoutHexPrefix(t *testing.T) {
	key, hexKey := newTestSigningKey(t)

	signer, err := newFlashbotsSigner(strings.TrimPrefix(string(hexKey), "0x"))

	require.NoError(t, err)
	assert.Equal(t, crypto.PubkeyToAddress(key.PublicKey).Hex(), signer.address)
}

func TestFlashbotsSigner_InvalidKeyIsNeverEchoed(t *testing.T) {
	for name, key := range map[string]string{
		"not hex":       "0xzz" + strings.Repeat("ab", 31),
		"too short":     "0xdeadbeefcafe",
		"off the curve": "0x" + strings.Repeat("00", 32),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newFlashbotsSigner(key)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), strings.TrimPrefix(key, "0x")[:8], "a private key must never appear in an error")
		})
	}
}

func TestHttpJsonRpcClient_SignsExactBytesSent(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var gotBody []byte
	var gotHeader string
	gock.New("http://relay.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			mu.Lock()
			defer mu.Unlock()
			gotBody = []byte(util.SafeReadBody(r))
			gotHeader = r.Header.Get("X-Flashbots-Signature")
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{"bundleHash": "0xb0"}})

	key, hexKey := newTestSigningKey(t)
	ups := common.NewFakeUpstream("relay")
	client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups,
		&url.URL{Scheme: "http", Host: "relay.localhost"},
		&common.JsonRpcUpstreamConfig{FlashbotsSigningKey: hexKey}, nil, &noopErrorExtractor{})
	require.NoError(t, err)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendBundle","params":[{"txs":["0x02"],"blockNumber":"0x1"}]}`))
	_, err = client.SendRequest(ctx, req)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, gotBody)
	require.NotEmpty(t, gotHeader, "a signing upstream must send X-Flashbots-Signature")
	assert.Equal(t, crypto.PubkeyToAddress(key.PublicKey).Hex(), recoverFlashbotsSigner(t, gotHeader, gotBody),
		"the signature must cover exactly the bytes on the wire")
}

func TestHttpJsonRpcClient_NoSignatureWithoutKey(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	headerSeen := false
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			mu.Lock()
			defer mu.Unlock()
			_, headerSeen = r.Header["X-Flashbots-Signature"]
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	ups := common.NewFakeUpstream("rpc1")
	client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups,
		&url.URL{Scheme: "http", Host: "rpc1.localhost"}, ups.Config().JsonRpc, nil, &noopErrorExtractor{})
	require.NoError(t, err)

	_, err = client.SendRequest(ctx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)))
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.False(t, headerSeen)
}

func TestHttpJsonRpcClient_SigningRefusesGzipAndBatch(t *testing.T) {
	logger := log.Logger
	_, hexKey := newTestSigningKey(t)
	for name, cfg := range map[string]*common.JsonRpcUpstreamConfig{
		"gzip":  {FlashbotsSigningKey: hexKey, EnableGzip: util.BoolPtr(true)},
		"batch": {FlashbotsSigningKey: hexKey, SupportsBatch: util.BoolPtr(true), BatchMaxSize: 10, BatchMaxWait: common.Duration(1)},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", common.NewFakeUpstream("relay"),
				&url.URL{Scheme: "http", Host: "relay.localhost"}, cfg, nil, &noopErrorExtractor{})
			require.Error(t, err, "the signed bytes would differ from the bytes sent")
		})
	}
}
