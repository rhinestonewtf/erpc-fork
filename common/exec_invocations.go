package common

import (
	"sync"
	"sync/atomic"
	"time"
)

// Dispatch states for an upstream invocation: whether the client reached
// network I/O. "attempted" does not prove the provider received the request.
const (
	DispatchAttempted = "attempted"
	DispatchNone      = "none"
	DispatchUnknown   = "unknown"
)

// InvocationRecord is one upstream invocation (or local rejection) of a user
// request, kept for execution diagnostics. Err is held in memory only and
// classified into bounded fields at emission time.
type InvocationRecord struct {
	UpstreamId      string
	VendorName      string
	StartedAt       time.Time
	Duration        time.Duration
	Dispatch        string
	Err             error
	IsHedge         bool
	IsRetry         bool
	SelectionReason string
	NetworkAttempt  int
	BreakerState    string
	BreakerScope    string
	// Won marks the invocation whose response the request returned.
	Won bool
}

// invocationLedger is separate from the upstream attempt log on purpose: it
// also records local rejections that never reach tryForward, and it is only
// populated for requests that opted in via EnableInvocationLedger.
type invocationLedger struct {
	ledgerEnabled  atomic.Bool
	ledgerMu       sync.Mutex
	ledger         []InvocationRecord
	ledgerInflight atomic.Int32
	// ledgerByResp ties each successful invocation to the response it
	// returned, so winners are identified exactly rather than by upstream id.
	ledgerByResp map[*NormalizedResponse]int
}

func (s *ExecState) EnableInvocationLedger() {
	if s == nil {
		return
	}
	s.ledgerEnabled.Store(true)
}

func (s *ExecState) InvocationLedgerEnabled() bool {
	return s != nil && s.ledgerEnabled.Load()
}

func (s *ExecState) RecordInvocation(r InvocationRecord) {
	if !s.InvocationLedgerEnabled() {
		return
	}
	s.ledgerMu.Lock()
	s.ledger = append(s.ledger, r)
	s.ledgerMu.Unlock()
}

// Invocations returns a copy of the recorded invocations.
func (s *ExecState) Invocations() []InvocationRecord {
	records, _ := s.InvocationSnapshot()
	return records
}

// InvocationSnapshot captures evidence and outstanding work consistently.
func (s *ExecState) InvocationSnapshot() ([]InvocationRecord, int) {
	if s == nil {
		return nil, 0
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	out := append([]InvocationRecord(nil), s.ledger...)
	return out, int(s.ledgerInflight.Load())
}

// CompleteInvocation publishes the record and retires its in-flight slot
// atomically with respect to diagnostic snapshots. A successful record is
// bound to resp for AmendInvocationError, AliasInvocationResponse and
// MarkInvocationWon.
func (s *ExecState) CompleteInvocation(r InvocationRecord, resp *NormalizedResponse) {
	if !s.InvocationLedgerEnabled() {
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	s.ledger = append(s.ledger, r)
	s.ledgerInflight.Add(-1)
	if r.Err == nil && resp != nil {
		if s.ledgerByResp == nil {
			s.ledgerByResp = make(map[*NormalizedResponse]int)
		}
		s.ledgerByResp[resp] = len(s.ledger) - 1
	}
}

// AmendInvocationError attaches an error found after the invocation returned
// (a post-forward hook rejecting resp) to the record that produced resp. It
// reports whether such a record exists.
func (s *ExecState) AmendInvocationError(resp *NormalizedResponse, err error) bool {
	if !s.InvocationLedgerEnabled() || resp == nil || err == nil {
		return false
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	idx, ok := s.ledgerByResp[resp]
	if ok && s.ledger[idx].Err == nil {
		s.ledger[idx].Err = err
	}
	return ok
}

// AliasInvocationResponse binds a response that replaced from (e.g. built by
// a post-forward hook) to the same record.
func (s *ExecState) AliasInvocationResponse(from, to *NormalizedResponse) {
	if !s.InvocationLedgerEnabled() || from == nil || to == nil || from == to {
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if idx, ok := s.ledgerByResp[from]; ok {
		s.ledgerByResp[to] = idx
	}
}

// MarkInvocationWon flags the record that produced resp, if any.
func (s *ExecState) MarkInvocationWon(resp *NormalizedResponse) {
	if !s.InvocationLedgerEnabled() || resp == nil {
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if idx, ok := s.ledgerByResp[resp]; ok {
		s.ledger[idx].Won = true
	}
}

// InvocationStarted tracks an invocation still running, so emission can tell
// late hedge losers apart from invocations that never ran. Callers must pair
// it with CompleteInvocation.
func (s *ExecState) InvocationStarted() {
	if s == nil {
		return
	}
	s.ledgerInflight.Add(1)
}

func (s *ExecState) InvocationsInflight() int {
	if s == nil {
		return 0
	}
	return int(s.ledgerInflight.Load())
}
