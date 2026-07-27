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

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

// approvalPending counts in-flight HITL approval prompts. Set by HITLWrapper.Exec
// before invoking the ask callback and cleared after it returns. Read by UI
// renderers (e.g. the CLI idle-hint suppressor) so they don't print noise while
// the user is being asked to decide.
var approvalPending atomic.Int32

// IsApprovalPending reports whether at least one HITL approval is awaiting a
// human response. UI layers can poll this to suppress activity indicators.
func IsApprovalPending() bool {
	return approvalPending.Load() > 0
}

// AskApproval is the callback the HITLWrapper calls to request human review.
// Implementations must block until the human decides, then return (true, nil) to
// approve or (false, nil) to deny. Returning an error propagates it to the chain.
type AskApproval func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error)

// ApprovalParkWindow is the fast-path park: how long an ActionApprove call
// keeps its goroutine (and the interactive prompt) waiting for a verdict
// before the run SUSPENDS instead — durable approval row + checkpoint, no
// parked goroutine (S6). A verdict inside the window behaves exactly as the
// pre-S6 blocking path did, so interactive latency is untouched; only slow
// approvals pay the checkpoint cost. 30s is deliberate: long enough that a
// human already looking at the card answers in-session, short enough that an
// unattended run releases its resources promptly.
const ApprovalParkWindow = 30 * time.Second

// HITLWrapper is a decorator around any ToolsRepo that intercepts configured tool
// calls and requests human approval before delegating to the inner tools.
//
// Tool calls whose policy action is ActionAllow pass through instantly.
// ActionDeny returns a soft denial string so the LLM can propose an alternative.
// ActionApprove asks the human. With a durable recorder (the policy evaluator
// implements hitlservice.ApprovalRecorder — the production hitlservice does),
// the ask is bounded by ApprovalParkWindow and a silent window ends in a typed
// taskengine.ApprovalPendingError, which the engine turns into a checkpointed
// suspension; without one (evaluator-only fakes) the pre-S6 blocking behavior
// is preserved.
type HITLWrapper struct {
	inner     taskengine.ToolsRepo
	ask       AskApproval
	policy    hitlservice.PolicyEvaluator
	tracker   libtracker.ActivityTracker
	eventSink taskengine.TaskEventSink
	// recorder is the durable half of the suspend path; nil when the policy
	// evaluator cannot record asks (then the legacy blocking path applies).
	recorder hitlservice.ApprovalRecorder
	// parkWindow is ApprovalParkWindow unless overridden via SetParkWindow.
	parkWindow time.Duration
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
	return &HITLWrapper{
		inner:      inner,
		ask:        ask,
		policy:     policy,
		tracker:    tracker,
		eventSink:  eventSink,
		recorder:   recorder,
		parkWindow: ApprovalParkWindow,
	}
}

// SetParkWindow overrides the fast-path park duration (ApprovalParkWindow by
// default). It exists for tests and controlled deployments; non-positive
// values are ignored. Call before the wrapper serves requests — it is not
// synchronized against in-flight Execs.
func (h *HITLWrapper) SetParkWindow(d time.Duration) {
	if d > 0 {
		h.parkWindow = d
	}
}

const DenyMessage = "User denied the operation. Please ask for clarification or try a different, less destructive approach."

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

	args, ok := input.(map[string]any)
	if !ok {
		reportErr(fmt.Errorf("hitl: non-map input %T; When-conditions will not be evaluated", input))
		args = make(map[string]any)
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
		return DenyMessage, taskengine.DataTypeString, nil

	case hitlservice.ActionApprove:
		toolCallID, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)

		// Resume-injected verdict (S6): the human already answered THIS exact
		// invocation — the approval ID equals the engine-minted call ID — so
		// asking again would gate one action twice. Approved executes; denied
		// takes the standard deny semantics the model already knows.
		if verdictApproved, ok := taskengine.ApprovalVerdictFromContext(ctx, toolCallID); ok {
			if !verdictApproved {
				reportChange("denied", DenyMessage)
				return DenyMessage, taskengine.DataTypeString, nil
			}
			return h.inner.Exec(ctx, startTime, input, debug, tools)
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
			ToolCallID: toolCallID,
			ToolsName:  tools.Name,
			ToolName:   toolName,
			Args:       args,
			Diff:       rendered,
			DiffOld:    oldContent,
			DiffNew:    newContent,
			// Carry the policy verdict along so a durable-store implementation
			// of AskApproval (hitlservice.RequestApproval) can record which
			// rule gated this and how long to wait; see ApprovalRequest's doc.
			PolicyName:  result.PolicyName,
			MatchedRule: result.MatchedRule,
			TimeoutS:    result.TimeoutS,
			OnTimeout:   result.OnTimeout,
		}
		h.publishDecision(ctx, tools.Name, toolName, args, result, true)

		if h.recorder != nil {
			return h.askDurable(ctx, startTime, input, debug, tools, req, result, toolCallID, reportErr, reportChange)
		}
		return h.askBlocking(ctx, startTime, input, debug, tools, req, result, reportErr, reportChange)

	default:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return h.inner.Exec(ctx, startTime, input, debug, tools)
	}
}

// askBlocking is the pre-S6 approval path, kept verbatim for wrappers whose
// policy evaluator cannot record durable asks (no ApprovalRecorder): ask the
// human and BLOCK — bounded only by the matched rule's own TimeoutS, applying
// its OnTimeout when that deadline fires.
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

	approvalPending.Add(1)
	approved, err := h.ask(askCtx, req)
	approvalPending.Add(-1)
	if err != nil {
		// Only treat as HITL timeout when our deadline fired, not when the parent
		// context was already cancelled (which also surfaces as DeadlineExceeded).
		if result.TimeoutS > 0 &&
			errors.Is(askCtx.Err(), context.DeadlineExceeded) &&
			ctx.Err() == nil {
			onTimeout := result.OnTimeout
			if onTimeout == "" {
				onTimeout = hitlservice.ActionDeny
			}
			if onTimeout == hitlservice.ActionAllow {
				return h.inner.Exec(ctx, startTime, input, debug, tools)
			}
			reportErr(fmt.Errorf("hitl: approval timed out: %w", err))
			return "Approval timed out. The operation was automatically denied.", taskengine.DataTypeString, nil
		}
		err = fmt.Errorf("hitl: approval error: %w", err)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}
	if !approved {
		reportChange("denied", DenyMessage)
		return DenyMessage, taskengine.DataTypeString, nil
	}
	return h.inner.Exec(ctx, startTime, input, debug, tools)
}

// askDurable is the S6 approval path: durable row FIRST, then a fast-path
// park bounded by min(parkWindow, rule TimeoutS). Outcomes:
//
//   - verdict inside the window → the row is closed inline and the call
//     proceeds exactly as the blocking path would (zero interactive
//     regression);
//   - the rule's own TimeoutS fires first → the rule's OnTimeout applies as
//     before, and the row is closed with that outcome;
//   - the park window elapses silently → taskengine.ApprovalPendingError:
//     the engine checkpoints the run and releases the goroutine, and the
//     still-pending row is answerable from any process (checkpoint second,
//     release third).
//
// Known race, accepted and bounded: a Respond landing between the row write
// and the engine's checkpoint write finds no checkpoint; agentservice.Prompt
// closes it by checking the row again after the checkpoint is durable and
// resuming inline when the verdict already landed.
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
		// The durable store is broken. Do not lose the gate — fall back to the
		// blocking path so the human is still asked; the ask is just not
		// restart-durable this once.
		reportErr(fmt.Errorf("hitl: durable approval row failed, falling back to blocking ask: %w", err))
		return h.askBlocking(ctx, startTime, input, debug, tools, req, result, reportErr, reportChange)
	}

	park := h.parkWindow
	if park <= 0 {
		park = ApprovalParkWindow
	}
	ruleTimeout := time.Duration(result.TimeoutS) * time.Second
	// When the rule's own deadline is shorter than the park window, the rule
	// wins and today's OnTimeout semantics apply — such a rule opted out of
	// long waits entirely and must not start suspending now.
	ruleBounds := result.TimeoutS > 0 && ruleTimeout <= park
	window := park
	if ruleBounds {
		window = ruleTimeout
	}

	askCtx, askCancel := context.WithTimeout(ctx, window)
	defer askCancel()

	type askOutcome struct {
		approved bool
		err      error
	}
	outcomeCh := make(chan askOutcome, 1)
	approvalPending.Add(1)
	go func() {
		approved, err := h.ask(askCtx, req)
		outcomeCh <- askOutcome{approved: approved, err: err}
	}()
	var out askOutcome
	select {
	case out = <-outcomeCh:
	case <-askCtx.Done():
		// The ask callback ignores its context (a TTY read): abandon it. The
		// goroutine drains into the buffered channel whenever it returns; the
		// verdict then arrives via the durable row instead.
		out = askOutcome{err: askCtx.Err()}
	}
	approvalPending.Add(-1)

	if out.err != nil {
		if errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			if ruleBounds {
				// The rule's own timeout: unchanged OnTimeout semantics, row
				// closed with the same outcome (best-effort — a racing Respond
				// already closed it, which is fine).
				onTimeout := result.OnTimeout
				if onTimeout == "" {
					onTimeout = hitlservice.ActionDeny
				}
				allow := onTimeout == hitlservice.ActionAllow
				if err := h.recorder.ResolveApprovalInline(ctx, approvalID, allow); err != nil && !errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
					reportErr(fmt.Errorf("hitl: closing timed-out approval row %s: %w", approvalID, err))
				}
				if allow {
					return h.inner.Exec(ctx, startTime, input, debug, tools)
				}
				reportErr(fmt.Errorf("hitl: approval timed out: %w", out.err))
				return "Approval timed out. The operation was automatically denied.", taskengine.DataTypeString, nil
			}
			// Fast-path park elapsed with no verdict: the third outcome. The
			// row stays pending; the engine checkpoints and releases.
			reportChange("approval_parked", approvalID)
			return nil, taskengine.DataTypeAny, &taskengine.ApprovalPendingError{
				ApprovalID: approvalID,
				ToolName:   req.ToolName,
			}
		}
		// Parent context ended or the ask itself failed: as before. The row is
		// left pending; SweepExpired closes it (and resumes nothing — no
		// checkpoint exists on this path).
		err := fmt.Errorf("hitl: approval error: %w", out.err)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	// In-session verdict inside the window — the fast path. Close the row so
	// the inbox never shows an already-decided ask; inline (never the resume
	// hook: the waiter is right here).
	if err := h.recorder.ResolveApprovalInline(ctx, approvalID, out.approved); err != nil && !errors.Is(err, hitlservice.ErrApprovalAlreadyResolved) {
		reportErr(fmt.Errorf("hitl: closing answered approval row %s: %w", approvalID, err))
	}
	if !out.approved {
		reportChange("denied", DenyMessage)
		return DenyMessage, taskengine.DataTypeString, nil
	}
	return h.inner.Exec(ctx, startTime, input, debug, tools)
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

// Compile-time assertion.
var _ taskengine.ToolsRepo = (*HITLWrapper)(nil)

// ─── diff helpers ─────────────────────────────────────────────────────────────

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
	}
	return "", "", nil
}

// errDiffBaseUnavailable is returned when the current contents of the file
// cannot be established at approval time. The caller shows the ask WITHOUT a
// diff, which is the only safe direction: a diff computed against anything
// other than the file is a diff of a change that is not the one being approved.
var errDiffBaseUnavailable = errors.New("hitl: current file contents unavailable for the diff")

// readCurrentContent returns the file's CURRENT contents — the before-side of
// the diff the operator is about to approve.
//
// It goes through the toolset's own read_file so that path resolution, symlink
// containment and the sandbox root are the same ones the write itself will be
// subject to; a read this function resolved differently would describe a
// different file than the one about to change. What it will NOT accept is a
// rendered or remembered answer in place of those bytes:
//
//   - force: true defeats read_file's session dedup. Warm-cache reads answer
//     with fileUnchangedStub — a sentence for the model, meaning "you already
//     have this" — and that sentence reached the diff builder as the prior
//     content, so an operator was shown a status message as the before-side of
//     a write and approved it (found by dogfooding). The approval gate is a
//     security boundary, not a performance path: it re-reads, every time.
//   - the read runs with the session identity stripped, so read_file neither
//     consults nor records this session's read markers. Recording one would be
//     worse than wasteful: the gate's own read would satisfy the read-before-
//     write rule (and clear a stale-read denial) for the very write it is
//     gating, i.e. the approval would grant the model a precondition it never
//     met.
//   - any answer still carrying a severity marker is a NOTICE, not a file: the
//     over-the-output-cap path returns a head plus "read_file truncated …",
//     whose head would render the file's tail as deleted. Refused rather than
//     diffed.
//
// Returns ("", nil) when the file does not yet exist — a new file legitimately
// has an empty before-side.
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
		// Defence in depth: force already prevents this, and a stripped session
		// prevents it again. If it ever arrives anyway, it is a status message,
		// and a status message must never be shown to a human as the file they
		// are about to overwrite. (The stub carries the real content for program
		// callers now — see FsUnchangedResult — but the gate deliberately does
		// not take that door: it re-reads, every time, so what the operator
		// approves is what is on disk at approval time.)
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

// severityTailWindow is how much of a tool answer is examined for the severity
// marker that identifies it as a notice. Only the tail is examined because a
// notice is APPENDED: scanning the whole answer would refuse to diff any file
// that merely quotes the marker — such as the file that defines it.
const severityTailWindow = 512

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ─── LCS unified diff ─────────────────────────────────────────────────────────

const (
	diffMaxFileLines   = 500
	diffContext        = 3
	diffMaxOutputLines = 120
)

type editOp struct {
	kind byte // ' ' unchanged, '+' added, '-' removed
	text string
}

// lcsEditScript returns the minimal edit script between old and new using a
// standard LCS backtrack. O(m×n) time and space — callers cap inputs first.
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

// unifiedDiff returns a unified-diff style summary of oldStr→newStr with ±3
// context lines around each changed hunk. Uses LCS so insertions and deletions
// at any position produce correct output.
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

	// Mark which ops to include: changed lines and their ±context neighbours.
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
			hunkBuf = append(hunkBuf, fmt.Sprintf("-%s\n", op.text))
			hunkOldCount++
			oldN++
		case '+':
			hunkBuf = append(hunkBuf, fmt.Sprintf("+%s\n", op.text))
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
