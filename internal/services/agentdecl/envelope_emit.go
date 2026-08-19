package agentdecl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missiontools"
)

// toolRef is one (toolset, tool) pair an axis expands to.
type toolRef struct{ tools, tool string }

// axisTools is the load-bearing binding from a capability axis onto the tools
// the engine actually evaluates. list_dir is here because it is the directory
// probe accessview and agentview run, not an executable tool: an envelope that
// grants files.read and omits it cannot list a directory.
var axisTools = map[string][]toolRef{
	AxisFilesRead: {
		{localtools.LocalFSToolsName, "read_file"},
		{localtools.LocalFSToolsName, "read_file_range"},
		{localtools.LocalFSToolsName, "list_dir"},
		{localtools.LocalFSBrowseToolsName, "grep"},
		{localtools.LocalFSBrowseToolsName, "find_files"},
		{localtools.LocalFSBrowseToolsName, "stat_file"},
		{localtools.LocalFSBrowseToolsName, "count_stats"},
		{localtools.LocalFSBrowseToolsName, "list_dir"},
	},
	AxisFilesWrite: {
		{localtools.LocalFSToolsName, "write_file"},
		{localtools.LocalFSToolsName, "edit_file"},
		{localtools.LocalFSToolsName, "sed"},
	},
	AxisShell:        {{localtools.LocalExecToolsName, localtools.LocalExecToolsName}},
	AxisMissionsFire: {{missiontools.ToolsProviderName, missiontools.ToolNameStartMission}},
}

// reservedAxisTools are the bindings a network axis will take when a provider
// serves it again. Nothing is emitted for them today, so an envelope that sets
// a network axis is valid and inert rather than refused.
var reservedAxisTools = map[string][]toolRef{
	AxisNetworkRead: {
		{"native-web", "web_get"},
		{"native-web", "web_head"},
	},
	AxisNetworkWrite: {
		{"native-web", "web_post"},
		{"native-web", "web_put"},
		{"native-web", "web_patch"},
		{"native-web", "web_delete"},
	},
}

// TranspiledEnvelope is one envelope compiled to the policy the engine loads,
// plus what the compilation could not carry.
type TranspiledEnvelope struct {
	Policy *hitlservice.Policy
	// Notes are informational: intent the envelope stated that nothing in this
	// build serves yet. They are not defects.
	Notes []string
}

// transpileOptions carries what a caller splices into the emitted rules without
// the envelope having stated it.
type transpileOptions struct {
	// extraRules land at the missions slot, after always_allow and before every
	// axis grant.
	extraRules []hitlservice.Rule
}

// TranspileEnvelope compiles an envelope into a HITL policy. Rules are
// first-match-wins, so the emission order below IS the semantics: unconditional
// denies lead, conditional refinements precede the grants they carve out of,
// and an axis nobody set falls through to default_action.
func TranspileEnvelope(env Envelope) (TranspiledEnvelope, error) {
	return env.transpile(transpileOptions{})
}

func (env Envelope) transpile(opts transpileOptions) (TranspiledEnvelope, error) {
	var out TranspiledEnvelope
	fail := func(err error) (TranspiledEnvelope, error) {
		return TranspiledEnvelope{}, fmt.Errorf("agentdecl: [%s.%s]: %w", EnvelopeSection, env.Name, err)
	}

	rules := make([]hitlservice.Rule, 0, 32)
	rules = append(rules, standingRules(env.AlwaysDeny, hitlservice.ActionDeny)...)
	rules = append(rules, env.pathRules(AxisFilesWrite, hitlservice.ActionDeny)...)
	rules = append(rules, env.pathRules(AxisFilesRead, hitlservice.ActionDeny)...)
	rules = append(rules, env.pathRules(AxisFilesRead, hitlservice.ActionApprove)...)
	rules = append(rules, env.pathRules(AxisFilesWrite, hitlservice.ActionApprove)...)
	toolRules, err := env.toolPatternRules()
	if err != nil {
		return fail(err)
	}
	rules = append(rules, toolRules...)
	rules = append(rules, standingRules(env.AlwaysAllow, hitlservice.ActionAllow)...)
	rules = append(rules, opts.extraRules...)
	grantRules, err := env.grantRules(AxisMissionsFire)
	if err != nil {
		return fail(err)
	}
	rules = append(rules, grantRules...)
	for _, axis := range []string{AxisFilesRead, AxisFilesWrite} {
		grantRules, err := env.grantRules(axis)
		if err != nil {
			return fail(err)
		}
		rules = append(rules, grantRules...)
	}
	shellRules, err := env.shellRules()
	if err != nil {
		return fail(err)
	}
	rules = append(rules, shellRules...)

	defaultAction := hitlservice.ActionApprove
	if env.DefaultAction.Grant != "" {
		parsed, err := parseAction(env.DefaultAction.Grant)
		if err != nil {
			return fail(fmt.Errorf("default_action: %w", err))
		}
		defaultAction = parsed
	}

	compute, err := envelopeCompute(env.Compute)
	if err != nil {
		return fail(err)
	}
	attention, err := env.attentionBounds()
	if err != nil {
		return fail(err)
	}
	trusted, err := envelopeTrustedBinaries(env.TrustedBinaries)
	if err != nil {
		return fail(err)
	}

	out.Policy = &hitlservice.Policy{
		Version:         hitlservice.PolicySchemaVersion,
		DefaultAction:   defaultAction,
		Rules:           rules,
		Compute:         compute,
		Attention:       attention,
		TrustedBinaries: trusted,
	}
	out.Notes = env.notes()
	return out, nil
}

// notes reports every grant whose intent nothing in this build carries.
func (env Envelope) notes() []string {
	var out []string
	if env.DefaultAction.bounds() {
		out = append(out, fmt.Sprintf("default_action = %q states %s, and the policy schema has no field to carry it: default_action is one action, and only a rule holds timeout_s/on_timeout. A call that matches no rule keeps waiting on the operator's approval ceiling (`contenox config set approval-ceiling`). Bound the wait on the axis or tools pattern that emits the ask.",
			env.DefaultAction.Grant, waitPhrase(env.DefaultAction)))
	}
	for _, axis := range []string{AxisNetworkRead, AxisNetworkWrite} {
		grant, ok := env.Axes[axis]
		if !ok || grant.Grant == "" {
			continue
		}
		refs := reservedAxisTools[axis]
		names := make([]string, 0, len(refs))
		for _, ref := range refs {
			names = append(names, ref.tools+"/"+ref.tool)
		}
		out = append(out, fmt.Sprintf("%s = %q emits no rule: no provider serves it in this build. The intent is kept and binds to %s when one does.",
			axis, grant.Grant, strings.Join(names, ", ")))
	}
	return out
}

func waitPhrase(grant AxisGrant) string {
	parts := make([]string, 0, 2)
	if grant.Timeout != 0 {
		parts = append(parts, fmt.Sprintf("timeout = %q", hitlservice.FormatWait(grant.Timeout)))
	}
	if grant.OnTimeout != "" {
		parts = append(parts, fmt.Sprintf("on_timeout = %q", grant.OnTimeout))
	}
	return strings.Join(parts, " and ")
}

// pathRules emits the conditional half of a files axis: one rule per glob per
// bound tool, so a deny path outranks the grant below it.
func (env Envelope) pathRules(axis string, action hitlservice.Action) []hitlservice.Rule {
	grant, ok := env.Axes[axis]
	if !ok {
		return nil
	}
	globs := grant.DenyPaths
	if action == hitlservice.ActionApprove {
		globs = grant.ApprovePaths
	}
	if len(globs) == 0 {
		return nil
	}
	refs := axisTools[axis]
	out := make([]hitlservice.Rule, 0, len(globs)*len(refs))
	for _, glob := range globs {
		for _, ref := range refs {
			out = append(out, grant.bounded(hitlservice.Rule{
				Tools:  ref.tools,
				Tool:   ref.tool,
				Action: action,
				When:   []hitlservice.Condition{{Key: "path", Op: hitlservice.OpGlob, Value: glob}},
			}))
		}
	}
	return out
}

// grantRules emits an axis's unconditional floor. An unset axis emits nothing.
func (env Envelope) grantRules(axis string) ([]hitlservice.Rule, error) {
	grant, ok := env.Axes[axis]
	if !ok || grant.Grant == "" {
		return nil, nil
	}
	action, err := parseAction(grant.Grant)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", axis, err)
	}
	refs := axisTools[axis]
	out := make([]hitlservice.Rule, 0, len(refs))
	for _, ref := range refs {
		out = append(out, grant.bounded(hitlservice.Rule{Tools: ref.tools, Tool: ref.tool, Action: action}))
	}
	return out, nil
}

// shellRules emits the shell tiers in the one order that keeps them meaningful:
// the blacklist cannot be reached past, substitution is judged before any verb
// is trusted, the allowlist grants, ask_always claws back, and the grant is the
// floor an unrecognized command lands on.
func (env Envelope) shellRules() ([]hitlservice.Rule, error) {
	grant, ok := env.Axes[AxisShell]
	if !ok {
		return nil, nil
	}
	var out []hitlservice.Rule
	shellRule := func(action hitlservice.Action, key string, op hitlservice.ConditionOp, value string) {
		out = append(out, grant.bounded(hitlservice.Rule{
			Tools:  localtools.LocalExecToolsName,
			Tool:   localtools.LocalExecToolsName,
			Action: action,
			When:   []hitlservice.Condition{{Key: key, Op: op, Value: value}},
		}))
	}
	if len(grant.Blacklist) > 0 {
		shellRule(hitlservice.ActionDeny, "command", hitlservice.OpCommandBlacklist, strings.Join(grant.Blacklist, ","))
	}
	if grant.Substitution != "" && grant.Substitution != axisSubstitutionOff {
		action, err := parseAction(grant.Substitution)
		if err != nil {
			return nil, fmt.Errorf("%s.substitution: %w", AxisShell, err)
		}
		shellRule(action, "args", hitlservice.OpNoCommandSubstitution, "")
	}
	if len(grant.PrefixAllowlist) > 0 {
		shellRule(hitlservice.ActionAllow, "command", hitlservice.OpCommandPrefixAllowlist, strings.Join(grant.PrefixAllowlist, ","))
	}
	if len(grant.AskAlways) > 0 {
		shellRule(hitlservice.ActionApprove, "command", hitlservice.OpCommandAskAlways, strings.Join(grant.AskAlways, ","))
	}
	floor, err := env.grantRules(AxisShell)
	if err != nil {
		return nil, err
	}
	return append(out, floor...), nil
}

// toolPatternRules orders the tools table by specificity, then by action, then
// lexically, so the same table always emits the same bytes.
func (env Envelope) toolPatternRules() ([]hitlservice.Rule, error) {
	if len(env.Tools) == 0 {
		return nil, nil
	}
	type entry struct {
		pattern     string
		tools, tool string
		action      hitlservice.Action
		grant       AxisGrant
	}
	entries := make([]entry, 0, len(env.Tools))
	for _, pattern := range sortedKeys(env.Tools) {
		tools, tool, err := ToolPatternRef(pattern)
		if err != nil {
			return nil, err
		}
		grant := env.Tools[pattern]
		action, err := parseAction(grant.Grant)
		if err != nil {
			return nil, fmt.Errorf("tools.%s: %w", pattern, err)
		}
		entries = append(entries, entry{pattern: pattern, tools: tools, tool: tool, action: action, grant: grant})
	}
	rank := map[hitlservice.Action]int{hitlservice.ActionDeny: 0, hitlservice.ActionApprove: 1, hitlservice.ActionAllow: 2}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if si, sj := toolPatternSpecificity(a.tools, a.tool), toolPatternSpecificity(b.tools, b.tool); si != sj {
			return si < sj
		}
		if rank[a.action] != rank[b.action] {
			return rank[a.action] < rank[b.action]
		}
		return a.pattern < b.pattern
	})
	out := make([]hitlservice.Rule, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.grant.bounded(hitlservice.Rule{Tools: e.tools, Tool: e.tool, Action: e.action}))
	}
	return out, nil
}

// attentionBounds compiles missions.answer, which is the one axis whose carrier
// is not a rule: the mission toolset is HITL-exempt, and delegation is read
// from the attention block instead. Explicit attention keys win over what the
// axis would set.
func (env Envelope) attentionBounds() (*hitlservice.AttentionBounds, error) {
	answer, hasAxis := env.Axes[AxisMissionsAnswer]
	var bounds hitlservice.AttentionBounds
	declared := false
	if hasAxis && answer.Grant != "" {
		switch answer.Grant {
		case string(hitlservice.ActionAllow):
			bounds.AllowAgentAnswers, declared = true, true
		case string(hitlservice.ActionApprove):
			bounds.AllowAgentAnswers, bounds.AllowAgentApprovals, declared = true, true, true
		case string(hitlservice.ActionDeny):
			// A human answers; the block is omitted entirely.
		}
	}
	block := env.Attention
	if len(block) == 0 && !declared {
		return nil, nil
	}
	if b, ok, err := blockBool(block, "allow_agent_answers"); err != nil {
		return nil, err
	} else if ok {
		bounds.AllowAgentAnswers, declared = b, true
	}
	if b, ok, err := blockBool(block, "allow_agent_approvals"); err != nil {
		return nil, err
	} else if ok {
		bounds.AllowAgentApprovals, declared = b, true
	}
	n, _, err := blockInt(block, "max_agent_answers")
	if err != nil {
		return nil, err
	}
	bounds.MaxAgentAnswers = n
	n, _, err = blockInt(block, "max_agent_approvals")
	if err != nil {
		return nil, err
	}
	bounds.MaxAgentApprovals = n
	if !declared || (!bounds.AllowAgentAnswers && !bounds.AllowAgentApprovals) {
		return nil, nil
	}
	return &bounds, nil
}

func envelopeCompute(block map[string]any) (*hitlservice.ComputeBounds, error) {
	if len(block) == 0 {
		return nil, nil
	}
	out := &hitlservice.ComputeBounds{}
	var err error
	if out.MaxToolCalls, _, err = blockInt(block, "max_tool_calls"); err != nil {
		return nil, err
	}
	if out.MaxTokens, _, err = blockInt(block, "max_tokens"); err != nil {
		return nil, err
	}
	if out.MaxTurns, _, err = blockInt(block, "max_turns"); err != nil {
		return nil, err
	}
	onExhausted, _, err := blockString(block, "on_exhausted")
	if err != nil {
		return nil, err
	}
	out.OnExhausted = hitlservice.OnExhausted(onExhausted)
	if out.ModelAllowlist, err = blockStrings(block, "model_allowlist"); err != nil {
		return nil, err
	}
	if out.BackendAllowlist, err = blockStrings(block, "backend_allowlist"); err != nil {
		return nil, err
	}
	return out, nil
}

func envelopeTrustedBinaries(block map[string]any) (*hitlservice.TrustedBinaries, error) {
	if len(block) == 0 {
		return nil, nil
	}
	dirs, err := blockStrings(block, "dirs")
	if err != nil {
		return nil, err
	}
	out := &hitlservice.TrustedBinaries{Dirs: dirs}
	raw, ok := block["hashes"]
	if !ok {
		return out, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("trusted_binaries.hashes must be a table of path = sha256, got %T", raw)
	}
	out.Hashes = make(map[string]string, len(table))
	for _, k := range sortedKeys(table) {
		s, ok := table[k].(string)
		if !ok {
			return nil, fmt.Errorf("trusted_binaries.hashes[%q] must be a string, got %T", k, table[k])
		}
		out.Hashes[k] = s
	}
	return out, nil
}

func blockInt(block map[string]any, key string) (int, bool, error) {
	raw, ok := block[key]
	if !ok {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case int64:
		return int(v), true, nil
	case int:
		return v, true, nil
	default:
		return 0, false, fmt.Errorf("%s must be an integer, got %T", key, raw)
	}
}

func blockBool(block map[string]any, key string) (bool, bool, error) {
	raw, ok := block[key]
	if !ok {
		return false, false, nil
	}
	v, ok := raw.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s must be true or false, got %T", key, raw)
	}
	return v, true, nil
}

func blockString(block map[string]any, key string) (string, bool, error) {
	raw, ok := block[key]
	if !ok {
		return "", false, nil
	}
	v, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string, got %T", key, raw)
	}
	return strings.TrimSpace(v), true, nil
}

func blockStrings(block map[string]any, key string) ([]string, error) {
	raw, ok := block[key]
	if !ok {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %T", key, raw)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %T", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}
