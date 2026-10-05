package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork patch (RHI-7827): end-to-end through the HTTP server, project, network,
// upstream and HTTP client. Every fork call site is on this path, so these
// tests fail if an upstream sync drops any of them.

// signedTxForChain123 returns a raw transaction signed for the test network's
// chain (123), and its hash. Bundle submission rejects transactions signed for
// another chain, as a node would, so the chain-1 samples elsewhere in this
// package cannot be used here.
func signedTxForChain123(t *testing.T) (string, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	to := ethcommon.HexToAddress("0x000000000000000000000000000000000000dEaD")
	tx, err := ethtypes.SignNewTx(key, ethtypes.LatestSignerForChainID(big.NewInt(123)), &ethtypes.DynamicFeeTx{
		ChainID:   big.NewInt(123),
		Nonce:     7,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(30_000_000_000),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(1),
	})
	require.NoError(t, err)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	return hexutil.Encode(raw), tx.Hash().Hex()
}

type capturedBundle struct {
	body      string
	signature string
}

type bundleFixture struct {
	mu           sync.Mutex
	bundles      []capturedBundle
	publicWrites atomic.Int32
}

func (f *bundleFixture) captured() []capturedBundle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedBundle(nil), f.bundles...)
}

// mockBundleNetwork registers the relay (rpc2) and the public upstream (rpc1).
// The public upstream counts any write it receives; the test fails if it ever
// sees one. gock runs Filter funcs before matching the host, so each filter
// checks the host itself.
func mockBundleNetwork(relayReply map[string]interface{}) *bundleFixture {
	f := &bundleFixture{}
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "rpc1.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, "eth_sendRawTransaction") || strings.Contains(body, "eth_sendBundle") {
				f.publicWrites.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x" + strings.Repeat("ee", 32)})
	// Keep-alive reads the sender's nonce through ordinary upstreams. 0 means
	// "not included yet".
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "rpc1.localhost" && strings.Contains(util.SafeReadBody(r), "eth_getTransactionCount")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x0"})
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "rpc2.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_sendBundle") {
				return false
			}
			f.mu.Lock()
			f.bundles = append(f.bundles, capturedBundle{body: body, signature: r.Header.Get("X-Flashbots-Signature")})
			f.mu.Unlock()
			return true
		}).
		Reply(200).
		JSON(relayReply)
	return f
}

func bundleSubmissionTestConfig(signingKey string, bs *common.BundleSubmissionConfig) *common.Config {
	if bs != nil && bs.ResubmitFor == 0 {
		// Keep-alive has its own tests; a short window stops its loop soon after each test.
		bs.ResubmitFor = common.Duration(500 * time.Millisecond)
	}
	return &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId:                  123,
					EnforceBlockAvailability: util.BoolPtr(true),
					BundleSubmission:         bs,
				},
			}},
			Upstreams: []*common.UpstreamConfig{
				{
					Id:       "rpc1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1.localhost",
					Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
				},
				{
					Id:            "relay",
					Type:          common.UpstreamTypeEvm,
					Endpoint:      "http://rpc2.localhost",
					Evm:           &common.EvmUpstreamConfig{ChainId: 123},
					Tags:          []string{"bundle-relay"},
					IgnoreMethods: []string{"*"},
					AllowMethods:  []string{"eth_sendBundle"},
					JsonRpc: &common.JsonRpcUpstreamConfig{
						SupportsBatch:       &common.FALSE,
						FlashbotsSigningKey: common.SecretString(signingKey),
					},
				},
			},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// startBundleServer boots the full stack and pins the selection order to
// [rpc1, relay]. The mocks give rpc1 a head far behind rpc2, so without the pin
// the default policy would exclude rpc1 for lag and "the public upstream never
// sees the tx" would hold trivially. Pinned, rpc1 is the first candidate for
// any method it accepts, which is what a dropped hook would expose.
func startBundleServer(t *testing.T, cfg *common.Config) (func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string), func(), func() int64) {
	t.Helper()
	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine, "rpc1", "relay")
	head := func() int64 {
		nw, err := prj.GetNetwork(context.Background(), "evm:123")
		require.NoError(t, err)
		return nw.EvmHighestLatestBlockNumber(context.Background())
	}
	return sendRequest, shutdown, head
}

// recoverBundleSigner rebuilds Flashbots' documented message (EIP-191 over the
// 0x-prefixed hex of keccak256(body)) and returns the address that signed it.
func recoverBundleSigner(t *testing.T, header, body string) string {
	t.Helper()
	addr, sigHex, ok := strings.Cut(header, ":")
	require.True(t, ok, "X-Flashbots-Signature must be <address>:<signature>, got %q", header)
	sig, err := hexutil.Decode(sigHex)
	require.NoError(t, err)
	text := hexutil.Encode(crypto.Keccak256([]byte(body)))
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(text), text)
	pub, err := crypto.SigToPub(crypto.Keccak256([]byte(msg)), sig)
	require.NoError(t, err)
	recovered := crypto.PubkeyToAddress(*pub).Hex()
	require.Equal(t, addr, recovered)
	return recovered
}

func sendRawTxBody(rawTx string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + rawTx + `"]}`
}

func TestBundleSubmission_EndToEnd(t *testing.T) {
	// Only the control case uses this chain-1 sample: without bundle submission
	// nothing decodes it.
	sendRawTx := sendRawTxBody(sampleSignedTx)

	t.Run("SubmitsSignedBundlesAndNeverTouchesPublicUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		f := mockBundleNetwork(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{"bundleHash": "0xb0"}})

		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		cfg := bundleSubmissionTestConfig(hexutil.Encode(crypto.FromECDSA(key)), &common.BundleSubmissionConfig{
			UseUpstream:  "bundle-relay",
			TargetBlocks: 3,
			BundleFields: map[string]interface{}{"builders": []interface{}{"flashbots", "Titan"}},
		})
		// The public upstream ranks first; the selector alone must keep bundles off it.
		sendRequest, shutdown, head := startBundleServer(t, cfg)
		defer shutdown()
		rawTx, txHash := signedTxForChain123(t)

		statusCode, _, body := sendRequest(sendRawTxBody(rawTx), nil, nil)

		require.Equal(t, http.StatusOK, statusCode, body)
		assert.Contains(t, body, `"result":"`+txHash+`"`, "callers get the tx hash back")
		assert.NotContains(t, body, "error")

		require.Eventually(t, func() bool { return len(f.captured()) >= 3 }, 5*time.Second, 10*time.Millisecond)
		bundles := f.captured()
		require.Len(t, bundles, 3)
		var blocks []string
		for _, b := range bundles {
			var req struct {
				Method string                   `json:"method"`
				Params []map[string]interface{} `json:"params"`
			}
			require.NoError(t, json.Unmarshal([]byte(b.body), &req))
			require.Equal(t, "eth_sendBundle", req.Method)
			require.Len(t, req.Params, 1)
			assert.Equal(t, []interface{}{rawTx}, req.Params[0]["txs"])
			assert.Equal(t, []interface{}{"flashbots", "Titan"}, req.Params[0]["builders"])
			blocks = append(blocks, req.Params[0]["blockNumber"].(string))
			assert.Equal(t, crypto.PubkeyToAddress(key.PublicKey).Hex(), recoverBundleSigner(t, b.signature, b.body),
				"every relay request is signed with the configured key, over the exact bytes sent")
		}
		sort.Strings(blocks)
		// Whatever head the network reports (how it is derived is upstream's
		// business), bundles target the next targetBlocks blocks.
		h := head()
		require.Positive(t, h)
		assert.Equal(t, []string{
			fmt.Sprintf("0x%x", h+1), fmt.Sprintf("0x%x", h+2), fmt.Sprintf("0x%x", h+3),
		}, blocks)
		assert.Zero(t, f.publicWrites.Load(), "the public upstream must never see the transaction")
	})

	t.Run("RelayRejectsEveryBlock_FailsClosed", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// The relay's real reply to an unsigned bundle (probed 2026-09-29): note "id": null.
		f := mockBundleNetwork(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]interface{}{"code": -32600, "message": "signature is required"},
		})

		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		cfg := bundleSubmissionTestConfig(hexutil.Encode(crypto.FromECDSA(key)), &common.BundleSubmissionConfig{
			UseUpstream:  "bundle-relay",
			TargetBlocks: 2,
		})
		sendRequest, shutdown, _ := startBundleServer(t, cfg)
		defer shutdown()
		rawTx, txHash := signedTxForChain123(t)

		statusCode, _, body := sendRequest(sendRawTxBody(rawTx), nil, nil)
		t.Logf("caller sees HTTP %d: %s", statusCode, body)

		assert.Contains(t, body, "error", "a rejected submission must surface as an error")
		assert.Contains(t, body, "signature is required", "the relay's reason reaches the caller")
		assert.NotContains(t, body, txHash, "never claim success the relay did not give")
		assert.NotEmpty(t, f.captured())
		// Give any (wrong) fallback a moment to reach the public upstream.
		time.Sleep(200 * time.Millisecond)
		assert.Zero(t, f.publicWrites.Load(), "a failed submission must not fall back to the public mempool")
	})

	t.Run("UndecodableTransaction_NothingSent", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		f := mockBundleNetwork(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{"bundleHash": "0xb0"}})

		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		cfg := bundleSubmissionTestConfig(hexutil.Encode(crypto.FromECDSA(key)), &common.BundleSubmissionConfig{
			UseUpstream:  "bundle-relay",
			TargetBlocks: 3,
		})
		sendRequest, shutdown, _ := startBundleServer(t, cfg)
		defer shutdown()

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x00"]}`, nil, nil)

		assert.Equal(t, http.StatusBadRequest, statusCode, body)
		time.Sleep(200 * time.Millisecond)
		assert.Empty(t, f.captured(), "nothing may be submitted")
		assert.Zero(t, f.publicWrites.Load())
	})

	t.Run("WithoutBundleSubmission_BroadcastsNormally", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		f := mockBundleNetwork(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{"bundleHash": "0xb0"}})

		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		cfg := bundleSubmissionTestConfig(hexutil.Encode(crypto.FromECDSA(key)), nil)
		sendRequest, shutdown, _ := startBundleServer(t, cfg)
		defer shutdown()

		statusCode, _, body := sendRequest(sendRawTx, nil, nil)

		require.Equal(t, http.StatusOK, statusCode, body)
		assert.Equal(t, int32(1), f.publicWrites.Load(), "the hook must be a no-op on networks without bundleSubmission")
		assert.Empty(t, f.captured())
	})
}
