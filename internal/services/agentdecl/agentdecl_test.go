package agentdecl_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
)

func loadFixture(t *testing.T, dialect, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", dialect, name, "input.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return data
}

func mustConfig(t *testing.T) agentdecl.Config {
	t.Helper()
	cfg, err := agentdecl.Shipped()
	if err != nil {
		t.Fatalf("load shipped config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shipped config is invalid: %v", err)
	}
	return cfg
}

func TestUnit_ShippedConfigValidates(t *testing.T) {
	t.Parallel()
	mustConfig(t)
}

func TestUnit_ParseClaudeCodeCodeReviewer(t *testing.T) {
	t.Parallel()
	ir, err := agentdecl.ParseClaudeCode(
		"code-reviewer.md", loadFixture(t, "claude-code", "code-reviewer"), mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if ir.Name != "code-reviewer" {
		t.Errorf("name = %q, want code-reviewer", ir.Name)
	}
	if ir.Source.Dialect != agentdecl.DialectClaudeCode {
		t.Errorf("dialect = %q", ir.Source.Dialect)
	}
	if ir.Source.SHA256 == "" {
		t.Error("provenance sha is empty")
	}
	if !strings.HasPrefix(ir.SystemPrompt, "You are a code reviewer.") {
		t.Errorf("system prompt not carried verbatim: %q", ir.SystemPrompt)
	}
	if ir.Model.Raw != "sonnet" {
		t.Errorf("model.raw = %q, want sonnet", ir.Model.Raw)
	}
	if ir.Model.Provider != "anthropic" {
		t.Errorf("model.provider = %q, want anthropic", ir.Model.Provider)
	}
	want := []string{"local_fs.read_file", "local_fs.find_files", "local_fs.grep"}
	if strings.Join(ir.Tools.Allow, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", ir.Tools.Allow, want)
	}
	if ir.Posture != agentdecl.PostureAskAlways {
		t.Errorf("posture = %q, want ask_always by default", ir.Posture)
	}
	if len(ir.Unmapped) != 0 {
		t.Errorf("expected nothing unmapped, got %+v", ir.Unmapped)
	}
	if ir.ScopedName(true) != "claude-code-code-reviewer" {
		t.Errorf("scoped name = %q", ir.ScopedName(true))
	}
	if ir.ScopedName(false) != "code-reviewer" {
		t.Errorf("unscoped name = %q", ir.ScopedName(false))
	}
}

// TestUnit_EmittedChainPassesRuntimeLint is the slice's exit test: the chain an
// import produces must satisfy the same load-time linter that gates chain
// discovery, not merely be well-formed JSON.
func TestUnit_EmittedChainPassesRuntimeLint(t *testing.T) {
	t.Parallel()
	ir, err := agentdecl.ParseClaudeCode(
		"code-reviewer.md", loadFixture(t, "claude-code", "code-reviewer"), mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chain, err := agentdecl.EmitChain(ir, mustConfig(t))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := taskengine.LintChain(chain); err != nil {
		t.Fatalf("emitted chain fails the runtime linter: %v", err)
	}
}

func TestUnit_EmittedChainShape(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	ir, err := agentdecl.ParseClaudeCode(
		"code-reviewer.md", loadFixture(t, "claude-code", "code-reviewer"), mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chain, err := agentdecl.EmitChain(ir, cfg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	if chain.TokenLimit != cfg.Chain.TokenLimit {
		t.Errorf("token_limit = %d, want %d", chain.TokenLimit, cfg.Chain.TokenLimit)
	}
	if chain.TokenLimit == 0 {
		t.Error("token_limit must never be zero: every tool result would report as too large")
	}
	if chain.ID != "claude-code-code-reviewer" {
		t.Errorf("chain id = %q", chain.ID)
	}

	byID := map[string]taskengine.TaskDefinition{}
	for _, task := range chain.Tasks {
		byID[task.ID] = task
	}
	agent, ok := byID["claude-code-code-reviewer-agent"]
	if !ok {
		t.Fatalf("no agent task in %v", byID)
	}
	if !strings.HasPrefix(agent.SystemInstruction, ir.SystemPrompt) {
		t.Error("agent task does not open with the source system prompt")
	}
	for _, macro := range []string{"{{tools}}", "{{host:os}}"} {
		if !strings.Contains(agent.SystemInstruction, macro) {
			t.Errorf("emitted prompt is missing %s; nothing is appended implicitly at run time", macro)
		}
	}
	if agent.ExecuteConfig == nil {
		t.Fatal("agent task has no execute_config")
	}
	// A claude-code declaration is a subagent, so it holds the toolsets it named
	// plus the mission back-channel it cannot name and cannot run without.
	if got := strings.Join(agent.ExecuteConfig.Tools, ","); got != "local_fs,mission" {
		t.Errorf("exposed toolsets = %q, want the declared local_fs plus mission", got)
	}
	if agent.ExecuteConfig.Model != cfg.Routing.Model && ir.Model.ID == "" {
		t.Errorf("routing should stay templated, got %q", agent.ExecuteConfig.Model)
	}
	if _, ok := agent.ExecuteConfig.ToolsPolicies["local_shell"]; ok {
		t.Error("tools_policies carries local_shell for an agent that cannot reach it")
	}
}

// TestUnit_UnresolvedToolIsDroppedNotFatal follows the declaration format: an
// entry that resolves to nothing is dropped and reported, and only a list where
// nothing resolves at all stops the agent.
func TestUnit_UnresolvedToolIsDroppedNotFatal(t *testing.T) {
	t.Parallel()
	src := []byte("---\nname: researcher\ndescription: Searches the web\ntools: Read, WebSearch\n---\n\nBody.\n")

	ir, err := agentdecl.ParseClaudeCode("researcher.md", src, mustConfig(t))
	if err != nil {
		t.Fatalf("one unresolved tool must not stop an agent that has others: %v", err)
	}
	if strings.Join(ir.Tools.Allow, ",") != "local_fs.read_file" {
		t.Errorf("tools = %v, want the resolvable one kept", ir.Tools.Allow)
	}
	if len(ir.Unmapped) != 1 || ir.Unmapped[0].Value != "WebSearch" {
		t.Fatalf("the dropped tool must be reported, got %+v", ir.Unmapped)
	}
	if !strings.Contains(ir.Unmapped[0].Reason, agentdecl.ConfigFilename) {
		t.Errorf("the report must name where to fix it: %s", ir.Unmapped[0].Reason)
	}
}

func TestUnit_DeclarationWithNoResolvableToolFails(t *testing.T) {
	t.Parallel()
	src := []byte("---\nname: researcher\ndescription: Searches\ntools: WebSearch, NotebookEdit\n---\n\nBody.\n")

	_, err := agentdecl.ParseClaudeCode("researcher.md", src, mustConfig(t))
	var unknown *agentdecl.ErrUnknownTool
	if !errors.As(err, &unknown) {
		t.Fatalf("want ErrUnknownTool when nothing resolves, got %v", err)
	}
	if len(unknown.Tools) != 2 {
		t.Errorf("the error must name every entry, got %v", unknown.Tools)
	}
}

// TestUnit_OmittedToolsInheritsEveryTool is the format's rule: no tools field
// means every tool, not none.
func TestUnit_OmittedToolsInheritsEveryTool(t *testing.T) {
	t.Parallel()
	src := []byte("---\nname: broad\ndescription: Uses whatever it needs\n---\n\nBody.\n")

	ir, err := agentdecl.ParseClaudeCode("broad.md", src, mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ir.Tools.Inherit {
		t.Fatal("an omitted tools list must inherit every tool")
	}
	chain, err := agentdecl.EmitChain(ir, mustConfig(t))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := chain.Tasks[0].ExecuteConfig.Tools
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("exposed toolsets = %v, want [*]", got)
	}
}

// TestUnit_UnsafePostureRefused guards the rule that a source asking for
// permission prompts to be skipped is never honoured silently.
func TestUnit_UnsafePostureRefused(t *testing.T) {
	t.Parallel()
	src := []byte("---\nname: yolo\ndescription: Does anything\ntools: Read\n" +
		"permissionMode: bypassPermissions\n---\n\nBody.\n")

	ir, err := agentdecl.ParseClaudeCode("yolo.md", src, mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ir.Posture != agentdecl.PostureUnsafe {
		t.Fatalf("posture = %q, want unsafe", ir.Posture)
	}
	if _, err := agentdecl.EmitChain(ir, mustConfig(t)); err == nil {
		t.Fatal("emitting an unsafe posture must fail without explicit consent")
	}
}

func TestUnit_UnknownFrontmatterIsReportedNotFatal(t *testing.T) {
	t.Parallel()
	src := []byte("---\nname: futuristic\ndescription: Uses a field we have never seen\n" +
		"tools: Read\nsomeFutureField: whatever\nmemory: project\n---\n\nBody.\n")

	ir, err := agentdecl.ParseClaudeCode("futuristic.md", src, mustConfig(t))
	if err != nil {
		t.Fatalf("unknown frontmatter must not be fatal: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range ir.Unmapped {
		seen[u.Field] = true
		if u.Reason == "" {
			t.Errorf("unmapped %q carries no reason", u.Field)
		}
	}
	for _, want := range []string{"someFutureField", "memory"} {
		if !seen[want] {
			t.Errorf("field %q was dropped without being reported", want)
		}
	}
}

func TestUnit_FrontmatterEdgeCases(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)

	t.Run("crlf and bom", func(t *testing.T) {
		t.Parallel()
		src := []byte("\uFEFF---\r\nname: crlf\r\ndescription: Windows authored\r\ntools: Read\r\n---\r\n\r\nBody line.\r\n")
		ir, err := agentdecl.ParseClaudeCode("crlf.md", src, cfg)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if ir.Name != "crlf" || ir.SystemPrompt != "Body line." {
			t.Errorf("got name=%q prompt=%q", ir.Name, ir.SystemPrompt)
		}
	})

	t.Run("dashes in body do not truncate", func(t *testing.T) {
		t.Parallel()
		src := []byte("---\nname: dashes\ndescription: Has a rule in the body\ntools: Read\n---\n\nIntro.\n\n---\n\nAfter the rule.\n")
		ir, err := agentdecl.ParseClaudeCode("dashes.md", src, cfg)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !strings.Contains(ir.SystemPrompt, "After the rule.") {
			t.Errorf("body truncated at a horizontal rule: %q", ir.SystemPrompt)
		}
	})

	t.Run("tools as a yaml sequence", func(t *testing.T) {
		t.Parallel()
		src := []byte("---\nname: seq\ndescription: Uses list form\ntools:\n  - Read\n  - Grep\n---\n\nBody.\n")
		ir, err := agentdecl.ParseClaudeCode("seq.md", src, cfg)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if strings.Join(ir.Tools.Allow, ",") != "local_fs.read_file,local_fs.grep" {
			t.Errorf("tools = %v", ir.Tools.Allow)
		}
	})

	t.Run("no frontmatter", func(t *testing.T) {
		t.Parallel()
		var missing *agentdecl.ErrNoFrontmatter
		_, err := agentdecl.ParseClaudeCode("plain.md", []byte("Just prose.\n"), cfg)
		if !errors.As(err, &missing) {
			t.Fatalf("want ErrNoFrontmatter, got %v", err)
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		t.Parallel()
		var missing *agentdecl.ErrMissingField
		_, err := agentdecl.ParseClaudeCode("nameless.md", []byte("---\ndescription: No name\n---\n\nBody.\n"), cfg)
		if !errors.As(err, &missing) {
			t.Fatalf("want ErrMissingField, got %v", err)
		}
	})
}

func TestUnit_LoadDefaultsOverlayIsPartial(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	overlay := "[chain]\ntoken_limit = 4096\n"
	if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Chain.TokenLimit != 4096 {
		t.Errorf("token_limit = %d, want the overlay's 4096", cfg.Chain.TokenLimit)
	}
	shipped := mustConfig(t)
	if cfg.Chain.MainRounds != shipped.Chain.MainRounds {
		t.Errorf("main_rounds = %d, want inherited %d", cfg.Chain.MainRounds, shipped.Chain.MainRounds)
	}
	if len(cfg.Policy.AlwaysDeny) != len(shipped.Policy.AlwaysDeny) {
		t.Error("overlay silently dropped the always-deny rules")
	}
}

// TestUnit_ConfigNamesAToolContenoxDoesNotHost is the operator's
// answer to an unresolved tool: contenox hosts no search, so the name is
// mapped onto whatever the operator connected under it.
func TestUnit_ConfigNamesAToolContenoxDoesNotHost(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	overlay := "[tools]\nWebSearch = \"tavily.search\"\n"
	if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	src := []byte("---\nname: researcher\ndescription: Searches the web\ntools: Read, WebSearch\n---\n\nBody.\n")
	ir, err := agentdecl.ParseClaudeCode("researcher.md", src, cfg)
	if err != nil {
		t.Fatalf("mapped tool should import cleanly: %v", err)
	}
	if got := strings.Join(ir.Tools.Allow, ","); got != "local_fs.read_file,tavily.search" {
		t.Errorf("tools = %q, want the overlay mapping applied", got)
	}
	if len(ir.Unmapped) != 0 {
		t.Errorf("nothing should be unmapped once connected, got %+v", ir.Unmapped)
	}
	if sets := strings.Join(agentdecl.ToolSets(ir.Tools.Allow), ","); sets != "local_fs,tavily" {
		t.Errorf("toolsets = %q, want the connected server exposed", sets)
	}
}

func TestUnit_ConfigOverlayMergesToolNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	overlay := "[tools]\nWebSearch = \"tavily.search\"\n"
	if err := os.WriteFile(filepath.Join(root, agentdecl.ConfigFilename), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	cfg, err := agentdecl.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	mapped, _, err := cfg.MapTools([]string{"Read", "Bash"})
	if err != nil {
		t.Fatalf("shipped mappings must survive an overlay: %v", err)
	}
	if strings.Join(mapped, ",") != "local_fs.read_file,local_shell.local_shell" {
		t.Errorf("shipped mappings = %v", mapped)
	}
}

func TestUnit_MissingOverlayIsNotAnError(t *testing.T) {
	t.Parallel()
	if _, err := agentdecl.Load(t.TempDir()); err != nil {
		t.Fatalf("absent overlay must be skipped, got %v", err)
	}
}
