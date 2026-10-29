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
}

// invocationLedger is separate from the upstream attempt log on purpose: it
// also records local rejections that never reach tryForward, and it is only
// populated for requests that opted in via EnableInvocationLedger.
type invocationLedger struct {
	ledgerEnabled  atomic.Bool
	ledgerMu       sync.Mutex
	ledger         []InvocationRecord
	ledgerInflight atomic.Int32
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
// atomically with respect to diagnostic snapshots. It returns the record's
// index for AmendInvocationError, or -1 when the ledger is disabled.
func (s *ExecState) CompleteInvocation(r InvocationRecord) int {
	if !s.InvocationLedgerEnabled() {
		return -1
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	s.ledger = append(s.ledger, r)
	s.ledgerInflight.Add(-1)
	return len(s.ledger) - 1
}

// AmendInvocationError attaches an error found after the invocation returned
// (a post-forward hook rejecting the response) to a record that had none.
func (s *ExecState) AmendInvocationError(idx int, err error) {
	if !s.InvocationLedgerEnabled() || err == nil {
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if idx >= 0 && idx < len(s.ledger) && s.ledger[idx].Err == nil {
		s.ledger[idx].Err = err
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
