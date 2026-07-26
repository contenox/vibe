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
	"github.com/getkin/kin-openapi/openapi3"
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

// HITLWrapper is a decorator around any ToolsRepo that intercepts configured tool
// calls and requests human approval before delegating to the inner tools.
//
// Tool calls whose policy action is ActionAllow pass through instantly.
// ActionDeny returns a soft denial string so the LLM can propose an alternative.
// ActionApprove calls Ask and blocks until the human decides.
type HITLWrapper struct {
	inner     taskengine.ToolsRepo
	ask       AskApproval
	policy    hitlservice.PolicyEvaluator
	tracker   libtracker.ActivityTracker
	eventSink taskengine.TaskEventSink
}

func NewHITLWrapper(inner taskengine.ToolsRepo, ask AskApproval, policy hitlservice.PolicyEvaluator, tracker libtracker.ActivityTracker, eventSinks ...taskengine.TaskEventSink) *HITLWrapper {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var eventSink taskengine.TaskEventSink
	if len(eventSinks) > 0 {
		eventSink = eventSinks[0]
	}
	return &HITLWrapper{
		inner:     inner,
		ask:       ask,
		policy:    policy,
		tracker:   tracker,
		eventSink: eventSink,
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
		oldContent, newContent, diffErr := h.buildDiff(ctx, tools, toolName, args)
		if diffErr != nil {
			reportErr(fmt.Errorf("hitl: diff generation failed: %w", diffErr))
		}
		rendered := ""
		if oldContent != newContent {
			filePath, _ := args["path"].(string)
			rendered = unifiedDiff(filePath, oldContent, newContent)
		}
		toolCallID, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)
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

	default:
		h.publishDecision(ctx, tools.Name, toolName, args, result, false)
		return h.inner.Exec(ctx, startTime, input, debug, tools)
	}
}

func (h *HITLWrapper) publishDecision(ctx context.Context, toolsName, toolName string, args map[string]any, result hitlservice.EvaluationResult, approvalRequested bool) {
	if h.eventSink == nil || !h.eventSink.Enabled() {
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
		oldContent, err := h.readViaTools(ctx, tools, path)
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
		oldContent, err := h.readViaTools(ctx, tools, path)
		if err != nil {
			return "", "", err
		}
		newContent := strings.ReplaceAll(oldContent, pattern, replacement)
		return oldContent, newContent, nil
	}
	return "", "", nil
}

// readViaTools calls the inner tools's read_file tool so path resolution,
// symlink checks, and sandbox enforcement are handled by the tools itself.
// Returns ("", nil) when the file does not yet exist.
func (h *HITLWrapper) readViaTools(ctx context.Context, tools *taskengine.ToolsCall, path string) (string, error) {
	readCall := &taskengine.ToolsCall{Name: tools.Name, ToolName: "read_file"}
	result, _, err := h.inner.Exec(ctx, time.Now(), map[string]any{"path": path}, false, readCall)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	s, _ := result.(string)
	return s, nil
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
