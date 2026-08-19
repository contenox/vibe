// Package localtools provides tools that fire around chain execution: approval gates and host-side helpers.
package localtools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func (h *HITLWrapper) hitlLog(ctx context.Context, msg string, kv ...any) {
	_, _, end := h.tracker.Start(ctx, "hitl", msg, kv...)
	end()
}

var approvalPending atomic.Int32

// IsApprovalPending reports whether at least one HITL approval is awaiting a
// human response.
func IsApprovalPending() bool {
	return approvalPending.Load() > 0
}

// AskApproval requests human review, blocking until decided: (true, nil)
// approves, (false, nil) denies, an error propagates to the chain.
type AskApproval func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error)

// HITLWrapper decorates a ToolsRepo, gating configured tool calls on
// ActionAllow/ActionDeny/ActionApprove before delegating to the inner tools.
type HITLWrapper struct {
	inner     taskengine.ToolsRepo
	ask       AskApproval
	policy    hitlservice.PolicyEvaluator
	tracker   libtracker.ActivityTracker
	eventSink taskengine.TaskEventSink
	recorder  hitlservice.ApprovalRecorder
	responder approvalResponder
	shellKind hitlservice.ShellKind
}

type approvalResponder interface {
	Respond(ctx context.Context, approvalID string, approved bool) error
}

func NewHITLWrapper(inner taskengine.ToolsRepo, ask AskApproval, policy hitlservice.PolicyEvaluator, tracker libtracker.ActivityTracker, eventSinks ...taskengine.TaskEventSink) *HITLWrapper {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var eventSink taskengine.TaskEventSink
	if len(eventSinks) > 0 {
		eventSink = eventSinks[0]
	}
	recorder, _ := policy.(hitlservice.ApprovalRecorder)
	responder, _ := policy.(approvalResponder)
	return &HITLWrapper{
		inner:     inner,
		ask:       ask,
		policy:    policy,
		tracker:   tracker,
		eventSink: eventSink,
		recorder:  recorder,
		responder: responder,
		shellKind: trustedShellKind(DetectPlatformShell()),
	}
}

// SetShell overrides the detected platform shell; not synchronized against
// in-flight Execs, so call it before the wrapper serves requests.
func (h *HITLWrapper) SetShell(shell PlatformShell) {
	h.shellKind = trustedShellKind(shell)
}

func trustedShellKind(shell PlatformShell) hitlservice.ShellKind {
	switch shell.WithDefaults().Kind {
	case ShellKindSh:
		return hitlservice.ShellKindPOSIX
	case ShellKindPowerShell:
		return hitlservice.ShellKindPowerShell
	case ShellKindCmd:
		return hitlservice.ShellKindCmd
	default:
		return hitlservice.ShellKindUnknown
	}
}

const DenyMessage = "User denied the operation. Please ask for clarification or try a different, less destructive approach."

func policyDenyMessage(result hitlservice.EvaluationResult) string {
	var b strings.Builder
	b.WriteString("Denied by the active policy")
	if name := strings.TrimSpace(result.PolicyName); name != "" {
		fmt.Fprintf(&b, " %s", name)
	}
	if result.MatchedRule != nil {
		fmt.Fprintf(&b, " (rule %d)", *result.MatchedRule)
	}
	b.WriteString(".")
	if detail := strings.TrimSpace(result.Detail); detail != "" {
		fmt.Fprintf(&b, " %s.", detail)
	}
	b.WriteString(" This is the envelope refusing the capability, not a transient error and not a judgement about this particular call." +
		" Do not retry it and do not attempt another route to the same effect." +
		" Either continue with the work you can still do, or stop and report that you are blocked on this.")
	return b.String()
}

// DenyTimeoutMessage is the result of a gated call auto-denied at its
// approval deadline; the tool never ran.
const DenyTimeoutMessage = "Approval timed out. The operation was automatically denied."

func policyEvalArgs(input any, tools *taskengine.ToolsCall) map[string]any {
	args := make(map[string]any)
	switch v := input.(type) {
	case map[string]any:
		for k, val := range v {
			args[k] = val
		}
	case string:
		if v != "" {
			args["stdin"] = v
		}
	}
	if tools != nil {
		for k, v := range tools.Args {
			args[k] = v
		}
	}
	return args
}

func askSessionID(ctx context.Context) string {
	id, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	return id
}

// Exec implements taskengine.ToolsRepo.
func (h *HITLWrapper) Exec(
	ctx context.Context,
	startTime time.Time,
	input any,
	debug bool,
	tools *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	toolName := tools.ToolName
	if toolName == "" {
		toolName = tools.Name
	}
	reportErr, reportChange, end := h.tracker.Start(ctx, "hitl", "exec", "tool_name", toolName, "args", input)
	defer end()

	if debug {
		reportChange("input", input)
	}

	args := policyEvalArgs(input, tools)

	// Declared here so a model-supplied "shell_kind" argument cannot decide
	// whether the analyzer runs at all.
	ctx = hitlservice.WithShellKind(ctx, string(h.shellKind))

	if pre, ok := h.inner.(taskengine.Prechecker); ok {
		if err := pre.Precheck(ctx, input, tools); err != nil {
			reportErr(err)
			return nil, taskengine.DataTypeAny, err
		}
	}

	result, err := h.policy.Evaluate(ctx, tools.Name, toolName, args)
	if err != nil {
		reportErr(fmt.Errorf("hitl: policy evaluation failed, denying: %w", err))
		h.publishDecision(ctx, tools.Name, toolName, args, hitlservice.EvaluationResult{
			Action: hitlservice.ActionDeny,
			Reason: "policy_error",
		}, false)
		return DenyMessage, taskengine.DataTypeString, nil
	}

	switch result.Action {
	case hitlservice.ActionAllow:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return h.inner.Exec(ctx, startTime, input, debug, tools)

	case hitlservice.ActionDeny:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return policyDenyMessage(result), taskengine.DataTypeString, nil

	case hitlservice.ActionApprove:
		toolCallID, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)

		// Resume-injected verdict: the approval ID equals the engine-minted
		// call ID, so asking again would gate the action twice.
		if verdictApproved, ok := taskengine.ApprovalVerdictFromContext(ctx, toolCallID); ok {
			h.hitlLog(ctx, "tool resumed via verdict", "tool", toolName, "approval_id", toolCallID, "approved", verdictApproved)
			if !verdictApproved {
				msg := h.denyMessage(ctx, toolCallID)
				reportChange("denied", msg)
				return msg, taskengine.DataTypeString, nil
			}
			// A retry of a partially completed resume must not run the world
			// twice: a prior result replays verbatim.
			if rec, ok := taskengine.RecordedGateResultFromContext(ctx, toolCallID); ok {
				h.hitlLog(ctx, "tool result replayed from gate record", "tool", toolName, "approval_id", toolCallID)
				return rec.Value, rec.Type, nil
			}
			out, dt, execErr := h.inner.Exec(ctx, startTime, input, debug, tools)
			if execErr != nil {
				return out, dt, execErr
			}
			if record := taskengine.GateResultRecorderFromContext(ctx); record != nil {
				if recErr := record(ctx, toolCallID, taskengine.GateResult{Value: out, Type: dt}); recErr != nil {
					// Hard failure: execute_tool_calls must not soften this into
					// a retryable "tool failed" result the model would re-issue.
					recErr = fmt.Errorf("hitl: gated call %s: %w: %w", toolCallID, taskengine.ErrGateRecordFailed, recErr)
					reportErr(recErr)
					return nil, taskengine.DataTypeAny, recErr
				}
			}
			return out, dt, nil
		}

		oldContent, newContent, diffErr := h.buildDiff(ctx, tools, toolName, args)
		if diffErr != nil {
			reportErr(fmt.Errorf("hitl: diff generation failed: %w", diffErr))
		}
		rendered := ""
		if oldContent != newContent {
			filePath, _ := args["path"].(string)
			rendered = unifiedDiff(filePath, oldContent, newContent)
		}
		req := hitlservice.ApprovalRequest{
			ToolCallID:  toolCallID,
			ToolsName:   tools.Name,
			ToolName:    toolName,
			Args:        args,
			Diff:        rendered,
			DiffOld:     oldContent,
			DiffNew:     newContent,
			PolicyName:  result.PolicyName,
			MatchedRule: result.MatchedRule,
			TimeoutS:    result.TimeoutS,
			OnTimeout:   result.OnTimeout,
			Detail:      result.Detail,
			SessionID:   askSessionID(ctx),
			// A unit's ask carries which subagent raised it, or nothing downstream can bound it: the adjudicator declines an unattributed ask and the envelope has no mission to count against.
			MissionID: missiontools.MissionIDFromContext(ctx),
			AgentName: hitlservice.AgentNameFromContext(ctx),
		}
		h.publishDecision(ctx, tools.Name, toolName, args, result, true)
		h.hitlLog(ctx, "ask raised", "tool", toolName, "approval_id", toolCallID, "rule", result.MatchedRule, "policy", result.PolicyName)

		return h.ask(ctx, startTime, input, debug, tools, req, result, toolCallID, reportErr, reportChange)

	default:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return h.inner.Exec(ctx, startTime, input, debug, tools)
	}
}

// ask is the one path a gated tool call takes. The durable row is written
// first, then the call blocks watching that row for a terminal verdict: an
// answer continues the turn in place and the gated tool runs. The row — not
// the local card — is what the wait watches, because the verdict may be
// written by another process or device entirely: a phone over the relay,
// `contenox approvals respond` in a second terminal, or an adjudicating agent.
// The call releases the process only when the process is genuinely leaving
// (ctx cancelled) or the caller detached this run's asks up front.
func (h *HITLWrapper) ask(
	ctx context.Context,
	startTime time.Time,
	input any,
	debug bool,
	tools *taskengine.ToolsCall,
	req hitlservice.ApprovalRequest,
	result hitlservice.EvaluationResult,
	toolCallID string,
	reportErr func(error),
	reportChange func(string, any),
) (any, taskengine.DataType, error) {
	approvalID := toolCallID
	if approvalID == "" {
		// tools-handler calls carry a synthetic per-invocation ID; anything
		// else still gets a unique durable key.
		approvalID = uuid.NewString()
	}

	// Row first, always: it costs nothing when the answer is instant, and it is
	// the only thing an answer from elsewhere can land on. A waiter is parked
	// before the row is recorded, because recording is also what offers the ask
	// to an adjudicator — an instant verdict must wake this call, not race it.
	durableID := ""
	var waiter <-chan bool
	if h.recorder != nil {
		var release func()
		waiter, release = h.parkWaiter(approvalID)
		defer release()
		if err := h.recorder.RecordPendingApproval(ctx, approvalID, req); err != nil {
			// Do not lose the gate: still ask, still block — just not restart-durable this once.
			release()
			waiter = nil
			reportErr(fmt.Errorf("hitl: durable approval row failed, the ask is blocking but not restart-durable: %w", err))
		} else {
			durableID = approvalID
		}
	}

	if durableID != "" && taskengine.AsksDetached(ctx) && taskengine.ToolCallSuspendable(ctx) {
		// The caller declared nobody is attached to answer this run. Releasing
		// the process is the point, so the card is detached too and the verdict
		// comes back through the resume hook.
		h.hitlLog(ctx, "card shown", "tool", req.ToolName, "approval_id", durableID)
		h.raiseDetachedCard(ctx, req, durableID, result)
		h.hitlLog(ctx, "checkpoint pending", "tool", req.ToolName, "approval_id", durableID)
		reportChange("approval_pending", durableID)
		return nil, taskengine.DataTypeAny, &taskengine.ApprovalPendingError{
			ApprovalID: durableID,
			ToolName:   req.ToolName,
		}
	}

	approved, outcome := h.waitForVerdict(ctx, req, result, durableID, waiter, reportErr)
	switch outcome {
	case verdictAnswered:
		h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", approvalID, "approved", approved)
	case verdictOnTimeout:
		h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", approvalID, "approved", approved, "reason", "on_timeout")
	case verdictLeaving:
		if durableID != "" && taskengine.ToolCallSuspendable(ctx) {
			// The process is going away with the ask unanswered. The row stays
			// pending and the engine checkpoints beside it, so answering later
			// resumes this run wherever it can be resumed.
			h.hitlLog(ctx, "checkpoint pending", "tool", req.ToolName, "approval_id", durableID, "reason", "process_leaving")
			reportChange("approval_pending", durableID)
			return nil, taskengine.DataTypeAny, &taskengine.ApprovalPendingError{
				ApprovalID: durableID,
				ToolName:   req.ToolName,
			}
		}
		err := fmt.Errorf("hitl: approval %s went unanswered and this run cannot suspend: %w", approvalID, ctx.Err())
		h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", approvalID, "reason", "cancelled")
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	case verdictUnaskable:
		h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", approvalID, "reason", "ask_error")
		err := fmt.Errorf("hitl: approval error: %w", h.lastAskErr(ctx))
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	h.closeRowInline(ctx, durableID, approved, reportErr)
	if !approved {
		msg := DenyMessage
		if durableID != "" {
			msg = h.denyMessage(ctx, durableID)
		}
		if outcome == verdictOnTimeout {
			msg = DenyTimeoutMessage
			h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", approvalID, "reason", "approval_timed_out")
		}
		reportChange("denied", msg)
		return msg, taskengine.DataTypeString, nil
	}
	return h.inner.Exec(ctx, startTime, input, debug, tools)
}

type verdictOutcome int

const (
	// verdictAnswered: somebody decided — the local card, another process, or an adjudicator.
	verdictAnswered verdictOutcome = iota
	// verdictOnTimeout: the operator's wait ran out and the rule's on_timeout stands in.
	verdictOnTimeout
	// verdictLeaving: this process is shutting down with the ask still open.
	verdictLeaving
	// verdictUnaskable: no durable row to watch and the card could not be presented.
	verdictUnaskable
)

// waitForVerdict blocks until the ask is decided. The durable row is the
// authority and is polled throughout; the card and the in-process waiter are
// only shortcuts onto it, so a verdict written anywhere still lands here.
func (h *HITLWrapper) waitForVerdict(
	ctx context.Context,
	req hitlservice.ApprovalRequest,
	result hitlservice.EvaluationResult,
	durableID string,
	waiter <-chan bool,
	reportErr func(error),
) (bool, verdictOutcome) {
	wait := h.askWait(result)
	waitCtx := ctx
	if !hitlservice.Indefinite(wait) {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}

	// The card is how this wait is presented, so the wait owns it: it is raised
	// on waitCtx and torn down with it, and a verdict it delivers late — after
	// the wait already resolved the row — is read by nobody.
	h.hitlLog(ctx, "card shown", "tool", req.ToolName, "approval_id", req.ToolCallID)
	card := h.showCard(waitCtx, req)

	approvalPending.Add(1)
	defer approvalPending.Add(-1)

	// Only a durable row can be answered from somewhere else; without one the
	// card is the only voice there is.
	var pollC <-chan time.Time
	if durableID != "" && h.watcher != nil {
		poll := time.NewTicker(hitlservice.ApprovalPollInterval)
		defer poll.Stop()
		pollC = poll.C
	}

	for {
		select {
		case res := <-card:
			if res.err != nil {
				if durableID == "" {
					h.rememberAskErr(res.err)
					return false, verdictUnaskable
				}
				// Nobody local could answer — a dropped editor, no attached
				// client. That is not a verdict: the row is still open to the
				// phone, another terminal, or an adjudicator.
				reportErr(fmt.Errorf("hitl: no local answer for approval %s, still waiting on its durable row: %w", durableID, res.err))
				card = nil
				continue
			}
			return res.approved, verdictAnswered

		case approved := <-waiter:
			return approved, verdictAnswered

		case <-pollC:
			approved, terminal, err := h.watcher.ApprovalVerdict(ctx, durableID)
			if err != nil || !terminal {
				// Unreadable right now, or still pending — including the
				// pending-to-pending writes a quorum or reassignment makes.
				continue
			}
			return approved, verdictAnswered

		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return false, verdictLeaving
			}
			// Our own wait elapsed: the rule's on_timeout is the verdict.
			onTimeout := result.OnTimeout
			if onTimeout == "" {
				onTimeout = hitlservice.ActionDeny
			}
			return onTimeout == hitlservice.ActionAllow, verdictOnTimeout
		}
	}
}

type cardResult struct {
	approved bool
	err      error
}

// showCard presents the ask on cardCtx and reports the answer once. The
// goroutine is bounded by cardCtx, which is the wait's own context, so the card
// never outlives the call that raised it.
func (h *HITLWrapper) showCard(cardCtx context.Context, req hitlservice.ApprovalRequest) <-chan cardResult {
	out := make(chan cardResult, 1)
	go func() {
		approved, err := h.ask(cardCtx, req)
		out <- cardResult{approved: approved, err: err}
	}()
	return out
}

// parkWaiter registers this call on the durable ask before the ask is offered,
// so an adjudicator's instant verdict wakes it rather than waiting for the next
// poll. It is a shortcut onto the row, never a substitute for reading it.
func (h *HITLWrapper) parkWaiter(approvalID string) (<-chan bool, func()) {
	reg, ok := h.recorder.(hitlservice.ApprovalWaiterRegistry)
	if !ok {
		return nil, func() {}
	}
	return reg.RegisterApprovalWaiter(approvalID)
}

// raiseDetachedCard is the card for a run whose asks were detached: it outlives
// the released turn and delivers its verdict through Respond, which is what
// fires the resume hook. Only the detached path may use it — a blocking wait
// that resolved through here would resume a checkpoint for a run still alive.
func (h *HITLWrapper) raiseDetachedCard(ctx context.Context, req hitlservice.ApprovalRequest, approvalID string, result hitlservice.EvaluationResult) {
	if h.responder == nil {
		return
	}
	askCtx := context.WithoutCancel(ctx)
	askCancel := context.CancelFunc(func() {})
	if lifetime := h.askWait(result); !hitlservice.Indefinite(lifetime) {
		askCtx, askCancel = context.WithTimeout(askCtx, lifetime)
	}
	go func() {
		defer askCancel()
		approved, err := h.ask(askCtx, req)
		h.deliverVerdict(approvalID, req.ToolName, approved, err)
	}()
}

func (h *HITLWrapper) closeRowInline(ctx context.Context, approvalID string, approved bool, reportErr func(error)) {
	if h.recorder == nil || approvalID == "" {
		return
	}
	// Inline, never Respond: this run is alive and about to continue, so no
	// resume hook may fire for it.
	if err := h.recorder.ResolveApprovalInline(ctx, approvalID, approved); err != nil && !errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
		reportErr(fmt.Errorf("hitl: closing answered approval row %s: %w", approvalID, err))
		return
	}
	h.hitlLog(ctx, "verdict recorded", "approval_id", approvalID, "approved", approved)
}

// askWait is the operator's wait for one ask: the rule's own, else the host's
// configured approval ceiling. It bounds the blocking call, the card and the
// durable row alike, so none of the three outlives the others.
func (h *HITLWrapper) askWait(result hitlservice.EvaluationResult) time.Duration {
	if result.TimeoutS != 0 {
		return hitlservice.WaitOf(result.TimeoutS)
	}
	if reader, ok := h.policy.(approvalCeilingReader); ok {
		return reader.ApprovalCeiling()
	}
	return hitlservice.FallbackApprovalCeiling
}

type approvalCeilingReader interface {
	ApprovalCeiling() time.Duration
}

type guidanceReader interface {
	AskGuidance(ctx context.Context, approvalID string) (by string, guidance string)
}

func (h *HITLWrapper) denyMessage(ctx context.Context, approvalID string) string {
	reader, ok := h.recorder.(guidanceReader)
	if !ok {
		return DenyMessage
	}
	by, guidance := reader.AskGuidance(ctx, approvalID)
	if by == "" {
		return DenyMessage
	}
	// A run outside a mission has no envelope to cite; the reviewer is what refused it.
	denier := fmt.Sprintf("Denied by %s", by)
	if missiontools.MissionIDFromContext(ctx) != "" {
		denier += " per the mission envelope"
	}
	if strings.TrimSpace(guidance) == "" {
		return denier + ". Do not retry this call; take a different approach."
	}
	return fmt.Sprintf("%s: %s Do not retry this call.", denier, guidance)
}

func (h *HITLWrapper) deliverVerdict(approvalID, toolName string, approved bool, askErr error) {
	bg := context.Background()
	reportErr, _, end := h.tracker.Start(bg, "hitl", "deliver_verdict", "tool_name", toolName, "approval_id", approvalID)
	defer end()

	if askErr != nil {
		h.hitlLog(bg, "turn failed", "tool", toolName, "approval_id", approvalID, "reason", "ask_error", "error", askErr.Error())
		return
	}
	h.hitlLog(bg, "verdict entered", "tool", toolName, "approval_id", approvalID, "approved", approved)
	if err := h.responder.Respond(bg, approvalID, approved); err != nil {
		if errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
			// A racing sweep or external Respond may have closed this row already.
			h.hitlLog(bg, "verdict entered", "tool", toolName, "approval_id", approvalID, "approved", approved, "outcome", "already_resolved")
			return
		}
		h.hitlLog(bg, "turn failed", "tool", toolName, "approval_id", approvalID, "reason", "respond_failed", "error", err.Error())
		reportErr(fmt.Errorf("hitl: delivering verdict for approval %s: %w", approvalID, err))
		return
	}
	h.hitlLog(bg, "verdict recorded", "tool", toolName, "approval_id", approvalID, "approved", approved)
	h.hitlLog(bg, "tool resumed", "tool", toolName, "approval_id", approvalID, "approved", approved)
}

func (h *HITLWrapper) publishDecision(ctx context.Context, toolsName, toolName string, args map[string]any, result hitlservice.EvaluationResult, approvalRequested bool) {
	if h.eventSink == nil || !h.eventSink.Wants(taskengine.TaskEventHITLDecision) {
		return
	}
	ev := taskengine.NewTaskEvent(ctx, taskengine.TaskEventHITLDecision)
	ev.HookName = toolsName
	ev.ToolName = toolName
	ev.HITLAction = string(result.Action)
	ev.HITLReason = result.Reason
	ev.HITLPolicyName = result.PolicyName
	ev.HITLArgsSummary = hitlArgsSummary(args)
	ev.HITLMatchedRule = result.MatchedRule
	ev.HITLTimeoutS = result.TimeoutS
	ev.HITLApprovalRequested = boolPtr(approvalRequested)
	if err := h.eventSink.PublishTaskEvent(ctx, ev); err != nil {
		reportErr, _, end := h.tracker.Start(ctx, "publish", "hitl_decision", "tool_name", toolName)
		reportErr(err)
		end()
	}
}

func hitlArgsSummary(args map[string]any) string {
	for _, key := range []string{"path", "command", "url", "pattern"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return trimHITLSummary(v)
		}
	}
	return ""
}

func trimHITLSummary(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) > 96 {
		return string([]rune(s)[:95]) + "..."
	}
	return s
}

func boolPtr(v bool) *bool {
	return &v
}

// Supports delegates to the inner repo.
func (h *HITLWrapper) Supports(ctx context.Context) ([]string, error) {
	return h.inner.Supports(ctx)
}

// GetSchemasForSupportedTools delegates to the inner repo.
func (h *HITLWrapper) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	return h.inner.GetSchemasForSupportedTools(ctx)
}

// GetToolsForToolsByName delegates to the inner repo.
func (h *HITLWrapper) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	return h.inner.GetToolsForToolsByName(ctx, name)
}

var _ taskengine.ToolsRepo = (*HITLWrapper)(nil)

func (h *HITLWrapper) buildDiff(ctx context.Context, tools *taskengine.ToolsCall, toolName string, args map[string]any) (string, string, error) {
	switch {
	case tools.Name == "local_fs" && toolName == "write_file":
		path, _ := args["path"].(string)
		newContent, _ := args["content"].(string)
		if path == "" {
			return "", "", nil
		}
		oldContent, err := h.readCurrentContent(ctx, tools, path)
		if err != nil {
			return "", "", err
		}
		return oldContent, newContent, nil

	case tools.Name == "local_fs" && toolName == "sed":
		path, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		replacement, _ := args["replacement"].(string)
		if path == "" || pattern == "" {
			return "", "", nil
		}
		oldContent, err := h.readCurrentContent(ctx, tools, path)
		if err != nil {
			return "", "", err
		}
		newContent := strings.ReplaceAll(oldContent, pattern, replacement)
		return oldContent, newContent, nil

	case tools.Name == "local_fs" && toolName == "edit_file":
		path, _ := args["path"].(string)
		oldString, _ := args["old_string"].(string)
		newString, _ := args["new_string"].(string)
		if path == "" || oldString == "" {
			return "", "", nil
		}
		oldContent, err := h.readCurrentContent(ctx, tools, path)
		if err != nil {
			return "", "", err
		}
		newContent := strings.ReplaceAll(oldContent, oldString, newString)
		return oldContent, newContent, nil
	}
	return "", "", nil
}

var errDiffBaseUnavailable = errors.New("hitl: current file contents unavailable for the diff")

func (h *HITLWrapper) readCurrentContent(ctx context.Context, tools *taskengine.ToolsCall, path string) (string, error) {
	readCall := &taskengine.ToolsCall{Name: tools.Name, ToolName: "read_file"}
	readCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "")
	result, _, err := h.inner.Exec(readCtx, time.Now(), map[string]any{"path": path, "force": true}, false, readCall)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}

	if _, cached := result.(FsUnchangedResult); cached {
		// A status message must never stand in for the file being overwritten.
		return "", fmt.Errorf("%w: read_file answered from its session cache", errDiffBaseUnavailable)
	}
	s, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("%w: read_file answered %T, not file contents", errDiffBaseUnavailable, result)
	}
	if hasSeverityMarker(strings.TrimSpace(tailOf(s, severityTailWindow))) {
		return "", fmt.Errorf("%w: read_file answered with a notice, not the file", errDiffBaseUnavailable)
	}
	return s, nil
}

const severityTailWindow = 512

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

const (
	diffMaxFileLines   = 500
	diffContext        = 3
	diffMaxOutputLines = 120
)

type editOp struct {
	kind byte
	text string
}

func lcsEditScript(old, new []string) []editOp {
	m, n := len(old), len(new)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if old[i-1] == new[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	ops := make([]editOp, 0, m+n)
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && old[i-1] == new[j-1]:
			ops = append(ops, editOp{' ', old[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, editOp{'+', new[j-1]})
			j--
		default:
			ops = append(ops, editOp{'-', old[i-1]})
			i--
		}
	}
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func annotatedDiffLine(ops []editOp, i int) (line, note string) {
	line = ops[i].text
	if dupAnnotation(ops, i) {
		return line, "  (inserts duplicate of adjacent line)"
	}
	if whitespaceOnlyAnnotation(ops, i) {
		return visibleWhitespace(line), "  (whitespace-only change)"
	}
	return line, ""
}

func dupAnnotation(ops []editOp, i int) bool {
	if ops[i].kind != '+' {
		return false
	}
	for _, j := range [2]int{i - 1, i + 1} {
		if j >= 0 && j < len(ops) && ops[j].kind != '+' && ops[j].text == ops[i].text {
			return true
		}
	}
	return false
}

func whitespaceOnlyAnnotation(ops []editOp, i int) bool {
	if ops[i].kind != '+' && ops[i].kind != '-' {
		return false
	}
	for _, j := range [2]int{i - 1, i + 1} {
		if j >= 0 && j < len(ops) && differsOnlyInWhitespace(ops[i].text, ops[j].text) {
			return true
		}
	}
	return false
}

func differsOnlyInWhitespace(a, b string) bool {
	if a == b {
		return false
	}
	return stripSpacesAndTabs(a) == stripSpacesAndTabs(b)
}

func stripSpacesAndTabs(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func visibleWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\t", "→")
	trimmed := strings.TrimRight(s, " ")
	if trimmed == s {
		return s
	}
	return trimmed + strings.Repeat("·", len(s)-len(trimmed))
}

func unifiedDiff(filename, oldStr, newStr string) string {
	if oldStr == newStr {
		return "(no changes)"
	}

	oldLines := splitLines(oldStr)
	newLines := splitLines(newStr)

	truncated := false
	if len(oldLines) > diffMaxFileLines {
		oldLines = oldLines[:diffMaxFileLines]
		truncated = true
	}
	if len(newLines) > diffMaxFileLines {
		newLines = newLines[:diffMaxFileLines]
		truncated = true
	}

	ops := lcsEditScript(oldLines, newLines)

	include := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind != ' ' {
			lo := max(0, i-diffContext)
			hi := min(len(ops), i+diffContext+1)
			for k := lo; k < hi; k++ {
				include[k] = true
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s (current)\n+++ %s (proposed)\n", filename, filename)

	outputLines := 0
	inHunk := false
	var hunkOldStart, hunkNewStart, hunkOldCount, hunkNewCount int
	var hunkBuf []string
	oldN, newN := 1, 1

	flushHunk := func() {
		if !inHunk {
			return
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", hunkOldStart, hunkOldCount, hunkNewStart, hunkNewCount)
		for _, l := range hunkBuf {
			sb.WriteString(l)
		}
		hunkBuf = hunkBuf[:0]
		inHunk = false
	}

	for i, op := range ops {
		if !include[i] {
			// Gaps between hunks: op.kind is always ' ' here by construction.
			flushHunk()
			oldN++
			newN++
			continue
		}
		if !inHunk {
			hunkOldStart = oldN
			hunkNewStart = newN
			hunkOldCount = 0
			hunkNewCount = 0
			inHunk = true
		}
		switch op.kind {
		case ' ':
			hunkBuf = append(hunkBuf, fmt.Sprintf(" %s\n", op.text))
			hunkOldCount++
			hunkNewCount++
			oldN++
			newN++
		case '-':
			line, note := annotatedDiffLine(ops, i)
			hunkBuf = append(hunkBuf, fmt.Sprintf("-%s%s\n", line, note))
			hunkOldCount++
			oldN++
		case '+':
			line, note := annotatedDiffLine(ops, i)
			hunkBuf = append(hunkBuf, fmt.Sprintf("+%s%s\n", line, note))
			hunkNewCount++
			newN++
		}
		outputLines++
		if outputLines >= diffMaxOutputLines {
			flushHunk()
			sb.WriteString("... (diff truncated)\n")
			if truncated {
				fmt.Fprintf(&sb, "... (file truncated to first %d lines)\n", diffMaxFileLines)
			}
			return sb.String()
		}
	}
	flushHunk()

	if truncated {
		fmt.Fprintf(&sb, "... (file truncated to first %d lines)\n", diffMaxFileLines)
	}
	return sb.String()
}
