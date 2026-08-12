package localtools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
)

type fsFake struct {
	content string
	warm    bool
	notice  string

	reads    []fsRead
	writes   []string
	recorded bool
}

type fsRead struct {
	force   bool
	session string
}

func (f *fsFake) Exec(ctx context.Context, _ time.Time, input any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	args, _ := input.(map[string]any)
	if tools.ToolName != "read_file" {
		path, _ := args["path"].(string)
		f.writes = append(f.writes, path+"|"+sessionIDFromContext(ctx))
		return "ok", taskengine.DataTypeString, nil
	}

	force, _ := args["force"].(bool)
	session := sessionIDFromContext(ctx)
	f.reads = append(f.reads, fsRead{force: force, session: session})

	if f.notice != "" {
		return f.notice, taskengine.DataTypeString, nil
	}
	if !force && session != "" && f.warm {
		return fileUnchangedStub, taskengine.DataTypeString, nil
	}
	if session != "" {
		// recordFullRead: a read that answers with content records that this session has now seen this version of the file.
		f.recorded = true
	}
	return f.content, taskengine.DataTypeString, nil
}

func (f *fsFake) Supports(context.Context) ([]string, error) { return nil, nil }
func (f *fsFake) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}
func (f *fsFake) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

type stubPolicy struct{ action hitlservice.Action }

func (s stubPolicy) Evaluate(context.Context, string, string, map[string]any) (hitlservice.EvaluationResult, error) {
	return hitlservice.EvaluationResult{Action: s.action}, nil
}

func sessionCtx(id string) context.Context {
	return context.WithValue(context.Background(), runtimetypes.SessionIDContextKey, id)
}

const onDisk = "package app\n\nfunc main() {\n\trun()\n}\n"

// TestUnit_DiffBaseIsTheFileNotTheReadCache pins that the diff's before-side is the file on disk, never the session read-cache stub sentence.
func TestUnit_DiffBaseIsTheFileNotTheReadCache(t *testing.T) {
	const proposed = "package app\n\nfunc main() {\n\trun(ctx)\n}\n"

	fs := &fsFake{content: onDisk, warm: true}
	h := &HITLWrapper{inner: fs}

	oldContent, newContent, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"},
		"write_file",
		map[string]any{"path": "app.go", "content": proposed},
	)
	if err != nil {
		t.Fatalf("buildDiff: %v", err)
	}

	if strings.Contains(oldContent, "File unchanged") || oldContent == fileUnchangedStub {
		t.Fatalf("the diff's before-side is the cache stub, not the file:\n%q", oldContent)
	}
	if oldContent != onDisk {
		t.Fatalf("before-side = %q, want the file on disk %q", oldContent, onDisk)
	}
	if newContent != proposed {
		t.Fatalf("after-side = %q, want the proposed content", newContent)
	}

	rendered := unifiedDiff("app.go", oldContent, newContent)
	if !strings.Contains(rendered, "-\trun()") || !strings.Contains(rendered, "+\trun(ctx)") {
		t.Fatalf("rendered diff does not describe the change:\n%s", rendered)
	}
	if strings.Contains(rendered, "File unchanged") {
		t.Fatalf("the stub reached the rendered diff:\n%s", rendered)
	}

	if len(fs.reads) != 1 {
		t.Fatalf("read_file called %d times, want once", len(fs.reads))
	}
	if !fs.reads[0].force {
		t.Fatal("the approval-time read did not pass force: it can still be answered from the cache")
	}
}

// TestUnit_DiffBaseReadLeavesNoReadMarker pins that the gate's own read must not count as the model's, since a marker recorded here would satisfy write_file's read-before-write rule for the very write being gated.
func TestUnit_DiffBaseReadLeavesNoReadMarker(t *testing.T) {
	fs := &fsFake{content: onDisk}
	h := &HITLWrapper{inner: fs}

	if _, _, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"},
		"write_file",
		map[string]any{"path": "app.go", "content": "changed\n"},
	); err != nil {
		t.Fatalf("buildDiff: %v", err)
	}

	if fs.reads[0].session != "" {
		t.Fatalf("the approval-time read carried session %q; it must be anonymous", fs.reads[0].session)
	}
	if fs.recorded {
		t.Fatal("the approval-time read recorded a read marker for the session")
	}
}

// TestUnit_ApprovedWriteStillCarriesTheSession pins that stripping the identity is scoped to the diff read; the approved call itself runs with the caller's own context, untouched.
func TestUnit_ApprovedWriteStillCarriesTheSession(t *testing.T) {
	fs := &fsFake{content: onDisk, warm: true}
	var got hitlservice.ApprovalRequest
	h := NewHITLWrapper(fs, func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		got = req
		return true, nil
	}, stubPolicy{action: hitlservice.ActionApprove}, nil)

	if _, _, err := h.Exec(sessionCtx("sess-1"), time.Now(),
		map[string]any{"path": "app.go", "content": "changed\n"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if got.DiffOld != onDisk {
		t.Fatalf("the ask carried %q as the current contents", got.DiffOld)
	}
	if len(fs.writes) != 1 || fs.writes[0] != "app.go|sess-1" {
		t.Fatalf("the approved write ran as %v, want it under the caller's session", fs.writes)
	}
}

// TestUnit_DiffBaseRefusesARenderedNotice pins that a truncated read_file answer refuses to become a diff base (it would show the file's tail as falsely deleted), though the approval ask still goes to the human without a diff.
func TestUnit_DiffBaseRefusesARenderedNotice(t *testing.T) {
	head := "package app\nfunc main() {}"
	fs := &fsFake{
		content: onDisk,
		notice: head + "\n\nlocal_fs: read_file truncated — showed lines 1-2 of 900; " +
			"output capped at 40 bytes. To read the next page call read_file with start_line: 3. " +
			severityRecoverable,
	}
	h := &HITLWrapper{inner: fs}

	oldContent, newContent, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"},
		"write_file",
		map[string]any{"path": "app.go", "content": "changed\n"},
	)
	if err == nil {
		t.Fatal("a truncation notice was accepted as the file's current contents")
	}
	if oldContent != "" || newContent != "" {
		t.Fatalf("a refused read still produced a diff: %q → %q", oldContent, newContent)
	}

	// A missing picture is recoverable, a wrong one is not: the ask still goes to the human without a diff.
	fs2 := &fsFake{content: onDisk, notice: fs.notice}
	var got hitlservice.ApprovalRequest
	h2 := NewHITLWrapper(fs2, func(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		got = req
		return true, nil
	}, stubPolicy{action: hitlservice.ActionApprove}, nil)
	if _, _, err := h2.Exec(sessionCtx("sess-1"), time.Now(),
		map[string]any{"path": "app.go", "content": "changed\n"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got.ToolName != "write_file" {
		t.Fatal("the ask was not raised at all")
	}
	if got.Diff != "" || got.DiffOld != "" || got.DiffNew != "" {
		t.Fatalf("a refused before-side still produced a diff: %+v", got)
	}
}

// TestUnit_DiffBaseAcceptsAFileQuotingTheMarker pins that a file merely containing the severity marker text is still accepted as a file, since the notice check reads only the tail.
func TestUnit_DiffBaseAcceptsAFileQuotingTheMarker(t *testing.T) {
	content := "const severityRecoverable = \"" + severityRecoverable + "\"\n" +
		strings.Repeat("// padding\n", 80)
	fs := &fsFake{content: content}
	h := &HITLWrapper{inner: fs}

	oldContent, _, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"},
		"write_file",
		map[string]any{"path": "hardening.go", "content": "changed\n"},
	)
	if err != nil {
		t.Fatalf("a file quoting the marker was refused: %v", err)
	}
	if oldContent != content {
		t.Fatalf("before-side = %q, want the file", oldContent)
	}
}

// TestUnit_DiffBaseSedUsesTheFileToo: sed's before-side comes from the same
// read, and its after-side is computed from it — a stub base would have
// produced a diff of a replacement applied to a status message.
func TestUnit_DiffBaseSedUsesTheFileToo(t *testing.T) {
	fs := &fsFake{content: "foo bar baz\n", warm: true}
	h := &HITLWrapper{inner: fs}

	oldContent, newContent, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "sed"},
		"sed",
		map[string]any{"path": "f.txt", "pattern": "bar", "replacement": "qux"},
	)
	if err != nil {
		t.Fatalf("buildDiff: %v", err)
	}
	if oldContent != "foo bar baz\n" || newContent != "foo qux baz\n" {
		t.Fatalf("sed diff sides = %q → %q", oldContent, newContent)
	}
}

// TestUnit_DiffBaseEditFileUsesTheFileToo: edit_file's before-side comes from
// the same read, and its after-side is computed from it — same contract as sed.
func TestUnit_DiffBaseEditFileUsesTheFileToo(t *testing.T) {
	fs := &fsFake{content: "foo bar baz\n", warm: true}
	h := &HITLWrapper{inner: fs}

	oldContent, newContent, err := h.buildDiff(
		sessionCtx("sess-1"),
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "edit_file"},
		"edit_file",
		map[string]any{"path": "f.txt", "old_string": "bar", "new_string": "qux"},
	)
	if err != nil {
		t.Fatalf("buildDiff: %v", err)
	}
	if oldContent != "foo bar baz\n" || newContent != "foo qux baz\n" {
		t.Fatalf("edit_file diff sides = %q → %q", oldContent, newContent)
	}
}
