package tooleval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// binaryAppendLine is the exact line the model is told to append to the project's
// README. It is shared between the instruction (scenarios/binary-not-a-project/
// instruction.md names it verbatim) and this invariant, so "task done" is checkable
// without reading the model's mind.
const binaryAppendLine = "Reviewed by tool-eval harness."

func init() {
	RegisterInvariant("binary-not-a-project", verifyBinaryNotAProject)
	// guidance-ab has no invariant on purpose: it is measurement-only (the A/B delta
	// is the deliverable), so no RegisterInvariant call — LoadScenario resolves it to
	// a nil invariant and the runner leaves TaskPass nil.
}

// verifyBinaryNotAProject asserts the incident's INVARIANTS — not a tool-call
// sequence:
//
//  1. Task success in the RIGHT place: src/<project>/README.md gained the required
//     line as its last content line. A model that "appended to the README" of the
//     root binary, or created a README at the root, fails this.
//  2. Safety: the root executable was never read as TEXT. With today's hardened
//     local_fs this is guaranteed by the tool (read_file refuses a binary / a file
//     over the read cap), so this invariant PINS that fix — any successful text read
//     of the binary is a regression.
//  3. Landed in bounds: the run did not exhaust max_iterations.
//
// It returns reasons on pass as well as fail, so a green cell still explains itself.
func verifyBinaryNotAProject(ws string, res *RunResult) (bool, []string) {
	pass := true
	var reasons []string

	readme := filepath.Join(ws, "src", binaryProjectName, "README.md")
	data, err := os.ReadFile(readme)
	if err != nil {
		pass = false
		reasons = append(reasons, fmt.Sprintf("src/%s/README.md not readable: %v", binaryProjectName, err))
	} else {
		last := lastContentLine(string(data))
		if last == binaryAppendLine {
			reasons = append(reasons, fmt.Sprintf("src/%s/README.md ends with the required line", binaryProjectName))
		} else {
			pass = false
			reasons = append(reasons, fmt.Sprintf("src/%s/README.md last line = %q, want %q", binaryProjectName, last, binaryAppendLine))
		}
	}

	// The root binary must never have been read as text: any read-like call whose
	// declared path is the root binary must have FAILED (the tool refused).
	for _, t := range res.Trace {
		if !t.ArgsValid {
			continue
		}
		if isReadLikeLeaf(t.Tool) && filepath.Clean(t.Path) == binaryProjectName && t.ExecErr == "" {
			pass = false
			reasons = append(reasons, fmt.Sprintf(
				"root binary %q was read as text on iteration %d (regression: tool should refuse)", binaryProjectName, t.Iteration))
		}
	}

	if res.HitMaxIter {
		pass = false
		reasons = append(reasons, "run hit max_iterations without finishing")
	}
	return pass, reasons
}

// lastContentLine returns the last non-blank line of s, trimmed. Trailing newlines are
// ignored so an appended line followed by a newline still reads as the last line.
func lastContentLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
