package localtools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/getkin/kin-openapi/openapi3"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitToolsName is the registered toolset name the envelope's rules address; the
// native- scope is a namespace, so a declared MCP source cannot mint the same
// key.
const GitToolsName = "native-git"

const (
	gitMaxOutputBytes = 32 * 1024

	gitDefaultLogCount = 10
	gitMaxLogCount     = 200

	gitMaxDiffFiles = 25
)

// GitTools provides read and write access to the workspace git repository, scoped the same way as local_fs: the repository root must lie inside the allowed directory when one is declared, otherwise found by walking up from the process working directory.
type GitTools struct {
	allowedDir  string
	name        string
	cwdResolver func(context.Context) string
}

// NewGitTools creates the git toolset scoped to allowedDir; an empty allowedDir means no declared boundary, so the repository is located from the process working directory.
func NewGitTools(allowedDir string) taskengine.ToolsRepo {
	return NewGitToolsWith(allowedDir, GitToolsName, nil)
}

// NewGitToolsWith is NewGitTools with the toolset name and a per-call working
// directory resolver, mirroring NewLocalFSToolsWith for surfaces (ACP) whose cwd
// is a property of the session rather than of the process.
func NewGitToolsWith(allowedDir, name string, cwdResolver func(context.Context) string) taskengine.ToolsRepo {
	cleaned := allowedDir
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
	}
	if name == "" {
		name = GitToolsName
	}
	return &GitTools{allowedDir: cleaned, name: name, cwdResolver: cwdResolver}
}

// Exec stamps every returned error with a fatal-vs-recoverable severity marker,
// exactly as LocalFSTools.Exec does, so a model can tell "fix your arguments and
// retry" from "this environment cannot do it".
func (h *GitTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := h.execDispatch(ctx, input, toolsCall)
	return res, dt, markSeverity(err)
}

func (h *GitTools) execDispatch(ctx context.Context, input any, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if toolsCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("git: tools required")
	}

	args, ok := input.(map[string]any)
	if !ok {
		// Declarative `tools` tasks carry arguments on the ToolsCall, the same fallback local_fs takes for a gated tool task.
		if len(toolsCall.Args) > 0 {
			args = make(map[string]any, len(toolsCall.Args))
			for k, v := range toolsCall.Args {
				args[k] = v
			}
		} else {
			args = map[string]any{}
		}
	}

	toolName := toolsCall.ToolName
	if toolName == "" {
		toolName = toolsCall.Name
	}

	switch toolName {
	case "git_status":
		if err := rejectUnknownArgs(toolName, args); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.status(ctx)
	case "git_diff":
		if err := rejectUnknownArgs(toolName, args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.diff(ctx, args)
	case "git_log":
		if err := rejectUnknownArgs(toolName, args, "n", "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.log(ctx, args)
	case "git_show":
		if err := rejectUnknownArgs(toolName, args, "ref"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.show(ctx, args)
	case "git_branch_list":
		if err := rejectUnknownArgs(toolName, args); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.branchList(ctx)
	case "git_blame":
		if err := rejectUnknownArgs(toolName, args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.blame(ctx, args)
	case "git_add":
		if err := rejectUnknownArgs(toolName, args, "paths"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.add(ctx, args)
	case "git_commit":
		if err := rejectUnknownArgs(toolName, args, "message"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.commit(ctx, args)
	case "git_checkout_branch":
		if err := rejectUnknownArgs(toolName, args, "branch", "create"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.checkoutBranch(ctx, args)
	case "git_restore":
		if err := rejectUnknownArgs(toolName, args, "paths", "staged"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.restore(ctx, args)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("git: unknown tool %s", toolName)
	}
}

func (h *GitTools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

func (h *GitTools) cwd(ctx context.Context) string {
	if root := vfs.SessionCwdFromContext(ctx); root != "" {
		return filepath.Clean(root)
	}
	if h.cwdResolver != nil {
		if r := strings.TrimSpace(h.cwdResolver(ctx)); r != "" {
			return filepath.Clean(r)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

func (h *GitTools) baseDir(ctx context.Context) (dir string, bounded bool, err error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args["_allowed_dir"]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if !filepath.IsAbs(cleaned) {
				if wd := h.cwd(ctx); wd != "" {
					cleaned = filepath.Clean(filepath.Join(wd, cleaned))
				}
			}
			return cleaned, true, nil
		}
	}
	if h.allowedDir != "" {
		return h.allowedDir, true, nil
	}
	if wd := h.cwd(ctx); wd != "" {
		return wd, false, nil
	}
	return "", false, errors.New("git: no working directory could be resolved")
}

func findRepoRoot(start string) (string, bool) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func (h *GitTools) openRepo(ctx context.Context, tool string) (*git.Repository, string, error) {
	base, bounded, err := h.baseDir(ctx)
	if err != nil {
		return nil, "", err
	}
	start, err := vfs.ResolveRoot(base)
	if err != nil {
		return nil, "", fmt.Errorf("%s: invalid allowed dir: %w", tool, err)
	}
	root, found := findRepoRoot(start)
	if !found {
		return nil, "", recoverablef("%s: no git repository at %s or any parent directory — this workspace is not under version control", tool, start)
	}
	if bounded && !vfs.Within(start, root) {
		return nil, "", recoverablef(
			"%s: the git repository at %s lies outside the allowed directory %s — start the agent at the repository root (or point the allowed dir at it) to use the git tools here",
			tool, root, start)
	}
	if err := containGitDir(tool, start, bounded, root); err != nil {
		return nil, "", err
	}
	repo, err := git.PlainOpenWithOptions(root, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return nil, "", recoverablef("%s: cannot open the git repository at %s: %v", tool, root, err)
	}
	return repo, root, nil
}

// containGitDir routes the repository's on-disk git directory — root/.git, or wherever a .git symlink or a .git FILE's `gitdir:` pointer redirects it — through the same vfs containment local_fs uses: it must never resolve into the runtime control plane, and (when a boundary is declared) must not escape it. This is the seam that stops a planted `.git` file pointing at the control plane from turning a git tool into a read/write handle on the runtime's own state.
func containGitDir(tool, start string, bounded bool, root string) error {
	gitdir, err := effectiveGitDir(root)
	if err != nil {
		return recoverablef("%s: cannot resolve the git directory of %s: %v", tool, root, err)
	}
	if bounded {
		if _, err := vfs.Contain(start, gitdir); err != nil {
			switch {
			case errors.Is(err, vfs.ErrControlPlane):
				return recoverablef("%s: the git directory of the repository at %s resolves inside the runtime control plane — refused", tool, root)
			case errors.Is(err, vfs.ErrEscape):
				return recoverablef("%s: the git directory of the repository at %s escapes the allowed directory %s (a .git gitdir: pointer or symlink redirects it there) — refused", tool, root, start)
			default:
				return recoverablef("%s: %v", tool, err)
			}
		}
		return nil
	}
	if denied, ok := vfs.IsControlPlanePath(gitdir); ok {
		return recoverablef("%s: the git directory of the repository at %s resolves inside the runtime control plane (%s) — refused", tool, root, denied)
	}
	return nil
}

// effectiveGitDir returns the path the repository's git directory lives at: root/.git for a normal repo (a directory, or a symlink vfs.Contain follows on its own), or the target a .git FILE redirects to via its `gitdir:` line (worktrees, submodules).
func effectiveGitDir(root string) (string, error) {
	dotgit := filepath.Join(root, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return "", err
	}
	if fi.Mode().IsRegular() {
		return parseGitdirFile(root, dotgit)
	}
	return dotgit, nil
}

func parseGitdirFile(root, dotgit string) (string, error) {
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(string(data), "\n") {
		target, ok := strings.CutPrefix(strings.TrimSpace(ln), "gitdir:")
		if !ok {
			continue
		}
		target = strings.TrimSpace(target)
		if target == "" {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		return filepath.Clean(target), nil
	}
	return "", errors.New(".git file carries no gitdir: pointer")
}

func repoRelPath(root, tool, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return "", nil
	}
	abs, err := vfs.Contain(root, p)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			return "", recoverablef("%s: path %s is outside the repository %s", tool, p, root)
		}
		return "", recoverablef("%s: %v", tool, err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", recoverablef("%s: path %s cannot be resolved inside the repository %s", tool, p, root)
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

func repoRelPaths(root, tool string, raw any) ([]string, error) {
	list, err := pathListArg(tool, "paths", raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		rel, err := repoRelPath(root, tool, p)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			// Empty entry means "the whole repository" — kept explicit so callers can decide what "everything" means for their operation.
			out = append(out, "")
			continue
		}
		out = append(out, rel)
	}
	if len(out) == 0 {
		return nil, recoverablef("%s: paths is required (a path, or an array of paths, relative to the repository root)", tool)
	}
	return out, nil
}

func truncateGitOutput(tool, s, hint string) string {
	if len(s) <= gitMaxOutputBytes {
		return s
	}
	cut := s[:gitMaxOutputBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	return cut + fmt.Sprintf("... (%s truncated at %d bytes — %s)\n", tool, gitMaxOutputBytes, hint)
}

func headCommit(repo *git.Repository) (c *object.Commit, ok bool, err error) {
	ref, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, false, err
	}
	return commit, true, nil
}

func shortHash(h plumbing.Hash) string {
	s := h.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func subjectOf(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

func currentBranch(repo *git.Repository) string {
	ref, err := repo.Head()
	if err != nil {
		return "(no commits yet)"
	}
	if ref.Name().IsBranch() {
		return ref.Name().Short()
	}
	return "(detached at " + shortHash(ref.Hash()) + ")"
}

// AppendGuidance lets a typed result carry text appended by a decorator (services/toolguidance) without losing its structure, since the decorator otherwise only appends to string results; the method name alone is the contract, so neither package imports the other.
func (r GitStatusResult) AppendGuidance(text string) any {
	r.text += text
	return r
}

func (r GitLogResult) AppendGuidance(text string) any {
	r.text += text
	return r
}

func (r GitBranchListResult) AppendGuidance(text string) any {
	r.text += text
	return r
}

// GitCommitRef is a commit reduced to what a status line shows.
type GitCommitRef struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// GitStatusEntry is one path with the single-letter git status code that applies
// to it — 'M', 'A', 'D', 'R', '?', the codes `git status --short` prints.
type GitStatusEntry struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// GitStatusResult is git_status: Staged, Unstaged and Untracked are the three answer lists, Clean is true when all are empty, and the lists stay complete even when the rendered text is truncated.
type GitStatusResult struct {
	Branch    string           `json:"branch"`
	Head      *GitCommitRef    `json:"head,omitempty"`
	Clean     bool             `json:"clean"`
	Staged    []GitStatusEntry `json:"staged"`
	Unstaged  []GitStatusEntry `json:"unstaged"`
	Untracked []string         `json:"untracked"`

	text string
}

func (r GitStatusResult) String() string { return r.text }

// GitLogEntry is one commit as git_log renders it.
type GitLogEntry struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// GitLogResult is git_log.
type GitLogResult struct {
	Commits []GitLogEntry `json:"commits"`

	text string
}

func (r GitLogResult) String() string { return r.text }

// GitBranch is one local branch.
type GitBranch struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`
	Current bool   `json:"current"`
}

// GitBranchListResult is git_branch_list.
type GitBranchListResult struct {
	Current  string      `json:"current"`
	Branches []GitBranch `json:"branches"`

	text string
}

func (r GitBranchListResult) String() string { return r.text }

func (h *GitTools) status(ctx context.Context) (any, taskengine.DataType, error) {
	const tool = "git_status"
	repo, _, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	st, err := wt.Status()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}

	out := GitStatusResult{
		Branch:    currentBranch(repo),
		Staged:    []GitStatusEntry{},
		Unstaged:  []GitStatusEntry{},
		Untracked: []string{},
	}
	var staged, unstaged, untracked []string
	for _, path := range sortedStatusPaths(st) {
		fs := st[path]
		if fs.Staging == git.Untracked && fs.Worktree == git.Untracked {
			untracked = append(untracked, "  "+path)
			out.Untracked = append(out.Untracked, path)
			continue
		}
		if fs.Staging != git.Unmodified && fs.Staging != git.Untracked {
			staged = append(staged, fmt.Sprintf("  %c %s", fs.Staging, path))
			out.Staged = append(out.Staged, GitStatusEntry{Path: path, Code: string(fs.Staging)})
		}
		if fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked {
			unstaged = append(unstaged, fmt.Sprintf("  %c %s", fs.Worktree, path))
			out.Unstaged = append(out.Unstaged, GitStatusEntry{Path: path, Code: string(fs.Worktree)})
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "branch %s\n", out.Branch)
	if commit, ok, cErr := headCommit(repo); cErr == nil && ok {
		out.Head = &GitCommitRef{Hash: shortHash(commit.Hash), Subject: subjectOf(commit.Message)}
		fmt.Fprintf(&sb, "HEAD %s %s\n", out.Head.Hash, out.Head.Subject)
	}
	if len(staged)+len(unstaged)+len(untracked) == 0 {
		sb.WriteString("working tree clean\n")
		out.Clean = true
		out.text = sb.String()
		return out, taskengine.DataTypeString, nil
	}
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&sb, "%s:\n%s\n", title, strings.Join(lines, "\n"))
	}
	section("staged for commit", staged)
	section("changed but not staged", unstaged)
	section("untracked", untracked)
	out.text = truncateGitOutput(tool, sb.String(), "narrow the workspace or commit some of it")
	return out, taskengine.DataTypeString, nil
}

func sortedStatusPaths(st git.Status) []string {
	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func (h *GitTools) diff(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_diff"
	repo, root, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	only := ""
	if raw, ok := argString(args, "path"); ok {
		if only, err = repoRelPath(root, tool, raw); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	st, err := wt.Status()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	head, hasHead, err := headCommit(repo)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}

	var changed, untracked []string
	for _, path := range sortedStatusPaths(st) {
		if only != "" && path != only && !strings.HasPrefix(path, only+"/") {
			continue
		}
		fs := st[path]
		if fs.Staging == git.Untracked && fs.Worktree == git.Untracked {
			untracked = append(untracked, path)
			continue
		}
		changed = append(changed, path)
	}

	var sb strings.Builder
	if len(changed) == 0 {
		if only != "" {
			fmt.Fprintf(&sb, "no changes against HEAD under %s\n", only)
		} else {
			sb.WriteString("no changes against HEAD\n")
		}
	}
	overflow := changed
	if len(changed) > gitMaxDiffFiles {
		overflow = changed[gitMaxDiffFiles:]
		changed = changed[:gitMaxDiffFiles]
	} else {
		overflow = nil
	}
	for _, path := range changed {
		oldContent := ""
		if hasHead {
			if f, fErr := head.File(path); fErr == nil {
				if c, cErr := f.Contents(); cErr == nil {
					oldContent = c
				}
			}
		}
		newContent := ""
		if data, rErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))); rErr == nil {
			newContent = string(data)
		}
		if isBinarySample(sniffPrefix([]byte(oldContent))) || isBinarySample(sniffPrefix([]byte(newContent))) {
			fmt.Fprintf(&sb, "diff --git a/%s b/%s\nBinary files differ\n", path, path)
			continue
		}
		fmt.Fprintf(&sb, "diff --git a/%s b/%s\n%s", path, path, gitFileDiff(path, oldContent, newContent))
	}
	if len(overflow) > 0 {
		fmt.Fprintf(&sb, "... %d more changed file(s) not shown: %s\n", len(overflow), strings.Join(overflow, ", "))
	}
	if len(untracked) > 0 {
		fmt.Fprintf(&sb, "untracked (no diff — not in the repository yet): %s\n", strings.Join(untracked, ", "))
	}
	return truncateGitOutput(tool, sb.String(), "pass path to diff one file or directory"), taskengine.DataTypeString, nil
}

func gitFileDiff(path, oldContent, newContent string) string {
	body := unifiedDiff(path, oldContent, newContent)
	if body == "(no changes)" {
		return ""
	}
	if i := strings.Index(body, "@@"); i >= 0 {
		body = body[i:]
	}
	return fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", path, path, body)
}

func (h *GitTools) log(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_log"
	repo, root, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	n := gitDefaultLogCount
	if v, ok := argInt(args, "n"); ok && v > 0 {
		n = v
	}
	if n > gitMaxLogCount {
		n = gitMaxLogCount
	}
	opts := &git.LogOptions{Order: git.LogOrderCommitterTime}
	if raw, ok := argString(args, "path"); ok {
		rel, relErr := repoRelPath(root, tool, raw)
		if relErr != nil {
			return nil, taskengine.DataTypeAny, relErr
		}
		if rel != "" {
			prefix := rel + "/"
			opts.PathFilter = func(p string) bool { return p == rel || strings.HasPrefix(p, prefix) }
		}
	}
	iter, err := repo.Log(opts)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return GitLogResult{Commits: []GitLogEntry{}, text: "no commits yet\n"}, taskengine.DataTypeString, nil
		}
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	defer iter.Close()

	out := GitLogResult{Commits: []GitLogEntry{}}
	var sb strings.Builder
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if count >= n {
			// go-git's ForEach stops on any non-nil error; this is the stop signal, not a real failure.
			return storerStop
		}
		count++
		entry := GitLogEntry{
			Hash:    shortHash(c.Hash),
			Author:  c.Author.Name,
			Email:   c.Author.Email,
			Date:    c.Author.When.Format(time.RFC3339),
			Subject: subjectOf(c.Message),
		}
		out.Commits = append(out.Commits, entry)
		fmt.Fprintf(&sb, "%s %s <%s> %s\n    %s\n",
			entry.Hash, entry.Author, entry.Email, entry.Date, entry.Subject)
		return nil
	})
	if err != nil && !errors.Is(err, storerStop) {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	if count == 0 {
		return GitLogResult{Commits: []GitLogEntry{}, text: "no commits match\n"}, taskengine.DataTypeString, nil
	}
	out.text = truncateGitOutput(tool, sb.String(), "ask for fewer commits with n")
	return out, taskengine.DataTypeString, nil
}

var storerStop = errors.New("stop iteration")

func (h *GitTools) show(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_show"
	ref, ok := argString(args, "ref")
	if !ok || strings.TrimSpace(ref) == "" {
		return nil, taskengine.DataTypeAny, recoverablef("%s: ref is required (a commit hash, branch, tag, or HEAD)", tool)
	}
	ref = strings.TrimSpace(ref)
	repo, _, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: cannot resolve %q in this repository — pass a commit hash, branch, tag, or HEAD", tool, ref)
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %q does not name a commit: %v", tool, ref, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "commit %s\nAuthor: %s <%s>\nDate:   %s\n\n%s\n\n",
		commit.Hash.String(), commit.Author.Name, commit.Author.Email,
		commit.Author.When.Format(time.RFC3339), strings.TrimSpace(commit.Message))

	if commit.NumParents() == 0 {
		files, fErr := commit.Files()
		if fErr == nil {
			sb.WriteString("(root commit) files:\n")
			_ = files.ForEach(func(f *object.File) error {
				fmt.Fprintf(&sb, "  %s\n", f.Name)
				return nil
			})
		}
		return truncateGitOutput(tool, sb.String(), "read individual files with local_fs"), taskengine.DataTypeString, nil
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	patch, err := parent.PatchContext(ctx, commit)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	sb.WriteString(patch.String())
	return truncateGitOutput(tool, sb.String(), "show a narrower ref, or read files with local_fs"), taskengine.DataTypeString, nil
}

func (h *GitTools) branchList(ctx context.Context) (any, taskengine.DataType, error) {
	const tool = "git_branch_list"
	repo, _, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	current := currentBranch(repo)
	iter, err := repo.Branches()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	defer iter.Close()

	out := GitBranchListResult{Current: current, Branches: []GitBranch{}}
	var names []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		marker := "  "
		if name == current {
			marker = "* "
		}
		out.Branches = append(out.Branches, GitBranch{Name: name, Hash: shortHash(ref.Hash()), Current: name == current})
		names = append(names, fmt.Sprintf("%s%s %s", marker, name, shortHash(ref.Hash())))
		return nil
	})
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	if len(names) == 0 {
		return GitBranchListResult{Current: current, Branches: []GitBranch{}, text: "no branches yet (the repository has no commits)\n"}, taskengine.DataTypeString, nil
	}
	sort.Strings(names)
	sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })
	out.text = truncateGitOutput(tool, strings.Join(names, "\n")+"\n", "the repository has many branches")
	return out, taskengine.DataTypeString, nil
}

func (h *GitTools) blame(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_blame"
	raw, ok := argString(args, "path")
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, taskengine.DataTypeAny, recoverablef("%s: path is required (a tracked file, relative to the repository root)", tool)
	}
	repo, root, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	rel, err := repoRelPath(root, tool, raw)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if rel == "" {
		return nil, taskengine.DataTypeAny, recoverablef("%s: path must name a file, not the repository root", tool)
	}
	commit, ok, err := headCommit(repo)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	if !ok {
		return nil, taskengine.DataTypeAny, recoverablef("%s: the repository has no commits yet, so %s has no history", tool, rel)
	}
	result, err := git.Blame(commit, rel)
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: cannot blame %s — is it tracked in this repository? (%v)", tool, rel, err)
	}
	var sb strings.Builder
	for i, line := range result.Lines {
		fmt.Fprintf(&sb, "%s %-20s %4d) %s\n", shortHash(line.Hash), truncateField(line.AuthorName, 20), i+1, line.Text)
	}
	return truncateGitOutput(tool, sb.String(), "blame a smaller file, or read it with local_fs"), taskengine.DataTypeString, nil
}

func truncateField(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (h *GitTools) add(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_add"
	raw, ok := args["paths"]
	if !ok {
		return nil, taskengine.DataTypeAny, recoverablef("%s: paths is required (a path, or an array of paths, relative to the repository root)", tool)
	}
	repo, root, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	rels, err := repoRelPaths(root, tool, raw)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	var staged []string
	for _, rel := range rels {
		opts := &git.AddOptions{Path: rel}
		if rel == "" {
			opts = &git.AddOptions{All: true}
		}
		if err := wt.AddWithOptions(opts); err != nil {
			return nil, taskengine.DataTypeAny, recoverablef("%s: cannot stage %s: %v", tool, displayRel(rel), err)
		}
		staged = append(staged, displayRel(rel))
	}
	return fmt.Sprintf("staged %s\n", strings.Join(staged, ", ")), taskengine.DataTypeString, nil
}

func displayRel(rel string) string {
	if rel == "" {
		return "everything"
	}
	return rel
}

func (h *GitTools) commit(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_commit"
	message, ok := argString(args, "message")
	if !ok || strings.TrimSpace(message) == "" {
		return nil, taskengine.DataTypeAny, recoverablef("%s: message is required", tool)
	}
	repo, _, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	st, err := wt.Status()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	// Refuses an empty staging area rather than minting an empty commit: nobody staged anything is always a mistake.
	staged := 0
	for _, path := range sortedStatusPaths(st) {
		if fs := st[path]; fs.Staging != git.Unmodified && fs.Staging != git.Untracked {
			staged++
		}
	}
	if staged == 0 {
		return nil, taskengine.DataTypeAny, recoverablef("%s: nothing is staged — call git_add with the paths to include, then commit", tool)
	}

	hash, err := wt.Commit(strings.TrimSpace(message), &git.CommitOptions{})
	if err != nil {
		if errors.Is(err, git.ErrMissingAuthor) {
			return nil, taskengine.DataTypeAny, recoverablef("%s: this repository has no commit identity — set user.name and user.email in its git config first", tool)
		}
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	return fmt.Sprintf("committed %s on %s: %s (%d file(s) staged)\n",
		shortHash(hash), currentBranch(repo), subjectOf(message), staged), taskengine.DataTypeString, nil
}

func (h *GitTools) checkoutBranch(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_checkout_branch"
	branch, ok := argString(args, "branch")
	if !ok || strings.TrimSpace(branch) == "" {
		return nil, taskengine.DataTypeAny, recoverablef("%s: branch is required", tool)
	}
	branch = strings.TrimSpace(branch)
	create, _ := argBool(args, "create")

	repo, _, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	err = wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: create})
	if err != nil {
		switch {
		case errors.Is(err, git.ErrBranchExists):
			return nil, taskengine.DataTypeAny, recoverablef("%s: branch %s already exists — call it again with create=false to switch to it", tool, branch)
		case errors.Is(err, plumbing.ErrReferenceNotFound):
			return nil, taskengine.DataTypeAny, recoverablef("%s: branch %s does not exist — call it again with create=true to create it", tool, branch)
		default:
			return nil, taskengine.DataTypeAny, recoverablef("%s: cannot switch to %s: %v", tool, branch, err)
		}
	}
	verb := "switched to"
	if create {
		verb = "created and switched to"
	}
	return fmt.Sprintf("%s branch %s\n", verb, branch), taskengine.DataTypeString, nil
}

func (h *GitTools) restore(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	const tool = "git_restore"
	raw, ok := args["paths"]
	if !ok {
		return nil, taskengine.DataTypeAny, recoverablef("%s: paths is required (a path, or an array of paths, relative to the repository root)", tool)
	}
	stagedOnly, _ := argBool(args, "staged")

	repo, root, err := h.openRepo(ctx, tool)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	rels, err := repoRelPaths(root, tool, raw)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	for _, rel := range rels {
		if rel == "" {
			return nil, taskengine.DataTypeAny, recoverablef("%s: name the files to restore — restoring the whole repository at once is refused", tool)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: %v", tool, err)
	}
	// go-git refuses a worktree-only restore, so discarding changes is expressed as index+worktree back to HEAD (same as `git restore <path>` after `git add`).
	opts := &git.RestoreOptions{Files: rels, Staged: true, Worktree: !stagedOnly}
	if err := wt.Restore(opts); err != nil {
		return nil, taskengine.DataTypeAny, recoverablef("%s: cannot restore %s: %v", tool, strings.Join(rels, ", "), err)
	}
	if stagedOnly {
		return fmt.Sprintf("unstaged %s (file contents left alone)\n", strings.Join(rels, ", ")), taskengine.DataTypeString, nil
	}
	return fmt.Sprintf("restored %s to HEAD (uncommitted changes discarded)\n", strings.Join(rels, ", ")), taskengine.DataTypeString, nil
}

// Supports advertises the scoped toolset name alone: a bare git_* verb here would be its own allowlist entry, separately addressable and surviving "!native-git", so the verbs could no longer be granted or removed as one set.
func (h *GitTools) Supports(ctx context.Context) ([]string, error) {
	return []string{h.name}, nil
}

func gitSchemaSpecs() []toolSchemaSpec {
	return []toolSchemaSpec{
		{tool: "git_status", component: "GitStatus", response: gitStatusResponse},
		{tool: "git_diff", component: "GitDiff", response: gitDiffResponse},
		{tool: "git_log", component: "GitLog", response: gitLogResponse},
		{tool: "git_show", component: "GitShow", response: gitShowResponse},
		{tool: "git_branch_list", component: "GitBranchList", response: gitBranchListResponse},
		{tool: "git_blame", component: "GitBlame", response: gitBlameResponse},
		{tool: "git_add", component: "GitAdd", response: gitAddResponse},
		{tool: "git_commit", component: "GitCommit", response: gitCommitResponse},
		{tool: "git_checkout_branch", component: "GitCheckoutBranch", response: gitCheckoutBranchResponse},
		{tool: "git_restore", component: "GitRestore", response: gitRestoreResponse},
	}
}

// GetSchemasForSupportedTools publishes one OpenAPI 3.1 request/response pair per declared tool, converted from the descriptors GetToolsForToolsByName hands the model; status/log/branch_list declare a typed object, the rest answer with text.
func (h *GitTools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	declared, err := h.GetToolsForToolsByName(ctx, h.name)
	if err != nil {
		return nil, err
	}
	doc, err := buildToolsetDoc(h.name, "Git Tools",
		"Repository operations run in-process against the workspace repository — no git binary, no shell quoting — so reads and mutations can be gated separately. Network operations (push, pull, fetch, clone) are deliberately absent; reach them through local_shell. Every result is capped at 32 KiB and says what it withheld.",
		declared, gitSchemaSpecs())
	if err != nil {
		return nil, err
	}
	return map[string]*openapi3.T{h.name: doc}, nil
}

func gitStatusEntrySchema() *openapi3.SchemaRef {
	return objectSchema("One changed path.", map[string]*openapi3.SchemaRef{
		"path": strSchema("Path relative to the repository root."),
		"code": strSchema("The single-letter status code `git status --short` prints for it — 'M', 'A', 'D', 'R' and friends, taken verbatim from go-git."),
	}, "path", "code")
}

func gitStatusResponse() *openapi3.SchemaRef {
	return objectSchema(
		"GitStatusResult. The lists are complete even when the rendered text was truncated: truncation bounds the reader's context, not the repository. Reaches the model as its rendered text.",
		map[string]*openapi3.SchemaRef{
			"branch": strSchema("The current branch."),
			"head": objectSchema("The commit HEAD points at. Absent in a repository with no commits yet.",
				map[string]*openapi3.SchemaRef{
					"hash":    strSchema("Short commit hash."),
					"subject": strSchema("First line of the commit message."),
				}, "hash", "subject"),
			"clean":     boolSchema("True when nothing is staged, changed or untracked."),
			"staged":    arraySchema("Paths staged for the next commit. Empty when none are.", gitStatusEntrySchema()),
			"unstaged":  arraySchema("Paths changed in the worktree but not staged. Empty when none are.", gitStatusEntrySchema()),
			"untracked": arraySchema("Paths the repository does not track. Empty when none are.", strSchema("Path relative to the repository root.")),
		}, "branch", "clean", "staged", "unstaged", "untracked")
}

func gitDiffResponse() *openapi3.SchemaRef {
	return strSchema("The worktree against HEAD as a unified diff — staged and unstaged together — with a `diff --git a/<path> b/<path>` header per file and \"Binary files differ\" in place of a binary file's body. \"no changes against HEAD\" (or \"… under <path>\") when there is nothing to show. Past 25 changed files the remainder is named but not rendered; untracked paths are listed at the end without a diff, since they are not in the repository yet. Truncated at 32 KiB with a notice naming what to narrow.")
}

func gitLogEntrySchema() *openapi3.SchemaRef {
	return objectSchema("One commit.", map[string]*openapi3.SchemaRef{
		"hash":    strSchema("Short commit hash."),
		"author":  strSchema("Author name."),
		"email":   strSchema("Author email."),
		"date":    strSchema("Author date, RFC3339."),
		"subject": strSchema("First line of the commit message."),
	}, "hash", "author", "email", "date", "subject")
}

func gitLogResponse() *openapi3.SchemaRef {
	return objectSchema(
		"GitLogResult. Reaches the model as its rendered text, one commit per two lines.",
		map[string]*openapi3.SchemaRef{
			"commits": arraySchema("The commits, newest first. Empty — with the text \"no commits yet\" or \"no commits match\" — when the repository has no history or nothing touched the given path.", gitLogEntrySchema()),
		}, "commits")
}

func gitShowResponse() *openapi3.SchemaRef {
	return strSchema("The commit: full hash, author, RFC3339 date and message, then its patch against the first parent. A root commit has no parent, so it lists its files instead of a patch. Truncated at 32 KiB with a notice naming what to narrow.")
}

func gitBranchListResponse() *openapi3.SchemaRef {
	return objectSchema(
		"GitBranchListResult, local branches only. Reaches the model as its rendered text, with the current branch marked '*'.",
		map[string]*openapi3.SchemaRef{
			"current": strSchema("The current branch."),
			"branches": arraySchema("The local branches, sorted by name. Empty — with the text \"no branches yet\" — in a repository with no commits.",
				objectSchema("One local branch.", map[string]*openapi3.SchemaRef{
					"name":    strSchema("Branch name."),
					"hash":    strSchema("Short hash of the branch head."),
					"current": boolSchema("Whether this is the checked-out branch."),
				}, "name", "hash", "current")),
		}, "current", "branches")
}

func gitBlameResponse() *openapi3.SchemaRef {
	return strSchema("Per-line authorship at HEAD, one line each: short commit hash, author name (clipped to 20 characters), the 1-based line number, and the line's text. Truncated at 32 KiB with a notice naming what to narrow.")
}

func gitAddResponse() *openapi3.SchemaRef {
	return strSchema("\"staged <paths>\" naming what was staged; a path of \".\" is reported as \"everything\".")
}

func gitCommitResponse() *openapi3.SchemaRef {
	return strSchema("\"committed <short hash> on <branch>: <subject> (N file(s) staged)\". Committing with nothing staged is an error, not an empty commit.")
}

func gitCheckoutBranchResponse() *openapi3.SchemaRef {
	return strSchema("\"switched to branch <name>\", or \"created and switched to branch <name>\" when create was set.")
}

func gitRestoreResponse() *openapi3.SchemaRef {
	return strSchema("\"unstaged <paths> (file contents left alone)\" when staged was set, otherwise \"restored <paths> to HEAD (uncommitted changes discarded)\" — the discarded changes are not recoverable.")
}

func gitPathsProp() map[string]any {
	return map[string]any{
		"type":        []any{"string", "array"},
		"items":       map[string]any{"type": "string"},
		"description": "A path relative to the repository root, or an array of them. One string is one path: it is not split on spaces, so a name containing spaces needs no quoting and two paths need an array.",
	}
}

// GetToolsForToolsByName returns the git tool declarations, kept terse like local_fs's since descriptions are paid on every turn.
func (h *GitTools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	allTools := []taskengine.Tool{
		fsTool("git_status", "Branch, HEAD commit, and what is staged, changed, or untracked in the workspace repository.", map[string]any{}),

		fsTool("git_diff", "Unified diff of the working tree against HEAD (staged and unstaged together). Optional path narrows it to one file or directory.", map[string]any{
			"path": fsProp("string", "Optional file or directory, relative to the repository root"),
		}),

		fsTool("git_log", "Recent commits, newest first: short hash, author, date, subject.", map[string]any{
			"n":    fsProp("integer", "How many commits to return (default 10, max 200)"),
			"path": fsProp("string", "Optional file or directory: only commits touching it"),
		}),

		fsTool("git_show", "One commit: metadata, message, and its diff against its first parent.", map[string]any{
			"ref": fsProp("string", "Commit hash, branch, tag, or HEAD (HEAD~1 and friends work)"),
		}, "ref"),

		fsTool("git_branch_list", "Local branches with their head commits; the current branch is marked with *.", map[string]any{}),

		fsTool("git_blame", "Per-line authorship of a tracked file at HEAD: commit, author, line number, text.", map[string]any{
			"path": fsProp("string", "Tracked file, relative to the repository root"),
		}, "path"),

		fsTool("git_add", "Stage paths for the next commit. Pass \".\" to stage everything.", map[string]any{
			"paths": gitPathsProp(),
		}, "paths"),

		fsTool("git_commit", "Commit what is staged. Refuses when nothing is staged; the author comes from the repository's own git config.", map[string]any{
			"message": fsProp("string", "Commit message; the first line is the subject"),
		}, "message"),

		fsTool("git_checkout_branch", "Switch to a branch, or create it first with create=true.", map[string]any{
			"branch": fsProp("string", "Branch name"),
			"create": fsProp("boolean", "Create the branch at the current commit instead of expecting it to exist"),
		}, "branch"),

		fsTool("git_restore", "DESTRUCTIVE: throw away uncommitted changes to the named paths (back to HEAD). With staged=true it only unstages them and leaves the file contents alone.", map[string]any{
			"paths":  gitPathsProp(),
			"staged": fsProp("boolean", "Only unstage (index back to HEAD); file contents are left untouched"),
		}, "paths"),
	}

	if name == h.name {
		return allTools, nil
	}
	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("unknown tools tool: %s", name)
}

var _ taskengine.ToolsRepo = (*GitTools)(nil)
