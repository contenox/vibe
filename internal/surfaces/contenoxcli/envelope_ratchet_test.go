package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/stretchr/testify/require"
)

// ratchetPairs bind a shipped envelope to the preset it replaces. serve,
// read_only, ask_always and auto_edit replace no preset, so they have no row:
// the gate is a ratchet against a predecessor, not a review of every envelope.
var ratchetPairs = []struct {
	envelope string
	preset   string
	content  string
}{
	{"default", "hitl-policy-default.json", hitlPolicyDefault},
	{"default", "hitl-policy-acp.json", hitlPolicyACP},
	{"strict", "hitl-policy-strict.json", hitlPolicyStrict},
	{"acpx", "hitl-policy-acpx.json", hitlPolicyACPX},
	{"oracle", "hitl-policy-oracle.json", hitlPolicyOracle},
}

// ratchetStrictness orders the three actions. The gate is one-way: the
// transpiled envelope may move a call up this scale, never down.
var ratchetStrictness = map[hitlservice.Action]int{
	hitlservice.ActionAllow:   0,
	hitlservice.ActionApprove: 1,
	hitlservice.ActionDeny:    2,
}

type ratchetIdentity struct{ tools, tool string }

type ratchetCall struct {
	ratchetIdentity
	args   map[string]any
	origin string
}

func (c ratchetCall) String() string {
	raw, _ := json.Marshal(c.args)
	return fmt.Sprintf("%s.%s(%s) [%s]", c.tools, c.tool, raw, c.origin)
}

// TestUnit_ShippedEnvelopes_NoWeakerThanReplacedPresets drives both the preset
// and the envelope it replaces through the real evaluator over a corpus
// generated from every rule in the preset, and fails on any call the envelope
// treats more permissively.
func TestUnit_ShippedEnvelopes_NoWeakerThanReplacedPresets(t *testing.T) {
	t.Parallel()
	universe := ratchetUniverse(t)
	require.NotEmpty(t, universe)

	for _, pair := range ratchetPairs {
		t.Run(pair.envelope+"_vs_"+pair.preset, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			old := ratchetParse(t, pair.preset, pair.content)
			rendered := renderedEnvelopePolicy(t, pair.envelope)
			file := agentdecl.EnvelopePolicyFile(pair.envelope)

			oldSvc := seededPolicyService(t, pair.preset, pair.content)
			newSvc := seededPolicyService(t, file, rendered)

			targeted, sweep := ratchetCorpus(old, universe)
			require.NotEmpty(t, sweep)

			// Coverage first: a ratchet over a corpus that never reaches a rule
			// proves nothing about that rule. A rule an earlier and at least as
			// strict rule shadows counts as reached — it has no verdict of its
			// own to preserve.
			for i, rule := range old.Rules {
				reached := false
				for _, call := range targeted[i] {
					res, err := oldSvc.Evaluate(ctx, call.tools, call.tool, call.args)
					require.NoError(t, err)
					if res.Reason != hitlservice.ReasonMatchedRule || res.MatchedRule == nil || *res.MatchedRule > i {
						continue
					}
					if *res.MatchedRule == i || ratchetStrictness[res.Action] >= ratchetStrictness[rule.Action] {
						reached = true
						break
					}
				}
				require.Truef(t, reached,
					"%s rule %d (%s.%s -> %s) is reached by no generated call; the corpus does not cover it",
					pair.preset, i, rule.Tools, rule.Tool, rule.Action)
			}

			var weaker []string
			for _, call := range sweep {
				before, err := oldSvc.Evaluate(ctx, call.tools, call.tool, call.args)
				require.NoError(t, err)
				after, err := newSvc.Evaluate(ctx, call.tools, call.tool, call.args)
				require.NoError(t, err)
				if ratchetStrictness[after.Action] < ratchetStrictness[before.Action] {
					weaker = append(weaker, fmt.Sprintf("%s: %s -> %s", call, before.Action, after.Action))
				}
			}
			require.Falsef(t, len(weaker) > 0,
				"[envelopes.%s] is weaker than %s on %d of %d calls:\n%s",
				pair.envelope, pair.preset, len(weaker), len(sweep), strings.Join(ratchetHead(weaker, 25), "\n"))
		})
	}
}

// TestUnit_ShippedEnvelopes_NoWeakerBoundsThanReplacedPresets is the same
// ratchet over the halves that are not rules: what a mission may spend, and who
// besides a human may answer for it.
func TestUnit_ShippedEnvelopes_NoWeakerBoundsThanReplacedPresets(t *testing.T) {
	t.Parallel()
	for _, pair := range ratchetPairs {
		t.Run(pair.envelope+"_vs_"+pair.preset, func(t *testing.T) {
			t.Parallel()
			old := ratchetParse(t, pair.preset, pair.content)
			var next hitlservice.Policy
			require.NoError(t, json.Unmarshal([]byte(renderedEnvelopePolicy(t, pair.envelope)), &next))

			oldCompute, newCompute := hitlservice.ComputeBounds{}, hitlservice.ComputeBounds{}
			if old.Compute != nil {
				oldCompute = *old.Compute
			}
			if next.Compute != nil {
				newCompute = *next.Compute
			}
			assertCeilingNoWeaker(t, pair.envelope, pair.preset, "maxToolCalls", oldCompute.MaxToolCalls, newCompute.MaxToolCalls)
			assertCeilingNoWeaker(t, pair.envelope, pair.preset, "maxTokens", oldCompute.MaxTokens, newCompute.MaxTokens)

			oldAttn, newAttn := hitlservice.AttentionBounds{}, hitlservice.AttentionBounds{}
			if old.Attention != nil {
				oldAttn = *old.Attention
			}
			if next.Attention != nil {
				newAttn = *next.Attention
			}
			if newAttn.AllowAgentAnswers {
				require.Truef(t, oldAttn.AllowAgentAnswers,
					"[envelopes.%s] lets an agent answer where %s required a human", pair.envelope, pair.preset)
				require.LessOrEqualf(t, newAttn.EffectiveMaxAgentAnswers(), oldAttn.EffectiveMaxAgentAnswers(),
					"[envelopes.%s] raises the agent-answer cap above %s", pair.envelope, pair.preset)
			}
			if newAttn.AllowAgentApprovals {
				require.Truef(t, oldAttn.AllowAgentApprovals,
					"[envelopes.%s] lets an agent approve where %s required a human", pair.envelope, pair.preset)
				require.LessOrEqualf(t, newAttn.EffectiveMaxAgentApprovals(), oldAttn.EffectiveMaxAgentApprovals(),
					"[envelopes.%s] raises the agent-approval cap above %s", pair.envelope, pair.preset)
			}
		})
	}
}

// assertCeilingNoWeaker treats zero as unbounded on both sides, so an envelope
// may add a ceiling the preset lacked but never drop or raise one it had.
func assertCeilingNoWeaker(t *testing.T, envelope, preset, field string, before, after int) {
	t.Helper()
	if before <= 0 {
		return
	}
	require.Greaterf(t, after, 0, "[envelopes.%s] drops %s, which %s bounded at %d", envelope, field, preset, before)
	require.LessOrEqualf(t, after, before, "[envelopes.%s] raises %s above %s", envelope, field, preset)
}

func renderedEnvelopePolicy(t *testing.T, envelope string) string {
	t.Helper()
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	raw, err := agentdecl.RenderEnvelopePolicy(cfg, envelope, agentdecl.ConfigFilename)
	require.NoErrorf(t, err, "render [envelopes.%s]", envelope)
	return string(raw)
}

func ratchetParse(t *testing.T, name, content string) hitlservice.Policy {
	t.Helper()
	var p hitlservice.Policy
	require.NoErrorf(t, json.Unmarshal([]byte(content), &p), "parse %s", name)
	require.NotEmptyf(t, p.Rules, "%s has no rules", name)
	return p
}

// ratchetUngatedProviders never reach the evaluator: enginesvc routes them
// around the HITL wrapper by construction (hitlExemptProviders), so a rule
// naming one is inert on both sides and there is nothing to ratchet.
var ratchetUngatedProviders = map[string]bool{missiontools.ToolsProviderName: true}

// ratchetUniverse is every (toolset, tool) pair either side names, so a tool
// only the envelope grants is probed too. Wildcards contribute no identity of
// their own — a name no build serves is not a call. A rule naming a tool this
// build no longer serves stays in on purpose: a dead tool's call must not
// become allowed either.
func ratchetUniverse(t *testing.T) []ratchetIdentity {
	t.Helper()
	byToolset := map[string]map[string]bool{}
	collect := func(name, content string) {
		for _, r := range ratchetParse(t, name, content).Rules {
			if r.Tools == "" || r.Tools == "*" || r.Tool == "" || r.Tool == "*" || ratchetUngatedProviders[r.Tools] {
				continue
			}
			if byToolset[r.Tools] == nil {
				byToolset[r.Tools] = map[string]bool{}
			}
			byToolset[r.Tools][r.Tool] = true
		}
	}
	for _, preset := range HITLPolicyPresets {
		collect(preset.Name, preset.Content)
	}
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	for _, envelope := range cfg.EnvelopeNames() {
		collect(agentdecl.EnvelopePolicyFile(envelope), renderedEnvelopePolicy(t, envelope))
	}
	var out []ratchetIdentity
	for _, tools := range sortedStringKeys(byToolset) {
		for _, tool := range sortedStringKeys(byToolset[tools]) {
			out = append(out, ratchetIdentity{tools: tools, tool: tool})
		}
	}
	return out
}

// ratchetCorpus returns the calls generated per rule, indexed by rule, plus the
// deduplicated sweep the ratchet runs over: every identity crossed with every
// argument shape any rule in the preset can distinguish.
func ratchetCorpus(p hitlservice.Policy, universe []ratchetIdentity) (targeted [][]ratchetCall, sweep []ratchetCall) {
	targeted = make([][]ratchetCall, len(p.Rules))
	fixtures := append([]map[string]any(nil), ratchetBenignArgs...)
	for i, r := range p.Rules {
		args := ruleArgFixtures(r)
		fixtures = append(fixtures, args...)
		for _, id := range ruleIdentities(r, universe) {
			for _, a := range args {
				targeted[i] = append(targeted[i], ratchetCall{ratchetIdentity: id, args: a, origin: fmt.Sprintf("rule %d", i)})
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range universe {
		for _, a := range fixtures {
			call := ratchetCall{ratchetIdentity: id, args: a, origin: "sweep"}
			key := call.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			sweep = append(sweep, call)
		}
	}
	return targeted, sweep
}

func ruleIdentities(r hitlservice.Rule, universe []ratchetIdentity) []ratchetIdentity {
	var out []ratchetIdentity
	for _, id := range universe {
		toolsOK := r.Tools == "" || r.Tools == "*" || r.Tools == id.tools
		toolOK := r.Tool == "" || r.Tool == "*" || r.Tool == id.tool
		if toolsOK && toolOK {
			out = append(out, id)
		}
	}
	return out
}

// ruleArgFixtures builds the argument maps that satisfy every condition on one
// rule at once. A rule with no conditions is satisfied by any args, so it takes
// the benign set.
func ruleArgFixtures(r hitlservice.Rule) []map[string]any {
	if len(r.When) == 0 {
		return []map[string]any{{}}
	}
	combined := []map[string]any{{}}
	for _, c := range r.When {
		variants := conditionArgFixtures(c)
		if len(variants) == 0 {
			return nil
		}
		next := make([]map[string]any, 0, len(combined)*len(variants))
		for _, base := range combined {
			for _, v := range variants {
				merged := make(map[string]any, len(base)+len(v))
				for k, val := range base {
					merged[k] = val
				}
				for k, val := range v {
					merged[k] = val
				}
				next = append(next, merged)
			}
		}
		combined = next
	}
	return combined
}

func conditionArgFixtures(c hitlservice.Condition) []map[string]any {
	key := c.Key
	if key == "" {
		key = "command"
	}
	one := func(values ...string) []map[string]any {
		out := make([]map[string]any, 0, len(values))
		for _, v := range values {
			out = append(out, map[string]any{key: v})
		}
		return out
	}
	switch c.Op {
	case hitlservice.OpEq:
		return one(c.Value)
	case hitlservice.OpGlob:
		return one(globInstances(c.Value)...)
	case hitlservice.OpHost:
		var hosts []string
		for _, h := range splitRuleList(c.Value) {
			if strings.Contains(h, ":") {
				hosts = append(hosts, "http://["+h+"]/probe")
				continue
			}
			hosts = append(hosts, "http://"+h+"/probe", "https://sub."+h+"/probe")
		}
		return one(hosts...)
	case hitlservice.OpCommandBlacklist, hitlservice.OpCommandAskAlways:
		var cmds []string
		for _, name := range splitRuleList(c.Value) {
			cmds = append(cmds, name, name+" target", "/usr/bin/"+name)
		}
		return one(cmds...)
	case hitlservice.OpCommandPrefixAllowlist:
		return one(splitRuleList(c.Value)...)
	case hitlservice.OpNoCommandSubstitution:
		return one("echo $(id)", "echo `id`", "cat <(ls)", "echo $((1+1))")
	default:
		return nil
	}
}

// ratchetBenignArgs are the shapes no rule condition names: an ordinary read,
// an ordinary command, an ordinary request, and the empty call.
var ratchetBenignArgs = []map[string]any{
	{},
	{"path": "src/main.go"},
	{"path": "/home/u/proj/README.md"},
	{"path": "docs/index.md"},
	{"path": "internal/foo_test.go"},
	{"command": "ls"},
	{"command": "ls -la /tmp"},
	{"command": "curl https://example.com"},
	{"command": "python setup.py install"},
	{"args": "echo hello"},
	{"url": "https://example.com/api"},
}

// globInstances turns one policy glob into concrete paths that match it: brace
// alternatives expanded, then each wildcard filled with a literal.
func globInstances(pattern string) []string {
	seen := map[string]bool{}
	var out []string
	for _, alt := range expandGlobAlternatives(pattern) {
		concrete := concreteGlobPath(alt)
		if concrete == "" || seen[concrete] {
			continue
		}
		seen[concrete] = true
		out = append(out, concrete)
	}
	return out
}

func expandGlobAlternatives(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	depth, closeIdx := 0, -1
	for i := open; i < len(pattern) && closeIdx < 0; i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				closeIdx = i
			}
		}
	}
	if closeIdx < 0 {
		return []string{pattern}
	}
	prefix, suffix := pattern[:open], pattern[closeIdx+1:]
	var out []string
	for _, alt := range splitTopLevelAlternatives(pattern[open+1 : closeIdx]) {
		out = append(out, expandGlobAlternatives(prefix+alt+suffix)...)
	}
	return out
}

func splitTopLevelAlternatives(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// concreteGlobPath fills the wildcards. "**" becomes a two-segment path so a
// pattern anchored on it is exercised across a separator, not just beside one.
func concreteGlobPath(pattern string) string {
	const doubleStar = "\x00"
	p := strings.ReplaceAll(pattern, "**", doubleStar)
	p = strings.ReplaceAll(p, "*", "x")
	p = strings.ReplaceAll(p, "?", "q")
	return strings.ReplaceAll(p, doubleStar, "home/u")
}

func splitRuleList(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ratchetHead(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return append(append([]string(nil), lines[:n]...), fmt.Sprintf("... and %d more", len(lines)-n))
}
