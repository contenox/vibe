package agentdecl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/stretchr/testify/require"
)

func writeSkill(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, agentdecl.SkillDirName, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

const timesheetSkill = `---
name: timesheet
description: File this week's hours to the timesheet system
---

Read the tracked hours, present them for approval, submit, then file the report.
`

func TestUnit_SkillsDiscoveredInBothLayouts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "timesheet.md", timesheetSkill)
	writeSkill(t, root, "release/SKILL.md", "---\nname: release\ndescription: Cut a release\n---\n\nSteps.\n")
	writeSkill(t, root, "notes.txt", "not a skill")

	skills := agentdecl.DiscoverSkills([]string{root}, root)
	require.Len(t, skills, 2, "a flat .md and a folder SKILL.md both count; a .txt does not")
	require.Equal(t, "release", skills[0].Name)
	require.Equal(t, "timesheet", skills[1].Name)
	require.Equal(t, "File this week's hours to the timesheet system", skills[1].Description)
}

// A procedure with no header is still a procedure — requiring frontmatter would
// make the cheapest possible skill impossible.
func TestUnit_SkillWithoutFrontmatterGetsAFallbackDescription(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "deploy.md", "# Deploy\n\nPush the branch, wait for CI, then promote.\n")

	skills := agentdecl.DiscoverSkills([]string{root}, root)
	require.Len(t, skills, 1)
	require.Equal(t, "deploy", skills[0].Name, "the filename names it")
	require.Equal(t, "Push the branch, wait for CI, then promote.", skills[0].Description,
		"the first prose line stands in; a skill with no description is invisible to the agent")
}

func TestUnit_NearestSkillShadowsTheOneFurtherOut(t *testing.T) {
	t.Parallel()
	workspace, home := t.TempDir(), t.TempDir()
	writeSkill(t, workspace, "timesheet.md", "---\nname: timesheet\ndescription: workspace version\n---\nBody.\n")
	writeSkill(t, home, "timesheet.md", "---\nname: timesheet\ndescription: home version\n---\nBody.\n")

	skills := agentdecl.DiscoverSkills([]string{workspace, home}, workspace)
	require.Len(t, skills, 1)
	require.Equal(t, "workspace version", skills[0].Description)
}

func TestUnit_SkillsMacroExpandsIntoTheEmittedChain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "timesheet.md", timesheetSkill)
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "assistant.md", `---
name: assistant
description: Does repeated office work
---

You help with recurring work.

`+agentdecl.SkillsMacro+`
`)

	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	_, err := agentdecl.Sync(
		agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, mustConfig(t),
		agentdecl.WithSkills(agentdecl.DiscoverSkills([]string{root}, root)),
	)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(gen, "chain-agent-assistant.json"))
	require.NoError(t, err)
	body := string(raw)

	require.NotContains(t, body, agentdecl.SkillsMacro, "the macro is expanded at generation, not left for the engine")
	require.Contains(t, body, "timesheet", "the inventory names the skill")
	require.Contains(t, body, "File this week's hours", "and carries its description")
	require.Contains(t, body, "timesheet.md", "and the path to read — the agent loads it with the file tool it already has")
}

// The inventory is the index, never the bodies: ten preloaded procedures would
// be most of a context window.
func TestUnit_InventoryCarriesTheIndexNotTheBodies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "timesheet.md", timesheetSkill)

	rendered := agentdecl.RenderSkillInventory(agentdecl.DiscoverSkills([]string{root}, root))
	require.Contains(t, rendered, "timesheet")
	require.NotContains(t, rendered, "present them for approval",
		"the body stays on disk until the agent reads it")
}

func TestUnit_DeclarationWithoutTheMacroIsUntouched(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "timesheet.md", timesheetSkill)
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "plain.md", `---
name: plain
description: Knows nothing of skills
---

Just a prompt.
`)

	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	_, err := agentdecl.Sync(
		agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, mustConfig(t),
		agentdecl.WithSkills(agentdecl.DiscoverSkills([]string{root}, root)),
	)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(gen, "chain-agent-plain.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "timesheet",
		"a declaration that never asks for the inventory does not get one")
}

func TestUnit_MacroWithNoSkillsSaysSoRatherThanLeakingItself(t *testing.T) {
	t.Parallel()
	require.Equal(t, "No skills are available.", agentdecl.RenderSkillInventory(nil))
	require.False(t, strings.Contains(agentdecl.RenderSkillInventory(nil), "{{"),
		"an unexpanded macro would reach the model as a literal")
}

// Adding a skill must reach agents that already exist — the inventory is baked
// into the generated chain, so the pass has to notice the file appeared.
func TestUnit_AddingASkillRegeneratesExistingAgents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "assistant.md", `---
name: assistant
description: Does repeated office work
---

`+agentdecl.SkillsMacro+`
`)
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	sync := func() []agentdecl.SyncResult {
		res, err := agentdecl.Sync(
			agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, mustConfig(t),
			agentdecl.WithSkills(agentdecl.DiscoverSkills([]string{root}, root)),
		)
		require.NoError(t, err)
		return res
	}

	sync()
	writeSkill(t, root, "timesheet.md", timesheetSkill)
	results := sync()

	r, ok := resultFor(results, "assistant.md")
	require.True(t, ok)
	require.Equal(t, agentdecl.ActionUpdated, r.Action, "a new skill changes the prompt, so the chain is rewritten")

	raw, err := os.ReadFile(filepath.Join(gen, "chain-agent-assistant.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "timesheet")
}

// local_fs refuses absolute paths, so the inventory must address a skill the
// way the agent can actually open it.
func TestUnit_InventoryPathIsRelativeToTheWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "timesheet.md", timesheetSkill)

	skills := agentdecl.DiscoverSkills([]string{filepath.Join(root, ".contenox")}, root)
	require.Empty(t, skills, "nothing under .contenox/skills here yet")

	contenoxDir := filepath.Join(root, ".contenox")
	writeSkill(t, contenoxDir, "release.md", "---\nname: release\ndescription: Cut a release\n---\nSteps.\n")

	skills = agentdecl.DiscoverSkills([]string{contenoxDir}, root)
	require.Len(t, skills, 1)
	require.Equal(t, ".contenox/skills/release.md", skills[0].Path,
		"relative to the project root, which is where the agent's file tool is rooted")
	require.False(t, filepath.IsAbs(skills[0].Path))
}

// A skill the file tool could never open is left out rather than listed as an
// instruction that fails at read time.
func TestUnit_SkillOutsideTheWorkspaceIsNotListed(t *testing.T) {
	t.Parallel()
	workspace, elsewhere := t.TempDir(), t.TempDir()
	writeSkill(t, elsewhere, "global.md", "---\nname: global\ndescription: Applies everywhere\n---\nSteps.\n")

	require.Empty(t, agentdecl.DiscoverSkills([]string{elsewhere}, workspace))
	require.Empty(t, agentdecl.DiscoverSkills([]string{elsewhere}, ""),
		"with no workspace root there is nothing to be relative to")
}
