package execdiag

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func configureReal(t *testing.T, global zerolog.Level) *CaptureBuffer {
	t.Helper()
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() {
		stopActive()
		zerolog.SetGlobalLevel(prev)
	})
	zerolog.SetGlobalLevel(global)
	buf := &CaptureBuffer{}
	Configure(context.Background(), &common.ExecutionDiagnosticsConfig{Enabled: util.BoolPtr(true)}, zerolog.New(buf))
	return buf
}

func TestConfigure_EmitsConfiguredLineUnderErrorFloor(t *testing.T) {
	buf := configureReal(t, zerolog.ErrorLevel)
	lines := parseLines(t, buf)
	require.Len(t, lines, 1)
	assert.Equal(t, "rpc.diagnostics.configured", lines[0]["event"])
	assert.Equal(t, "info", lines[0]["level"])
	assertEventContract(t, lines)
}

func TestConfigure_GlobalDisabledSilencesEverything(t *testing.T) {
	buf := configureReal(t, zerolog.Disabled)
	require.True(t, Enabled())

	req := newReq()
	ctx, d := BeginRequest(context.Background(), req, "main", "evm:1")
	record(req.ExecState(), time.Now(), "rpc1", common.DispatchAttempted, serverErr(503))
	d.Finish(ctx, nil, exhausted(serverErr(503)))
	BreakerTransition("main", "evm:1", "rpc1", "v", "", "closed", "open", "r")
	stopActive()

	assert.Empty(t, buf.String())
}
