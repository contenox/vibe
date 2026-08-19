package hitlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// ApprovalPollInterval is how often a blocked asker re-reads its durable row.
// The row is the verdict's only home: it may be written by this process, by
// `contenox approvals respond` in another terminal, by a phone over the relay,
// or by an adjudicating agent, so no waiter may rely on a local channel alone.
const ApprovalPollInterval = approvalPollInterval

// ApprovalWatcher reads the terminal verdict off a durable approval row.
// terminal is false while the row is still pending — including the
// pending-to-pending writes a multi-step flow (quorum, reassignment) makes
// before anyone has decided.
type ApprovalWatcher interface {
	ApprovalVerdict(ctx context.Context, approvalID string) (approved bool, terminal bool, err error)
}

// ApprovalWaiterRegistry lets a caller park on an approval before the row is
// offered to an adjudicator, so a verdict landing immediately wakes it instead
// of racing it. The returned channel is a latency shortcut, never the only
// source of a verdict: the caller must poll the row as well.
type ApprovalWaiterRegistry interface {
	RegisterApprovalWaiter(approvalID string) (<-chan bool, func())
}

var (
	_ ApprovalWatcher        = (*service)(nil)
	_ ApprovalWaiterRegistry = (*service)(nil)
)

func (s *service) ApprovalVerdict(ctx context.Context, approvalID string) (bool, bool, error) {
	if s.approvals == nil {
		return false, false, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	if strings.TrimSpace(approvalID) == "" {
		return false, false, fmt.Errorf("hitlservice: ApprovalVerdict requires a non-empty approval ID")
	}
	row, err := s.approvals.GetHITLApproval(ctx, approvalID)
	if err != nil {
		return false, false, fmt.Errorf("hitlservice: read approval %s: %w", approvalID, err)
	}
	switch row.State {
	case runtimetypes.HITLApprovalApproved:
		return true, true, nil
	case runtimetypes.HITLApprovalDenied:
		return false, true, nil
	case runtimetypes.HITLApprovalExpired:
		// The sweeper already applied the row's on-timeout verdict; the waiter
		// must land on the same one rather than inventing a denial.
		return onTimeoutOutcome(Action(row.OnTimeout)), true, nil
	default:
		return false, false, nil
	}
}

func (s *service) RegisterApprovalWaiter(approvalID string) (<-chan bool, func()) {
	if strings.TrimSpace(approvalID) == "" {
		return nil, func() {}
	}
	// Buffered so a verdict landing before the caller selects is kept, not dropped.
	ch := make(chan answer, 1)
	s.mu.Lock()
	s.pending[approvalID] = ch
	s.mu.Unlock()

	out := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		select {
		case ans := <-ch:
			out <- ans.approved
		case <-done:
		}
	}()
	return out, func() {
		s.mu.Lock()
		if cur, ok := s.pending[approvalID]; ok && cur == ch {
			delete(s.pending, approvalID)
		}
		s.mu.Unlock()
		close(done)
	}
}
