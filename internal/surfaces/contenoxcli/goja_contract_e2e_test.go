package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/gojatool"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

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

// TestSystem_Goja_AProseResultCannotBeMisparsedSilently asserts a script parsing git_status's prose result as structured data now gets structured data, and a script that string-surgeries it can no longer produce an answer.
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

		require.Equal(t, float64(0), m["staged"], "nothing is staged in this tree")
		require.Equal(t, float64(1), m["unstaged"], "tracked.txt is modified")
		require.Equal(t, float64(2), m["untracked"], "brand_new.txt and the harness's README.md are untracked")
		require.Equal(t, []any{"README.md", "brand_new.txt"}, m["untracked_names"],
			"the untracked list is data, in a stable order — not a paragraph to count lines in")
		require.Equal(t, []any{"M"}, m["unstaged_codes"], "the git status code travels as data, not as a column of text")
		require.Equal(t, "main", m["branch"])
		require.Equal(t, false, m["clean"])

		// Structure is not a policy bypass: the inner call is still evaluated.
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

// TestSystem_Goja_TheUnchangedStubNeverReachesAScript asserts a script's repeated read_file call never sees the session-dedup stub sentence, since a script has no conversation for that sentence to be true about.
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

// TestSystem_Goja_DeclaredReachIsEnforced asserts a script's declared `tools` list is enforced by the sandbox: undeclared calls are refused, an empty list means no reach, and a script with no declaration at all keeps working.
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

		// An undeclared tool is stopped before the trip, not after it.
		require.NotContains(t, h.sink.decisions(), "git.git_status=allow")
	})

	t.Run("an empty declaration means nothing at all", func(t *testing.T) {
		_, err := h.call(ctx, "declares_nothing", nil)
		require.Error(t, err, "tools: [] is a declaration that the script reaches nothing")
		require.Contains(t, err.Error(), "local_fs.read_file")
		require.Contains(t, err.Error(), "reaches nothing")
	})

	t.Run("a script with no declaration keeps working", func(t *testing.T) {
		h2 := newGojaHarness(t, map[string]string{"file_outline.js": scriptFileOutline, "stats_summary.js": scriptStatsSummary}, nil)
		_, err := h2.call(context.Background(), "stats_summary", map[string]string{"numbers": "1,2,3"})
		require.NoError(t, err)
	})
}

// TestUnit_Goja_TheDeclaredReachIsVisibleToAnApprovalSurface asserts Toolset.Scripts() exposes each script's declared reach (and whether it declared at all) for an approval card to render before the script runs.
func TestUnit_Goja_TheDeclaredReachIsVisibleToAnApprovalSurface(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"status_count.js":     scriptStatusCount,
		"declares_nothing.js": scriptDeclaresNothing,
		"stats_summary.js":    scriptStatsSummary,
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
