package execdiag

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RequestScope tracks one user request (or sub-request) through
// Network.Forward. A nil scope is valid and every method is a no-op on it.
type RequestScope struct {
	req       *common.NormalizedRequest
	project   string
	network   string
	startedAt time.Time

	idOnce sync.Once
	id     string

	parent      *RequestScope
	leader      atomic.Pointer[RequestScope]
	subrequests atomic.Int32
}

type scopeKey struct{}

// ScopeFromContext returns the innermost request scope in ctx, if any.
func ScopeFromContext(ctx context.Context) *RequestScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*RequestScope)
	return s
}

// ID returns the operational request id, generating it on first use.
func (s *RequestScope) ID() string {
	if s == nil {
		return ""
	}
	s.idOnce.Do(func() {
		s.id = fmt.Sprintf("%016x%016x", rand.Uint64(), rand.Uint64()) // #nosec G404 -- correlation id, not a secret
	})
	return s.id
}

// BeginRequest opens a diagnostics scope for req. It returns a nil scope when
// diagnostics are disabled, for internal requests, and when ctx already
// carries a scope for the same request (re-entry), so Finish only runs once.
func BeginRequest(ctx context.Context, req *common.NormalizedRequest, projectId, networkId string) (context.Context, *RequestScope) {
	if active.Load() == nil || req == nil || req.IsInternal() {
		return ctx, nil
	}
	parent := ScopeFromContext(ctx)
	if parent != nil {
		if parent.req == req {
			return ctx, nil
		}
		parent.subrequests.Add(1)
	}
	s := &RequestScope{
		req:       req,
		project:   projectId,
		network:   networkId,
		startedAt: time.Now(),
		parent:    parent,
	}
	req.ExecState().EnableInvocationLedger()
	ctx = context.WithValue(ctx, scopeKey{}, s)
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attribute.String("erpc.operational_request_id", s.ID()))
	}
	return ctx, s
}

// MarkFollower links the request in ctx to the multiplexer leader that will
// answer it.
func MarkFollower(ctx context.Context, leader *RequestScope) {
	s := ScopeFromContext(ctx)
	if s == nil || leader == nil || leader == s {
		return
	}
	s.leader.Store(leader)
}

// Finish applies the emission rule and, when the request is notable and the
// budget allows, writes its summary and attempt events.
func (s *RequestScope) Finish(ctx context.Context, resp *common.NormalizedResponse, err error) {
	if s == nil {
		return
	}
	e := active.Load()
	if e == nil {
		return
	}
	st := s.req.ExecState()
	records, inflight := st.InvocationSnapshot()

	finalClass, finalCode, finalCause := FinalError(err)
	failed := err != nil && finalClass != ClassApplicationError
	if !failed && !anyNotable(records) {
		return
	}

	cost := 1 + min(len(records)+inflight, e.maxAttempts)
	if !e.reqLim.AllowN(e.now(), cost) {
		e.reqSuppressed.Add(1)
		e.reqEventsSuppressed.Add(int64(cost))
		return
	}

	snap := s.capture(ctx, resp, err, records, finalClass, finalCode, finalCause)
	snap.attemptBudget = cost - 1
	grace := lateInvocationGrace
	if inflight == 0 {
		e.writeRequest(snap, records, 0)
		return
	}
	go func() {
		deadline := time.Now().Add(grace)
		for st.InvocationsInflight() > 0 && time.Now().Before(deadline) {
			time.Sleep(latePollInterval)
		}
		records, inflight := st.InvocationSnapshot()
		e.writeRequest(snap, records, inflight)
	}()
}

const (
	pathForward  = "forward"
	pathCacheHit = "cache_hit"
	pathFollower = "multiplexed_follower"
)

func (s *RequestScope) capture(
	ctx context.Context,
	resp *common.NormalizedResponse,
	err error,
	records []common.InvocationRecord,
	finalClass, finalCode, finalCause string,
) requestSnapshot {
	method, _ := s.req.Method()
	es := s.req.ExecState().Snapshot()
	tr := resolveTrace(ctx)
	snap := requestSnapshot{
		project:         s.project,
		network:         s.network,
		method:          SafeToken(method),
		finality:        s.req.Finality(ctx).String(),
		user:            userToken(s.req),
		opID:            s.ID(),
		trace:           tr,
		hasError:        err != nil,
		finalClass:      finalClass,
		finalCode:       finalCode,
		finalCause:      finalCause,
		startedAt:       s.startedAt,
		duration:        time.Since(s.startedAt),
		logicalAttempts: es.UpstreamAttempts,
		networkAttempts: es.NetworkAttempts,
		retries:         es.Retries,
		hedges:          es.Hedges,
		consensusSlots:  es.ConsensusSlots,
		subrequests:     int(s.subrequests.Load()),
		path:            pathForward,
	}
	if s.parent != nil {
		snap.parentID = s.parent.ID()
	}
	if l := s.leader.Load(); l != nil {
		snap.leaderID = l.ID()
		snap.path = pathFollower
	} else if resp != nil && resp.FromCache() {
		snap.path = pathCacheHit
	}
	if err == nil && resp != nil && snap.path == pathForward {
		if u := resp.Upstream(); u != nil {
			snap.wonUpstream = u.Id()
		}
	}
	return snap
}

func requestOutcome(hasError bool, finalClass, path string, records []common.InvocationRecord) string {
	switch {
	case !hasError || finalClass == ClassApplicationError:
		return "degraded"
	case finalClass == ClassCancelled:
		return "cancelled"
	case path == pathForward && !anyDispatched(records):
		return "rejected"
	default:
		return "failed"
	}
}

func anyNotable(records []common.InvocationRecord) bool {
	for i := range records {
		if IsNotable(classOf(records[i].Err, records[i].Dispatch, true)) {
			return true
		}
	}
	return false
}

func anyDispatched(records []common.InvocationRecord) bool {
	for i := range records {
		if records[i].Dispatch != common.DispatchNone {
			return true
		}
	}
	return false
}

func userToken(req *common.NormalizedRequest) string {
	if u := req.User(); u != nil && u.Id != "" {
		return SafeToken(u.Id)
	}
	return ""
}
