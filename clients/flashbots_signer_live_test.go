//go:build flashbots_live

// Live check (fork patch RHI-7827) that Flashbots relays accept this signer's
// X-Flashbots-Signature. Gated by the `flashbots_live` build tag so it never runs
// in CI:
//
//	go test -tags=flashbots_live -count=1 -run TestFlashbotsSigner_Live ./clients/
//
// It signs with a throwaway key generated in memory and calls eth_cancelBundle
// for a random replacementUuid. That key has never submitted a bundle, so the
// call cancels nothing.

package clients

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

func TestFlashbotsSigner_LiveRelayAcceptsSignature(t *testing.T) {
	for _, relay := range []string{"https://relay.flashbots.net", "https://relay-sepolia.flashbots.net"} {
		t.Run(relay, func(t *testing.T) {
			key, err := crypto.GenerateKey()
			require.NoError(t, err)
			signer, err := newFlashbotsSigner(hexutil.Encode(crypto.FromECDSA(key)))
			require.NoError(t, err)

			u := make([]byte, 16)
			_, err = rand.Read(u)
			require.NoError(t, err)
			u[6] = (u[6] & 0x0f) | 0x40 // UUID v4
			u[8] = (u[8] & 0x3f) | 0x80
			h := hex.EncodeToString(u)
			uuid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_cancelBundle","params":[{"replacementUuid":"` + uuid + `"}]}`)

			post := func(signature string) string {
				req, err := http.NewRequest("POST", relay, bytes.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				if signature != "" {
					req.Header.Set(flashbotsSignatureHeader, signature)
				}
				resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				out, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				t.Logf("HTTP %d %s", resp.StatusCode, out)
				return string(out)
			}

			// Control: the same body unsigned is refused for its signature.
			require.Contains(t, strings.ToLower(post("")), "signature")

			signature, err := signer.headerValue(body)
			require.NoError(t, err)
			require.NotContains(t, strings.ToLower(post(signature)), "signature",
				"the relay must accept the signature; any other answer is fine")
		})
	}
}
