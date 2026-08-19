package agentdecl_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

// envelopeConfig layers one overlay onto the shipped configuration, which is
// how an operator's own agents.toml reaches the transpiler.
func envelopeConfig(t *testing.T, body string) agentdecl.Config {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, agentdecl.ConfigFilename), []byte(body), 0o644))
	cfg, err := agentdecl.Load(dir)
	require.NoError(t, err)
	return cfg
}

type ruleShape struct {
	tools, tool string
	action      hitlservice.Action
	op          hitlservice.ConditionOp
	value       string
}

func shapes(p *hitlservice.Policy) []ruleShape {
	out := make([]ruleShape, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := ruleShape{tools: r.Tools, tool: r.Tool, action: r.Action}
		if len(r.When) > 0 {
			s.op, s.value = r.When[0].Op, r.When[0].Value
		}
		out = append(out, s)
	}
	return out
}

func transpile(t *testing.T, cfg agentdecl.Config, name string) *hitlservice.Policy {
	t.Helper()
	env, err := cfg.ResolveEnvelope(name)
	require.NoError(t, err)
	out, err := agentdecl.TranspileEnvelope(env)
	require.NoError(t, err)
	return out.Policy
}

// TestUnit_Envelope_BareActionIsSugarForTheTableForm pins the one rule that
// keeps the seven axes from drifting apart.
func TestUnit_Envelope_BareActionIsSugarForTheTableForm(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.bare]
shell = "approve"
files.read = "allow"

[envelopes.table]
shell = { grant = "approve" }
[envelopes.table.files]
read = { grant = "allow" }
`)
	require.Equal(t, shapes(transpile(t, cfg, "bare")), shapes(transpile(t, cfg, "table")))
}

// TestUnit_Envelope_AxisExpandsToTheToolsTheEngineEvaluates is the golden per
// axis: list_dir rides with the read grant because it is the directory probe,
// and an axis nobody set emits nothing at all.
func TestUnit_Envelope_AxisExpandsToTheToolsTheEngineEvaluates(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.axes]
default_action = "deny"
files.read = "allow"
files.write = "approve"
shell = "approve"
missions.fire = "allow"
`)
	require.Equal(t, []ruleShape{
		{tools: "mission", tool: "mission_start", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "read_file", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "read_file_range", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "list_dir", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "grep", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "find_files", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "stat_file", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "count_stats", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "list_dir", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "write_file", action: hitlservice.ActionApprove},
		{tools: "local_fs", tool: "edit_file", action: hitlservice.ActionApprove},
		{tools: "local_fs", tool: "sed", action: hitlservice.ActionApprove},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove},
	}, shapes(transpile(t, cfg, "axes")))

	// An unset axis falls through rather than defaulting to approve.
	unset := envelopeConfig(t, `
[envelopes.quiet]
default_action = "deny"
files.read = "allow"
`)
	for _, r := range transpile(t, unset, "quiet").Rules {
		require.NotEqual(t, "local_shell", r.Tools, "an unset shell axis must emit no rule")
	}
}

// TestUnit_Envelope_EmissionOrderIsFirstMatchWins pins the frozen order: the
// order IS the semantics, so a conditional deny that lands after its grant is a
// rule that never fires.
func TestUnit_Envelope_EmissionOrderIsFirstMatchWins(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.ordered]
default_action = "approve"
files.read = { grant = "allow", approve_paths = ["**/.env"] }
files.write = { grant = "allow", deny_paths = ["**/.git/**"] }
shell = { grant = "approve", blacklist = ["mkfs"], substitution = "approve", prefix_allowlist = ["ls"], ask_always = ["rm"] }

[envelopes.ordered.tools]
"tavily.search" = "allow"

[[envelopes.ordered.always_deny]]
tools = "local_fs"
tool = "*"
when_key = "path"
when_op = "glob"
when_value = "**/.ssh/**"

[[envelopes.ordered.always_allow]]
tools = "git"
tool = "git_status"
`)
	require.Equal(t, []ruleShape{
		{tools: "local_fs", tool: "*", action: hitlservice.ActionDeny, op: hitlservice.OpGlob, value: "**/.ssh/**"},
		{tools: "local_fs", tool: "write_file", action: hitlservice.ActionDeny, op: hitlservice.OpGlob, value: "**/.git/**"},
		{tools: "local_fs", tool: "edit_file", action: hitlservice.ActionDeny, op: hitlservice.OpGlob, value: "**/.git/**"},
		{tools: "local_fs", tool: "sed", action: hitlservice.ActionDeny, op: hitlservice.OpGlob, value: "**/.git/**"},
		{tools: "local_fs", tool: "read_file", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "local_fs", tool: "read_file_range", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "local_fs", tool: "list_dir", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "native-fs-browse", tool: "grep", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "native-fs-browse", tool: "find_files", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "native-fs-browse", tool: "stat_file", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "native-fs-browse", tool: "count_stats", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "native-fs-browse", tool: "list_dir", action: hitlservice.ActionApprove, op: hitlservice.OpGlob, value: "**/.env"},
		{tools: "tavily", tool: "search", action: hitlservice.ActionAllow},
		{tools: "git", tool: "git_status", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "read_file", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "read_file_range", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "list_dir", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "grep", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "find_files", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "stat_file", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "count_stats", action: hitlservice.ActionAllow},
		{tools: "native-fs-browse", tool: "list_dir", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "write_file", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "edit_file", action: hitlservice.ActionAllow},
		{tools: "local_fs", tool: "sed", action: hitlservice.ActionAllow},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionDeny, op: hitlservice.OpCommandBlacklist, value: "mkfs"},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove, op: hitlservice.OpNoCommandSubstitution},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionAllow, op: hitlservice.OpCommandPrefixAllowlist, value: "ls"},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove, op: hitlservice.OpCommandAskAlways, value: "rm"},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove},
	}, shapes(transpile(t, cfg, "ordered")))
}

// TestUnit_Envelope_ToolPatternsOrderBySpecificityNotFileOrder keeps the output
// byte-deterministic whatever order the operator wrote the table in.
func TestUnit_Envelope_ToolPatternsOrderBySpecificityNotFileOrder(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.patterns.tools]
"*" = "approve"
"stripe" = "approve"
"stripe.refund" = "deny"
"tavily.search" = "allow"
`)
	require.Equal(t, []ruleShape{
		{tools: "stripe", tool: "refund", action: hitlservice.ActionDeny},
		{tools: "tavily", tool: "search", action: hitlservice.ActionAllow},
		{tools: "stripe", tool: "*", action: hitlservice.ActionApprove},
		{tools: "*", tool: "*", action: hitlservice.ActionApprove},
	}, shapes(transpile(t, cfg, "patterns")))
}

// TestUnit_Envelope_PartialGlobIsRefused: ruleMatches compares exactly, so a
// pattern that can never match is a defect, not a wildcard.
func TestUnit_Envelope_PartialGlobIsRefused(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.bad.tools]
"stri*" = "approve"
`)
	_, err := cfg.ResolveEnvelope("bad")
	require.ErrorContains(t, err, "can never match")
}

// TestUnit_Envelope_UnknownKeysNameTheOffender: a silently ignored knob reads as
// a grant that is simply not there.
func TestUnit_Envelope_UnknownKeysNameTheOffender(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"top level": `
[envelopes.x]
filez = "allow"
`,
		"axis section": `
[envelopes.x]
files.delete = "allow"
`,
		"refinement": `
[envelopes.x]
shell = { grant = "allow", deny_hosts = ["a"] }
`,
		"compute": `
[envelopes.x.compute]
max_widgets = 3
`,
		"standing rule": `
[[envelopes.x.always_deny]]
toolz = "local_fs"
`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := envelopeConfig(t, body)
			_, err := cfg.ResolveEnvelope("x")
			require.Error(t, err)
			require.Contains(t, err.Error(), "unknown key")
		})
	}
}

// TestUnit_Envelope_JoinedListRefusesACommaButAGlobKeepsOne: a shell list is one
// comma-separated value to the engine; a path list is one rule per glob, where a
// brace expression's commas are the point.
func TestUnit_Envelope_JoinedListRefusesACommaButAGlobKeepsOne(t *testing.T) {
	t.Parallel()
	joined := envelopeConfig(t, `
[envelopes.x]
shell = { grant = "approve", blacklist = ["rm,sudo"] }
`)
	_, err := joined.ResolveEnvelope("x")
	require.ErrorContains(t, err, "contains a comma")

	globbed := envelopeConfig(t, `
[envelopes.x]
files.read = { grant = "allow", deny_paths = ["**/{*.pem,*.key}"] }
`)
	p := transpile(t, globbed, "x")
	require.Equal(t, "**/{*.pem,*.key}", p.Rules[0].When[0].Value)
}

// TestUnit_Envelope_ExtendsMergesPerLeafAndReplacesLists freezes the three
// merge rules implementations diverge on.
func TestUnit_Envelope_ExtendsMergesPerLeafAndReplacesLists(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.base]
default_action = "approve"
files.read = "allow"
files.write = { grant = "approve", deny_paths = ["**/.git/**", "**/.ssh/**"] }

[envelopes.base.tools]
"stripe.refund" = "approve"
"tavily.search" = "allow"

[[envelopes.base.always_deny]]
tools = "local_fs"
tool = "*"
when_key = "path"
when_op = "glob"
when_value = "**/.aws/**"

[envelopes.child]
extends = "base"
files.write = "deny"

[envelopes.child.tools]
"stripe.refund" = "deny"

[[envelopes.child.always_deny]]
tools = "git"
tool = "git_commit"
`)
	child := transpile(t, cfg, "child")
	got := shapes(child)

	// (a) an untouched axis is inherited whole.
	require.Contains(t, got, ruleShape{tools: "local_fs", tool: "read_file", action: hitlservice.ActionAllow})
	// (b) the bare form replaces the whole axis, deny_paths included.
	for _, r := range got {
		require.NotEqual(t, "**/.git/**", r.value,
			"a bare grant must replace the parent's deny_paths, not patch around them")
	}
	require.Contains(t, got, ruleShape{tools: "local_fs", tool: "write_file", action: hitlservice.ActionDeny})
	// (c) the tools map merges key by key, and the child's entry wins.
	require.Contains(t, got, ruleShape{tools: "stripe", tool: "refund", action: hitlservice.ActionDeny})
	require.Contains(t, got, ruleShape{tools: "tavily", tool: "search", action: hitlservice.ActionAllow})
	// (d) standing rules accumulate parent-first: they exist to be un-waivable.
	require.Equal(t, ruleShape{tools: "local_fs", tool: "*", action: hitlservice.ActionDeny, op: hitlservice.OpGlob, value: "**/.aws/**"}, got[0])
	require.Contains(t, got, ruleShape{tools: "git", tool: "git_commit", action: hitlservice.ActionDeny})
}

// TestUnit_Envelope_ExtendsRefusesWhatItCannotResolve.
func TestUnit_Envelope_ExtendsRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	missing := envelopeConfig(t, `
[envelopes.child]
extends = "nowhere"
`)
	_, err := missing.ResolveEnvelope("child")
	require.ErrorContains(t, err, "which no envelope declares")

	cyclic := envelopeConfig(t, `
[envelopes.a]
extends = "b"

[envelopes.b]
extends = "a"
`)
	_, err = cyclic.ResolveEnvelope("a")
	require.ErrorContains(t, err, "cycle")
}

// TestUnit_Envelope_MissionsAnswerCompilesToAttentionNotARule: the mission
// toolset is HITL-exempt, so delegation can only ride in the attention block.
func TestUnit_Envelope_MissionsAnswerCompilesToAttentionNotARule(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.answering]
missions.answer = "approve"

[envelopes.answering.attention]
max_agent_answers = 7

[envelopes.silent]
missions.answer = "deny"
`)
	p := transpile(t, cfg, "answering")
	require.NotNil(t, p.Attention)
	require.True(t, p.Attention.AllowAgentAnswers)
	require.True(t, p.Attention.AllowAgentApprovals)
	require.Equal(t, 7, p.Attention.MaxAgentAnswers)
	for _, r := range p.Rules {
		require.NotEqual(t, "mission", r.Tools, "missions.answer must not emit a rule")
	}
	require.Nil(t, transpile(t, cfg, "silent").Attention)
}

// TestUnit_Envelope_NetworkAxisIsValidAndInert: the intent must survive a
// native-web revival, so it vets clean and says so rather than being refused.
func TestUnit_Envelope_NetworkAxisIsValidAndInert(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.netted]
network.read = "allow"
network.write = { grant = "deny", deny_hosts = ["169.254.169.254"] }
`)
	env, err := cfg.ResolveEnvelope("netted")
	require.NoError(t, err)
	out, err := agentdecl.TranspileEnvelope(env)
	require.NoError(t, err)
	require.Empty(t, out.Policy.Rules, "no provider serves the network axes in this build")
	require.Len(t, out.Notes, 2)
	require.Contains(t, strings.Join(out.Notes, "\n"), "native-web/web_get")
}

// TestUnit_Envelope_RenderedPolicyPassesVet is the slice's exit test: every
// shipped envelope must satisfy the validator `contenox vet` runs.
func TestUnit_Envelope_RenderedPolicyPassesVet(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	require.NotEmpty(t, cfg.EnvelopeNames())
	for _, name := range cfg.EnvelopeNames() {
		t.Run(name, func(t *testing.T) {
			raw, err := agentdecl.RenderEnvelopePolicy(cfg, name, agentdecl.ConfigFilename)
			require.NoError(t, err)
			require.NoError(t, hitlservice.VetPolicy(raw), string(raw))

			var doc map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &doc))
			require.Contains(t, doc, "$schema")
			require.Contains(t, doc, "//")
			require.Contains(t, string(doc["//"]), name)
		})
	}
}

// TestUnit_Envelope_NamesAndFilenamesAreOneNamespace.
func TestUnit_Envelope_NamesAndFilenamesAreOneNamespace(t *testing.T) {
	t.Parallel()
	require.Equal(t, "hitl-policy-acpx.json", agentdecl.EnvelopePolicyFile("acpx"))
	for _, written := range []string{"acpx", "hitl-policy-acpx.json", "  acpx  "} {
		name, ok := agentdecl.EnvelopeName(written)
		require.True(t, ok, written)
		require.Equal(t, "acpx", name)
	}
	for _, bad := range []string{"", "Acpx", "a.b", "../x", "hitl-policy-.json"} {
		_, ok := agentdecl.EnvelopeName(bad)
		require.False(t, ok, bad)
	}
}

// TestUnit_Envelope_PostureResolvesThroughTheEnvelopeTable: the emitted policy
// of a declared agent and a profile envelope come out of one transpiler.
func TestUnit_Envelope_PostureResolvesThroughTheEnvelopeTable(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.read_only]
files.read = "deny"
files.write = "deny"
shell = "deny"
`)
	p, err := agentdecl.EmitPolicy(irWithPosture(t, agentdecl.PostureReadOnly, nil), cfg)
	require.NoError(t, err)
	r, ok := ruleFor(p, "local_fs", "read_file")
	require.True(t, ok)
	require.Equal(t, hitlservice.ActionDeny, r.Action, "the envelope table, not the retired posture table, decides")
}

// TestUnit_Envelope_LegacyPostureTableStillEmits keeps a configuration that
// predates the envelopes table working.
func TestUnit_Envelope_LegacyPostureTableStillEmits(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	cfg.Envelopes = nil
	require.NoError(t, cfg.Validate())
	p, err := agentdecl.EmitPolicy(irWithPosture(t, agentdecl.PostureAutoEdit, nil), cfg)
	require.NoError(t, err)
	r, ok := ruleFor(p, "local_fs", "write_file")
	require.True(t, ok)
	require.Equal(t, hitlservice.ActionAllow, r.Action)
}
