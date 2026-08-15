package agentdecl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
)

const commonCore = "---\nname: reviewer\ndescription: Reviews code\ntools: Read\n---\n\nBody.\n"

func TestUnit_DetectDialect_ByAnchorDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want agentdecl.Dialect
	}{
		{".claude/agents/reviewer.md", agentdecl.DialectClaudeCode},
		{"/home/x/proj/.claude/agents/review/deep.md", agentdecl.DialectClaudeCode},
		{"/home/x/.claude/agents/reviewer.md", agentdecl.DialectClaudeCode},
		{".agents/agents/reviewer/agent.md", agentdecl.DialectAntigravity},
		{"/home/x/.gemini/config/agents/reviewer/agent.md", agentdecl.DialectAntigravity},
		{".github/agents/accessibility.agent.md", agentdecl.DialectCopilot},
		{".opencode/agents/review.md", agentdecl.DialectOpenCode},
		{"/home/x/.config/opencode/agents/review.md", agentdecl.DialectOpenCode},
		{".cursor/agents/review.md", agentdecl.DialectCursor},
	}
	for _, c := range cases {
		got, err := agentdecl.DetectDialect(c.path, []byte(commonCore))
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: detected %q, want %q", c.path, got, c.want)
		}
	}
}

func TestUnit_DetectDialect_ByFilenameShape(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]agentdecl.Dialect{
		"somewhere/accessibility.agent.md": agentdecl.DialectCopilot,
		"somewhere/reviewer/agent.md":      agentdecl.DialectAntigravity,
	} {
		got, err := agentdecl.DetectDialect(path, []byte(commonCore))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("%s: detected %q, want %q", path, got, want)
		}
	}
}

func TestUnit_DetectDialect_ByFingerprint(t *testing.T) {
	t.Parallel()
	for field, want := range map[string]agentdecl.Dialect{
		"disallowedTools: Bash":        agentdecl.DialectClaudeCode,
		"commandExecutionPolicy: auto": agentdecl.DialectAntigravity,
		"handoffs: [other]":            agentdecl.DialectCopilot,
		"mode: subagent":               agentdecl.DialectOpenCode,
		"is_background: true":          agentdecl.DialectCursor,
	} {
		src := "---\nname: x\ndescription: y\n" + field + "\n---\n\nBody.\n"
		got, err := agentdecl.DetectDialect("loose/x.md", []byte(src))
		if err != nil {
			t.Errorf("%s: %v", field, err)
			continue
		}
		if got != want {
			t.Errorf("%s: detected %q, want %q", field, got, want)
		}
	}
}

// TestUnit_DetectDialect_RefusesOnAmbiguity is the rule that keeps a
// mis-detection from silently applying the wrong tool vocabulary.
func TestUnit_DetectDialect_RefusesOnAmbiguity(t *testing.T) {
	t.Parallel()
	var ambiguous *agentdecl.ErrAmbiguousDialect

	if _, err := agentdecl.DetectDialect("loose/reviewer.md", []byte(commonCore)); !errors.As(err, &ambiguous) {
		t.Fatalf("a common-core file outside any anchor must refuse, got %v", err)
	}

	conflicting := "---\nname: x\ndescription: y\nmemory: project\nhandoffs: [other]\n---\n\nBody.\n"
	if _, err := agentdecl.DetectDialect("loose/x.md", []byte(conflicting)); !errors.As(err, &ambiguous) {
		t.Fatalf("fields pointing at two dialects must refuse, got %v", err)
	}
}

// TestUnit_DetectDialect_GenericAgentsAnchorIsNotEnough guards the one anchor
// that is not vendor-scoped: `.agents/` reads like a neutral directory and
// something else will eventually claim it.
func TestUnit_DetectDialect_GenericAgentsAnchorIsNotEnough(t *testing.T) {
	t.Parallel()
	var ambiguous *agentdecl.ErrAmbiguousDialect
	_, err := agentdecl.DetectDialect(".agents/agents/reviewer.md", []byte(commonCore))
	if !errors.As(err, &ambiguous) {
		t.Fatalf(".agents/ without agent.md or a fingerprint must refuse, got %v", err)
	}

	confirmed := "---\nname: x\ndescription: y\ncommandExecutionPolicy: auto\n---\n\nBody.\n"
	got, err := agentdecl.DetectDialect(".agents/agents/reviewer.md", []byte(confirmed))
	if err != nil || got != agentdecl.DialectAntigravity {
		t.Fatalf("a fingerprint must confirm the generic anchor, got %q %v", got, err)
	}
}

func TestUnit_ParseAntigravity(t *testing.T) {
	t.Parallel()
	src := "---\n" +
		"description: Upgrades local packages and verifies tests pass\n" +
		"model: flash\n" +
		"tools: [view_file, replace_file_content, run_command]\n" +
		"permissionMode: acceptEdits\n" +
		"commandExecutionPolicy: auto\n" +
		"mainAgent: true\n" +
		"---\n\nYou upgrade dependencies.\n"

	ir, err := agentdecl.Parse(".agents/agents/dependency-modernizer/agent.md", []byte(src), mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ir.Source.Dialect != agentdecl.DialectAntigravity {
		t.Errorf("dialect = %q", ir.Source.Dialect)
	}
	if ir.Name != "dependency-modernizer" {
		t.Errorf("name = %q; it should come from the enclosing directory", ir.Name)
	}
	if ir.Role != agentdecl.RolePrimary {
		t.Errorf("role = %q, want primary for mainAgent", ir.Role)
	}
	if ir.Posture != agentdecl.PostureAutoEdit {
		t.Errorf("posture = %q", ir.Posture)
	}
	if ir.Model.Provider != "gemini" {
		t.Errorf("model.provider = %q, want gemini", ir.Model.Provider)
	}
	want := "local_fs.read_file,local_fs.write_file,local_shell.local_shell"
	if got := strings.Join(ir.Tools.Allow, ","); got != want {
		t.Errorf("tools = %q, want %q", got, want)
	}
	var sawShellNote bool
	for _, u := range ir.Unmapped {
		if u.Field == "commandExecutionPolicy" {
			sawShellNote = true
		}
	}
	if !sawShellNote {
		t.Error("auto shell execution was tightened without reporting it")
	}
}

// TestUnit_TwoDialectsSameNameDoNotCollide is what the dialect scope exists
// for: chainagents resolves agents by chain id and drops the loser silently.
func TestUnit_TwoDialectsSameNameDoNotCollide(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)

	cc, err := agentdecl.Parse(".claude/agents/reviewer.md",
		[]byte("---\nname: reviewer\ndescription: Reviews code\ntools: Read\n---\n\nBody.\n"), cfg)
	if err != nil {
		t.Fatalf("claude-code: %v", err)
	}
	ag, err := agentdecl.Parse(".agents/agents/reviewer/agent.md",
		[]byte("---\ndescription: Reviews code\ntools: [view_file]\n---\n\nBody.\n"), cfg)
	if err != nil {
		t.Fatalf("antigravity: %v", err)
	}

	if cc.ScopedName(true) == ag.ScopedName(true) {
		t.Fatalf("both agents scoped to %q; one would shadow the other", cc.ScopedName(true))
	}
	if cc.ScopedName(false) != ag.ScopedName(false) {
		t.Errorf("unscoped names should collide, proving why scoping is the default")
	}
}

// TestUnit_AntigravityRidesTheSameEmitters proves the IR held: a second dialect
// reaches both emitters without either learning about it.
func TestUnit_AntigravityRidesTheSameEmitters(t *testing.T) {
	t.Parallel()
	src := "---\ndescription: Reviews code\ntools: [view_file]\npermissionMode: acceptEdits\n---\n\nBody.\n"
	ir, err := agentdecl.Parse(".agents/agents/reviewer/agent.md", []byte(src), mustConfig(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := mustConfig(t)
	if _, err := agentdecl.EmitChain(ir, cfg); err != nil {
		t.Fatalf("chain emitter rejected a second dialect: %v", err)
	}
	if _, err := agentdecl.EmitPolicy(ir, cfg); err != nil {
		t.Fatalf("policy emitter rejected a second dialect: %v", err)
	}
}
