// Package localhooks provides local hook integrations.
package localhooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// AskApproval is the callback the HITLWrapper calls to request human review.
// Implementations must block until the human decides, then return (true, nil) to
// approve or (false, nil) to deny. Returning an error propagates it to the chain.
type AskApproval func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error)

// HITLWrapper is a decorator around any HookRepo that intercepts configured tool
// calls and requests human approval before delegating to the inner hook.
//
// Tool calls whose policy action is ActionAllow pass through instantly.
// ActionDeny returns a soft denial string so the LLM can propose an alternative.
// ActionApprove calls Ask and blocks until the human decides.
type HITLWrapper struct {
	inner   taskengine.HookRepo
	ask     AskApproval
	policy  hitlservice.PolicyEvaluator
	tracker libtracker.ActivityTracker
}

func NewHITLWrapper(inner taskengine.HookRepo, ask AskApproval, policy hitlservice.PolicyEvaluator, tracker libtracker.ActivityTracker) *HITLWrapper {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &HITLWrapper{
		inner:   inner,
		ask:     ask,
		policy:  policy,
		tracker: tracker,
	}
}

const denyMessage = "User denied the operation. Please ask for clarification or try a different, less destructive approach."

// Exec implements taskengine.HookRepo.
func (h *HITLWrapper) Exec(
	ctx context.Context,
	startTime time.Time,
	input any,
	debug bool,
	hook *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	toolName := hook.ToolName
	if toolName == "" {
		toolName = hook.Name
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

	result, err := h.policy.Evaluate(ctx, hook.Name, toolName, args)
	if err != nil {
		reportErr(fmt.Errorf("hitl: policy evaluation failed, denying: %w", err))
		return denyMessage, taskengine.DataTypeString, nil
	}

	switch result.Action {
	case hitlservice.ActionAllow:
		return h.inner.Exec(ctx, startTime, input, debug, hook)

	case hitlservice.ActionDeny:
		return denyMessage, taskengine.DataTypeString, nil

	case hitlservice.ActionApprove:
		diff, diffErr := h.buildDiff(ctx, hook, toolName, args)
		if diffErr != nil {
			reportErr(fmt.Errorf("hitl: diff generation failed: %w", diffErr))
		}
		req := hitlservice.ApprovalRequest{
			HookName: hook.Name,
			ToolName: toolName,
			Args:     args,
			Diff:     diff,
		}

		askCtx := ctx
		var askCancel context.CancelFunc
		if result.TimeoutS > 0 {
			askCtx, askCancel = context.WithTimeout(ctx, time.Duration(result.TimeoutS)*time.Second)
			defer askCancel()
		}

		approved, err := h.ask(askCtx, req)
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
					return h.inner.Exec(ctx, startTime, input, debug, hook)
				}
				reportErr(fmt.Errorf("hitl: approval timed out: %w", err))
				return "Approval timed out. The operation was automatically denied.", taskengine.DataTypeString, nil
			}
			err = fmt.Errorf("hitl: approval error: %w", err)
			reportErr(err)
			return nil, taskengine.DataTypeAny, err
		}
		if !approved {
			reportChange("denied", denyMessage)
			return denyMessage, taskengine.DataTypeString, nil
		}
		return h.inner.Exec(ctx, startTime, input, debug, hook)

	default:
		return h.inner.Exec(ctx, startTime, input, debug, hook)
	}
}

// Supports delegates to the inner repo.
func (h *HITLWrapper) Supports(ctx context.Context) ([]string, error) {
	return h.inner.Supports(ctx)
}

// GetSchemasForSupportedHooks delegates to the inner repo.
func (h *HITLWrapper) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	return h.inner.GetSchemasForSupportedHooks(ctx)
}

// GetToolsForHookByName delegates to the inner repo.
func (h *HITLWrapper) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	return h.inner.GetToolsForHookByName(ctx, name)
}

// Compile-time assertion.
var _ taskengine.HookRepo = (*HITLWrapper)(nil)

// ─── diff helpers ─────────────────────────────────────────────────────────────

// buildDiff fetches the current file content via the inner hook so that path
// resolution and sandbox enforcement are applied by the hook that owns those
// semantics, then returns a unified diff string.
func (h *HITLWrapper) buildDiff(ctx context.Context, hook *taskengine.HookCall, toolName string, args map[string]any) (string, error) {
	switch {
	case hook.Name == "local_fs" && toolName == "write_file":
		path, _ := args["path"].(string)
		newContent, _ := args["content"].(string)
		if path == "" {
			return "", nil
		}
		oldContent, err := h.readViaHook(ctx, hook, path)
		if err != nil {
			return "", err
		}
		return unifiedDiff(path, oldContent, newContent), nil

	case hook.Name == "local_fs" && toolName == "sed":
		path, _ := args["path"].(string)
		pattern, _ := args["pattern"].(string)
		replacement, _ := args["replacement"].(string)
		if path == "" || pattern == "" {
			return "", nil
		}
		oldContent, err := h.readViaHook(ctx, hook, path)
		if err != nil {
			return "", err
		}
		newContent := strings.ReplaceAll(oldContent, pattern, replacement)
		return unifiedDiff(path, oldContent, newContent), nil
	}
	return "", nil
}

// readViaHook calls the inner hook's read_file tool so path resolution,
// symlink checks, and sandbox enforcement are handled by the hook itself.
// Returns ("", nil) when the file does not yet exist.
func (h *HITLWrapper) readViaHook(ctx context.Context, hook *taskengine.HookCall, path string) (string, error) {
	readCall := &taskengine.HookCall{Name: hook.Name, ToolName: "read_file"}
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
