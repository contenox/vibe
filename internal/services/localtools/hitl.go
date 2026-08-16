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

// Prechecker refuses a call from static configuration alone, running and
// changing nothing; optional, an inner repo without it is gated unchanged.
type Prechecker interface {
	Precheck(ctx context.Context, input any, tools *taskengine.ToolsCall) error
}

// ApprovalParkWindow is how long an ActionApprove call waits for a verdict
// before the run suspends to a durable checkpoint instead of blocking.
const ApprovalParkWindow = 30 * time.Second

// HITLWrapper decorates a ToolsRepo, gating configured tool calls on
// ActionAllow/ActionDeny/ActionApprove before delegating to the inner tools.
type HITLWrapper struct {
	inner      taskengine.ToolsRepo
	ask        AskApproval
	policy     hitlservice.PolicyEvaluator
	tracker    libtracker.ActivityTracker
	eventSink  taskengine.TaskEventSink
	recorder   hitlservice.ApprovalRecorder
	responder  approvalResponder
	parkWindow time.Duration
	shellKind  hitlservice.ShellKind
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
		inner:      inner,
		ask:        ask,
		policy:     policy,
		tracker:    tracker,
		eventSink:  eventSink,
		recorder:   recorder,
		responder:  responder,
		parkWindow: ApprovalParkWindow,
		shellKind:  trustedShellKind(DetectPlatformShell()),
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

// SetParkWindow overrides the fast-path park duration; non-positive values
// are ignored, and it is not synchronized against in-flight Execs.
func (h *HITLWrapper) SetParkWindow(d time.Duration) {
	if d > 0 {
		h.parkWindow = d
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

type askOutcome struct {
	approved bool
	err      error
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

	if pre, ok := h.inner.(Prechecker); ok {
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
				reportChange("denied", DenyMessage)
				return DenyMessage, taskengine.DataTypeString, nil
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
		}
		h.publishDecision(ctx, tools.Name, toolName, args, result, true)
		h.hitlLog(ctx, "ask raised", "tool", toolName, "approval_id", toolCallID, "rule", result.MatchedRule, "policy", result.PolicyName)

		if h.recorder != nil {
			return h.askDurable(ctx, startTime, input, debug, tools, req, result, toolCallID, reportErr, reportChange)
		}
		return h.askBlocking(ctx, startTime, input, debug, tools, req, result, reportErr, reportChange)

	default:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return h.inner.Exec(ctx, startTime, input, debug, tools)
	}
}

func (h *HITLWrapper) askBlocking(
	ctx context.Context,
	startTime time.Time,
	input any,
	debug bool,
	tools *taskengine.ToolsCall,
	req hitlservice.ApprovalRequest,
	result hitlservice.EvaluationResult,
	reportErr func(error),
	reportChange func(string, any),
) (any, taskengine.DataType, error) {
	askCtx := ctx
	var askCancel context.CancelFunc
	if result.TimeoutS > 0 {
		askCtx, askCancel = context.WithTimeout(ctx, time.Duration(result.TimeoutS)*time.Second)
		defer askCancel()
	}

	h.hitlLog(ctx, "card shown", "tool", req.ToolName, "approval_id", req.ToolCallID)
	approvalPending.Add(1)
	approved, err := h.ask(askCtx, req)
	approvalPending.Add(-1)
	if err != nil {
		// Only treat as HITL timeout when our own deadline fired, not the
		// parent context's (both surface as DeadlineExceeded).
		if result.TimeoutS > 0 &&
			errors.Is(askCtx.Err(), context.DeadlineExceeded) &&
			ctx.Err() == nil {
			onTimeout := result.OnTimeout
			if onTimeout == "" {
				onTimeout = hitlservice.ActionDeny
			}
			if onTimeout == hitlservice.ActionAllow {
				h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", req.ToolCallID, "approved", true, "reason", "on_timeout")
				return h.inner.Exec(ctx, startTime, input, debug, tools)
			}
			h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", req.ToolCallID, "reason", "approval_timed_out")
			reportErr(fmt.Errorf("hitl: approval timed out: %w", err))
			return DenyTimeoutMessage, taskengine.DataTypeString, nil
		}
		h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", req.ToolCallID, "reason", "ask_error", "error", err.Error())
		err = fmt.Errorf("hitl: approval error: %w", err)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}
	h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", req.ToolCallID, "approved", approved)
	if !approved {
		reportChange("denied", DenyMessage)
		return DenyMessage, taskengine.DataTypeString, nil
	}
	return h.inner.Exec(ctx, startTime, input, debug, tools)
}

func (h *HITLWrapper) askDurable(
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

	// Row first: a restart from here on still shows the ask as pending.
	if err := h.recorder.RecordPendingApproval(ctx, approvalID, req); err != nil {
		// Do not lose the gate: fall back to blocking (not restart-durable this once).
		reportErr(fmt.Errorf("hitl: durable approval row failed, falling back to blocking ask: %w", err))
		return h.askBlocking(ctx, startTime, input, debug, tools, req, result, reportErr, reportChange)
	}

	park := h.parkWindow
	if park <= 0 {
		park = ApprovalParkWindow
	}
	ruleTimeout := time.Duration(result.TimeoutS) * time.Second
	// A shorter rule deadline wins: such a rule opted out of long waits and
	// must not start suspending now.
	ruleBounds := result.TimeoutS > 0 && ruleTimeout <= park
	window := park
	if ruleBounds {
		window = ruleTimeout
	}

	askCtx, askCancel := context.WithTimeout(ctx, window)
	defer askCancel()

	outcomeCh := make(chan askOutcome, 1)
	approvalPending.Add(1)
	h.hitlLog(ctx, "card shown", "tool", req.ToolName, "approval_id", approvalID)
	go func() {
		approved, err := h.ask(askCtx, req)
		outcomeCh <- askOutcome{approved: approved, err: err}
	}()
	var out askOutcome
	select {
	case out = <-outcomeCh:
	case <-askCtx.Done():
		// ask() ignores its context; abandon it here, the buffered channel
		// lets the goroutine drain whenever it returns.
		out = askOutcome{err: askCtx.Err()}
	}
	approvalPending.Add(-1)

	if out.err != nil {
		if errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			if ruleBounds {
				// Row closed best-effort: a racing Respond may already have
				// closed it, which is fine.
				onTimeout := result.OnTimeout
				if onTimeout == "" {
					onTimeout = hitlservice.ActionDeny
				}
				allow := onTimeout == hitlservice.ActionAllow
				h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", approvalID, "approved", allow, "reason", "on_timeout")
				if err := h.recorder.ResolveApprovalInline(ctx, approvalID, allow); err != nil && !errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
					reportErr(fmt.Errorf("hitl: closing timed-out approval row %s: %w", approvalID, err))
				} else {
					h.hitlLog(ctx, "verdict recorded", "tool", req.ToolName, "approval_id", approvalID, "approved", allow)
				}
				if allow {
					return h.inner.Exec(ctx, startTime, input, debug, tools)
				}
				h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", approvalID, "reason", "approval_timed_out")
				reportErr(fmt.Errorf("hitl: approval timed out: %w", out.err))
				return DenyTimeoutMessage, taskengine.DataTypeString, nil
			}
			// Park window elapsed with no verdict: the row stays pending and
			// the engine checkpoints and releases.
			h.hitlLog(ctx, "checkpoint parked", "tool", req.ToolName, "approval_id", approvalID)
			reportChange("approval_parked", approvalID)
			// The abandoned ask() goroutine's outcome must still be forwarded.
			if h.responder != nil {
				go h.deliverLateVerdict(approvalID, req.ToolName, outcomeCh)
			}
			return nil, taskengine.DataTypeAny, &taskengine.ApprovalPendingError{
				ApprovalID: approvalID,
				ToolName:   req.ToolName,
			}
		}
		// Row left pending: SweepExpired closes it; no checkpoint exists on
		// this path so nothing resumes.
		h.hitlLog(ctx, "turn failed", "tool", req.ToolName, "approval_id", approvalID, "reason", "ask_error", "error", out.err.Error())
		err := fmt.Errorf("hitl: approval error: %w", out.err)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	// Fast path: close the row inline (never the resume hook) since the
	// waiter is right here.
	h.hitlLog(ctx, "verdict entered", "tool", req.ToolName, "approval_id", approvalID, "approved", out.approved)
	if err := h.recorder.ResolveApprovalInline(ctx, approvalID, out.approved); err != nil && !errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
		reportErr(fmt.Errorf("hitl: closing answered approval row %s: %w", approvalID, err))
	} else {
		h.hitlLog(ctx, "verdict recorded", "tool", req.ToolName, "approval_id", approvalID, "approved", out.approved)
	}
	if !out.approved {
		msg := h.denyMessage(ctx, approvalID)
		reportChange("denied", msg)
		return msg, taskengine.DataTypeString, nil
	}
	return h.inner.Exec(ctx, startTime, input, debug, tools)
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
	if strings.TrimSpace(guidance) == "" {
		return fmt.Sprintf("Denied by %s per the mission envelope. Do not retry this call; take a different approach.", by)
	}
	return fmt.Sprintf("Denied by %s per the mission envelope: %s Do not retry this call.", by, guidance)
}

func (h *HITLWrapper) deliverLateVerdict(approvalID, toolName string, outcomeCh <-chan askOutcome) {
	out, ok := <-outcomeCh
	if !ok {
		return
	}
	bg := context.Background()
	reportErr, _, end := h.tracker.Start(bg, "hitl", "late_verdict", "tool_name", toolName, "approval_id", approvalID)
	defer end()

	if out.err != nil {
		h.hitlLog(bg, "turn failed", "tool", toolName, "approval_id", approvalID, "reason", "late_ask_error", "error", out.err.Error())
		return
	}
	h.hitlLog(bg, "verdict entered", "tool", toolName, "approval_id", approvalID, "approved", out.approved, "late", true)
	if err := h.responder.Respond(bg, approvalID, out.approved); err != nil {
		if errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
			// A racing sweep or external Respond may have closed this row already.
			h.hitlLog(bg, "verdict entered", "tool", toolName, "approval_id", approvalID, "approved", out.approved, "late", true, "outcome", "already_resolved")
			return
		}
		h.hitlLog(bg, "turn failed", "tool", toolName, "approval_id", approvalID, "reason", "late_respond_failed", "error", err.Error())
		reportErr(fmt.Errorf("hitl: delivering late verdict for approval %s: %w", approvalID, err))
		return
	}
	h.hitlLog(bg, "verdict recorded", "tool", toolName, "approval_id", approvalID, "approved", out.approved, "late", true)
	h.hitlLog(bg, "tool resumed", "tool", toolName, "approval_id", approvalID, "approved", out.approved)
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
