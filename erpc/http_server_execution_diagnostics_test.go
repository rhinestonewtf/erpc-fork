package erpc

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/execdiag"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpServer_ExecutionDiagnostics_InboundTraceIsLogOnly(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var leakedTraceparent atomic.Bool
	noteHeaders := func(r *http.Request) bool {
		// gock evaluates filters for every intercepted request, including the
		// test client's own call into eRPC; only provider calls count.
		if strings.HasSuffix(r.URL.Hostname(), ".localhost") && r.Header.Get("traceparent") != "" {
			leakedTraceparent.Store(true)
		}
		return diagIsBalance(r)
	}
	gock.New("http://rpc1.localhost").Post("").Filter(noteHeaders).Persist().Reply(503).
		JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})
	// Whatever the selection order, at least one provider failure precedes
	// the success, so the request is notable.
	gock.New("http://rpc2.localhost").Post("").Filter(noteHeaders).Times(1).Reply(503).
		JSON(map[string]interface{}{"error": map[string]interface{}{"message": "unavailable"}})
	gock.New("http://rpc2.localhost").Post("").Filter(noteHeaders).Persist().Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	buf := execdiag.ConfigureForTest(t, nil)
	dumpOnFailure(t, buf)

	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(5 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
				Failsafe:     []*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}}},
			}},
			Upstreams: []*common.UpstreamConfig{
				{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
				{Id: "rpc2", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc2.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	status, _, body := sendRequest(diagBalanceReq, map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"X-Api-Key":   "canaryhdrHHH",
	}, nil)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "0x1")

	require.Eventually(t, func() bool {
		return len(diagFilter(diagEvents(t, buf), "rpc.request.completed")) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	sum := diagFilter(diagEvents(t, buf), "rpc.request.completed")[0]
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", sum.str("trace_id"))
	assert.Equal(t, "00f067aa0ba902b7", sum.str("parent_span_id"))
	assert.NotContains(t, sum, "span_id")
	assert.Equal(t, "degraded", sum.str("outcome"))

	assert.False(t, leakedTraceparent.Load(), "inbound traceparent must never reach providers when tracing is off")
	assert.NotContains(t, buf.String(), "canaryhdrHHH")
}
