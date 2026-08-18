package agentdecl

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// realTools is every tool name a shipped prompt may reference: the canonical
// toolset names plus each toolset's function names. Sources:
// localtools/fs_schema.go (local_fs), localtools/localexec.go (local_shell),
// the git toolset, and missiontools. A name added to a toolset is added here;
// a name here that no toolset serves is a phantom and must not ship.
var realTools = map[string]bool{
	"local_fs":        true,
	"local_shell":     true,
	"read_file":       true,
	"read_file_range": true,
	"write_file":      true,
	"edit_file":       true,

	"git_status":          true,
	"git_diff":            true,
	"git_log":             true,
	"git_show":            true,
	"git_blame":           true,
	"git_branch_list":     true,
	"git_add":             true,
	"git_commit":          true,
	"git_checkout_branch": true,
	"git_restore":         true,

	"mission_start":         true,
	"mission_plan":          true,
	"mission_report":        true,
	"mission_finish":        true,
	"mission_ask_attention": true,
	"mission_answer":        true,
	"mission_list":          true,
}

// promptProse is the underscore vocabulary of the shipped prompts that names
// no tool: agents.toml keys the recovery prompts explain, and ordinary prose.
var promptProse = map[string]bool{
	"node_modules":         true,
	"main_rounds":          true,
	"recovery_rounds":      true,
	"rounds_used":          true,
	"recovery_rounds_used": true,
	"in_progress":          true,
}

var toolShapedToken = regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`)

// preseedDeclarations returns every markdown file this package ships, keyed by
// a display path: the preseedTrees embed plus the flat Preseeded files.
func preseedDeclarations(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(preseedTrees, "preseed/agents", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		b, readErr := fs.ReadFile(preseedTrees, path)
		if readErr != nil {
			return readErr
		}
		files[path] = string(b)
		return nil
	})
	require.NoError(t, err)
	for _, p := range Preseeded {
		if strings.HasSuffix(p.RelPath, ".md") {
			files[p.RelPath] = p.Content()
		}
	}
	require.NotEmpty(t, files)
	return files
}

// declarationParts splits a shipped file with the package's own frontmatter
// splitter; a file without a fence is all body.
func declarationParts(content string) (frontmatter []string, body string) {
	front, rest, ok := splitFrontmatter([]byte(content))
	if !ok {
		return nil, content
	}
	return strings.Split(string(front), "\n"), string(rest)
}

// TestUnit_PreseedDeclarations_GrantsResolve pins that every `tools:` grant in
// a shipped declaration resolves through the shipped agents.toml mapping.
// MapTools drops an unresolved name and reports it in a field no session
// surfaces, so a shipped declaration that names an unknown tool loses the
// grant silently — the 2026-08-18 review session ran without Grep and Glob
// because the mapping has no such names.
func TestUnit_PreseedDeclarations_GrantsResolve(t *testing.T) {
	cfg, err := Shipped()
	require.NoError(t, err)
	for path, content := range preseedDeclarations(t) {
		frontmatter, _ := declarationParts(content)
		for _, line := range frontmatter {
			rest, ok := strings.CutPrefix(line, "tools:")
			if !ok {
				continue
			}
			names := strings.Split(rest, ",")
			mapped, skipped, err := cfg.MapTools(names)
			require.NoError(t, err, "%s: tools grant does not resolve", path)
			require.Empty(t, skipped, "%s: tools grant names dropped by the shipped mapping", path)
			require.NotEmpty(t, mapped, "%s: tools grant resolved to nothing", path)
		}
	}
}

// TestUnit_PreseedDeclarations_PromptsNameNoPhantoms pins that every
// tool-shaped token in a shipped prompt body names a tool that exists. A
// prompt that names a phantom primes the model to call it: the review prompt
// said "(grep, find_files)" and the 2026-08-18 session spent turns on
// find_files and got "tool not found". A token that is neither a tool nor
// listed prose fails here; prose additions go in promptProse.
func TestUnit_PreseedDeclarations_PromptsNameNoPhantoms(t *testing.T) {
	for path, content := range preseedDeclarations(t) {
		_, body := declarationParts(content)
		for _, token := range toolShapedToken.FindAllString(body, -1) {
			if realTools[token] || promptProse[token] {
				continue
			}
			t.Errorf("%s: prompt names %q, which no shipped toolset serves", path, token)
		}
	}
}
