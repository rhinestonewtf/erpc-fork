// Package execdiag emits the payload-free execution diagnostics stream:
// per-request upstream invocation evidence (dispatched vs locally rejected)
// and circuit-breaker transitions, bounded by a per-replica line budget and
// independent of the configured log level.
//
// Events carry only typed scalar fields chosen here. Never pass requests,
// responses, errors, URLs or headers to the event builders.
package execdiag

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"golang.org/x/time/rate"
)

const (
	componentName             = "execution-diagnostics"
	suppressionReportInterval = 10 * time.Second
	latePollInterval          = 25 * time.Millisecond
)

// lateInvocationGrace bounds how long a notable request's emission waits for
// still-running invocations (hedge losers) to record their outcome.
var lateInvocationGrace = time.Second

type emitter struct {
	log         zerolog.Logger
	maxAttempts int
	reqLim      *rate.Limiter
	trLim       *rate.Limiter
	now         func() time.Time

	reqSuppressed       atomic.Int64
	reqEventsSuppressed atomic.Int64
	trSuppressed        atomic.Int64
	windowStart         atomic.Int64

	stop context.CancelFunc
	done chan struct{}
}

var active atomic.Pointer[emitter]

// Enabled reports whether diagnostics are active. It is the only cost hooks
// pay on the hot path when the stream is disabled.
func Enabled() bool {
	return active.Load() != nil
}

func newEmitter(base zerolog.Logger, cfg *common.ExecutionDiagnosticsConfig, now func() time.Time) *emitter {
	e := &emitter{
		log: base.Level(zerolog.InfoLevel).With().
			Str("component", componentName).
			Str("version", common.ErpcVersion).
			Str("commit", common.ErpcCommitSha).
			Str("replica", replicaID()).
			Logger(),
		maxAttempts: cfg.MaxAttemptEventsPerRequest,
		reqLim:      rate.NewLimiter(rate.Limit(cfg.MaxEventsPerSecond), cfg.Burst),
		trLim:       rate.NewLimiter(rate.Limit(cfg.MaxEventsPerSecond), cfg.Burst),
		now:         now,
		done:        make(chan struct{}),
	}
	e.windowStart.Store(now().UnixNano())
	return e
}

// Configure installs (or removes, when disabled) the process-wide emitter.
// Safe to call repeatedly: the previous emitter's reporter is stopped.
func Configure(appCtx context.Context, cfg *common.ExecutionDiagnosticsConfig, base zerolog.Logger) {
	stopActive()
	if !cfg.IsEnabled() {
		return
	}
	eff := common.ExecutionDiagnosticsConfig{}
	if cfg != nil {
		eff = *cfg
	}
	eff.SetDefaults()

	e := newEmitter(base, &eff, time.Now)
	start(appCtx, e)

	e.event(zerolog.InfoLevel).
		Str("event", "rpc.diagnostics.configured").
		Float64("max_events_per_second", eff.MaxEventsPerSecond).
		Int("burst", eff.Burst).
		Int("max_attempt_events_per_request", eff.MaxAttemptEventsPerRequest).
		Msg("execution diagnostics configured")
}

// NoLevel bypasses the LOG_LEVEL floor except Disabled; write level as a field.
func (e *emitter) event(level zerolog.Level) *zerolog.Event {
	return e.log.WithLevel(zerolog.NoLevel).Str(zerolog.LevelFieldName, level.String())
}

func start(appCtx context.Context, e *emitter) {
	ctx, cancel := context.WithCancel(appCtx)
	e.stop = cancel
	active.Store(e)
	go e.reportLoop(ctx)
}

func stopActive() {
	if prev := active.Swap(nil); prev != nil && prev.stop != nil {
		prev.stop()
		<-prev.done
	}
}

func (e *emitter) reportLoop(ctx context.Context) {
	defer close(e.done)
	t := time.NewTicker(suppressionReportInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.flushSuppressed()
			return
		case <-t.C:
			e.flushSuppressed()
		}
	}
}

func (e *emitter) flushSuppressed() {
	now := e.now()
	reqs := e.reqSuppressed.Swap(0)
	lines := e.reqEventsSuppressed.Swap(0)
	trs := e.trSuppressed.Swap(0)
	start := e.windowStart.Swap(now.UnixNano())
	if reqs == 0 && lines == 0 && trs == 0 {
		return
	}
	e.event(zerolog.WarnLevel).
		Str("event", "rpc.diagnostics.suppressed").
		Float64("window_ms", float64(now.UnixNano()-start)/float64(time.Millisecond)).
		Int64("requests_suppressed", reqs).
		Int64("request_events_suppressed", lines).
		Int64("transitions_suppressed", trs).
		Msg("execution diagnostics events suppressed by budget")
}

var replicaOnce atomic.Pointer[string]

func replicaID() string {
	if p := replicaOnce.Load(); p != nil {
		return *p
	}
	id := strings.TrimSpace(os.Getenv("POD_NAME"))
	if id == "" {
		if hn, err := os.Hostname(); err == nil {
			id = strings.TrimSpace(hn)
		}
	}
	if id == "" {
		id = "unknown"
	}
	replicaOnce.Store(&id)
	return id
}
