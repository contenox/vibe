package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
)

type oracleResolver struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	store    runtimetypes.Store
	out      io.Writer
}

var _ oracletools.Resolver = oracleResolver{}

func (r oracleResolver) refuse(askID, reason string) error {
	if r.out != nil {
		fmt.Fprintf(r.out, "oracle: verdict refused for ask %s: %s\n", askID, reason)
	}
	return &oracletools.RefusedError{Reason: reason}
}

func (r oracleResolver) Answer(ctx context.Context, askID, text string) error {
	row, err := r.store.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdbexec.ErrNotFound) {
			return r.refuse(askID, fmt.Sprintf("ask %s no longer exists", askID))
		}
		return fmt.Errorf("read ask %s: %w", askID, err)
	}
	if !hitlservice.IsAttentionAsk(row) {
		return r.refuse(askID, fmt.Sprintf("ask %s is a gated tool call, not a question", askID))
	}
	if err := hitlservice.AnswerAsAgentWithinBounds(ctx, r.missions, r.hitl, row, oracleAgentName, text); err != nil {
		switch {
		case hitlservice.IsAgentAnswerRefusal(err),
			errors.Is(err, hitlservice.ErrApprovalNotFound),
			errors.Is(err, hitlservice.ErrApprovalAlreadyResolved),
			errors.Is(err, hitlservice.ErrApprovalExpired):
			return r.refuse(askID, err.Error())
		default:
			return err
		}
	}
	return nil
}

func (r oracleResolver) Decide(ctx context.Context, askID string, approve bool, guidance string) error {
	row, err := r.store.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdbexec.ErrNotFound) {
			return r.refuse(askID, fmt.Sprintf("ask %s no longer exists", askID))
		}
		return fmt.Errorf("read ask %s: %w", askID, err)
	}
	if hitlservice.IsAttentionAsk(row) {
		return r.refuse(askID, fmt.Sprintf("ask %s is a question, not a gated tool call", askID))
	}
	if row.MissionID == nil || *row.MissionID == "" {
		return r.refuse(askID, fmt.Sprintf("ask %s belongs to no subagent, so no envelope bounds it", askID))
	}
	bounds, err := r.boundsFor(ctx, *row.MissionID)
	if err != nil {
		return r.refuse(askID, "the subagent's envelope could not be read")
	}
	if !bounds.AllowAgentApprovals {
		return r.refuse(askID, "the subagent's envelope does not allow agent-decided tool calls")
	}
	err = r.hitl.RespondAsAgentBounded(ctx, askID, oracleAgentName, approve, guidance, bounds.EffectiveMaxAgentApprovals())
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hitlservice.ErrAgentApprovalBoundSpent):
		return r.refuse(askID, fmt.Sprintf("the subagent's agent-approval bound (%d) is spent", bounds.EffectiveMaxAgentApprovals()))
	case errors.Is(err, hitlservice.ErrApprovalNotFound),
		errors.Is(err, hitlservice.ErrApprovalAlreadyResolved),
		errors.Is(err, hitlservice.ErrApprovalExpired):
		return r.refuse(askID, err.Error())
	default:
		return err
	}
}

func (r oracleResolver) boundsFor(ctx context.Context, missionID string) (hitlservice.AttentionBounds, error) {
	m, err := r.missions.Get(ctx, missionID)
	if err != nil || m == nil {
		return hitlservice.AttentionBounds{}, fmt.Errorf("read subagent %s: %w", missionID, err)
	}
	return r.hitl.AttentionBoundsFor(ctx, m.HITLPolicyName)
}

type oracleDriver struct {
	agent         agentservice.Agent
	chain         *taskengine.TaskChainDefinition
	chainRef      string
	policy        string
	templateVars  map[string]string
	contextLength int
	// approves gates the permission half; the question half needs no switch of its own.
	approves bool
	missions missionservice.Service
	out      io.Writer

	mu       sync.Mutex
	inFlight map[string]bool
}

var _ hitlservice.Adjudicator = (*oracleDriver)(nil)

func (d *oracleDriver) Adjudicate(ctx context.Context, ask hitlservice.Adjudication) {
	if ask.AskID == "" || ask.MissionID == "" {
		return
	}
	if ask.Kind == hitlservice.AskKindPermission && !d.approves {
		return
	}
	// The oracle chain runs on the same engine and the same hitl instance, so an ask it raises itself must never re-enter here.
	if !d.claim(ask.AskID) {
		return
	}
	defer d.release(ask.AskID)

	input, err := json.Marshal(newOracleInput(ask, d.intentOf(ctx, ask.MissionID)))
	if err != nil {
		return
	}
	binding := oracletools.NewAskBinding(ask.AskID, bindingKind(ask.Kind), string(input))
	runCtx := hitlservice.WithPolicyName(ctx, d.policyName())
	runCtx = oracletools.WithBinding(runCtx, binding)

	d.tracef("oracle: reviewing %s ask %s (subagent %s): %s", ask.Kind, ask.AskID, ask.MissionID, askHeadline(ask))
	start := time.Now()
	_, runErr := d.agent.Prompt(runCtx, agentservice.PromptRequest{
		Input:         string(input),
		InputValue:    string(input),
		InputType:     taskengine.DataTypeString,
		Chain:         d.chain,
		ChainRef:      d.chainRef,
		TemplateVars:  d.templateVars,
		ContextLength: d.contextLength,
	})
	elapsed := time.Since(start).Round(time.Millisecond)

	switch binding.Outcome() {
	case oracletools.OutcomeAnswered:
		d.tracef("oracle: answered ask %s in %s: %q", ask.AskID, elapsed, binding.Answer())
	case oracletools.OutcomeApproved:
		d.tracef("oracle: APPROVED %s.%s for subagent %s in %s", ask.ToolsName, ask.ToolName, ask.MissionID, elapsed)
	case oracletools.OutcomeDenied:
		// The guidance rides the ask row, not a mission report: a report here would read as the unit reaching its operator and mute the drive loop's next turn.
		d.tracef("oracle: DENIED %s.%s for subagent %s in %s: %s", ask.ToolsName, ask.ToolName, ask.MissionID, elapsed, binding.Guidance())
	case oracletools.OutcomeWait:
		d.tracef("oracle: WAIT for ask %s (%s) — it stays with a human", ask.AskID, elapsed)
	default:
		if runErr != nil {
			d.tracef("oracle: chain error for ask %s (%s): %v — it stays with a human", ask.AskID, elapsed, runErr)
		} else {
			d.tracef("oracle: no verdict for ask %s within the chain budgets (%s) — it stays with a human", ask.AskID, elapsed)
		}
	}
}

func (d *oracleDriver) intentOf(ctx context.Context, missionID string) string {
	if d.missions == nil {
		return ""
	}
	m, err := d.missions.Get(ctx, missionID)
	if err != nil || m == nil {
		return ""
	}
	return m.Intent
}

func (d *oracleDriver) policyName() string {
	if d.policy == "" {
		return oracleDefaultPolicyName
	}
	return d.policy
}

func (d *oracleDriver) claim(askID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inFlight == nil {
		d.inFlight = map[string]bool{}
	}
	if d.inFlight[askID] {
		return false
	}
	d.inFlight[askID] = true
	return true
}

func (d *oracleDriver) release(askID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inFlight, askID)
}

func (d *oracleDriver) tracef(format string, args ...any) {
	if d.out != nil {
		fmt.Fprintf(d.out, format+"\n", args...)
	}
}

func bindingKind(k hitlservice.AskKind) oracletools.AskKind {
	if k == hitlservice.AskKindAttention {
		return oracletools.AskKindAttention
	}
	return oracletools.AskKindPermission
}

func askHeadline(ask hitlservice.Adjudication) string {
	if ask.Kind == hitlservice.AskKindAttention {
		return ask.Summary
	}
	return fmt.Sprintf("%s.%s %s", ask.ToolsName, ask.ToolName, ask.ArgsSummary)
}

// oracleInput is what the chain receives: the whole ask, plus the intent it must be judged against.
type oracleInput struct {
	AskID       string `json:"askId"`
	Kind        string `json:"kind"`
	MissionID   string `json:"missionId"`
	AgentName   string `json:"agentName,omitempty"`
	Intent      string `json:"intent,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ToolsName   string `json:"toolsName,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	ArgsSummary string `json:"argsSummary,omitempty"`
	Diff        string `json:"diff,omitempty"`
	PolicyName  string `json:"policyName,omitempty"`
	OnTimeout   string `json:"onTimeout,omitempty"`
}

func newOracleInput(ask hitlservice.Adjudication, intent string) oracleInput {
	return oracleInput{
		AskID:       ask.AskID,
		Kind:        string(ask.Kind),
		MissionID:   ask.MissionID,
		AgentName:   ask.AgentName,
		Intent:      intent,
		Summary:     ask.Summary,
		Detail:      ask.Detail,
		ToolsName:   ask.ToolsName,
		ToolName:    ask.ToolName,
		ArgsSummary: ask.ArgsSummary,
		Diff:        ask.Diff,
		PolicyName:  ask.PolicyName,
		OnTimeout:   ask.OnTimeout,
	}
}
