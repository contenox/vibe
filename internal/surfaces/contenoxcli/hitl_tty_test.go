package contenoxcli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/hitlservice"
)

// These tests pin the CLI half of the rendering contract shared with comp/approval's card.

const cliScriptCode = `const notes = host.tool("local_fs.read_file", { path: "CHANGELOG.md" });
if (!notes) {
	throw new Error("CHANGELOG.md is empty");
}
return { lines: notes.split("\n").length };`

func renderToString(req hitlservice.ApprovalRequest) string {
	var b bytes.Buffer
	renderApprovalRequest(&b, req)
	return b.String()
}

// TestUnit_MultiLineArgPrintsAsABlock asserts a multi-line code argument renders as a readable block, not an escaped one-liner truncated at 240 bytes.
func TestUnit_MultiLineArgPrintsAsABlock(t *testing.T) {
	out := renderToString(hitlservice.ApprovalRequest{
		ToolsName: "goja",
		ToolName:  "goja_eval",
		Args:      map[string]any{"code": cliScriptCode, "timeout_ms": 2000},
	})

	// The one-liner's own marker is what the escaped shape leaves behind.
	if strings.Contains(out, "bytes,") {
		t.Fatalf("the code argument is still summarised onto one line:\n%s", out)
	}
	if !strings.Contains(out, "    code =\n") {
		t.Fatalf("no block header for the code argument:\n%s", out)
	}
	// A backslash-n that is part of the source stays one: the block neither
	// escapes newlines nor un-escapes what the author wrote.
	if !strings.Contains(out, `      return { lines: notes.split("\n").length };`) {
		t.Fatalf("the source's own escape was rewritten:\n%s", out)
	}
	for _, src := range strings.Split(cliScriptCode, "\n") {
		want := "      " + strings.ReplaceAll(src, "\t", "        ")
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("source line missing from the block: %q\n%s", want, out)
		}
	}
	// Scalar arguments are untouched.
	if !strings.Contains(out, "    timeout_ms = 2000\n") {
		t.Fatalf("a scalar argument changed shape:\n%s", out)
	}
}

// TestUnit_MultiLineArgYieldsToTheDiff asserts a diff-bearing arg collapses to a "see diff" summary rather than also printing its own block.
func TestUnit_MultiLineArgYieldsToTheDiff(t *testing.T) {
	body := strings.Repeat("a line of replacement content\n", 40)
	out := renderToString(hitlservice.ApprovalRequest{
		ToolsName: "local_fs",
		ToolName:  "write_file",
		Args:      map[string]any{"path": "app.go", "content": body},
		Diff:      "--- app.go (current)\n+++ app.go (proposed)\n+a line of replacement content",
	})

	if strings.Contains(out, "    content =\n") {
		t.Fatalf("the write body was blocked out above its own diff:\n%s", out)
	}
	if !strings.Contains(out, "see diff") {
		t.Fatalf("the scalar summary lost its pointer at the diff:\n%s", out)
	}
}

// TestUnit_ArgBlockCapIsVisible asserts a block over the line cap is truncated with a visible notice of what approving would accept unseen.
func TestUnit_ArgBlockCapIsVisible(t *testing.T) {
	lines := make([]string, 140)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	out := renderToString(hitlservice.ApprovalRequest{
		ToolsName: "goja",
		ToolName:  "goja_eval",
		Args:      map[string]any{"code": strings.Join(lines, "\n")},
	})

	shown := strings.Count(out, "      line ")
	if shown != maxArgBlockLines {
		t.Fatalf("printed %d block lines, want %d", shown, maxArgBlockLines)
	}
	if !strings.Contains(out, "      line 39\n") {
		t.Fatalf("the cap did not fall after the 40th line:\n%s", out)
	}
	if want := "      ⚠ +100 more lines — approving accepts content you have not seen\n"; !strings.Contains(out, want) {
		t.Fatalf("cap notice missing:\n%s", out)
	}

	// A body exactly at the cap announces nothing.
	out = renderToString(hitlservice.ApprovalRequest{
		Args: map[string]any{"code": strings.Join(lines[:maxArgBlockLines], "\n")},
	})
	if strings.Contains(out, "more lines") {
		t.Fatalf("a body exactly at the cap announced a cap:\n%s", out)
	}
}

// TestUnit_ArgBlockIsSanitized asserts terminal escape sequences in a block argument never reach the terminal.
func TestUnit_ArgBlockIsSanitized(t *testing.T) {
	out := renderToString(hitlservice.ApprovalRequest{
		Args: map[string]any{"code": "run()\x1b[2Jcleared\n\x1b]0;pwned\x07gone\n\tindented\n‮drawkcab"},
	})

	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("an escape sequence reached the terminal: %q", out)
	}
	for _, want := range []string{
		"      run()cleared\n",
		"      gone\n",
		"              indented\n", // six of indent plus the expanded tab
		"      drawkcab\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("block line %q missing:\n%q", want, out)
		}
	}
}

// TestUnit_ScalarArgsAndDiffAreSanitizedToo asserts escape sequences in scalar arguments and diff bodies are also stripped before reaching the terminal.
func TestUnit_ScalarArgsAndDiffAreSanitizedToo(t *testing.T) {
	out := renderToString(hitlservice.ApprovalRequest{
		ToolsName: "local_fs",
		ToolName:  "write_file",
		Args:      map[string]any{"path": "a\x1b[2Jb\x1b]0;t\x07c"},
		Diff:      "--- a.go (current)\n-\told\x1b[2J\n+\tnew",
	})

	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("an escape sequence reached the terminal: %q", out)
	}
	if !strings.Contains(out, "    path = abc\n") {
		t.Fatalf("scalar argument not sanitized:\n%s", out)
	}
	// The diff keeps its content and indentation; only the escape goes, and the
	// tab expands from the line's own +/- column.
	if !strings.Contains(out, "    -       old\n") || !strings.Contains(out, "    +       new\n") {
		t.Fatalf("diff body lost content or indentation:\n%s", out)
	}
}

// TestUnit_NonStringArgsKeepTheirSummary asserts non-string arguments keep their scalar summary rendering instead of being treated as a block.
func TestUnit_NonStringArgsKeepTheirSummary(t *testing.T) {
	out := renderToString(hitlservice.ApprovalRequest{
		Args: map[string]any{
			"create": true,
			"paths":  []string{"a.go", "b.go"},
			"path":   "/repo/internal/app.go",
		},
	})
	for _, want := range []string{
		"    create = true\n",
		"    paths = [a.go b.go]\n",
		"    path = /repo/internal/app.go\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
}
