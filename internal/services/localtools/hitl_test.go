package localtools_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
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

type mockInnerTools struct {
	fn    func(ctx context.Context, startTime time.Time, input any, debug bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error)
	calls []string
}

func (m *mockInnerTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	toolName := tools.ToolName
	if toolName == "" {
		toolName = tools.Name
	}
	m.calls = append(m.calls, toolName)
	if m.fn != nil {
		return m.fn(ctx, startTime, input, debug, tools)
	}
	return "ok", taskengine.DataTypeString, nil
}

func (m *mockInnerTools) Supports(_ context.Context) ([]string, error) { return nil, nil }
func (m *mockInnerTools) GetSchemasForSupportedTools(_ context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}
func (m *mockInnerTools) GetToolsForToolsByName(_ context.Context, _ string) ([]taskengine.Tool, error) {
	return nil, nil
}

type captureTaskEventSink struct {
	events []taskengine.TaskEvent
}

func (s *captureTaskEventSink) PublishTaskEvent(_ context.Context, event taskengine.TaskEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *captureTaskEventSink) Wants(taskengine.TaskEventKind) bool { return true }

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

func TestUnit_HITLWrapper_Allow_PassesThrough(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, allowPolicy(), nil)

	res, dt, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "read_file"})

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

func TestUnit_HITLWrapper_Allow_PublishesDecision(t *testing.T) {
	inner := &mockInnerTools{}
	sink := &captureTaskEventSink{}
	matchedRule := 3
	policy := &mockPolicyEval{result: hitlservice.EvaluationResult{
		Action:      hitlservice.ActionAllow,
		Reason:      hitlservice.ReasonMatchedRule,
		MatchedRule: &matchedRule,
		PolicyName:  "hitl-policy-dev.json",
	}}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil, sink)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"command": "python3"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "local_shell"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected one HITL decision event, got %d", len(sink.events))
	}
	event := sink.events[0]
	if event.Kind != taskengine.TaskEventHITLDecision {
		t.Fatalf("expected HITL decision event, got %s", event.Kind)
	}
	if event.HookName != "local_shell" || event.ToolName != "local_shell" {
		t.Fatalf("unexpected tool identity: %s.%s", event.HookName, event.ToolName)
	}
	if event.HITLAction != "allow" || event.HITLReason != hitlservice.ReasonMatchedRule {
		t.Fatalf("unexpected HITL decision: action=%s reason=%s", event.HITLAction, event.HITLReason)
	}
	if event.HITLPolicyName != "hitl-policy-dev.json" {
		t.Fatalf("expected policy name, got %q", event.HITLPolicyName)
	}
	if event.HITLArgsSummary != "python3" {
		t.Fatalf("expected command summary, got %q", event.HITLArgsSummary)
	}
	if event.HITLMatchedRule == nil || *event.HITLMatchedRule != matchedRule {
		t.Fatalf("expected matched rule %d, got %v", matchedRule, event.HITLMatchedRule)
	}
	if event.HITLApprovalRequested == nil || *event.HITLApprovalRequested {
		t.Fatalf("expected approvalRequested=false, got %v", event.HITLApprovalRequested)
	}
}

func TestUnit_HITLWrapper_Deny_BlocksInner(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, denyPolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != localtools.DenyMessage {
		t.Errorf("expected deny message, got %v", res)
	}
	if len(inner.calls) != 0 {
		t.Errorf("inner must not be called on deny, got %v", inner.calls)
	}
}

func TestUnit_HITLWrapper_Approve_HumanApproves_CallsInner(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, approvePolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "new"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

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

func TestUnit_HITLWrapper_Approve_HumanDenies_BlocksInner(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysDeny, approvePolicy(), nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "new"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != localtools.DenyMessage {
		t.Errorf("expected deny message, got %v", res)
	}
	// inner may have been called for read_file (diff) but not for write_file
	for _, c := range inner.calls {
		if c == "write_file" {
			t.Errorf("inner must not be called for write_file on human deny, calls: %v", inner.calls)
		}
	}
}

func TestUnit_HITLWrapper_PolicyError_FailsClosed(t *testing.T) {
	inner := &mockInnerTools{}
	policy := &mockPolicyEval{err: errors.New("policy unavailable")}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, policy, nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != localtools.DenyMessage {
		t.Errorf("expected deny on policy error, got %v", res)
	}
	if len(inner.calls) != 0 {
		t.Errorf("inner must not be called when policy fails, got %v", inner.calls)
	}
}

func TestUnit_HITLWrapper_NonMapInput_ReportsAndContinues(t *testing.T) {
	inner := &mockInnerTools{}
	w := localtools.NewHITLWrapper(inner, alwaysApprove, allowPolicy(), nil)

	// non-map input: policy evaluates with empty args, allow passes through
	_, _, err := w.Exec(context.Background(), time.Now(),
		"not-a-map", false,
		&taskengine.ToolsCall{Name: "echo", ToolName: "echo"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inner.calls) != 1 {
		t.Errorf("expected inner called once, got %v", inner.calls)
	}
}

func TestUnit_HITLWrapper_HITLTimeout_DeniesOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}
	inner := &mockInnerTools{}
	policy := &mockPolicyEval{result: hitlservice.EvaluationResult{
		Action:    hitlservice.ActionApprove,
		TimeoutS:  1,
		OnTimeout: hitlservice.ActionDeny,
	}}
	ask := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	w := localtools.NewHITLWrapper(inner, ask, policy, nil)

	res, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "x"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

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

func TestUnit_HITLWrapper_ParentCancellation_ReturnsError(t *testing.T) {
	inner := &mockInnerTools{}
	policy := &mockPolicyEval{result: hitlservice.EvaluationResult{
		Action:    hitlservice.ActionApprove,
		TimeoutS:  60,
		OnTimeout: hitlservice.ActionDeny,
	}}
	ask := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	w := localtools.NewHITLWrapper(inner, ask, policy, nil)

	parent, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := w.Exec(parent, time.Now(),
			map[string]any{"path": "a.txt", "content": "x"}, false,
			&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})
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

// ── diff via inner tools ────────────────────────────────────────────────────────

func TestUnit_HITLWrapper_DiffWriteFile_ExistingFile(t *testing.T) {
	oldContent := "line1\nline2\nline3\n"
	newContent := "line1\nchanged\nline3\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerTools{
		fn: func(_ context.Context, _ time.Time, input any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
			toolName := tools.ToolName
			if toolName == "" {
				toolName = tools.Name
			}
			if toolName == "read_file" {
				return oldContent, taskengine.DataTypeString, nil
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "test.txt", "content": newContent}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

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

func TestUnit_HITLWrapper_DiffWriteFile_NewFile(t *testing.T) {
	newContent := "hello\nworld\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerTools{
		fn: func(_ context.Context, _ time.Time, input any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
			toolName := tools.ToolName
			if toolName == "" {
				toolName = tools.Name
			}
			if toolName == "read_file" {
				// Simulate file not existing.
				return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", os.ErrNotExist)
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "new.txt", "content": newContent}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

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

func TestUnit_HITLWrapper_DiffSed(t *testing.T) {
	oldContent := "foo bar baz\n"

	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerTools{
		fn: func(_ context.Context, _ time.Time, _ any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
			if tools.ToolName == "read_file" {
				return oldContent, taskengine.DataTypeString, nil
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "f.txt", "pattern": "bar", "replacement": "qux"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "sed"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedReq.Diff, "-foo bar baz") || !strings.Contains(capturedReq.Diff, "+foo qux baz") {
		t.Errorf("unexpected sed diff:\n%s", capturedReq.Diff)
	}
}

func TestUnit_HITLWrapper_DiffReadError_ApprovalStillShown(t *testing.T) {
	var capturedReq hitlservice.ApprovalRequest
	ask := func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		capturedReq = req
		return true, nil
	}

	inner := &mockInnerTools{
		fn: func(_ context.Context, _ time.Time, _ any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
			if tools.ToolName == "read_file" {
				return nil, taskengine.DataTypeAny, errors.New("permission denied")
			}
			return "ok", taskengine.DataTypeString, nil
		},
	}
	w := localtools.NewHITLWrapper(inner, ask, approvePolicy(), nil)

	_, _, err := w.Exec(context.Background(), time.Now(),
		map[string]any{"path": "secret.txt", "content": "new"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Diff is empty but the approval request was still sent.
	if capturedReq.ToolsName != "local_fs" {
		t.Errorf("approval request was not sent, got toolsName=%q", capturedReq.ToolsName)
	}
}
