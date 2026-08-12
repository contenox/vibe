package hitlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
)

// AttentionToolsName/AttentionToolName mark a durable ask as an attention
// ask rather than a permission one — a fact about the row, not a flag.
const (
	AttentionToolsName = "mission"
	AttentionToolName  = "mission_ask_attention"
)

// ErrAttentionUnanswered reports an attention ask reached its deadline
// unanswered, or was answered with a refusal.
var ErrAttentionUnanswered = errors.New("hitlservice: attention ask went unanswered")

// AttentionRequest is one unit's question for a human — narrower than
// ApprovalRequest: no policy verdict, diff, or args.
type AttentionRequest struct {
	// Summary is the one-line question; required.
	Summary string
	// Detail is the optional longer form — context the operator needs to answer.
	Detail string
	// MissionID, InstanceID, SessionID, and AgentName attribute the ask.
	MissionID  string
	InstanceID string
	SessionID  string
	AgentName  string

	// OnRaised, when set, is called with the ask's id once the row exists and
	// before the wait begins; runs inline, so keep it cheap and non-blocking.
	OnRaised func(askID string)

	// AskID, when set, is the durable row's ID; empty generates a fresh uuid.
	AskID string

	// ParkWindow, when > 0, bounds how long RequestAttention blocks before
	// returning *AttentionPendingError with the row left pending.
	ParkWindow time.Duration
}

// AttentionPendingError reports an attention ask's park window elapsed
// unanswered; the row is still pending, and the caller should checkpoint.
type AttentionPendingError struct {
	AskID string
}

func (e *AttentionPendingError) Error() string {
	return fmt.Sprintf("hitlservice: attention ask %s is pending past its park window; suspend and resume on answer", e.AskID)
}

// IsAttentionAsk reports whether row is an attention ask (expects data)
// rather than a permission ask (expects yes/no).
func IsAttentionAsk(row *runtimetypes.HITLApproval) bool {
	return row != nil && row.ToolsName == AttentionToolsName && row.ToolName == AttentionToolName
}

// AnswerOf returns the operator's text answer from a resolved attention ask,
// or "" when none (still pending, expired, or a permission ask).
func AnswerOf(row *runtimetypes.HITLApproval) string {
	if row == nil || len(row.Resolution) == 0 {
		return ""
	}
	var res approvalResolution
	if err := json.Unmarshal(row.Resolution, &res); err != nil || res.Answer == nil {
		return ""
	}
	return *res.Answer
}

// RequestAttention records a unit's question as a durable ask and blocks
// until an operator answers it, the serve-level ceiling expires it, or ctx
// ends, returning the operator's own words.
func (s *service) RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error) {
	if s.approvals == nil {
		return "", fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		return "", fmt.Errorf("hitlservice: attention ask requires a summary")
	}

	askID := req.AskID
	if askID == "" {
		askID = uuid.NewString()
	}
	now := time.Now().UTC()
	timeout := s.ceiling()

	row := &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   AttentionToolsName,
		ToolName:    AttentionToolName,
		ArgsSummary: summary,
		OnTimeout:   string(ActionDeny),
		State:       runtimetypes.HITLApprovalPending,
		InstanceID:  req.InstanceID,
		SessionID:   req.SessionID,
		AgentName:   req.AgentName,
		CreatedAt:   now,
		ExpiresAt:   now.Add(timeout),
	}
	if detail := strings.TrimSpace(req.Detail); detail != "" {
		// The long form rides the Diff column, the row's one free-text field.
		row.Diff = &detail
	}
	if req.MissionID != "" {
		missionID := req.MissionID
		row.MissionID = &missionID
	}
	// Durable first, as RequestApproval does, so a restart still shows it pending.
	if err := s.approvals.CreateHITLApproval(ctx, row); err != nil {
		return "", fmt.Errorf("hitlservice: persist attention ask: %w", err)
	}

	if req.OnRaised != nil {
		req.OnRaised(askID)
	}

	ch := make(chan answer, 1)
	s.mu.Lock()
	s.pending[askID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, askID)
		s.mu.Unlock()
	}()

	if sink != nil {
		ev := taskengine.NewTaskEvent(ctx, taskengine.TaskEventApprovalRequested)
		ev.ApprovalID = askID
		ev.HookName = AttentionToolsName
		ev.ToolName = AttentionToolName
		ev.ApprovalArgs = map[string]any{"summary": summary, "detail": req.Detail}
		_ = sink.PublishTaskEvent(ctx, ev)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Watches the durable row as well as the in-process channel: the unit may run in a different process than the answerer.
	poll := time.NewTicker(attentionPollInterval)
	defer poll.Stop()

	// A nil channel arm never fires: unset ParkWindow disables the typed-pending-error path below.
	var park <-chan time.Time
	if req.ParkWindow > 0 {
		park = time.After(req.ParkWindow)
	}

	for {
		select {
		case <-park:
			// One last read: an answer landed durably during the window wins over parking.
			if row, err := s.approvals.GetHITLApproval(ctx, askID); err == nil && row.State != runtimetypes.HITLApprovalPending {
				if text := AnswerOf(row); strings.TrimSpace(text) != "" {
					return text, nil
				}
				return "", ErrAttentionUnanswered
			}
			return "", &AttentionPendingError{AskID: askID}
		case ans := <-ch:
			if !ans.approved || strings.TrimSpace(ans.text) == "" {
				// Refusal or empty text: the fallback must treat this as still blocked.
				return "", ErrAttentionUnanswered
			}
			return ans.text, nil
		case <-poll.C:
			row, err := s.approvals.GetHITLApproval(ctx, askID)
			if err != nil || row.State == runtimetypes.HITLApprovalPending {
				continue // unreadable right now, or still waiting on a human
			}
			if text := AnswerOf(row); strings.TrimSpace(text) != "" {
				return text, nil
			}
			// Terminal without an answer: denied, expired, or resolved by the boolean path.
			return "", ErrAttentionUnanswered
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// The ceiling fired. The row is left pending; SweepExpired closes it out.
			return "", ErrAttentionUnanswered
		}
	}
}

const attentionPollInterval = time.Second

// Answer resolves an attention ask with the operator's text, waking the unit
// parked on it; refuses a permission ask by design.
func (s *service) Answer(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, "", nil)
}

func (s *service) answerAttention(ctx context.Context, askID, text, by string, bound *int) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("hitlservice: an attention answer cannot be empty")
	}
	row, err := s.approvals.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return ErrApprovalNotFound
		}
		return fmt.Errorf("hitlservice: look up ask %s: %w", askID, err)
	}
	if !IsAttentionAsk(row) {
		return fmt.Errorf("hitlservice: ask %s is a permission request (%s.%s), which is answered approve/deny, not with text",
			askID, row.ToolsName, row.ToolName)
	}
	// Same ordering gate as resolve(): a process that cannot resume must not record a checkpointed run's one-shot answer.
	if err := s.requireResumerForVerdict(ctx, askID); err != nil {
		return err
	}

	now := time.Now().UTC()
	resolution := marshalAttentionResolution(text, by)
	var resolveErr error
	if bound == nil {
		resolveErr = s.approvals.ResolveHITLApproval(ctx, askID, runtimetypes.HITLApprovalApproved, resolution, now)
	} else {
		if row.MissionID == nil || strings.TrimSpace(*row.MissionID) == "" {
			return fmt.Errorf("hitlservice: ask %s belongs to no mission, so no agent-answer bound can be applied to it", askID)
		}
		resolveErr = s.approvals.ResolveHITLApprovalWithinBound(ctx, askID, runtimetypes.AgentAnswerBound{
			MissionID:      *row.MissionID,
			ToolsName:      AttentionToolsName,
			ToolName:       AttentionToolName,
			ResolutionLike: agentResolutionLike,
			Max:            *bound,
		}, runtimetypes.HITLApprovalApproved, resolution, now)
	}
	if resolveErr != nil {
		if !errors.Is(resolveErr, libdb.ErrNotFound) {
			return fmt.Errorf("hitlservice: resolve ask %s: %w", askID, resolveErr)
		}
		// Lost the CAS: tell expired from already-answered, same as Respond.
		current, getErr := s.approvals.GetHITLApproval(ctx, askID)
		if getErr != nil {
			return fmt.Errorf("hitlservice: look up ask %s: %w", askID, getErr)
		}
		// Still pending means the row was writable and the count predicate is what refused — reachable only under a bound.
		if current.State == runtimetypes.HITLApprovalPending {
			return ErrAgentAnswerBoundSpent
		}
		if current.State == runtimetypes.HITLApprovalExpired {
			return ErrApprovalExpired
		}
		return ErrApprovalAlreadyResolved
	}

	s.mu.Lock()
	ch, ok := s.pending[askID]
	hook := s.resumeHook
	s.mu.Unlock()
	if ok {
		select {
		case ch <- answer{approved: true, text: text}:
		default:
		}
		return nil
	}

	// Waiter gone: the asking run parked past its window; run the resume hook with resolve()'s same contract.
	if hook != nil {
		if err := hook(ctx, askID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			return fmt.Errorf("hitlservice: answer for ask %s recorded, but resuming its suspended run failed: %w", askID, err)
		}
	}
	return nil
}

const answeredByAgent = "agent"

const agentResolutionLike = `%"answeredBy":%`

// AnswerAsAgent resolves an attention ask exactly as Answer does, but
// records that an agent answered; enforces no cap itself.
func (s *service) AnswerAsAgent(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, answeredByAgent, nil)
}

// AnswerAsAgentNamed is AnswerAsAgent with the answering agent's name as the
// recorded actor; a blank name degrades to the generic marker.
func (s *service) AnswerAsAgentNamed(ctx context.Context, askID, agentName, text string) error {
	return s.answerAttention(ctx, askID, text, agentActor(agentName), nil)
}

// AnswerAsAgentBounded implements Service: the same write AnswerAsAgentNamed
// makes, but conditional on the mission holding fewer than max
// agent-answered asks, counted atomically.
func (s *service) AnswerAsAgentBounded(ctx context.Context, askID, agentName, text string, max int) error {
	return s.answerAttention(ctx, askID, text, agentActor(agentName), &max)
}

func agentActor(agentName string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		return answeredByAgent
	}
	return name
}

// AnsweredByOf returns the recorded non-human actor of a resolved attention
// ask — "agent" or an agent's name — and "" for a human answer, a pending
// row, or a permission ask.
func AnsweredByOf(row *runtimetypes.HITLApproval) string {
	if row == nil || len(row.Resolution) == 0 {
		return ""
	}
	var res approvalResolution
	if err := json.Unmarshal(row.Resolution, &res); err != nil || res.AnsweredBy == nil {
		return ""
	}
	return strings.TrimSpace(*res.AnsweredBy)
}

// PendingAttentionAsks returns missionID's unanswered questions, newest first.
func (s *service) PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	out := make([]*runtimetypes.HITLApproval, 0, len(rows))
	for _, row := range rows {
		if IsAttentionAsk(row) && row.State == runtimetypes.HITLApprovalPending {
			out = append(out, row)
		}
	}
	return out, nil
}

// AgentAnswerCount reports how many of missionID's questions were answered
// by a supervising agent, a durable counter that survives a restart.
func (s *service) AgentAnswerCount(ctx context.Context, missionID string) (int, error) {
	if s.approvals == nil {
		return 0, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return 0, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	count := 0
	for _, row := range rows {
		if !IsAttentionAsk(row) {
			continue
		}
		if AnsweredByOf(row) != "" {
			count++
		}
	}
	return count, nil
}

const missionAskScanLimit = 200
