package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

type askInbox interface {
	AnswerFrom(ctx context.Context, askID, text, by string) error
	RespondWithGuidance(ctx context.Context, approvalID string, approved bool, decidedBy, guidance string) error
	ListPendingBefore(ctx context.Context, createdBefore *time.Time, limit int) ([]*runtimetypes.HITLApproval, error)
}

const republishAskLimit = 200

type relayAskBridge struct {
	inbox   askInbox
	tracker libtracker.ActivityTracker

	mu       sync.Mutex
	instance string
	send     func(librelay.Frame) error

	wg sync.WaitGroup
}

var _ hitlservice.AskWatcher = (*relayAskBridge)(nil)

func newRelayAskBridge(inbox askInbox, tracker libtracker.ActivityTracker) *relayAskBridge {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &relayAskBridge{inbox: inbox, tracker: tracker}
}

func (b *relayAskBridge) attach(instance string, send func(librelay.Frame) error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.instance, b.send = instance, send
	b.mu.Unlock()
}

func (b *relayAskBridge) detach() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.send = nil
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *relayAskBridge) link() (string, func(librelay.Frame) error) {
	if b == nil {
		return "", nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.instance, b.send
}

func (b *relayAskBridge) begin() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.send == nil {
		return false
	}
	b.wg.Add(1)
	return true
}

func (b *relayAskBridge) republish(ctx context.Context) {
	if b == nil || b.inbox == nil || !b.begin() {
		return
	}
	go func() {
		defer b.wg.Done()
		now := time.Now().UTC()
		var cursor *time.Time
		for {
			rows, err := b.inbox.ListPendingBefore(ctx, cursor, republishAskLimit)
			if err != nil {
				reportErr, _, end := b.tracker.Start(ctx, "republish", librelay.TypeAskPublished)
				reportErr(err)
				end()
				return
			}
			for _, row := range rows {
				if !publishableAsk(row, now) {
					continue
				}
				b.AskRecorded(ctx, row)
			}
			if len(rows) < republishAskLimit {
				return
			}
			// The pages run newest first; the oldest row of this one opens the next.
			oldest := rows[len(rows)-1].CreatedAt
			if cursor != nil && !oldest.Before(*cursor) {
				return
			}
			cursor = &oldest
		}
	}()
}

func publishableAsk(row *runtimetypes.HITLApproval, now time.Time) bool {
	switch {
	case row == nil, strings.TrimSpace(row.ID) == "":
		return false
	case row.State != runtimetypes.HITLApprovalPending:
		return false
	case !row.ExpiresAt.IsZero() && !row.ExpiresAt.After(now):
		return false
	}
	return true
}

func (b *relayAskBridge) publish(ctx context.Context, frameType string, payload any) {
	instance, send := b.link()
	if send == nil {
		return
	}
	f, err := librelay.Frame{Type: frameType, Instance: instance}.WithPayload(payload)
	if err == nil {
		err = send(f)
	}
	if err == nil || errors.Is(err, relaylink.ErrNotConnected) || errors.Is(err, relaylink.ErrClosed) {
		return
	}
	reportErr, _, end := b.tracker.Start(ctx, "publish", frameType)
	reportErr(err)
	end()
}

func (b *relayAskBridge) AskRecorded(ctx context.Context, row *runtimetypes.HITLApproval) {
	if b == nil || row == nil {
		return
	}
	published := librelay.AskPublished{
		AskID:       row.ID,
		SessionID:   row.SessionID,
		AgentName:   row.AgentName,
		ToolsName:   row.ToolsName,
		ToolName:    row.ToolName,
		PolicyName:  row.PolicyName,
		MatchedRule: row.MatchedRule,
		ArgsSummary: row.ArgsSummary,
		ExpiresAt:   row.ExpiresAt,
	}
	if row.MissionID != nil {
		published.MissionID = strings.TrimSpace(*row.MissionID)
	}
	b.publish(ctx, librelay.TypeAskPublished, published)
}

func (b *relayAskBridge) AskResolved(ctx context.Context, askID string, reason hitlservice.AskResolution) {
	if b == nil || strings.TrimSpace(askID) == "" {
		return
	}
	b.publish(ctx, librelay.TypeAskResolved, librelay.AskResolved{AskID: askID, Reason: askResolvedReason(reason)})
}

func askResolvedReason(reason hitlservice.AskResolution) string {
	switch reason {
	case hitlservice.AskExpired:
		return librelay.AskResolvedExpired
	case hitlservice.AskSuperseded:
		return librelay.AskResolvedSuperseded
	default:
		return librelay.AskResolvedAnswered
	}
}

func (b *relayAskBridge) handleVerdict(ctx context.Context, f librelay.Frame) {
	if b == nil {
		return
	}
	instance, send := b.link()
	if f.Instance != "" && instance != "" && f.Instance != instance {
		return
	}
	var verdict librelay.AskVerdict
	err := f.DecodePayload(&verdict)
	if err == nil {
		err = validAskVerdict(verdict)
	}
	if err == nil && b.inbox == nil {
		err = errors.New("no ask inbox is running in this process")
	}
	if err != nil {
		reportErr, _, end := b.tracker.Start(ctx, "refuse", "ask_verdict", "ask_id", verdict.AskID)
		reportErr(err)
		end()
		if f.IsRequest() && send != nil {
			_ = send(librelay.NewError(f, librelay.CodeMalformedFrame, "unusable ask.verdict payload"))
		}
		return
	}
	// Admission under the same lock detach releases: once detach has cleared the
	// link, no further verdict goroutine can start behind its Wait.
	if !b.begin() {
		return
	}
	go func() {
		defer b.wg.Done()
		b.applyVerdict(ctx, verdict)
	}()
}

func validAskVerdict(verdict librelay.AskVerdict) error {
	if strings.TrimSpace(verdict.AskID) == "" {
		return errors.New("missing ask_id")
	}
	switch verdict.Decision {
	case librelay.AskDecisionAllow, librelay.AskDecisionDeny:
		return nil
	case librelay.AskDecisionAnswer:
		if strings.TrimSpace(verdict.Answer) == "" {
			return errors.New("an answer verdict carries no answer")
		}
		return nil
	}
	return fmt.Errorf("unknown decision %q", verdict.Decision)
}

func (b *relayAskBridge) applyVerdict(ctx context.Context, verdict librelay.AskVerdict) {
	reportErr, reportChange, end := b.tracker.Start(ctx, "settle", "ask_verdict",
		"ask_id", verdict.AskID, "decision", verdict.Decision)
	defer end()

	var err error
	if verdict.Decision == librelay.AskDecisionAnswer {
		err = b.inbox.AnswerFrom(ctx, verdict.AskID, verdict.Answer, verdict.DecidedBy)
	} else {
		err = b.inbox.RespondWithGuidance(ctx, verdict.AskID,
			verdict.Decision == librelay.AskDecisionAllow, verdict.DecidedBy, verdict.Guidance)
	}
	switch {
	case err == nil:
		reportChange(verdict.AskID, verdict.Decision)
	case errors.Is(err, hitlservice.ErrApprovalNotFound),
		errors.Is(err, hitlservice.ErrApprovalAlreadyResolved),
		errors.Is(err, hitlservice.ErrApprovalExpired):
		reportChange(verdict.AskID, "already_settled")
	default:
		reportErr(err)
	}
}
