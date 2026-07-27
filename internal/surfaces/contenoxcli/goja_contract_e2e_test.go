package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/gojatool"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// THE PROGRAM-FACING CONTRACT, on the engine path.
//
// goja_e2e_test.go proves the one boundary rule — a script meets the envelope a
// model would. This file proves the caveat that outlived it, which live use
// found (2026-07-27) and no unit test could have:
//
//	a script that calls a tool inherits that tool's MODEL-FACING output
//	conventions, and mis-parses them CONFIDENTLY WRONG with nothing to catch it.
//
// Two cases were caught in the wild, and both are regression tests here:
//
//	(a) a script assumed git.git_status answered in porcelain. It answers in
//	    prose. The script reported "4 staged, 2 other, no untracked" for a tree
//	    with one modified and one untracked file, and returned successfully.
//	(b) local_fs.read_file answered with its session-dedup stub ("File unchanged
//	    since last read …") and the script treated that sentence as the file's
//	    content.
//
// The fixtures below are written the way the WRONG script was written — the
// mistake, not the repair — because a regression test for a silent failure has
// to make the silence audible. Each one must now either come back with
// structured data it cannot mis-read, or fail loudly at the line that guessed.
// ---------------------------------------------------------------------------

// scriptStatusCount is case (a), preserved: a script that treats git_status as a
// record it can count. Written against the STRUCTURED result, which is what the
// tool now hands a program.
const scriptStatusCount = `const tool = {
  name: "status_count",
  description: "Count what git_status reports: staged, unstaged, untracked.",
  schema: { type: "object", properties: {} },
  tools: ["git.git_status"],
};

function run() {
  const st = host.tool("git.git_status");
  return {
    branch: st.branch,
    clean: st.clean,
    staged: st.staged.length,
    unstaged: st.unstaged.length,
    untracked: st.untracked.length,
    untracked_names: st.untracked,
    unstaged_codes: st.unstaged.map(function (e) { return e.code; }),
  };
}
`

// scriptStatusSurgery is case (a) EXACTLY as it was written when it was wrong:
// string surgery on git_status. It must not be able to answer at all.
const scriptStatusSurgery = `const tool = {
  name: "status_surgery",
  description: "Test-only: the confidently-wrong script, preserved.",
  schema: { type: "object", properties: {} },
  tools: ["git.git_status"],
};

function run() {
  const out = host.tool("git.git_status");
  const lines = String(out).split("\n").filter(Boolean);
  return { staged: lines.length, untracked: 0 };
}
`

// scriptReadTwice is case (b): the second read of the same file in one session
// is the one that used to hand back the stub sentence.
const scriptReadTwice = `const tool = {
  name: "read_twice",
  description: "Test-only: reads the same file twice and reports both answers.",
  schema: {
    type: "object",
    properties: { path: { type: "string", description: "Workspace-relative path." } },
    required: ["path"],
  },
  tools: ["local_fs.read_file"],
};

function run(args) {
  const first = host.tool("local_fs.read_file", { path: args.path });
  const second = host.tool("local_fs.read_file", { path: args.path });
  return {
    first_bytes: first.bytes,
    second_bytes: second.bytes,
    same: first.text === second.text,
    second_head: second.text.split("\n")[0],
  };
}
`

// scriptUndeclared declares one tool and reaches for another.
const scriptUndeclared = `const tool = {
  name: "undeclared_reach",
  description: "Test-only: calls a tool it did not declare.",
  schema: { type: "object", properties: {} },
  tools: ["local_fs.read_file"],
};

function run() {
  return host.tool("git.git_status");
}
`

// scriptDeclaresNothing declares an EMPTY reach, which is a declaration and not
// an omission: it says the script touches nothing at all.
const scriptDeclaresNothing = `const tool = {
  name: "declares_nothing",
  description: "Test-only: declares an empty reach and then reaches anyway.",
  schema: { type: "object", properties: {} },
  tools: [],
};

function run() {
  return host.tool("local_fs.read_file", { path: "README.md" });
}
`

// scriptRawEscape is the escape hatch used deliberately: an author who means to
// parse prose says so at the call site, in a line anyone reviewing the script
// can see. git_diff is genuinely text — a diff IS a text artifact — so it is the
// honest place to show the door.
const scriptRawEscape = `const tool = {
  name: "raw_escape",
  description: "Test-only: asks for the bare string on purpose.",
  schema: { type: "object", properties: {} },
  tools: ["git.git_diff", "git.git_status"],
};

function run() {
  const text = host.tool("git.git_diff", {}, { raw: true });
  const wrapped = host.tool("git.git_diff");
  return {
    kind: typeof text,
    wrapped_kind: typeof wrapped,
    wrapped_shape: wrapped.shape,
    first_line: text.split("\n")[0],
    same: wrapped.text === text,
  };
}
`

// scriptDiffSurgery is the TEXT half of the guard: git_diff answers in prose,
// and a script that splits it is guessing at a format nobody promised.
const scriptDiffSurgery = `const tool = {
  name: "diff_surgery",
  description: "Test-only: string surgery on a prose result.",
  schema: { type: "object", properties: {} },
  tools: ["git.git_diff"],
};

function run() {
  return { lines: host.tool("git.git_diff").split("\n").length };
}
`

func contractScripts() map[string]string {
	return map[string]string{
		"diff_surgery.js":     scriptDiffSurgery,
		"status_count.js":     scriptStatusCount,
		"status_surgery.js":   scriptStatusSurgery,
		"read_twice.js":       scriptReadTwice,
		"undeclared_reach.js": scriptUndeclared,
		"declares_nothing.js": scriptDeclaresNothing,
		"raw_escape.js":       scriptRawEscape,
	}
}

// initRepo turns the harness workspace into a git repository with one commit, a
// modified tracked file and an untracked one — the exact tree shape that
// produced the wrong answer in the wild: 1 modified, 1 untracked, 0 staged.
//
// Built with go-git, the same library the tools use, so the test needs no git
// binary on PATH and no commit identity from the machine running it.
func initRepo(t *testing.T, root string) {
	t.Helper()
	repo, err := git.PlainInitWithOptions(root, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one\n"), 0o644))
	_, err = wt.Add("tracked.txt")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{Author: &object.Signature{
		Name:  "Test Author",
		Email: "test@example.com",
		When:  time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one\ntwo\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "brand_new.txt"), []byte("new\n"), 0o644))
}

// TestSystem_Goja_AProseResultCannotBeMisparsedSilently is the regression test
// for case (a). Both halves are asserted from the same tree, because the point
// is not that one script works — it is that the WRONG script can no longer
// produce an answer.
func TestSystem_Goja_AProseResultCannotBeMisparsedSilently(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, contractScripts(), nil)
	initRepo(t, h.root)
	ctx := context.Background()

	t.Run("git_status hands a program fields, not prose", func(t *testing.T) {
		h.sink.drain()
		out, err := h.call(ctx, "status_count", nil)
		require.NoError(t, err)
		_, value := decodeGojaResult(t, out)
		m, ok := value.(map[string]any)
		require.Truef(t, ok, "value is %T: %#v", value, value)

		// The numbers the wild failure got wrong. One modified tracked file, one
		// untracked file, nothing staged.
		require.Equal(t, float64(0), m["staged"], "nothing is staged in this tree")
		require.Equal(t, float64(1), m["unstaged"], "tracked.txt is modified")
		require.Equal(t, float64(2), m["untracked"], "brand_new.txt and the harness's README.md are untracked")
		require.Equal(t, []any{"README.md", "brand_new.txt"}, m["untracked_names"],
			"the untracked list is data, in a stable order — not a paragraph to count lines in")
		require.Equal(t, []any{"M"}, m["unstaged_codes"], "the git status code travels as data, not as a column of text")
		require.Equal(t, "main", m["branch"])
		require.Equal(t, false, m["clean"])

		// The inner call was still evaluated by the envelope: structure is not a
		// bypass. git_status is an allow-tier read, so it must not raise a card.
		require.Contains(t, h.sink.decisions(), "git.git_status=allow")
	})

	t.Run("the confidently-wrong script now fails at the line that guessed", func(t *testing.T) {
		h.sink.drain()
		out, err := h.call(ctx, "status_surgery", nil)
		require.Errorf(t, err, "the mis-parsing script answered anyway: %#v", out)
		msg := err.Error()
		require.Containsf(t, msg, "git.git_status", "the refusal does not name the tool that was mis-read: %s", msg)
		require.Containsf(t, msg, "structured DATA", "the refusal does not say what the value actually is: %s", msg)
		require.Containsf(t, msg, "[object Object]", "the refusal does not name what JS would have silently produced instead: %s", msg)
		require.Contains(t, msg, "recoverable")
	})

	t.Run("a prose tool refuses string surgery by name", func(t *testing.T) {
		// git_diff still answers in text — a diff IS text — so it is the tool
		// that shows the TEXT half of the guard.
		h.sink.drain()
		out, err := h.call(ctx, "diff_surgery", nil)
		require.Errorf(t, err, "string surgery on a prose result answered anyway: %#v", out)
		msg := err.Error()
		require.Containsf(t, msg, "git.git_diff", "the refusal does not name the tool: %s", msg)
		require.Containsf(t, msg, "TEXT", "the refusal does not say the value is text: %s", msg)
		require.Containsf(t, msg, ".text", "the refusal does not name the deliberate repair: %s", msg)
		require.Containsf(t, msg, "raw", "the refusal does not name the escape hatch: %s", msg)
		require.Contains(t, msg, "recoverable")
	})

	t.Run("an author who means to parse prose says so", func(t *testing.T) {
		h.sink.drain()
		out, err := h.call(ctx, "raw_escape", nil)
		require.NoError(t, err)
		_, value := decodeGojaResult(t, out)
		m, ok := value.(map[string]any)
		require.Truef(t, ok, "value is %T", value)
		require.Equal(t, "string", m["kind"], "{raw: true} must hand back the bare string")
		require.Equal(t, "object", m["wrapped_kind"], "without it, the same call is wrapped")
		require.Equal(t, "text", m["wrapped_shape"], "the value declares its own shape; nothing has to be looked up")
		require.Equal(t, true, m["same"], "the wrapper must not alter the text it carries")
		require.Equal(t, "diff --git a/tracked.txt b/tracked.txt", m["first_line"])
	})
}

// TestSystem_Goja_TheUnchangedStubNeverReachesAScript is the regression test for
// case (b).
//
// read_file's session dedup answers a REPEAT read with "File unchanged since
// last read — the content from your earlier read_file call in this conversation
// is still current." That is true of a model, whose earlier read is still in its
// context, and false of a script, which has no conversation and no earlier read.
// The script that met it treated the sentence as the file.
func TestSystem_Goja_TheUnchangedStubNeverReachesAScript(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, contractScripts(), nil)
	ctx := context.Background()

	out, err := h.call(ctx, "read_twice", map[string]string{"path": "README.md"})
	require.NoError(t, err)
	_, value := decodeGojaResult(t, out)
	m, ok := value.(map[string]any)
	require.Truef(t, ok, "value is %T: %#v", value, value)

	require.Equal(t, true, m["same"],
		"the second read of the same file in one session answered with something other than the first: the dedup stub reached the script")
	require.Equal(t, m["first_bytes"], m["second_bytes"])
	require.Equal(t, "# fixture", m["second_head"],
		"the second read answered with a sentence about the read instead of the file")
	require.NotContains(t, fmt.Sprint(m["second_head"]), "File unchanged")
}

// TestSystem_Goja_DeclaredReachIsEnforced is the matrix for the second half of
// the fix: a script says what it reaches, and the sandbox holds it to that.
//
// This is defence in depth, not the policy boundary — the envelope still
// evaluates every call that gets through. What the declaration adds is the one
// thing the envelope cannot give: a statement of reach that exists BEFORE the
// script runs, which is what an approval card for a script tool can show.
func TestSystem_Goja_DeclaredReachIsEnforced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, contractScripts(), nil)
	initRepo(t, h.root)
	ctx := context.Background()

	t.Run("a declared call goes through", func(t *testing.T) {
		_, err := h.call(ctx, "status_count", nil)
		require.NoError(t, err, "a call the script declared must not be refused")
	})

	t.Run("an undeclared call is refused, naming the declaration to add", func(t *testing.T) {
		h.sink.drain()
		out, err := h.call(ctx, "undeclared_reach", nil)
		require.Errorf(t, err, "an undeclared tool call went through: %#v", out)
		msg := err.Error()
		require.Contains(t, msg, "git.git_status")
		require.Containsf(t, msg, "does not declare", "the refusal does not say what the problem is: %s", msg)
		require.Containsf(t, msg, "local_fs.read_file", "the refusal does not show what the script DID declare: %s", msg)
		require.Containsf(t, msg, "undeclared_reach.js", "the refusal does not name the file to edit: %s", msg)
		require.Contains(t, msg, "recoverable")

		// And the refused call never reached the envelope at all: an undeclared
		// tool is stopped before the trip, not after it.
		require.NotContains(t, h.sink.decisions(), "git.git_status=allow")
	})

	t.Run("an empty declaration means nothing at all", func(t *testing.T) {
		_, err := h.call(ctx, "declares_nothing", nil)
		require.Error(t, err, "tools: [] is a declaration that the script reaches nothing")
		require.Contains(t, err.Error(), "local_fs.read_file")
		require.Contains(t, err.Error(), "reaches nothing")
	})

	t.Run("a script with no declaration keeps working", func(t *testing.T) {
		// The backward-compatible case, proved with the ORIGINAL example set:
		// stats_summary and file_outline predate the field.
		h2 := newGojaHarness(t, map[string]string{"file_outline.js": scriptFileOutline, "stats_summary.js": scriptStatsSummary}, nil)
		_, err := h2.call(context.Background(), "stats_summary", map[string]string{"numbers": "1,2,3"})
		require.NoError(t, err)
	})
}

// TestUnit_Goja_TheDeclaredReachIsVisibleToAnApprovalSurface pins what a card
// renderer reads. It is the whole reason the declaration is metadata and not
// just a runtime check: an approval card for a script tool has to answer "what
// will this touch?" BEFORE the script runs, and Toolset.Scripts() is where that
// answer lives.
func TestUnit_Goja_TheDeclaredReachIsVisibleToAnApprovalSurface(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"status_count.js":     scriptStatusCount,
		"declares_nothing.js": scriptDeclaresNothing,
		"stats_summary.js":    scriptStatsSummary, // no `tools` field at all
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}

	ts, err := gojatool.New(gojatool.Config{ScriptDir: dir})
	require.NoError(t, err)
	defer ts.Shutdown()

	byName := map[string]*gojatool.Script{}
	for _, sc := range ts.Scripts() {
		byName[sc.Name] = sc
	}
	require.Len(t, byName, 3)

	declared := byName["status_count"]
	require.True(t, declared.ToolsDeclared)
	require.Equal(t, []string{"git.git_status"}, declared.Tools,
		"a card must be able to say: this script may call git.git_status")

	empty := byName["declares_nothing"]
	require.True(t, empty.ToolsDeclared, "an empty list is a DECLARATION")
	require.Empty(t, empty.Tools)

	// The two that both present as an empty list must be distinguishable, or a
	// card tells an operator the safest possible thing about the least safe case.
	none := byName["stats_summary"]
	require.False(t, none.ToolsDeclared, "a script with no `tools` field must be reported as undeclared, not as empty")
	require.Empty(t, none.Tools)
}
