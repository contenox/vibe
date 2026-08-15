package agentdecl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
)

func declare(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const declTriage = `---
name: triage
description: Triages an incoming issue
tools: Read, Grep
---

You triage issues.
`

func resultFor(results []agentdecl.SyncResult, source string) (agentdecl.SyncResult, bool) {
	for _, r := range results {
		if strings.HasSuffix(r.Source, source) {
			return r, true
		}
	}
	return agentdecl.SyncResult{}, false
}

// TestUnit_Sync_NativeDeclarationNeedsNoDetection covers the case a declaration
// author actually writes: a plain file, in contenox's own directory, carrying
// nothing that identifies a vendor.
func TestUnit_Sync_NativeDeclarationNeedsNoDetection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "triage.md", declTriage)
	gen := filepath.Join(root, agentdecl.GeneratedDirName)

	dirs := agentdecl.DiscoverSourceDirs([]string{root}, nil)
	if len(dirs) != 1 || !dirs[0].Native {
		t.Fatalf("expected one native source dir, got %+v", dirs)
	}

	results, err := agentdecl.Sync(dirs, gen, mustConfig(t))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	r, ok := resultFor(results, "triage.md")
	if !ok {
		t.Fatalf("triage.md was not processed: %+v", results)
	}
	if r.Action != agentdecl.ActionCreated {
		t.Fatalf("action = %q, reason %q", r.Action, r.Reason)
	}
	if r.Name != "triage" {
		t.Errorf("name = %q; a declaration you wrote yourself is not scoped by a vendor", r.Name)
	}
	if _, err := os.Stat(filepath.Join(gen, "chain-agent-triage.json")); err != nil {
		t.Errorf("no chain emitted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gen, "hitl-policy-triage.json")); err != nil {
		t.Errorf("no policy emitted: %v", err)
	}
}

// TestUnit_Sync_ForeignDeclarationsAreScoped keeps two products' identically
// named agents apart, since chainagents resolves by chain id.
func TestUnit_Sync_ForeignDeclarationsAreScoped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws := filepath.Join(root, "project")
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "triage.md", declTriage)
	declare(t, filepath.Join(ws, ".claude", "agents"), "triage.md", declTriage)

	dirs := agentdecl.DiscoverSourceDirs([]string{root}, []string{ws})
	results, err := agentdecl.Sync(dirs, filepath.Join(root, agentdecl.GeneratedDirName), mustConfig(t))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	names := map[string]bool{}
	for _, r := range results {
		if r.Action == agentdecl.ActionRefused {
			t.Fatalf("%s refused: %s", r.Source, r.Reason)
		}
		names[r.Name] = true
	}
	if !names["triage"] || !names["claude-code-triage"] {
		t.Fatalf("both agents must survive side by side, got %v", names)
	}
}

func TestUnit_Sync_UnchangedSourceIsANoOp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "triage.md", declTriage)
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	dirs := agentdecl.DiscoverSourceDirs([]string{root}, nil)
	cfg := mustConfig(t)

	if _, err := agentdecl.Sync(dirs, gen, cfg); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	results, err := agentdecl.Sync(dirs, gen, cfg)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	r, _ := resultFor(results, "triage.md")
	if r.Action != agentdecl.ActionUnchanged {
		t.Errorf("action = %q, want unchanged", r.Action)
	}
}

// TestUnit_Sync_EditingTheDeclarationRegenerates is the property the whole
// design turns on: the declaration is the source of truth.
func TestUnit_Sync_EditingTheDeclarationRegenerates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentdecl.NativeSourceDir)
	declare(t, dir, "triage.md", declTriage)
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	dirs := agentdecl.DiscoverSourceDirs([]string{root}, nil)
	cfg := mustConfig(t)

	if _, err := agentdecl.Sync(dirs, gen, cfg); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	declare(t, dir, "triage.md", strings.Replace(declTriage, "You triage issues.", "You triage issues carefully.", 1))

	results, err := agentdecl.Sync(dirs, gen, cfg)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	r, _ := resultFor(results, "triage.md")
	if r.Action != agentdecl.ActionUpdated {
		t.Fatalf("action = %q, want updated", r.Action)
	}
	raw, err := os.ReadFile(filepath.Join(gen, "chain-agent-triage.json"))
	if err != nil {
		t.Fatalf("read chain: %v", err)
	}
	if !strings.Contains(string(raw), "carefully") {
		t.Error("the edited declaration did not reach the chain")
	}
}

// TestUnit_Sync_RemovingTheDeclarationRetiresTheAgent stops a deleted file
// leaving a chain behind that still answers to its name.
func TestUnit_Sync_RemovingTheDeclarationRetiresTheAgent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentdecl.NativeSourceDir)
	path := declare(t, dir, "triage.md", declTriage)
	gen := filepath.Join(root, agentdecl.GeneratedDirName)
	cfg := mustConfig(t)

	if _, err := agentdecl.Sync(agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, cfg); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := agentdecl.Sync(agentdecl.DiscoverSourceDirs([]string{root}, nil), gen, cfg); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gen, "chain-agent-triage.json")); err == nil {
		t.Error("the chain outlived its declaration")
	}
}

// TestUnit_Sync_OneBadDeclarationDoesNotCostTheRest is what keeps a single
// unmappable agent from emptying an operator's roster.
func TestUnit_Sync_OneBadDeclarationDoesNotCostTheRest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, agentdecl.NativeSourceDir)
	declare(t, dir, "triage.md", declTriage)
	declare(t, dir, "researcher.md",
		"---\nname: researcher\ndescription: Searches\ntools: WebSearch, NotebookEdit\n---\n\nBody.\n")

	results, err := agentdecl.Sync(
		agentdecl.DiscoverSourceDirs([]string{root}, nil),
		filepath.Join(root, agentdecl.GeneratedDirName), mustConfig(t))
	if err != nil {
		t.Fatalf("sync must not fail on one bad declaration: %v", err)
	}
	good, _ := resultFor(results, "triage.md")
	if good.Action != agentdecl.ActionCreated {
		t.Errorf("the healthy agent was lost: %q %s", good.Action, good.Reason)
	}
	bad, _ := resultFor(results, "researcher.md")
	if bad.Action != agentdecl.ActionRefused {
		t.Fatalf("the unmappable agent should be refused, got %q", bad.Action)
	}
	if !strings.Contains(bad.Reason, "WebSearch") || !strings.Contains(bad.Reason, agentdecl.ConfigFilename) {
		t.Errorf("the refusal must name the tool and where to fix it: %s", bad.Reason)
	}
}

func TestUnit_Sync_ConnectingTheToolMakesTheAgentWork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	declare(t, filepath.Join(root, agentdecl.NativeSourceDir), "researcher.md",
		"---\nname: researcher\ndescription: Searches\ntools: WebSearch\n---\n\nBody.\n")
	if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename),
		[]byte("[tools]\nWebSearch = \"tavily.search\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	results, err := agentdecl.Sync(
		agentdecl.DiscoverSourceDirs([]string{root}, nil),
		filepath.Join(root, agentdecl.GeneratedDirName), cfg)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	r, _ := resultFor(results, "researcher.md")
	if r.Action != agentdecl.ActionCreated {
		t.Fatalf("naming the tool should make it import: %q %s", r.Action, r.Reason)
	}
}

func TestUnit_Preseed_EstablishesTheConvention(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	created, err := agentdecl.Preseed(root)
	if err != nil {
		t.Fatalf("preseed: %v", err)
	}
	if len(created) != len(agentdecl.Preseeded) {
		t.Errorf("created %d files, want %d", len(created), len(agentdecl.Preseeded))
	}
	for _, rel := range []string{
		agentdecl.ConfigFilename,
		filepath.Join(agentdecl.NativeSourceDir, "reviewer.md"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s was not seeded: %v", rel, err)
		}
	}

	// The shipped agent must be a working declaration, not a sample.
	results, err := agentdecl.Sync(
		agentdecl.DiscoverSourceDirs([]string{root}, nil),
		filepath.Join(root, agentdecl.GeneratedDirName), mustConfig(t))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	r, ok := resultFor(results, "reviewer.md")
	if !ok || r.Action != agentdecl.ActionCreated {
		t.Fatalf("the seeded agent does not run: %+v", results)
	}

	again, err := agentdecl.Preseed(root)
	if err != nil {
		t.Fatalf("second preseed: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("preseed overwrote existing files: %v", again)
	}
}
