package hitlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

// AskKind tells an Adjudicator which contract the offered ask answers to: a gated tool call wants approve/deny, a question wants words.
type AskKind string

const (
	// AskKindPermission is a tool call the envelope put on the approve tier.
	AskKindPermission AskKind = "permission"
	// AskKindAttention is a unit's question, raised with mission_ask_attention.
	AskKindAttention AskKind = "attention"
)

// Adjudication is one pending ask offered for a non-human verdict.
type Adjudication struct {
	AskID     string
	Kind      AskKind
	MissionID string
	// SessionID, InstanceID, and AgentName attribute the unit that raised it.
	SessionID  string
	InstanceID string
	AgentName  string
	// ToolsName, ToolName, ArgsSummary, and Diff describe a permission ask; Summary and Detail describe an attention one.
	ToolsName   string
	ToolName    string
	ArgsSummary string
	Diff        string
	Summary     string
	Detail      string
	// PolicyName and MatchedRule name the envelope and the rule index that gated the call.
	PolicyName  string
	MatchedRule *int
	// OnTimeout is the verdict the envelope applies if nobody answers.
	OnTimeout string
}

// Adjudicator is offered every ask this process raises, before a human sees it. It resolves the durable row or does nothing; the parked requester picks the verdict up either way, so an Adjudicator can only ever be faster than a human, never authoritative over the envelope.
type Adjudicator interface {
	Adjudicate(ctx context.Context, ask Adjudication)
}

// SetAdjudicator mounts adj on svc; a nil adj unmounts.
func SetAdjudicator(svc Service, adj Adjudicator) {
	if s, ok := svc.(*service); ok {
		s.mu.Lock()
		s.adjudicator = adj
		s.mu.Unlock()
	}
}

// offer hands ask to this process's adjudicator at most once. Both seams that
// can raise an ask call it, because neither one always runs.
func (s *service) offer(ctx context.Context, ask Adjudication) {
	s.mu.Lock()
	adj := s.adjudicator
	if adj == nil {
		s.mu.Unlock()
		return
	}
	if _, dup := s.offered[ask.AskID]; dup {
		s.mu.Unlock()
		return
	}
	if s.offered == nil {
		s.offered = make(map[string]struct{})
	}
	s.offered[ask.AskID] = struct{}{}
	s.mu.Unlock()
	// Detached: the requester is about to park on this ask, and a verdict that races its own waiter would deadlock.
	go adj.Adjudicate(context.WithoutCancel(ctx), ask)
}

func (s *service) forgetOffer(askID string) {
	s.mu.Lock()
	delete(s.offered, askID)
	s.mu.Unlock()
}

func adjudicationFromApprovalRequest(askID string, req ApprovalRequest) Adjudication {
	return Adjudication{
		AskID:       askID,
		Kind:        AskKindPermission,
		MissionID:   req.MissionID,
		SessionID:   req.SessionID,
		InstanceID:  req.InstanceID,
		AgentName:   req.AgentName,
		ToolsName:   req.ToolsName,
		ToolName:    req.ToolName,
		ArgsSummary: summarizeApprovalArgs(req.Args),
		Diff:        req.Diff,
		PolicyName:  req.PolicyName,
		MatchedRule: req.MatchedRule,
		OnTimeout:   string(req.OnTimeout),
	}
}

func adjudicationFromAttentionRequest(askID string, req AttentionRequest) Adjudication {
	return Adjudication{
		AskID:      askID,
		Kind:       AskKindAttention,
		MissionID:  req.MissionID,
		SessionID:  req.SessionID,
		InstanceID: req.InstanceID,
		AgentName:  req.AgentName,
		ToolsName:  AttentionToolsName,
		ToolName:   AttentionToolName,
		Summary:    req.Summary,
		Detail:     req.Detail,
	}
}

// ErrAgentApprovalBoundSpent reports the mission's agent-adjudication cap was already spent when the conditional write ran.
var ErrAgentApprovalBoundSpent = errors.New("hitlservice: mission agent-approval bound spent")

const agentApprovalResolutionLike = `%"decidedBy":%`

func (s *service) RespondAsAgentBounded(ctx context.Context, askID, agentName string, approved bool, guidance string, max int) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	row, err := s.approvals.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return ErrApprovalNotFound
		}
		return fmt.Errorf("hitlservice: look up ask %s: %w", askID, err)
	}
	if IsAttentionAsk(row) {
		return fmt.Errorf("hitlservice: ask %s is a question (%s.%s), which is answered with words, not approve/deny",
			askID, row.ToolsName, row.ToolName)
	}
	if row.MissionID == nil || strings.TrimSpace(*row.MissionID) == "" {
		return fmt.Errorf("hitlservice: ask %s belongs to no mission, so no agent-approval bound can be applied to it", askID)
	}
	if err := s.requireResumerForVerdict(ctx, askID); err != nil {
		return err
	}

	state := runtimetypes.HITLApprovalApproved
	if !approved {
		state = runtimetypes.HITLApprovalDenied
	}
	resolveErr := s.approvals.ResolveHITLApprovalWithinBound(ctx, askID, runtimetypes.AgentAnswerBound{
		MissionID:      *row.MissionID,
		ResolutionLike: agentApprovalResolutionLike,
		Max:            max,
	}, state, marshalAgentApprovalResolution(approved, agentActor(agentName), guidance), time.Now().UTC())
	if resolveErr != nil {
		if !errors.Is(resolveErr, libdb.ErrNotFound) {
			return fmt.Errorf("hitlservice: resolve ask %s: %w", askID, resolveErr)
		}
		current, getErr := s.approvals.GetHITLApproval(ctx, askID)
		if getErr != nil {
			return fmt.Errorf("hitlservice: look up ask %s: %w", askID, getErr)
		}
		if current.State == runtimetypes.HITLApprovalPending {
			return ErrAgentApprovalBoundSpent
		}
		if current.State == runtimetypes.HITLApprovalExpired {
			return ErrApprovalExpired
		}
		return ErrApprovalAlreadyResolved
	}
	s.forgetOffer(askID)

	s.mu.Lock()
	ch, ok := s.pending[askID]
	hook := s.resumeHook
	s.mu.Unlock()
	if ok {
		select {
		case ch <- answer{approved: approved}:
		default:
		}
		return nil
	}
	if hook != nil {
		if err := hook(ctx, askID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			return fmt.Errorf("hitlservice: verdict for ask %s recorded, but resuming its suspended run failed: %w", askID, err)
		}
	}
	return nil
}

func marshalAgentApprovalResolution(approved bool, by, guidance string) json.RawMessage {
	res := approvalResolution{Approved: &approved}
	if by != "" {
		res.DecidedBy = &by
	}
	if g := strings.TrimSpace(guidance); g != "" {
		res.Guidance = &g
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// DecidedByOf returns the non-human actor recorded on a resolved permission ask, and "" for a human verdict or a pending row.
func DecidedByOf(row *runtimetypes.HITLApproval) string {
	if row == nil || len(row.Resolution) == 0 {
		return ""
	}
	var res approvalResolution
	if err := json.Unmarshal(row.Resolution, &res); err != nil || res.DecidedBy == nil {
		return ""
	}
	return strings.TrimSpace(*res.DecidedBy)
}

// GuidanceOf returns what an agent-decided ask told the unit to do instead, or "" when none was given.
func GuidanceOf(row *runtimetypes.HITLApproval) string {
	if row == nil || len(row.Resolution) == 0 {
		return ""
	}
	var res approvalResolution
	if err := json.Unmarshal(row.Resolution, &res); err != nil || res.Guidance == nil {
		return ""
	}
	return strings.TrimSpace(*res.Guidance)
}

func (s *service) AgentApprovalCount(ctx context.Context, missionID string) (int, error) {
	if s.approvals == nil {
		return 0, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return 0, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	count := 0
	for _, row := range rows {
		if IsAttentionAsk(row) {
			continue
		}
		if DecidedByOf(row) != "" {
			count++
		}
	}
	return count, nil
}

func (s *service) AgentGuidanceFor(ctx context.Context, missionID string) ([]GuidanceNote, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	out := make([]GuidanceNote, 0, 4)
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if IsAttentionAsk(row) || row.State != runtimetypes.HITLApprovalDenied {
			continue
		}
		note := GuidanceOf(row)
		if note == "" {
			continue
		}
		out = append(out, GuidanceNote{
			ToolsName: row.ToolsName,
			ToolName:  row.ToolName,
			DecidedBy: DecidedByOf(row),
			Guidance:  note,
		})
	}
	return out, nil
}

// GuidanceNote is one refused call and the redirect that came with it.
type GuidanceNote struct {
	ToolsName string
	ToolName  string
	DecidedBy string
	Guidance  string
}

func (s *service) AskGuidance(ctx context.Context, approvalID string) (string, string) {
	if s.approvals == nil {
		return "", ""
	}
	row, err := s.approvals.GetHITLApproval(ctx, approvalID)
	if err != nil {
		return "", ""
	}
	return DecidedByOf(row), GuidanceOf(row)
}
