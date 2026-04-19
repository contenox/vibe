package localhooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/hitlservice"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ── mocks ─────────────────────────────────────────────────────────────────────

type mockPolicyEval struct {
	result hitlservice.EvaluationResult
	err    error
}

func (m *mockPolicyEval) Evaluate(_ context.Context, _, _ string, _ map[string]any) (hitlservice.EvaluationResult, error) {
	return m.result, m.err
}

type mockInnerHook struct {
	fn    func(ctx context.Context, startTime time.Time, input any, debug bool, hook *taskengine.HookCall) (any, taskengine.DataType, error)
	calls []string
}

func (m *mockInnerHook) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	toolName := hook.ToolName
	if toolName == "" {
		toolName = hook.Name
	}
	m.calls = append(m.calls, toolName)
	if m.fn != nil {
		return m.fn(ctx, startTime, input, debug, hook)
	}
	return "ok", taskengine.DataTypeString, nil
}

func (m *mockInnerHook) Supports(_ context.Context) ([]string, error) { return nil, nil }
func (m *mockInnerHook) GetSchemasForSupportedHooks(_ context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}
func (m *mockInnerHook) GetToolsForHookByName(_ context.Context, _ string) ([]taskengine.Tool, error) {
	return nil, nil
}

func allowPolicy() *mockPolicyEval {
	return &mockPolicyEval{result: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}}
}

func denyPolicy() *mockPolicyEval {
	return &mockPolicyEval{result: hitlservice.EvaluationResult{Action: hitlservice.ActionDeny}}
}

func approvePolicy() *mockPolicyEval {
	return &mockPolicyEval{result: hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}}
}

func alwaysApprove(_ context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
	return true, nil
}

func alwaysDeny(_ context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
	return false, nil
}

// ── HITLWrapper.Exec ──────────────────────────────────────────────────────────

func TestHITLWrapper_Allow_PassesThrough(t *testing.T) {
	inner := &mockInnerHook{}
	w := NewHITLWrapper(inner, alwaysApprove, allowPolicy(), nil)

	res, dt, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "read_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "ok" || dt != taskengine.DataTypeString {
		t.Errorf("unexpected result: %v %v", res, dt)
	}
	if len(inner.calls) != 1 || inner.calls[0] != "read_file" {
		t.Errorf("expected inner called once with read_file, got %v", inner.calls)
	}
}

func TestHITLWrapper_Deny_BlocksInner(t *testing.T) {
	inner := &mockInnerHook{}
	w := NewHITLWrapper(inner, alwaysApprove, denyPolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != denyMessage {
		t.Errorf("expected deny message, got %v", res)
	}
	if len(inner.calls) != 0 {
		t.Errorf("inner must not be called on deny, got %v", inner.calls)
	}
}

func TestHITLWrapper_Approve_HumanApproves_CallsInner(t *testing.T) {
	inner := &mockInnerHook{}
	w := NewHITLWrapper(inner, alwaysApprove, approvePolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "new"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "ok" {
		t.Errorf("expected ok, got %v", res)
	}
	// inner called twice: once for read_file (diff), once for write_file (actual)
	if len(inner.calls) < 1 || inner.calls[len(inner.calls)-1] != "write_file" {
		t.Errorf("expected write_file as last inner call, got %v", inner.calls)
	}
}

func TestHITLWrapper_Approve_HumanDenies_BlocksInner(t *testing.T) {
	inner := &mockInnerHook{}
	w := NewHITLWrapper(inner, alwaysDeny, approvePolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "new"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != denyMessage {
		t.Errorf("expected deny message, got %v", res)
	}
	// inner may have been called for read_file (diff) but not for write_file
	for _, c := range inner.calls {
		if c == "write_file" {
			t.Errorf("inner must not be called for write_file on human deny, calls: %v", inner.calls)
		}
	}
}

func TestHITLWrapper_PolicyError_FailsClosed(t *testing.T) {
	inner := &mockInnerHook{}
	policy := &mockPolicyEval{err: errors.New("policy unavailable")}
	w := NewHITLWrapper(inner, alwaysApprove, policy, nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != denyMessage {
		t.Errorf("expected deny on policy error, got %v", res)
	}
	if len(inner.calls) != 0 {
		t.Errorf("inner must not be called when policy fails, got %v", inner.calls)
	}
}

func TestHITLWrapper_NonMapInput_ReportsAndContinues(t *testing.T) {
	inner := &mockInnerHook{}
	w := NewHITLWrapper(inner, alwaysApprove, allowPolicy(), nil)

	// non-map input: policy evaluates with empty args, allow passes through
	_, _, err := w.Exec(context.Background(), time.Now(),
		"not-a-map", false,
		&taskengine.HookCall{Name: "echo", ToolName: "echo"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inner.calls) != 1 {
		t.Errorf("expected inner called once, got %v", inner.calls)
	}
}

func TestHITLWrapper_HITLTimeout_DeniesOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}
	inner := &mockInnerHook{}
	policy := &mockPolicyEval{result: hitlservice.EvaluationResult{
		Action:    hitlservice.ActionApprove,
		TimeoutS:  1,
		OnTimeout: hitlservice.ActionDeny,
	}}
	ask := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	w := NewHITLWrapper(inner, ask, policy, nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "x"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error on HITL timeout: %v", err)
	}
	if s, ok := res.(string); !ok || !strings.Contains(s, "timed out") {
		t.Errorf("expected timeout message, got %v", res)
	}
	for _, c := range inner.calls {
		if c == "write_file" {
			t.Errorf("inner must not execute write_file after HITL timeout, calls: %v", inner.calls)
		}
	}
}

func TestHITLWrapper_ParentCancellation_ReturnsError(t *testing.T) {
	inner := &mockInnerHook{}
	policy := &mockPolicyEval{result: hitlservice.EvaluationResult{
		Action:    hitlservice.ActionApprove,
		TimeoutS:  60,
		OnTimeout: hitlservice.ActionDeny,
	}}
	ask := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	w := NewHITLWrapper(inner, ask, policy, nil)

	parent, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := w.Exec(parent, time.Now(),
			map[string]any{"path": "a.txt", "content": "x"}, false,
			&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})
		result <- err
	}()

	cancel()
	err := <-result

	if err == nil {
		t.Fatal("expected error on parent cancellation, got nil")
	}
	if !strings.Contains(err.Error(), "approval error") {
		t.Errorf("expected approval error, got %v", err)
	}
	for _, c := range inner.calls {
		if c == "write_file" {
			t.Errorf("inner must not execute write_file on parent cancel, calls: %v", inner.calls)
		}
	}
}

// ── diff via inner hook ────────────────────────────────────────────────────────

func TestHITLWrapper_DiffWriteFile_ExistingFile(t *testing.T) {
	oldContent := "line1\nline2\nline3\n"
	newContent := "line1\nchanged\nline3\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerHook{
		fn: func(_ context.Context, _ time.Time, input any, _ bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
			toolName := hook.ToolName
			if toolName == "" {
				toolName = hook.Name
			}
			if toolName == "read_file" {
				return oldContent, taskengine.DataTypeString, nil
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "test.txt", "content": newContent}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedReq.Diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(capturedReq.Diff, "-line2") {
		t.Errorf("diff should show removed line2, got:\n%s", capturedReq.Diff)
	}
	if !strings.Contains(capturedReq.Diff, "+changed") {
		t.Errorf("diff should show added 'changed', got:\n%s", capturedReq.Diff)
	}
}

func TestHITLWrapper_DiffWriteFile_NewFile(t *testing.T) {
	newContent := "hello\nworld\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerHook{
		fn: func(_ context.Context, _ time.Time, input any, _ bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
			toolName := hook.ToolName
			if toolName == "" {
				toolName = hook.Name
			}
			if toolName == "read_file" {
				// Simulate file not existing.
				return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", os.ErrNotExist)
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "new.txt", "content": newContent}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedReq.Diff == "" {
		t.Fatal("expected non-empty diff for new file")
	}
	if !strings.Contains(capturedReq.Diff, "+hello") {
		t.Errorf("diff should show new file lines as additions, got:\n%s", capturedReq.Diff)
	}
}

func TestHITLWrapper_DiffSed(t *testing.T) {
	oldContent := "foo bar baz\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerHook{
		fn: func(_ context.Context, _ time.Time, _ any, _ bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
			if hook.ToolName == "read_file" {
				return oldContent, taskengine.DataTypeString, nil
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "f.txt", "pattern": "bar", "replacement": "qux"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "sed"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedReq.Diff, "-foo bar baz") || !strings.Contains(capturedReq.Diff, "+foo qux baz") {
		t.Errorf("unexpected sed diff:\n%s", capturedReq.Diff)
	}
}

func TestHITLWrapper_DiffReadError_ApprovalStillShown(t *testing.T) {
	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerHook{
		fn: func(_ context.Context, _ time.Time, _ any, _ bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
			if hook.ToolName == "read_file" {
				return nil, taskengine.DataTypeAny, errors.New("permission denied")
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "secret.txt", "content": "new"}, false,
		&taskengine.HookCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Diff is empty but the approval request was still sent.
	if capturedReq.HookName != "local_fs" {
		t.Errorf("approval request was not sent, got hookName=%q", capturedReq.HookName)
	}
}

// ── lcsEditScript unit tests ──────────────────────────────────────────────────

func TestLCSEditScript_NoChange(t *testing.T) {
	ops := lcsEditScript([]string{"a", "b", "c"}, []string{"a", "b", "c"})
	for _, op := range ops {
		if op.kind != ' ' {
			t.Errorf("expected all ops to be unchanged, got %q for %q", op.kind, op.text)
		}
	}
}

func TestLCSEditScript_InsertLine(t *testing.T) {
	ops := lcsEditScript([]string{"a", "c"}, []string{"a", "b", "c"})
	kinds := extractKinds(ops)
	if kinds != " + " {
		t.Errorf("expected ' + ', got %q", kinds)
	}
}

func TestLCSEditScript_DeleteLine(t *testing.T) {
	ops := lcsEditScript([]string{"a", "b", "c"}, []string{"a", "c"})
	kinds := extractKinds(ops)
	if kinds != " - " {
		t.Errorf("expected ' - ', got %q (ops: %v)", kinds, ops)
	}
}

func TestLCSEditScript_ChangeLine(t *testing.T) {
	// Change "b" to "x": delete b, insert x.
	ops := lcsEditScript([]string{"a", "b", "c"}, []string{"a", "x", "c"})
	kinds := extractKinds(ops)
	if kinds != " -+  " {
		// a(eq), b(del), x(ins), c(eq)
		// " - + " with spaces → let me re-check
		t.Logf("ops: %v, kinds: %q", ops, kinds)
		// expected: ' '(a), '-'(b), '+'(x), ' '(c)
		if kinds != " -+ " {
			t.Errorf("expected ' -+ ', got %q", kinds)
		}
	}
}

func TestLCSEditScript_EmptyOld(t *testing.T) {
	ops := lcsEditScript(nil, []string{"a", "b"})
	kinds := extractKinds(ops)
	for _, k := range kinds {
		if k != '+' {
			t.Errorf("all ops should be additions, got %q", kinds)
			break
		}
	}
}

func TestLCSEditScript_EmptyNew(t *testing.T) {
	ops := lcsEditScript([]string{"a", "b"}, nil)
	kinds := extractKinds(ops)
	for _, k := range kinds {
		if k != '-' {
			t.Errorf("all ops should be deletions, got %q", kinds)
			break
		}
	}
}

func extractKinds(ops []editOp) string {
	var b strings.Builder
	for _, op := range ops {
		b.WriteByte(op.kind)
	}
	return b.String()
}

// ── unifiedDiff unit tests ────────────────────────────────────────────────────

func TestUnifiedDiff_NoChange(t *testing.T) {
	result := unifiedDiff("f.txt", "same\n", "same\n")
	if result != "(no changes)" {
		t.Errorf("expected '(no changes)', got %q", result)
	}
}

func TestUnifiedDiff_NewFile(t *testing.T) {
	result := unifiedDiff("f.txt", "", "hello\nworld\n")
	if !strings.Contains(result, "+hello") || !strings.Contains(result, "+world") {
		t.Errorf("expected additions for new file, got:\n%s", result)
	}
	if strings.Contains(result, "\n-") {
		t.Errorf("unexpected deletions for new file:\n%s", result)
	}
}

func TestUnifiedDiff_DeleteFile(t *testing.T) {
	result := unifiedDiff("f.txt", "bye\n", "")
	if !strings.Contains(result, "-bye") {
		t.Errorf("expected deletion, got:\n%s", result)
	}
}

func TestUnifiedDiff_InsertionCorrect(t *testing.T) {
	// Inserting a line in the middle must not mark subsequent unchanged lines as changed.
	old := "a\nb\nc\nd\ne\n"
	new := "a\nb\nINSERTED\nc\nd\ne\n"
	result := unifiedDiff("f.txt", old, new)
	if strings.Contains(result, "-c") || strings.Contains(result, "-d") || strings.Contains(result, "-e") {
		t.Errorf("insertion must not mark subsequent lines as deleted:\n%s", result)
	}
	if !strings.Contains(result, "+INSERTED") {
		t.Errorf("expected +INSERTED in diff:\n%s", result)
	}
}

func TestUnifiedDiff_ContextLines(t *testing.T) {
	// Change only line 5 in a 10-line file; lines 2-4 and 6-8 should appear as context.
	lines := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	old := strings.Join(lines, "\n") + "\n"
	newLines := make([]string, len(lines))
	copy(newLines, lines)
	newLines[4] = "FIVE"
	new := strings.Join(newLines, "\n") + "\n"

	result := unifiedDiff("f.txt", old, new)
	if !strings.Contains(result, " 2") {
		t.Errorf("expected line '2' as context, got:\n%s", result)
	}
	if strings.Contains(result, " 1\n") {
		t.Errorf("line '1' is outside ±3 context and should not appear:\n%s", result)
	}
}

func TestUnifiedDiff_HunkHeader(t *testing.T) {
	result := unifiedDiff("f.txt", "a\nb\nc\n", "a\nX\nc\n")
	if !strings.Contains(result, "@@") {
		t.Errorf("expected @@ hunk header, got:\n%s", result)
	}
}

func TestUnifiedDiff_LargeFileTruncated(t *testing.T) {
	// Build a file larger than diffMaxFileLines.
	var sb strings.Builder
	for i := range diffMaxFileLines + 100 {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	big := sb.String()
	result := unifiedDiff("f.txt", big, big+"extra\n")
	if !strings.Contains(result, "truncated") {
		t.Errorf("expected truncation notice for large file:\n%s", result)
	}
}
