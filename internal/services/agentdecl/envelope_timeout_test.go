package agentdecl_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

// waitShape is a rule's identity plus the wait it carries, which is the pair
// this slice exists to keep together.
type waitShape struct {
	tools, tool string
	action      hitlservice.Action
	op          hitlservice.ConditionOp
	timeoutS    int
	onTimeout   hitlservice.Action
}

func waitShapes(p *hitlservice.Policy) []waitShape {
	out := make([]waitShape, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := waitShape{tools: r.Tools, tool: r.Tool, action: r.Action, timeoutS: r.TimeoutS, onTimeout: r.OnTimeout}
		if len(r.When) > 0 {
			s.op = r.When[0].Op
		}
		out = append(out, s)
	}
	return out
}

func resolveErr(t *testing.T, body, name string) string {
	t.Helper()
	_, err := envelopeConfig(t, body).ResolveEnvelope(name)
	require.Error(t, err)
	return err.Error()
}

// TestUnit_EnvelopeWait_SugarAndTableFormBothDecode pins that adding the wait
// did not cost the bare-action sugar, and that the table form carries it.
func TestUnit_EnvelopeWait_SugarAndTableFormBothDecode(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.sugar]
shell = "approve"

[envelopes.bounded]
shell = { grant = "approve", timeout = "30m", on_timeout = "deny" }
`)
	require.Equal(t, []waitShape{{
		tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove,
	}}, waitShapes(transpile(t, cfg, "sugar")))

	require.Equal(t, []waitShape{{
		tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove,
		timeoutS: 1800, onTimeout: hitlservice.ActionDeny,
	}}, waitShapes(transpile(t, cfg, "bounded")))
}

// TestUnit_EnvelopeWait_OmittedKeepsTodaysBehavior is the no-silent-change
// gate: an operator who wrote nothing must render the bytes they rendered
// before the key existed — no rule deadline, so the ask is bounded by the
// operator's approval ceiling and an expiry resolves as a denial.
func TestUnit_EnvelopeWait_OmittedKeepsTodaysBehavior(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.quiet]
default_action = "deny"
shell = "approve"
files.read = { grant = "allow", approve_paths = ["**/*.env"] }
"tools"."github.*" = "approve"
`)
	raw, err := agentdecl.RenderEnvelopePolicy(cfg, "quiet", agentdecl.ConfigFilename)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "timeout_s")
	require.NotContains(t, string(raw), "on_timeout")

	for _, r := range transpile(t, cfg, "quiet").Rules {
		require.Zero(t, r.TimeoutS, "%s.%s must wait as it did before", r.Tools, r.Tool)
		require.Empty(t, r.OnTimeout, "%s.%s must fall to the runtime's deny", r.Tools, r.Tool)
	}

	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotContains(t, doc, "//reserved", "a policy nobody bounded has nothing to report")
}

// TestUnit_EnvelopeWait_DurationsParseAsGoWritesThem covers the units an
// operator actually types.
func TestUnit_EnvelopeWait_DurationsParseAsGoWritesThem(t *testing.T) {
	t.Parallel()
	for written, want := range map[string]int{
		"90s":    90,
		"30m":    1800,
		"2h":     7200,
		"1.5m":   90,
		"1h30m":  5400,
		"168h":   hitlservice.MaxRuleTimeoutS,
		"3600s":  3600,
		"0.5h":   1800,
		"1m30s":  90,
		"10080m": hitlservice.MaxRuleTimeoutS,
	} {
		t.Run(written, func(t *testing.T) {
			t.Parallel()
			cfg := envelopeConfig(t, `
[envelopes.x]
shell = { grant = "approve", timeout = "`+written+`" }
`)
			p := transpile(t, cfg, "x")
			require.Len(t, p.Rules, 1)
			require.Equal(t, want, p.Rules[0].TimeoutS)
		})
	}
}

// TestUnit_EnvelopeWait_RefusedDurationsNameTheValue: every refusal has to say
// which envelope, which axis and which value, because a wait that quietly
// became something else is the bug this grammar exists to prevent.
func TestUnit_EnvelopeWait_RefusedDurationsNameTheValue(t *testing.T) {
	t.Parallel()
	for written, want := range map[string]string{
		"half an hour": "not a duration",
		"30":           "not a duration",
		"30 m":         "not a duration",
		"-5m":          "cannot run backwards",
		"0s":           "omit timeout",
		"1500ms":       "whole seconds",
		"200h":         "longer than the 168h0m0s a rule accepts",
	} {
		t.Run(written, func(t *testing.T) {
			t.Parallel()
			msg := resolveErr(t, `
[envelopes.late]
shell = { grant = "approve", timeout = "`+written+`" }
`, "late")
			require.Contains(t, msg, want)
			require.Contains(t, msg, "[envelopes.late]", "the error must name the envelope")
			require.Contains(t, msg, "shell.timeout", "the error must name the axis")
			require.Contains(t, msg, written, "the error must name the value")
		})
	}
}

// TestUnit_EnvelopeWait_TimeoutMustBeAString refuses the TOML an operator
// reaches for when they think in seconds.
func TestUnit_EnvelopeWait_TimeoutMustBeAString(t *testing.T) {
	t.Parallel()
	msg := resolveErr(t, `
[envelopes.late]
shell = { grant = "approve", timeout = 1800 }
`, "late")
	require.Contains(t, msg, "shell.timeout must be a string")
}

// TestUnit_EnvelopeWait_OnTimeoutIsValidatedAgainstTheActions: deny is the one
// outcome this build can express, and each refusal states its own reason.
func TestUnit_EnvelopeWait_OnTimeoutIsValidatedAgainstTheActions(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.denies]
shell = { grant = "approve", timeout = "10m", on_timeout = "deny" }
`)
	p := transpile(t, cfg, "denies")
	require.Equal(t, hitlservice.ActionDeny, p.Rules[0].OnTimeout)

	for written, want := range map[string]string{
		"allow":    "bypasses the approval it exists to require",
		"approve":  "decides nothing",
		"maybe":    `unknown action "maybe"`,
		"deny_all": `unknown action "deny_all"`,
	} {
		t.Run(written, func(t *testing.T) {
			t.Parallel()
			msg := resolveErr(t, `
[envelopes.late]
shell = { grant = "approve", timeout = "10m", on_timeout = "`+written+`" }
`, "late")
			require.Contains(t, msg, want)
			require.Contains(t, msg, "[envelopes.late]")
			require.Contains(t, msg, "shell.on_timeout")
		})
	}
}

// TestUnit_EnvelopeWait_OnlyAnAskCarriesTheWait: the emitter attaches the wait
// to the approve rules a grant produces and to nothing else, which is also what
// keeps the render past `contenox vet`.
func TestUnit_EnvelopeWait_OnlyAnAskCarriesTheWait(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.mixed]
files.write = { grant = "allow", approve_paths = ["**/*.env"], timeout = "10m", on_timeout = "deny" }

[envelopes.mixed.shell]
grant = "approve"
timeout = "45m"
on_timeout = "deny"
blacklist = ["mkfs"]
substitution = "approve"
prefix_allowlist = ["ls"]
ask_always = ["rm"]
`)
	p := transpile(t, cfg, "mixed")
	for _, r := range p.Rules {
		if r.Action == hitlservice.ActionApprove {
			require.NotZero(t, r.TimeoutS, "an ask must carry the wait: %s.%s", r.Tools, r.Tool)
			require.Equal(t, hitlservice.ActionDeny, r.OnTimeout)
			continue
		}
		require.Zerof(t, r.TimeoutS, "%s.%s is %s and never waits", r.Tools, r.Tool, r.Action)
		require.Emptyf(t, r.OnTimeout, "%s.%s is %s and never waits", r.Tools, r.Tool, r.Action)
	}

	// The allow floor under approve_paths keeps its own action and no wait.
	var floors, asks int
	for _, r := range p.Rules {
		if r.Tool == "write_file" {
			if r.Action == hitlservice.ActionAllow {
				floors++
			} else {
				asks++
				require.Equal(t, 600, r.TimeoutS)
			}
		}
	}
	require.Equal(t, 1, floors)
	require.Equal(t, 1, asks)

	raw, err := agentdecl.RenderEnvelopePolicy(cfg, "mixed", agentdecl.ConfigFilename)
	require.NoError(t, err)
	require.NoError(t, hitlservice.VetPolicy(raw), string(raw))
}

// TestUnit_EnvelopeWait_ShellTiersCarryItWhereTheyAsk names the tiers rather
// than trusting the loop above to have reached them.
func TestUnit_EnvelopeWait_ShellTiersCarryItWhereTheyAsk(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.tiers.shell]
grant = "approve"
timeout = "1h"
on_timeout = "deny"
blacklist = ["mkfs"]
substitution = "approve"
prefix_allowlist = ["ls"]
ask_always = ["rm"]
`)
	require.Equal(t, []waitShape{
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionDeny, op: hitlservice.OpCommandBlacklist},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove, op: hitlservice.OpNoCommandSubstitution, timeoutS: 3600, onTimeout: hitlservice.ActionDeny},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionAllow, op: hitlservice.OpCommandPrefixAllowlist},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove, op: hitlservice.OpCommandAskAlways, timeoutS: 3600, onTimeout: hitlservice.ActionDeny},
		{tools: "local_shell", tool: "local_shell", action: hitlservice.ActionApprove, timeoutS: 3600, onTimeout: hitlservice.ActionDeny},
	}, waitShapes(transpile(t, cfg, "tiers")))
}

// TestUnit_EnvelopeWait_ToolPatternsTakeItToo covers the per-tool half of the
// grammar, in both forms.
func TestUnit_EnvelopeWait_ToolPatternsTakeItToo(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.pertool.tools]
"github.merge_pr" = { grant = "approve", timeout = "2h", on_timeout = "deny" }
"github.list_prs" = "allow"
"tavily.search" = "approve"
`)
	p := transpile(t, cfg, "pertool")
	byTool := map[string]hitlservice.Rule{}
	for _, r := range p.Rules {
		byTool[r.Tool] = r
	}
	require.Equal(t, 7200, byTool["merge_pr"].TimeoutS)
	require.Equal(t, hitlservice.ActionDeny, byTool["merge_pr"].OnTimeout)
	require.Zero(t, byTool["list_prs"].TimeoutS)
	require.Zero(t, byTool["search"].TimeoutS, "an ask nobody bounded carries no rule deadline, leaving it on the service's approval ceiling")
	require.Empty(t, byTool["search"].OnTimeout)
}

// TestUnit_EnvelopeWait_RefusedWhereNothingWaits: a wait attached to a grant
// that never asks is refused where it is written, not dropped at emit time.
func TestUnit_EnvelopeWait_RefusedWhereNothingWaits(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ body, want string }{
		"allow grant": {`
[envelopes.x]
shell = { grant = "allow", timeout = "30m" }
`, `grant = "allow" never asks`},
		"deny grant": {`
[envelopes.x]
files.write = { grant = "deny", on_timeout = "deny" }
`, `grant = "deny" never asks`},
		"tools pattern": {`
[envelopes.x.tools]
"github.list_prs" = { grant = "allow", timeout = "5m" }
`, `grant = "allow" never asks`},
		"default_action": {`
[envelopes.x]
default_action = { grant = "deny", timeout = "5m" }
`, `grant = "deny" never asks`},
		"missions.answer": {`
[envelopes.x]
missions.answer = { grant = "approve", timeout = "5m" }
`, "compiles to the attention block"},
		"network axis": {`
[envelopes.x]
network.read = { grant = "approve", timeout = "5m" }
`, "no provider serves this axis"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, resolveErr(t, tc.body, "x"), tc.want)
		})
	}
}

// TestUnit_EnvelopeWait_ApprovePathsCarryItUnderAnAllowFloor: the grant that
// never asks is still allowed to bound the asks its refinements carve out.
func TestUnit_EnvelopeWait_ApprovePathsCarryItUnderAnAllowFloor(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.carved]
files.read = { grant = "allow", approve_paths = ["**/*.pem"], timeout = "15m", on_timeout = "deny" }
`)
	p := transpile(t, cfg, "carved")
	require.NotEmpty(t, p.Rules)
	for _, r := range p.Rules {
		if r.Action == hitlservice.ActionApprove {
			require.Equal(t, 900, r.TimeoutS)
			require.Equal(t, hitlservice.ActionDeny, r.OnTimeout)
		} else {
			require.Zero(t, r.TimeoutS)
		}
	}
}

// TestUnit_EnvelopeWait_AskAlwaysCarriesItUnderAnAllowFloor is the shell twin
// of the case above.
func TestUnit_EnvelopeWait_AskAlwaysCarriesItUnderAnAllowFloor(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.carved]
shell = { grant = "allow", ask_always = ["rm"], timeout = "15m", on_timeout = "deny" }
`)
	p := transpile(t, cfg, "carved")
	require.Len(t, p.Rules, 2)
	require.Equal(t, hitlservice.ActionApprove, p.Rules[0].Action)
	require.Equal(t, 900, p.Rules[0].TimeoutS)
	require.Equal(t, hitlservice.ActionAllow, p.Rules[1].Action)
	require.Zero(t, p.Rules[1].TimeoutS)
}

// TestUnit_EnvelopeWait_DefaultActionIsReportedNotFaked: the policy schema has
// one default_action and no field beside it, so the wait is named in the render
// rather than silently dropped or smuggled in as a catch-all rule.
func TestUnit_EnvelopeWait_DefaultActionIsReportedNotFaked(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.fallthrough]
default_action = { grant = "approve", timeout = "20m", on_timeout = "deny" }
`)
	env, err := cfg.ResolveEnvelope("fallthrough")
	require.NoError(t, err)
	out, err := agentdecl.TranspileEnvelope(env)
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionApprove, out.Policy.DefaultAction)
	require.Empty(t, out.Policy.Rules, "default_action must not become a rule")
	require.Len(t, out.Notes, 1)
	require.Contains(t, out.Notes[0], "no field to carry it")
	require.Contains(t, out.Notes[0], `timeout = "20m0s"`)
	require.Contains(t, out.Notes[0], `on_timeout = "deny"`)

	raw, err := agentdecl.RenderEnvelopePolicy(cfg, "fallthrough", agentdecl.ConfigFilename)
	require.NoError(t, err)
	require.Contains(t, string(raw), "//reserved")
	require.NoError(t, hitlservice.VetPolicy(raw), string(raw))
}

// TestUnit_EnvelopeWait_ExtendsReplacesTheGrantAndItsWait: an axis merges
// wholesale, so a child restating one drops the parent's wait with it. Pinned
// because an operator inheriting a bounded envelope must not read a bare
// override as keeping the bound.
func TestUnit_EnvelopeWait_ExtendsReplacesTheGrantAndItsWait(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.parent]
shell = { grant = "approve", timeout = "30m", on_timeout = "deny" }
files.write = { grant = "approve", timeout = "10m", on_timeout = "deny" }

[envelopes.child]
extends = "parent"
shell = "approve"
`)
	p := transpile(t, cfg, "child")
	byTool := map[string]hitlservice.Rule{}
	for _, r := range p.Rules {
		byTool[r.Tool] = r
	}
	require.Zero(t, byTool["local_shell"].TimeoutS, "restating the axis drops the wait with the grant")
	require.Equal(t, 600, byTool["write_file"].TimeoutS, "an axis the child left alone keeps the parent's wait")
}

// TestUnit_EnvelopeWait_UnknownKeyNamesTheWaitKeys keeps the two new keys in
// the message an operator gets when they misspell one.
func TestUnit_EnvelopeWait_UnknownKeyNamesTheWaitKeys(t *testing.T) {
	t.Parallel()
	msg := resolveErr(t, `
[envelopes.x]
shell = { grant = "approve", timeout_s = 1800 }
`, "x")
	require.Contains(t, msg, `unknown key "timeout_s"`)
	for _, key := range []string{"grant", "timeout", "on_timeout"} {
		require.Contains(t, msg, key)
	}
}

// TestUnit_EnvelopeWait_ShippedEnvelopesStayUnbounded: which shipped envelope
// gets which wait is the maintainer's call, so this slice must have changed no
// rendered byte.
func TestUnit_EnvelopeWait_ShippedEnvelopesStayUnbounded(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	require.NotEmpty(t, cfg.EnvelopeNames())
	for _, name := range cfg.EnvelopeNames() {
		raw, err := agentdecl.RenderEnvelopePolicy(cfg, name, agentdecl.ConfigFilename)
		require.NoError(t, err)
		require.NotContainsf(t, string(raw), "timeout_s", "[envelopes.%s] gained a wait this slice did not choose", name)
		require.NotContainsf(t, string(raw), "on_timeout", "[envelopes.%s] gained a wait this slice did not choose", name)
	}
}

// TestUnit_EnvelopeWait_RenderedWaitSurvivesTheLoader drives the whole path:
// the render is what `contenox vet` accepts and what the evaluator reports back
// to the caller that has to honour it.
func TestUnit_EnvelopeWait_RenderedWaitSurvivesTheLoader(t *testing.T) {
	t.Parallel()
	cfg := envelopeConfig(t, `
[envelopes.bounded]
default_action = "deny"
shell = { grant = "approve", timeout = "30m", on_timeout = "deny" }
`)
	raw, err := agentdecl.RenderEnvelopePolicy(cfg, "bounded", agentdecl.ConfigFilename)
	require.NoError(t, err)
	require.NoError(t, hitlservice.VetPolicy(raw), string(raw))
	require.Contains(t, string(raw), `"timeout_s": 1800`)
	require.Contains(t, string(raw), `"on_timeout": "deny"`)

	var p hitlservice.Policy
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Len(t, p.Rules, 1)
	require.Equal(t, 1800, p.Rules[0].TimeoutS)
	require.Equal(t, hitlservice.ActionDeny, p.Rules[0].OnTimeout)
	require.False(t, strings.Contains(string(raw), "//reserved"))
}

// TestUnit_EnvelopeWait_NeverIsAWaitWithNoDeadline pins the case that was
// unsayable before this slice: an operator who wants an ask to survive a
// closed laptop writes a word, not a number, and the rule carries the
// sentinel rather than a large duration.
func TestUnit_EnvelopeWait_NeverIsAWaitWithNoDeadline(t *testing.T) {
	t.Parallel()
	for _, written := range []string{"never", "forever", "indefinite", "NEVER", " never "} {
		t.Run(written, func(t *testing.T) {
			t.Parallel()
			cfg := envelopeConfig(t, `
[envelopes.patient]
default_action = "deny"
shell = { grant = "approve", timeout = "`+written+`" }
`)
			p := transpile(t, cfg, "patient")
			require.Len(t, p.Rules, 1)
			require.Equal(t, hitlservice.TimeoutIndefinite, p.Rules[0].TimeoutS)
			require.Empty(t, p.Rules[0].OnTimeout, "nothing expires, so nothing resolves it")

			raw, err := agentdecl.RenderEnvelopePolicy(cfg, "patient", agentdecl.ConfigFilename)
			require.NoError(t, err)
			require.NoError(t, hitlservice.VetPolicy(raw), string(raw))
			require.Contains(t, string(raw), `"timeout_s": -1`)
		})
	}
}

// TestUnit_EnvelopeWait_OnTimeoutBesideNeverIsRefused: an ask with no deadline
// never expires, so an on_timeout beside it is a belief about the runtime that
// is simply false. Refuse it rather than emit a rule nobody will ever read.
func TestUnit_EnvelopeWait_OnTimeoutBesideNeverIsRefused(t *testing.T) {
	t.Parallel()
	msg := resolveErr(t, `
[envelopes.confused]
shell = { grant = "approve", timeout = "never", on_timeout = "deny" }
`, "confused")
	require.Contains(t, msg, "shell")
	require.Contains(t, msg, `on_timeout = "deny"`)
	require.Contains(t, msg, `timeout = "never"`)
	require.Contains(t, msg, "never expires")
}

// TestUnit_EnvelopeWait_UnknownWordNamesTheSpellingsItAccepts keeps the
// refusal teachable: a near miss has to say what to write instead.
func TestUnit_EnvelopeWait_UnknownWordNamesTheSpellingsItAccepts(t *testing.T) {
	t.Parallel()
	for _, written := range []string{"always", "infinite", "none", "no deadline", "-1"} {
		t.Run(written, func(t *testing.T) {
			t.Parallel()
			msg := resolveErr(t, `
[envelopes.late]
shell = { grant = "approve", timeout = "`+written+`" }
`, "late")
			require.Contains(t, msg, "shell.timeout")
			require.Contains(t, msg, hitlservice.IndefiniteSpellings())
		})
	}
}

// TestUnit_EnvelopeWait_TooLongPointsAtTheWordInstead: seven days is the
// longest number a rule takes, and the value past it now has somewhere to go.
func TestUnit_EnvelopeWait_TooLongPointsAtTheWordInstead(t *testing.T) {
	t.Parallel()
	msg := resolveErr(t, `
[envelopes.late]
shell = { grant = "approve", timeout = "200h" }
`, "late")
	require.Contains(t, msg, "longer than the 168h0m0s a rule accepts")
	require.Contains(t, msg, hitlservice.IndefiniteSpellings())
}
