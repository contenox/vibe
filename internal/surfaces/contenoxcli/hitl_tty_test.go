package contenoxcli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/hitlservice"
)

// The CLI prompt and comp/approval's card are two renderings of one gate, and
// an argument that reads as a lie on one reads as a lie on the other. These
// tests pin the CLI half of that shared contract.

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

// TestUnit_MultiLineArgPrintsAsABlock is defect 1 on this surface: a code
// argument was printed with its newlines written out as literal "\n" and cut at
// 240 bytes, so the one argument that had to be read was the least readable
// thing above the prompt.
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
	// And a backslash-n that is part of the SOURCE stays one: the block neither
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

// TestUnit_MultiLineArgYieldsToTheDiff: with a diff rendered below, the scalar
// summary's "see diff" is TRUE and the diff is the better rendering of the same
// bytes — printing both would push the diff away from the prompt, which is the
// reason the summary exists.
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

// TestUnit_ArgBlockCapIsVisible: the cap says what approving would mean, in the
// same words the diff cap a few lines below it uses.
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

// TestUnit_ArgBlockIsSanitized: the block is peer-supplied text going to a
// terminal, and this one is printed directly rather than through a frame. An
// escape in it would erase the prompt it sits above.
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

// TestUnit_ScalarArgsAndDiffAreSanitizedToo: the block is not the only
// peer-supplied text on this prompt. A scalar argument and a diff body are
// printed straight to the terminal directly above the question, so an escape in
// either erases the very thing the operator is reading.
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
	// The diff keeps its content and its indentation; only the escape goes.
	// The tab expands from the diff line's own column 0, where its +/- sits, so
	// the body lands on the column stop its author saw.
	if !strings.Contains(out, "    -       old\n") || !strings.Contains(out, "    +       new\n") {
		t.Fatalf("diff body lost content or indentation:\n%s", out)
	}
}

// TestUnit_NonStringArgsKeepTheirSummary: only strings can be source text, and
// the block must not swallow the shapes that were already fine.
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
