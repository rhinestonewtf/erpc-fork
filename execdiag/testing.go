package execdiag

import (
	"bufio"
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// CaptureBuffer is a goroutine-safe sink for diagnostics output in tests.
type CaptureBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *CaptureBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *CaptureBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Lines returns the captured JSON lines.
func (b *CaptureBuffer) Lines() []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader([]byte(b.String())))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestingT is the subset of testing.TB needed here; it keeps the testing
// package out of production imports.
type TestingT interface {
	Helper()
	Cleanup(func())
}

// ConfigureForTest installs an emitter writing JSON lines to the returned
// buffer, lowering the zerolog global floor to info so events are not
// filtered, and restores both when the test ends.
func ConfigureForTest(t TestingT, cfg *common.ExecutionDiagnosticsConfig) *CaptureBuffer {
	t.Helper()
	return configureForTest(t, cfg, time.Now)
}

func configureForTest(t TestingT, cfg *common.ExecutionDiagnosticsConfig, now func() time.Time) *CaptureBuffer {
	eff := common.ExecutionDiagnosticsConfig{}
	if cfg != nil {
		eff = *cfg
	}
	eff.SetDefaults()

	prevLevel := zerolog.GlobalLevel()
	if prevLevel > zerolog.InfoLevel {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
	stopActive()

	buf := &CaptureBuffer{}
	t.Cleanup(func() {
		stopActive()
		zerolog.SetGlobalLevel(prevLevel)
	})
	if !eff.IsEnabled() {
		return buf
	}
	e := newEmitter(zerolog.New(buf).With().Timestamp().Logger(), &eff, now)
	start(context.Background(), e)
	return buf
}

// eventKeys is the documented field contract of every diagnostics event.
// Adding a key is a contract change: update the docs alongside.
var eventKeys = map[string]bool{
	"level": true, "time": true, "message": true,
	"component": true, "event": true, "version": true, "commit": true, "replica": true, "project": true,
	// rpc.request.completed
	"operational_request_id": true, "parent_request_id": true, "leader_request_id": true,
	"trace_id": true, "span_id": true, "parent_span_id": true,
	"network": true, "rpc_method": true, "finality": true, "user_id": true,
	"path": true, "outcome": true, "final_class": true, "final_error_code": true, "final_cause": true,
	"duration_ms": true, "invocations": true, "dispatches": true, "dispatch_unknown": true,
	"logical_attempts": true, "network_attempts": true, "retries": true, "hedges": true,
	"consensus_slots": true, "subrequests": true, "class_counts": true,
	"attempts_emitted": true, "attempts_omitted": true, "invocations_inflight": true, "evidence_complete": true,
	// rpc.attempt.completed
	"attempt_no": true, "upstream_id": true, "vendor": true, "dispatch": true, "class": true, "cause": true,
	"http_status": true, "jsonrpc_code": true, "selection_reason": true, "hedge": true, "retry": true,
	"network_attempt": true, "breaker_state": true, "breaker_scope": true, "won": true, "started_offset_ms": true,
	// rpc.breaker.transition
	"from": true, "to": true, "reason": true,
	// rpc.diagnostics.suppressed / rpc.diagnostics.configured
	"window_ms": true, "requests_suppressed": true, "request_events_suppressed": true, "transitions_suppressed": true,
	"max_events_per_second": true, "burst": true, "max_attempt_events_per_request": true,
}

// UnexpectedEventKeys returns the keys of a decoded event that are not part
// of the documented contract.
func UnexpectedEventKeys(event map[string]interface{}) []string {
	var out []string
	for k := range event {
		if !eventKeys[k] {
			out = append(out, k)
		}
	}
	return out
}
