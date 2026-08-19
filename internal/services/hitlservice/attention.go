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

	// AskID is the ask's durable identity when the caller has one to give —
	// the engine's tool-call ID, which is also the checkpoint key a resume
	// looks the run up by. Empty mints a fresh one.
	AskID string

	// Detached releases the caller instead of blocking: the row is recorded and
	// an AttentionPendingError comes straight back, so the run suspends and the
	// answer arrives through the resume hook. Set it only where nobody is
	// attached to answer — the default is to block on the ask.
	Detached bool
}

type AttentionPendingError struct {
	AskID string
}

func (e *AttentionPendingError) Error() string {
	return fmt.Sprintf("hitlservice: attention ask %s is pending; suspend and resume on answer", e.AskID)
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

func (s *service) RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error) {
	if s.approvals == nil {
		return "", fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		return "", fmt.Errorf("hitlservice: attention ask requires a summary")
	}

	askID := req.AskID
	// A caller-supplied id is the checkpoint key: it is what makes suspending
	// on this ask resumable at all.
	resumable := askID != ""
	if askID == "" {
		askID = uuid.NewString()
	}
	now := time.Now().UTC()
	timeout := s.ceiling()
	// An ask with no deadline never expires, so it carries no on-timeout verdict for a surface to quote.
	onTimeout := ActionDeny
	if Indefinite(timeout) {
		onTimeout = ""
	}

	row := &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   AttentionToolsName,
		ToolName:    AttentionToolName,
		ArgsSummary: summary,
		OnTimeout:   string(onTimeout),
		State:       runtimetypes.HITLApprovalPending,
		InstanceID:  req.InstanceID,
		SessionID:   req.SessionID,
		AgentName:   req.AgentName,
		CreatedAt:   now,
		ExpiresAt:   expiryAt(now, timeout),
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

	req.AskID = askID
	s.askRecorded(ctx, row)
	s.offer(ctx, adjudicationFromAttentionRequest(askID, req))

	if req.Detached {
		// The caller declared nobody is attached to answer this run.
		return "", &AttentionPendingError{AskID: askID}
	}

	waitCtx := ctx
	if !Indefinite(timeout) {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Watches the durable row as well as the in-process channel: the unit may run in a different process than the answerer.
	poll := time.NewTicker(attentionPollInterval)
	defer poll.Stop()

	for {
		select {
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
				if resumable {
					// The process is leaving with the question open. The row
					// stays pending and the run checkpoints beside it, so an
					// answer resumes it elsewhere rather than being lost.
					return "", &AttentionPendingError{AskID: askID}
				}
				return "", ctx.Err()
			}
			// The ceiling fired. The row is left pending; SweepExpired closes it out.
			return "", ErrAttentionUnanswered
		}
	}
}

const attentionPollInterval = time.Second

func (s *service) Answer(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, "", false, nil)
}

func (s *service) AnswerFrom(ctx context.Context, askID, text, by string) error {
	return s.answerAttention(ctx, askID, text, strings.TrimSpace(by), false, nil)
}

func (s *service) answerAttention(ctx context.Context, askID, text, by string, byAgent bool, bound *int) error {
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
	resolution := marshalAttentionResolution(text, by, byAgent)
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

	s.askClosed(ctx, askID, AskAnswered)

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

	// Nobody is waiting here: the asking run released this question or left; run the resume hook with resolve()'s same contract.
	if hook != nil {
		if err := hook(ctx, askID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			return fmt.Errorf("hitlservice: answer for ask %s recorded, but resuming its suspended run failed: %w", askID, err)
		}
	}
	return nil
}

const answeredByAgent = "agent"

const agentResolutionLike = `%"answeredBy":%`

func (s *service) AnswerAsAgent(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, answeredByAgent, true, nil)
}

func (s *service) AnswerAsAgentNamed(ctx context.Context, askID, agentName, text string) error {
	return s.answerAttention(ctx, askID, text, agentActor(agentName), true, nil)
}

func (s *service) AnswerAsAgentBounded(ctx context.Context, askID, agentName, text string, max int) error {
	return s.answerAttention(ctx, askID, text, agentActor(agentName), true, &max)
}

func agentActor(agentName string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		return answeredByAgent
	}
	return name
}

// AnsweredByOf returns the recorded actor of a resolved attention ask — "agent",
// an agent's name, or the person a relayed answer named — and "" for an
// unattributed answer, a pending row, or a permission ask.
func AnsweredByOf(row *runtimetypes.HITLApproval) string {
	res, ok := resolutionOf(row)
	if !ok {
		return ""
	}
	if res.AnsweredBy != nil {
		return strings.TrimSpace(*res.AnsweredBy)
	}
	if res.AnsweredByHuman != nil {
		return strings.TrimSpace(*res.AnsweredByHuman)
	}
	return ""
}

// answeredByAgentActor reports an agent-attributed answer, the only kind a mission's agent-answer bound counts.
func answeredByAgentActor(row *runtimetypes.HITLApproval) bool {
	res, ok := resolutionOf(row)
	return ok && res.AnsweredBy != nil && strings.TrimSpace(*res.AnsweredBy) != ""
}

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
		if answeredByAgentActor(row) {
			count++
		}
	}
	return count, nil
}

const missionAskScanLimit = 200
