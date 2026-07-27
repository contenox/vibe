package fleetservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/libacp"
)

// UnattendedPermissionDeps is what an answerer needs to turn "a unit nobody is
// watching wants to do something" into an answer. Every field is required
// except Tracker (a nil one degrades to a Noop) and DefaultPolicyName.
type UnattendedPermissionDeps struct {
	// HITL evaluates the envelope and owns the durable ask. It must be backed
	// by a runtimetypes.Store — an evaluator-only service cannot create the
	// ask an escalation requires, and the answerer reports that as a refusal
	// rather than pretending to have asked.
	HITL hitlservice.Service

	// Missions resolves the mission from the instance that raised the request,
	// which is where the envelope lives.
	Missions missionservice.Service

	// Sink publishes the approval_requested TaskEvent that live surfaces
	// render. taskengine.NoopTaskEventSink{} is a legitimate value; nil is not
	// (RequestApproval publishes unconditionally).
	Sink taskengine.TaskEventSink

	// DefaultPolicyName is the envelope for an unattended session with NO
	// mission behind it. Empty means "whatever the HITL service already
	// defaults to" (its constructor fallback, then the built-in policy) rather
	// than a second rule set invented here — see the answerer's doc.
	DefaultPolicyName string

	Tracker libtracker.ActivityTracker
}

// NewUnattendedPermissionAnswerer returns the agentinstance.PermissionFallback
// that makes a mission's envelope real.
//
// # What it is for
//
// A mission runs with NO viewer attached, by design: an operator fires an
// intent and detaches. The kernel routes a downstream session/request_permission
// to the session's controller viewer, and with no controller it denies. So
// before this existed, the FIRST permission-gated action any unattended unit
// attempted was refused, the durable ask store never saw it, and the inbox had
// nothing to show — mission mode was broken for precisely the case it exists
// for. This is the answerer the kernel calls instead.
//
// # What it does
//
//  1. Resolves the mission from the instance id the kernel supplied. No
//     mission (a fleet unit brought up outside a dispatch) is a normal answer,
//     not a failure.
//  2. Evaluates the MISSION'S HITL policy — its envelope — against the
//     requested action. A rule that ALLOWS answers immediately, with no durable
//     ask created and no human involved: that is the whole point of an
//     envelope, a unit acting unattended inside declared bounds. A rule that
//     DENIES refuses. Anything else creates a durable ask and BLOCKS until it
//     is answered or expires.
//  3. With no mission, falls back to DefaultPolicyName rather than inventing a
//     second rule set for the no-mission case. There is one policy language and
//     one evaluator; "which policy" is the only question this layer answers.
//
// # The shape difference it bridges, and the gap rule
//
// The evaluator takes a contenox tool call — (toolsName, toolName, args). What
// arrives is a libacp.RequestPermissionRequest authored by whatever agent is
// downstream. approvalflow.MapRequest performs the projection and reports how
// much of it is real (see that file for the rules and for what is deliberately
// NOT guessed). This answerer owns the POLICY for the gaps it reports, and the
// rule is one-directional:
//
//   - A request whose contenox tool identity could not be established is
//     ESCALATED to a durable ask. It is never evaluated, because evaluating a
//     contenox policy against a vocabulary it was not written for produces a
//     verdict about a different action than the one being requested.
//   - A request whose ARGUMENTS could not be established is evaluated, but an
//     ALLOW verdict from that evaluation is escalated too: a policy's
//     condition-bearing deny rules cannot match arguments that are not there,
//     so "allow" reached without arguments is not a statement about this call.
//     A DENY verdict is honored as-is — the unsafe direction is permitting, not
//     refusing.
//
// In both cases the escalation costs a human's attention, which is the correct
// price for an action the envelope could not judge. Nothing about a gap ever
// resolves to allow.
func NewUnattendedPermissionAnswerer(deps UnattendedPermissionDeps) agentinstance.PermissionFallback {
	if deps.Tracker == nil {
		deps.Tracker = libtracker.NoopTracker{}
	}
	a := &unattendedAnswerer{deps: deps, calls: newMissionCallCounter(0)}
	return a.answer
}

type unattendedAnswerer struct {
	deps UnattendedPermissionDeps
	// calls tallies each mission's envelope-gated tool dispatches — the count the
	// maxToolCalls compute bound is checked against. See missionCallCounter.
	calls *missionCallCounter
}

func (a *unattendedAnswerer) answer(ctx context.Context, req agentinstance.UnattendedPermission) (libacp.RequestPermissionResponse, error) {
	reportErr, reportChange, end := a.deps.Tracker.Start(ctx, "hitl", "unattended_permission",
		"instance_id", req.InstanceID, "session_id", string(req.SessionID), "agent_name", req.AgentName)
	defer end()

	if a.deps.HITL == nil || a.deps.Missions == nil || a.deps.Sink == nil {
		// A wiring defect, not a decision. Report it and refuse: an answerer
		// that cannot reach its policy must not become an answerer that allows.
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

	// The COMPUTE half of the envelope, checked before the action rules: this
	// gated dispatch counts against the mission's maxToolCalls budget, and the call
	// that crosses it is refused with a teaching outcome while the mission is
	// finished stuck through the real terminal machinery. A mission with no
	// maxToolCalls bound (the default) is never counted here — today's behavior.
	if reason, refuse := a.toolCallBudgetRefusal(ctx, policyName, missionID); refuse {
		reportChange("compute_bound", reason)
		if _, err := a.deps.Missions.Finish(ctx, missionID, missionservice.StatusStuck, reason); err != nil {
			// Best-effort: a Finish that conflicts (the mission already terminal —
			// e.g. an earlier gated call from the same draining turn already finished
			// it) still leaves the durable status correct, and refusing THIS call is
			// the safe direction regardless. Record and move on.
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
			// Inside the envelope: answered without a human, and no durable ask is
			// created at all. An ask nobody needed to see is noise in the inbox.
			return approvalflow.Answer(req.Request, true), nil
		case hitlservice.ActionDeny:
			return approvalflow.Answer(req.Request, false), nil
		}
	}

	approved, err := a.escalate(ctx, req, mapped, verdict, policyName, missionID)
	if err != nil {
		// Every failure mode here — a missing durable store, a store write that
		// failed, the caller's context ending — refuses. The ask is either
		// pending in the store (where the sweeper will resolve it by policy) or
		// was never created; neither is a reason to let the action through.
		reportErr(err)
		return approvalflow.Answer(req.Request, false), nil
	}
	reportChange("approved", approved)
	return approvalflow.Answer(req.Request, approved), nil
}

// envelope resolves the policy that governs instanceID and the mission it came
// from. A unit with no mission behind it (an ACP chat session's external agent,
// a hand-started instance) is governed by the configured default rather than by
// a second rule set: it is still an unattended unit, and the only question is
// which policy names its bounds.
//
// EVERY lookup failure is treated alike — "no such mission" and a store that
// could not answer both fall through to the default envelope. They are not
// distinguished because the safe response is identical: the request is still
// judged, by a policy that is fail-closed (an unmatched action approves rather
// than allows). Failing the request open on a store hiccup, or refusing it
// outright, would both be worse answers than governing it by the default.
func (a *unattendedAnswerer) envelope(ctx context.Context, instanceID string) (policyName, missionID string) {
	m, err := a.deps.Missions.GetByInstance(ctx, instanceID)
	if err != nil {
		return a.deps.DefaultPolicyName, ""
	}
	if m.HITLPolicyName == "" {
		// Create() rejects a mission with no envelope, so this is unreachable
		// through the service; a record written another way still gets bounds.
		return a.deps.DefaultPolicyName, m.ID
	}
	return m.HITLPolicyName, m.ID
}

// toolCallBudgetRefusal applies the mission envelope's maxToolCalls compute bound
// to THIS gated dispatch. It counts the call and, when the count crosses the bound,
// returns the teaching reason and refuse=true so the caller refuses the call and
// finishes the mission stuck. It never itself writes to the mission store — the
// caller owns the Finish, so this stays a pure decision over the counter and the
// bound.
//
// It is a NO-OP (refuse=false) whenever a compute bound cannot apply: a session
// with no mission behind it (compute bounds are per-mission; there is nothing to
// count against or to finish), a HITL service that does not implement
// ComputeBoundsReader, a bounds-load error (unbounded on failure, exactly as
// Evaluate falls back), or a mission whose envelope sets no maxToolCalls. Every one
// of those leaves the request to the normal judge/escalate path — today's behavior.
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

// judge evaluates the envelope against the mapped request and reports whether
// the verdict must be escalated to a human anyway. The returned string is the
// REASON to escalate (empty when the verdict stands), which the caller records
// so an operator can see why an ask exists.
func (a *unattendedAnswerer) judge(ctx context.Context, policyName string, mapped approvalflow.Mapped) (hitlservice.EvaluationResult, string) {
	if !mapped.Named {
		// Unmappable: no evaluation is attempted at all. The verdict carries the
		// policy name so the durable row still says which envelope was in force.
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
		// Evaluate already falls back to the built-in policy on a load failure,
		// so an error here is exceptional. Escalate rather than guess.
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

// escalate creates the durable ask and BLOCKS until it is answered or bounded
// away. The attribution rides along so the inbox can name the unit, the session,
// the agent and the mission — not just the tool.
//
// The matched rule's own TimeoutS is applied as a deadline on the wait, exactly
// as localtools.HITLWrapper.Exec applies it for the native path — so a rule that
// says "ask, but only wait a minute" bounds this ask too, and an unanswered ask
// resolves by the rule's OnTimeout (deny unless the policy says otherwise, and a
// validated policy cannot say allow) instead of holding the unit's turn open. A
// rule with no timeout of its own is bounded by hitlservice's serve-level
// ceiling instead; either way the wait ends. The durable row outlives both: the
// expiry sweeper closes it out, so an ask nobody answered is visible as expired
// rather than vanishing.
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
		// An unnamed request still needs SOMETHING in the tool columns or the
		// inbox row is undecidable. The downstream's own title is the honest
		// stand-in: it is what the agent said it wants to do, marked as coming
		// from the wire rather than from a contenox tools namespace.
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
		// Our own rule deadline fired (not the caller's turn ending, which also
		// surfaces as a context error): resolve by the rule's OnTimeout, the same
		// judgement localtools.HITLWrapper makes for the native path. A validated
		// policy cannot set on_timeout=allow, so in practice this refuses — but
		// the branch is real so the two paths stay consistent if that changes.
		if verdict.TimeoutS > 0 && errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return verdict.OnTimeout == hitlservice.ActionAllow, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The downstream turn ended (a cancel, a client gone) while the ask
			// was pending. The row stays pending and the sweeper resolves it by
			// its own policy; nothing is lost, and the answer to THIS request is
			// a refusal.
			return false, err
		}
		return false, fmt.Errorf("fleetservice: durable approval for unattended permission: %w", err)
	}
	return approved, nil
}

// unmappedToolsName is the tools column for an ask whose contenox tool identity
// could not be established. It is a marker, not a namespace: no policy rule
// matches it (nothing evaluates an unmapped request in the first place), and it
// exists so an inbox row reads as "this came off the wire unnamed" instead of
// as an empty cell that looks like a bug.
const unmappedToolsName = "acp"

func nonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// maxToolColumn bounds what is written into the row's tool columns, which are
// VARCHAR(255) on Postgres. Only the unmapped path can reach it — a downstream's
// free-text title stands in for a tool name there — but it is applied to both so
// a wire value can never fail the insert the ask depends on.
const maxToolColumn = 255

func clampColumn(v string) string {
	r := []rune(v)
	if len(r) <= maxToolColumn {
		return v
	}
	return string(r[:maxToolColumn-3]) + "..."
}
