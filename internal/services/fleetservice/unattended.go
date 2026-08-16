package fleetservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
)

// UnattendedPermissionDeps configures NewUnattendedPermissionAnswerer; every field is required except Tracker (nil degrades to Noop) and DefaultPolicyName.
type UnattendedPermissionDeps struct {
	// HITL evaluates the envelope and owns the durable ask; must be backed
	// by a runtimetypes.Store, or escalation refuses.
	HITL hitlservice.Service

	// Missions resolves the mission from the instance that raised the request.
	Missions missionservice.Service

	// Sink publishes approval_requested; taskengine.NoopTaskEventSink{} is legitimate but nil is not, since RequestApproval publishes unconditionally.
	Sink taskengine.TaskEventSink

	// DefaultPolicyName is the envelope for a session with no mission behind it; empty means whatever the HITL service already defaults to.
	DefaultPolicyName string

	Tracker libtracker.ActivityTracker
}

// NewUnattendedPermissionAnswerer returns the agentinstance.PermissionFallback that judges a viewer-less unit's request against its mission's envelope, answering allow/deny directly and escalating everything else (including any request whose tool identity or arguments could not be established, which never resolves to allow).
func NewUnattendedPermissionAnswerer(deps UnattendedPermissionDeps) agentinstance.PermissionFallback {
	if deps.Tracker == nil {
		deps.Tracker = libtracker.NoopTracker{}
	}
	a := &unattendedAnswerer{deps: deps, calls: newMissionCallCounter(0)}
	return a.answer
}

type unattendedAnswerer struct {
	deps  UnattendedPermissionDeps
	calls *missionCallCounter
}

func (a *unattendedAnswerer) answer(ctx context.Context, req agentinstance.UnattendedPermission) (libacp.RequestPermissionResponse, error) {
	reportErr, reportChange, end := a.deps.Tracker.Start(ctx, "hitl", "unattended_permission",
		"instance_id", req.InstanceID, "session_id", string(req.SessionID), "agent_name", req.AgentName)
	defer end()

	if a.deps.HITL == nil || a.deps.Missions == nil || a.deps.Sink == nil {
		err := fmt.Errorf("fleetservice: unattended permission answerer is not fully wired")
		reportErr(err)
		return approvalflow.Answer(req.Request, false), nil
	}

	mapped := approvalflow.MapRequest(req.Request)
	policyName, missionID := a.envelope(ctx, req.InstanceID)
	reportChange("policy", policyName)
	if missionID != "" {
		reportChange("mission_id", missionID)
	}

	// Checked before the action rules.
	if reason, refuse := a.toolCallBudgetRefusal(ctx, policyName, missionID); refuse {
		reportChange("compute_bound", reason)
		if _, err := a.deps.Missions.Finish(ctx, missionID, missionservice.StatusStuck, reason); err != nil {
			reportChange("compute_bound_finish_error", err.Error())
		}
		return approvalflow.Answer(req.Request, false), nil
	}

	verdict, escalate := a.judge(ctx, policyName, mapped)
	reportChange("action", string(verdict.Action))
	if escalate != "" {
		reportChange("escalated", escalate)
	}

	if escalate == "" {
		switch verdict.Action {
		case hitlservice.ActionAllow:
			return approvalflow.Answer(req.Request, true), nil
		case hitlservice.ActionDeny:
			return approvalflow.Answer(req.Request, false), nil
		}
	}

	approved, err := a.escalate(ctx, req, mapped, verdict, policyName, missionID)
	if err != nil {
		reportErr(err)
		return approvalflow.Answer(req.Request, false), nil
	}
	reportChange("approved", approved)
	return approvalflow.Answer(req.Request, approved), nil
}

func (a *unattendedAnswerer) envelope(ctx context.Context, instanceID string) (policyName, missionID string) {
	m, err := a.deps.Missions.GetByInstance(ctx, instanceID)
	if err != nil {
		return a.deps.DefaultPolicyName, ""
	}
	if m.HITLPolicyName == "" {
		return a.deps.DefaultPolicyName, m.ID
	}
	return m.HITLPolicyName, m.ID
}

func (a *unattendedAnswerer) toolCallBudgetRefusal(ctx context.Context, policyName, missionID string) (string, bool) {
	if missionID == "" {
		return "", false
	}
	reader, ok := a.deps.HITL.(hitlservice.ComputeBoundsReader)
	if !ok {
		return "", false
	}
	bounds, err := reader.ComputeBoundsFor(ctx, policyName)
	if err != nil || bounds.MaxToolCalls <= 0 {
		return "", false
	}
	count := a.calls.increment(missionID)
	if !toolCallBudgetExceeded(count, bounds) {
		return "", false
	}
	return toolCallsExhaustedReason(bounds), true
}

func (a *unattendedAnswerer) judge(ctx context.Context, policyName string, mapped approvalflow.Mapped) (hitlservice.EvaluationResult, string) {
	if !mapped.Named {
		return hitlservice.EvaluationResult{
			Action:     hitlservice.ActionApprove,
			Reason:     "unmapped_request",
			PolicyName: policyName,
		}, "unmapped_request"
	}

	evalCtx := ctx
	if policyName != "" {
		evalCtx = hitlservice.WithPolicyName(ctx, policyName)
	}
	verdict, err := a.deps.HITL.Evaluate(evalCtx, mapped.ToolsName, mapped.ToolName, mapped.Args)
	if err != nil {
		return hitlservice.EvaluationResult{
			Action:     hitlservice.ActionApprove,
			Reason:     "policy_error",
			PolicyName: policyName,
		}, "policy_error"
	}
	if verdict.Action == hitlservice.ActionAllow && !mapped.ArgsKnown {
		return verdict, "allow_without_args"
	}
	if verdict.Action == hitlservice.ActionApprove {
		return verdict, "policy_requires_approval"
	}
	return verdict, ""
}

func (a *unattendedAnswerer) escalate(
	ctx context.Context,
	req agentinstance.UnattendedPermission,
	mapped approvalflow.Mapped,
	verdict hitlservice.EvaluationResult,
	policyName, missionID string,
) (bool, error) {
	policy := verdict.PolicyName
	if policy == "" {
		policy = policyName
	}
	ask := hitlservice.ApprovalRequest{
		ToolCallID: mapped.ToolCallID,
		// An unnamed request's tool columns fall back to the downstream's title.
		ToolsName:   clampColumn(nonEmpty(mapped.ToolsName, unmappedToolsName)),
		ToolName:    clampColumn(nonEmpty(mapped.ToolName, mapped.Title)),
		Args:        mapped.Args,
		Diff:        mapped.Diff,
		PolicyName:  policy,
		MatchedRule: verdict.MatchedRule,
		TimeoutS:    verdict.TimeoutS,
		OnTimeout:   verdict.OnTimeout,
		InstanceID:  req.InstanceID,
		SessionID:   string(req.SessionID),
		AgentName:   req.AgentName,
		MissionID:   missionID,
	}
	askCtx := ctx
	if verdict.TimeoutS > 0 {
		var cancel context.CancelFunc
		askCtx, cancel = context.WithTimeout(ctx, time.Duration(verdict.TimeoutS)*time.Second)
		defer cancel()
	}

	approved, err := a.deps.HITL.RequestApproval(askCtx, ask, a.deps.Sink)
	if err != nil {
		// Our own rule deadline fired (distinct from the caller's turn
		// ending): resolve by OnTimeout.
		if verdict.TimeoutS > 0 && errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return verdict.OnTimeout == hitlservice.ActionAllow, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The downstream turn ended while the ask was pending; the row
			// stays pending for the sweeper.
			return false, err
		}
		return false, fmt.Errorf("fleetservice: durable approval for unattended permission: %w", err)
	}
	return approved, nil
}

const unmappedToolsName = "acp"

func nonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

const maxToolColumn = 255

func clampColumn(v string) string {
	// maxToolColumn bounds the row's tool columns (VARCHAR(255) on Postgres).
	r := []rune(v)
	if len(r) <= maxToolColumn {
		return v
	}
	return string(r[:maxToolColumn-3]) + "..."
}
