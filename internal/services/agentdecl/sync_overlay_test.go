package agentdecl_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
)

const declReviewer = `---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Grep
---

You review code.
`

func syncOne(t *testing.T, root, config string) ([]agentdecl.SyncResult, string) {
	t.Helper()
	if config != "" {
		if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename), []byte(config), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	results, err := agentdecl.Sync(agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, cfg)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return results, gen
}

func readChain(t *testing.T, gen, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(gen, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return out
}

// The point of per-agent scoping: two declarations in one workspace, given
// different budgets, without either reaching for the emitted JSON.
func TestUnit_Sync_PerAgentOverlayReachesTheEmittedChain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, agentdecl.NativeSourceDir)
	declare(t, src, "reviewer.md", declReviewer)
	declare(t, src, "triage.md", declTriage)

	_, gen := syncOne(t, root, `
[agents.reviewer.chain]
token_limit = 4096
`)

	reviewer := readChain(t, gen, "chain-agent-reviewer.json")
	if got := reviewer["token_limit"]; got != float64(4096) {
		t.Fatalf("reviewer token_limit = %v, want 4096", got)
	}
	triage := readChain(t, gen, "chain-agent-triage.json")
	if got := triage["token_limit"]; got == float64(4096) {
		t.Fatalf("triage inherited reviewer's overlay: %v", got)
	}
	if got := triage["token_limit"]; got != float64(131072) {
		t.Fatalf("triage token_limit = %v, want the shipped 131072", got)
	}
}

// agents.toml is the other half of the input. Before this, only the source
// hash was compared, so editing a knob left every generated chain stale.
func TestUnit_Sync_ConfigEditRegeneratesWithoutTouchingTheDeclaration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "reviewer.md", declReviewer)

	_, gen := syncOne(t, root, "")
	if got := readChain(t, gen, "chain-agent-reviewer.json")["token_limit"]; got != float64(131072) {
		t.Fatalf("first pass token_limit = %v", got)
	}

	results, _ := syncOne(t, root, `
[agents.reviewer.chain]
token_limit = 4096
`)
	r, ok := resultFor(results, "reviewer.md")
	if !ok {
		t.Fatal("no result for reviewer.md")
	}
	if r.Action != agentdecl.ActionUpdated {
		t.Fatalf("action = %q, want %q", r.Action, agentdecl.ActionUpdated)
	}
	if got := readChain(t, gen, "chain-agent-reviewer.json")["token_limit"]; got != float64(4096) {
		t.Fatalf("token_limit after config edit = %v, want 4096", got)
	}
}

func TestUnit_Sync_UnchangedWhenNeitherSourceNorConfigMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "reviewer.md", declReviewer)

	syncOne(t, root, `
[agents.reviewer.chain]
token_limit = 4096
`)
	results, _ := syncOne(t, root, "")

	r, ok := resultFor(results, "reviewer.md")
	if !ok {
		t.Fatal("no result for reviewer.md")
	}
	if r.Action != agentdecl.ActionUnchanged {
		t.Fatalf("action = %q, want %q", r.Action, agentdecl.ActionUnchanged)
	}
}

// A truncated generated file heals on the next pass rather than persisting as
// a chain the linter will refuse at discovery.
func TestUnit_Sync_RewritesAGeneratedFileThatWasCorrupted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "reviewer.md", declReviewer)

	_, gen := syncOne(t, root, "")
	chainPath := filepath.Join(gen, "chain-agent-reviewer.json")
	if err := os.WriteFile(chainPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	syncOne(t, root, "")
	if got := readChain(t, gen, "chain-agent-reviewer.json")["token_limit"]; got != float64(131072) {
		t.Fatalf("corrupted file was not rewritten: token_limit = %v", got)
	}
}

func TestUnit_Sync_OverlayThatCannotRunRefusesThatAgentOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, agentdecl.NativeSourceDir)
	declare(t, src, "reviewer.md", declReviewer)
	declare(t, src, "triage.md", declTriage)

	results, gen := syncOne(t, root, `
[agents.reviewer.chain]
token_limit = 0
`)

	r, ok := resultFor(results, "reviewer.md")
	if !ok {
		t.Fatal("no result for reviewer.md")
	}
	if r.Action != agentdecl.ActionRefused {
		t.Fatalf("action = %q, want %q", r.Action, agentdecl.ActionRefused)
	}
	if r.Name != "reviewer" {
		t.Fatalf("refusal names %q, want the agent it is about", r.Name)
	}
	if _, err := os.Stat(filepath.Join(gen, "chain-agent-triage.json")); err != nil {
		t.Fatalf("one bad overlay cost the operator their other agent: %v", err)
	}
}

func TestUnit_Sync_ReportsAnOverlayNamingNoDeclaration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "reviewer.md", declReviewer)

	results, _ := syncOne(t, root, `
[agents.reviewr.chain]
token_limit = 4096
`)

	r, ok := resultFor(results, "[agents.reviewr]")
	if !ok {
		t.Fatalf("a mistyped overlay was silently ignored; results: %+v", results)
	}
	if r.Action != agentdecl.ActionIgnored {
		t.Fatalf("action = %q, want %q", r.Action, agentdecl.ActionIgnored)
	}
	if r.Name != "reviewr" {
		t.Fatalf("name = %q, want the mistyped key", r.Name)
	}
}

// Foreign agents are dialect-scoped, so their overlay key is the scoped id the
// operator sees in `agent list`, not the name in the source frontmatter.
func TestUnit_Sync_ForeignAgentOverlayKeysOffTheScopedID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := t.TempDir()
	declare(t, filepath.Join(workspace, ".claude", "agents"), "reviewer.md", declReviewer)

	if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename), []byte(`
[agents.claude-code-reviewer.chain]
token_limit = 4096
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	results, err := agentdecl.Sync(agentdecl.DiscoverSourceDirs([]string{root}, []string{workspace}), gen, cfg)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := resultFor(results, "[agents."); ok {
		t.Fatalf("the scoped id did not match the overlay key; results: %+v", results)
	}
	if got := readChain(t, gen, "chain-agent-claude-code-reviewer.json")["token_limit"]; got != float64(4096) {
		t.Fatalf("token_limit = %v, want 4096", got)
	}
}

// contenox init writes agents/README.md itself. Reading it as a declaration
// refused it on every pass, which trains an operator to ignore the warnings.
func TestUnit_Sync_ReadmeBesideDeclarationsIsNotADeclaration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, agentdecl.NativeSourceDir)
	declare(t, src, "reviewer.md", declReviewer)
	declare(t, src, "README.md", "# Agents\n\nWrite one Markdown file per agent.\n")

	results, _ := syncOne(t, root, "")
	if r, ok := resultFor(results, "README.md"); ok {
		t.Fatalf("README.md was read as a declaration: %+v", r)
	}
	if _, ok := resultFor(results, "reviewer.md"); !ok {
		t.Fatal("the real declaration must still be found")
	}
}
