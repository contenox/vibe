package localtools_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSignature() *object.Signature {
	return &object.Signature{
		Name:  "Test Author",
		Email: "test@example.com",
		When:  time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
}

func newTestRepo(t *testing.T, dir string, files map[string]string) *git.Worktree {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for name, content := range files {
		writeRepoFile(t, dir, name, content)
		_, err := wt.Add(name)
		require.NoError(t, err)
	}
	_, err = wt.Commit("initial commit", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)
	return wt
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func gitExec(t *testing.T, tools taskengine.ToolsRepo, tool string, args map[string]any) (string, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, dt, err := tools.Exec(context.Background(), time.Now(), args, false,
		&taskengine.ToolsCall{Name: "git", ToolName: tool})
	if err != nil {
		return "", err
	}
	require.Equal(t, taskengine.DataTypeString, dt,
		"git tool %s changed its data type; the engine renders DataTypeString with %%v and anything else with json.Marshal", tool)
	switch v := res.(type) {
	case string:
		return v, nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		require.Failf(t, "unrenderable git result",
			"git tool %s returned %T, which is neither a string nor a fmt.Stringer — the engine would render it as a Go struct dump into the model's context", tool, res)
		return "", nil
	}
}

func mustGitExec(t *testing.T, tools taskengine.ToolsRepo, tool string, args map[string]any) string {
	t.Helper()
	out, err := gitExec(t, tools, tool, args)
	require.NoError(t, err, "git tool %s must succeed", tool)
	return out
}

func TestUnit_GitTools_ReadOpsRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{
		"main.go":     "package main\n\nfunc main() {}\n",
		"docs/x.md":   "# docs\n",
		"untouched.t": "same\n",
	})
	writeRepoFile(t, dir, "main.go", "package main\n\nfunc main() { println(\"hi\") }\n")
	writeRepoFile(t, dir, "notes.txt", "scratch\n")

	tools := localtools.NewGitTools(dir)

	status := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, status, "branch ")
	assert.Contains(t, status, "HEAD ")
	assert.Contains(t, status, "initial commit")
	assert.Contains(t, status, "main.go", "a modified tracked file must appear in status")
	assert.Contains(t, status, "untracked")
	assert.Contains(t, status, "notes.txt")
	assert.NotContains(t, status, "untouched.t", "an unchanged file must not be reported")

	diff := mustGitExec(t, tools, "git_diff", nil)
	assert.Contains(t, diff, "diff --git a/main.go b/main.go")
	assert.Contains(t, diff, "--- a/main.go")
	assert.Contains(t, diff, "+++ b/main.go")
	assert.Contains(t, diff, "+func main() { println(\"hi\") }")
	assert.Contains(t, diff, "-func main() {}")
	assert.Contains(t, diff, "notes.txt", "untracked files are named, not diffed")

	scoped := mustGitExec(t, tools, "git_diff", map[string]any{"path": "docs"})
	assert.Contains(t, scoped, "no changes against HEAD under docs")
	assert.NotContains(t, scoped, "diff --git a/main.go")

	logOut := mustGitExec(t, tools, "git_log", nil)
	assert.Contains(t, logOut, "initial commit")
	assert.Contains(t, logOut, "Test Author")

	scopedLog := mustGitExec(t, tools, "git_log", map[string]any{"path": "docs/x.md", "n": 5})
	assert.Contains(t, scopedLog, "initial commit")

	show := mustGitExec(t, tools, "git_show", map[string]any{"ref": "HEAD"})
	assert.Contains(t, show, "commit ")
	assert.Contains(t, show, "test@example.com")
	assert.Contains(t, show, "initial commit")
	assert.Contains(t, show, "(root commit) files:")
	assert.Contains(t, show, "main.go")

	branches := mustGitExec(t, tools, "git_branch_list", nil)
	assert.Contains(t, branches, "*", "the current branch must be marked")

	blame := mustGitExec(t, tools, "git_blame", map[string]any{"path": "docs/x.md"})
	assert.Contains(t, blame, "Test Author")
	assert.Contains(t, blame, "# docs")
}

func TestUnit_GitTools_StatusOnCleanTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})

	out := mustGitExec(t, localtools.NewGitTools(dir), "git_status", nil)
	assert.Contains(t, out, "working tree clean")
}

func TestUnit_GitTools_NoRepositoryIsAnHonestError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	_, err := gitExec(t, localtools.NewGitTools(sub), "git_status", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no git repository")
	assert.Contains(t, err.Error(), "not under version control")
}

// TestUnit_GitTools_RepoOutsideAllowedDirIsRefused asserts the git tools refuse a repository outside the allowed dir, mirroring local_fs's path containment.
func TestUnit_GitTools_RepoOutsideAllowedDirIsRefused(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	newTestRepo(t, outer, map[string]string{"a.txt": "a\n"})
	inner := filepath.Join(outer, "sub")
	require.NoError(t, os.MkdirAll(inner, 0o755))

	tools := localtools.NewGitTools(inner)
	for _, tool := range []string{"git_status", "git_log", "git_branch_list"} {
		_, err := gitExec(t, tools, tool, nil)
		require.Error(t, err, "%s must refuse a repository outside the allowed dir", tool)
		assert.Contains(t, err.Error(), "outside the allowed directory")
	}
}

// TestUnit_GitTools_UnboundedFallsBackToEnclosingRepo asserts that with no declared allowed dir, the enclosing repository is found by walking up, like git does.
func TestUnit_GitTools_UnboundedFallsBackToEnclosingRepo(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	newTestRepo(t, outer, map[string]string{"a.txt": "a\n"})
	inner := filepath.Join(outer, "sub")
	require.NoError(t, os.MkdirAll(inner, 0o755))

	tools := localtools.NewGitToolsWith("", "git", func(context.Context) string { return inner })
	out := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, out, "working tree clean")
}

func TestUnit_GitTools_PathOutsideRepoIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})
	tools := localtools.NewGitTools(dir)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"git_diff", map[string]any{"path": "../escape"}},
		{"git_blame", map[string]any{"path": "../../etc/passwd"}},
		{"git_add", map[string]any{"paths": "../escape"}},
		{"git_restore", map[string]any{"paths": []any{"../escape"}}},
	} {
		_, err := gitExec(t, tools, tc.tool, tc.args)
		require.Error(t, err, "%s must refuse a path outside the repository", tc.tool)
		assert.Contains(t, err.Error(), "outside the repository")
	}
}

func TestUnit_GitTools_CommitRefusesEmptyStaging(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})
	writeRepoFile(t, dir, "a.txt", "a modified\n")

	tools := localtools.NewGitTools(dir)
	_, err := gitExec(t, tools, "git_commit", map[string]any{"message": "nothing staged"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing is staged")
	assert.Contains(t, err.Error(), "git_add")
}

func TestUnit_GitTools_AddThenCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})
	writeRepoFile(t, dir, "a.txt", "a modified\n")
	writeRepoFile(t, dir, "b.txt", "b\n")

	// go-git reads the commit identity from the repository's own config, so the fixture gets one set exactly as `git config user.name` would.
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.User.Name = "Test Author"
	cfg.User.Email = "test@example.com"
	require.NoError(t, repo.SetConfig(cfg))

	tools := localtools.NewGitTools(dir)

	added := mustGitExec(t, tools, "git_add", map[string]any{"paths": []any{"a.txt", "b.txt"}})
	assert.Contains(t, added, "staged")
	assert.Contains(t, added, "a.txt")

	staged := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, staged, "staged for commit")

	out := mustGitExec(t, tools, "git_commit", map[string]any{"message": "second commit\n\nbody"})
	assert.Contains(t, out, "committed ")
	assert.Contains(t, out, "second commit")

	logOut := mustGitExec(t, tools, "git_log", map[string]any{"n": 5})
	assert.Contains(t, logOut, "second commit")
	assert.Contains(t, logOut, "initial commit")

	clean := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, clean, "working tree clean")

	show := mustGitExec(t, tools, "git_show", map[string]any{"ref": "HEAD"})
	assert.Contains(t, show, "second commit")
	assert.Contains(t, show, "a modified")
	assert.NotContains(t, show, "(root commit)")
}

func TestUnit_GitTools_CheckoutBranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})
	tools := localtools.NewGitTools(dir)

	out := mustGitExec(t, tools, "git_checkout_branch", map[string]any{"branch": "feature/x", "create": true})
	assert.Contains(t, out, "created and switched to branch feature/x")

	status := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, status, "branch feature/x")

	branches := mustGitExec(t, tools, "git_branch_list", nil)
	assert.Contains(t, branches, "* feature/x")

	_, err := gitExec(t, tools, "git_checkout_branch", map[string]any{"branch": "feature/x", "create": true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	_, err = gitExec(t, tools, "git_checkout_branch", map[string]any{"branch": "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create=true")
}

func TestUnit_GitTools_RestoreDiscardsAndUnstages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "original\n"})
	tools := localtools.NewGitTools(dir)

	writeRepoFile(t, dir, "a.txt", "changed\n")
	mustGitExec(t, tools, "git_add", map[string]any{"paths": "a.txt"})
	out := mustGitExec(t, tools, "git_restore", map[string]any{"paths": "a.txt", "staged": true})
	assert.Contains(t, out, "unstaged")
	content, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "changed\n", string(content), "staged=true must not touch file contents")

	out = mustGitExec(t, tools, "git_restore", map[string]any{"paths": []any{"a.txt"}})
	assert.Contains(t, out, "discarded")
	content, err = os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original\n", string(content))

	_, err = gitExec(t, tools, "git_restore", map[string]any{"paths": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restoring the whole repository")
}

// TestUnit_GitTools_PathsStringIsOnePath pins that one string is one path for git_add/git_restore, never shell-split.
func TestUnit_GitTools_PathsStringIsOnePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "one\n"})
	writeRepoFile(t, dir, "my notes.txt", "spaced\n")
	tools := localtools.NewGitTools(dir)

	added := mustGitExec(t, tools, "git_add", map[string]any{"paths": "my notes.txt"})
	assert.Contains(t, added, "staged my notes.txt")

	status := mustGitExec(t, tools, "git_status", nil)
	assert.Contains(t, status, "staged for commit")
	assert.Contains(t, status, "my notes.txt")

	out := mustGitExec(t, tools, "git_restore", map[string]any{"paths": "my notes.txt", "staged": true})
	assert.Contains(t, out, "unstaged my notes.txt")
	content, err := os.ReadFile(filepath.Join(dir, "my notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "spaced\n", string(content), "staged=true must not touch file contents")

	_, err = gitExec(t, tools, "git_add", map[string]any{"paths": "a.txt my notes.txt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a.txt my notes.txt")

	_, err = gitExec(t, tools, "git_add", map[string]any{"paths": "   "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paths is required")
}

// TestUnit_GitTools_PathsDeclaredAsStringOrArray pins that the descriptor and published schema both declare paths as the string-or-array union repoRelPaths accepts.
func TestUnit_GitTools_PathsDeclaredAsStringOrArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tools := localtools.NewGitTools(t.TempDir())

	declared, err := tools.GetToolsForToolsByName(ctx, "git")
	require.NoError(t, err)
	byName := map[string]taskengine.Tool{}
	for _, tool := range declared {
		byName[tool.Function.Name] = tool
	}

	docs, err := tools.(schemaRepo).GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.NoError(t, docs["git"].Validate(ctx), "a type union must still render as a valid document")

	for tool, component := range map[string]string{"git_add": "GitAdd", "git_restore": "GitRestore"} {
		params, ok := byName[tool].Function.Parameters.(map[string]any)
		require.Truef(t, ok, "%s: %T", tool, byName[tool].Function.Parameters)
		paths, ok := params["properties"].(map[string]any)["paths"].(map[string]any)
		require.Truef(t, ok, "%s declares no paths property", tool)
		types, ok := paths["type"].([]any)
		require.Truef(t, ok, "%s.paths declares type %v, want a union", tool, paths["type"])
		assert.Containsf(t, types, "string", "%s.paths takes one path", tool)
		assert.Containsf(t, types, "array", "%s.paths takes an array of paths", tool)

		published := docs["git"].Components.Schemas[component+"Request"].Value.Properties["paths"].Value
		require.NotNilf(t, published.Type, "%s.paths is published without a type", tool)
		assert.Containsf(t, []string(*published.Type), "string", "%s.paths loses its string branch", tool)
		assert.Containsf(t, []string(*published.Type), "array", "%s.paths loses its array branch", tool)
		require.NotNilf(t, published.Items, "%s.paths: an array branch needs an item schema to be a valid document", tool)
	}
}

func TestUnit_GitTools_ArgumentContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "a\n"})
	tools := localtools.NewGitTools(dir)

	_, err := gitExec(t, tools, "git_status", map[string]any{"branch": "main"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument")

	_, err = gitExec(t, tools, "git_commit", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message is required")

	_, err = gitExec(t, tools, "git_show", map[string]any{"ref": "no-such-ref"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot resolve")

	_, err = gitExec(t, tools, "git_add", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paths is required")

	_, err = gitExec(t, tools, "git_push", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool git_push", "network ops are deliberately absent in V1")
}

// TestUnit_GitTools_SchemaSurface pins the tool set the envelope's rules are
// written against: a tool renamed here without the seeded policies following it
// would silently fall through to the default action.
func TestUnit_GitTools_SchemaSurface(t *testing.T) {
	t.Parallel()
	tools := localtools.NewGitTools(t.TempDir())
	ctx := context.Background()

	supported, err := tools.Supports(ctx)
	require.NoError(t, err)
	for _, want := range []string{
		"git", "git_status", "git_diff", "git_log", "git_show", "git_branch_list",
		"git_blame", "git_add", "git_commit", "git_checkout_branch", "git_restore",
	} {
		assert.Contains(t, supported, want)
	}

	declared, err := tools.GetToolsForToolsByName(ctx, "git")
	require.NoError(t, err)
	assert.Len(t, declared, 10, "the toolset declares exactly the ten git operations")
	for _, tool := range declared {
		assert.NotEmpty(t, tool.Function.Description, "%s needs a description", tool.Function.Name)
		assert.True(t, strings.HasPrefix(tool.Function.Name, "git_"), "unexpected tool %s", tool.Function.Name)
	}

	one, err := tools.GetToolsForToolsByName(ctx, "git_status")
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "git_status", one[0].Function.Name)

	_, err = tools.GetToolsForToolsByName(ctx, "git_push")
	require.Error(t, err)
}

// TestUnit_GitTools_ModelFacingTextIsUnchangedByStructuredResults asserts the model-facing rendered text is unchanged (golden, not substring) even though git_status/git_log/git_branch_list now also return typed fields for a program.
func TestUnit_GitTools_ModelFacingTextIsUnchangedByStructuredResults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"tracked.txt": "one\n"})
	writeRepoFile(t, dir, "tracked.txt", "one\ntwo\n")
	writeRepoFile(t, dir, "brand_new.txt", "new\n")

	tools := localtools.NewGitTools(dir)
	status := mustGitExec(t, tools, "git_status", nil)

	// Header lines carry a hash and branch name, asserted by shape; everything below is pinned exactly.
	lines := strings.Split(strings.TrimRight(status, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	assert.True(t, strings.HasPrefix(lines[0], "branch "), "first line: %q", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "HEAD "), "second line: %q", lines[1])
	assert.Equal(t, []string{
		"changed but not staged:",
		"  M tracked.txt",
		"untracked:",
		"  brand_new.txt",
	}, lines[2:], "the model-facing status body changed")

	branches := mustGitExec(t, tools, "git_branch_list", nil)
	assert.Regexp(t, `^\* \S+ [0-9a-f]{8}\n$`, branches, "the model-facing branch listing changed")

	logOut := mustGitExec(t, tools, "git_log", nil)
	assert.Regexp(t, `^[0-9a-f]{8} Test Author <test@example\.com> \S+\n    initial commit\n$`, logOut,
		"the model-facing log rendering changed")
}

// TestUnit_GitTools_StructuredResultsCarryTheSameFacts asserts the typed fields a program reads carry the same repository state as the prose, in fields it cannot misread.
func TestUnit_GitTools_StructuredResultsCarryTheSameFacts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	wt := newTestRepo(t, dir, map[string]string{"tracked.txt": "one\n", "staged.txt": "s\n"})
	writeRepoFile(t, dir, "tracked.txt", "one\ntwo\n")
	writeRepoFile(t, dir, "staged.txt", "s2\n")
	_, err := wt.Add("staged.txt")
	require.NoError(t, err)
	writeRepoFile(t, dir, "brand_new.txt", "new\n")

	tools := localtools.NewGitTools(dir)
	res, _, err := tools.Exec(context.Background(), time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "git", ToolName: "git_status"})
	require.NoError(t, err)

	st, ok := res.(localtools.GitStatusResult)
	require.Truef(t, ok, "git_status returned %T; a program cannot read prose", res)
	assert.False(t, st.Clean)
	assert.NotEmpty(t, st.Branch)
	require.NotNil(t, st.Head)
	assert.Equal(t, "initial commit", st.Head.Subject)
	assert.Len(t, st.Head.Hash, 8)

	assert.Equal(t, []localtools.GitStatusEntry{{Path: "staged.txt", Code: "M"}}, st.Staged)
	assert.Equal(t, []localtools.GitStatusEntry{{Path: "tracked.txt", Code: "M"}}, st.Unstaged)
	assert.Equal(t, []string{"brand_new.txt"}, st.Untracked)

	assert.Contains(t, st.String(), "staged for commit:")
	assert.Contains(t, st.String(), "  M staged.txt")

	res, _, err = tools.Exec(context.Background(), time.Now(), map[string]any{"n": 5}, false,
		&taskengine.ToolsCall{Name: "git", ToolName: "git_log"})
	require.NoError(t, err)
	logRes, ok := res.(localtools.GitLogResult)
	require.Truef(t, ok, "git_log returned %T", res)
	require.Len(t, logRes.Commits, 1)
	assert.Equal(t, "initial commit", logRes.Commits[0].Subject)
	assert.Equal(t, "Test Author", logRes.Commits[0].Author)
	assert.Equal(t, "test@example.com", logRes.Commits[0].Email)

	res, _, err = tools.Exec(context.Background(), time.Now(), map[string]any{}, false,
		&taskengine.ToolsCall{Name: "git", ToolName: "git_branch_list"})
	require.NoError(t, err)
	branches, ok := res.(localtools.GitBranchListResult)
	require.Truef(t, ok, "git_branch_list returned %T", res)
	require.Len(t, branches.Branches, 1)
	assert.True(t, branches.Branches[0].Current)
	assert.Equal(t, branches.Current, branches.Branches[0].Name)
}
