package contenoxcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/services/setupcheck"
)

// RefreshPoliciesCommand is the ONE verb that rewrites the shipped HITL policy
// presets and nothing else.
//
// `contenox init --force` also refreshes them, but it overwrites every chain
// file in ~/.contenox on the way — far too much collateral to put in front of
// someone whose only problem is a policy envelope that predates a toolset. So
// the notice names this narrower verb instead.
const RefreshPoliciesCommand = "contenox init --refresh-policies"

// catchAllToolset is the value a rule uses to match every toolset. The
// evaluator treats an EMPTY tools field the same way (policy.go: `r.Tools == ""
// || r.Tools == "*"`), so both map onto this marker here.
const catchAllToolset = "*"

// stalePolicyPreset is one policy file on disk that carries no rule AT ALL for
// toolsets the preset of the same name in THIS build rules on.
//
// It is not "the operator is wrong": it is "this envelope was written before
// those toolsets existed". The file stays exactly as it is — see
// upgradeEmbeddedHITLPolicies for why an unprovable file is never rewritten —
// and this record exists so a surface can SAY so instead of letting the
// operator discover it one approval card at a time.
type stalePolicyPreset struct {
	Name string
	Path string
	// Toolsets are the shipped toolset names this file never mentions, sorted.
	Toolsets []string
	// DefaultAction is the on-disk file's default_action, i.e. what those
	// unmentioned toolsets actually get. Empty means the loader's fail-closed
	// default (approve).
	DefaultAction string
}

// policyToolsets returns the set of toolset names a policy document's rules
// mention, and whether the document could be read at all.
//
// Deliberately tolerant: an operator's file is parsed with a minimal shape and
// per-rule error skipping rather than the strict loader, because the question
// asked here ("does this file know about gointel?") must not turn into a second
// validator with its own opinions about the operator's envelope. A file that
// cannot be parsed reports ok=false, and every caller then says nothing — a
// wrong warning about someone's security boundary is worse than no warning.
func policyToolsets(raw []byte) (map[string]bool, bool) {
	var doc struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	// A field of an unexpected shape (a list where a string belongs, say) means
	// this rule is not something we understand, and a rule we cannot read might
	// be the very rule that covers the toolset in question — so the whole
	// document reports "no claim" rather than guessing.
	field := func(rule map[string]json.RawMessage, key string) (string, bool) {
		v, present := rule[key]
		if !present {
			return "", true
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
	out := map[string]bool{}
	for _, rule := range doc.Rules {
		tools, ok := field(rule, "tools")
		if !ok {
			return nil, false
		}
		if tools == "" || tools == catchAllToolset {
			// A wildcard on BOTH axes covers every call, so nothing can fall
			// through to default_action and nothing is missing. A wildcard
			// toolset pinned to one tool name (tools:"*", tool:"read_file")
			// covers only that name, so it is not evidence of anything.
			tool, ok := field(rule, "tool")
			if !ok {
				return nil, false
			}
			if tool == "" || tool == catchAllToolset {
				out[catchAllToolset] = true
			}
			continue
		}
		out[tools] = true
	}
	return out, true
}

// policyDefaultAction reads a policy file's default_action, "" when absent or
// unreadable.
func policyDefaultAction(raw []byte) string {
	var doc struct {
		DefaultAction string `json:"default_action"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.DefaultAction)
}

// missingPolicyToolsets is THE DETECTOR: the toolsets `shipped` writes rules for
// that `onDisk` does not mention at all.
//
// The rule, stated as the confidence claim it makes: an operator who
// deliberately DELETED the gointel rule from their envelope is vanishingly
// rarer than an operator whose envelope was written before gointel existed —
// and the two are distinguishable from nothing else on disk. Everything weaker
// than "the toolset appears nowhere in this file's rules" is deliberately NOT a
// signal:
//   - a reordered or reworded rule mentions the toolset → not missing;
//   - a rule that DENIES the toolset mentions it → not missing (a deny is a
//     decision, and re-suggesting the shipped preset over it would be telling
//     an operator to loosen their own envelope);
//   - a catch-all rule matches every toolset → nothing falls through, so
//     nothing is missing;
//   - an unparseable file → no claim at all.
func missingPolicyToolsets(shipped, onDisk []byte) []string {
	want, ok := policyToolsets(shipped)
	if !ok {
		return nil
	}
	have, ok := policyToolsets(onDisk)
	if !ok || have[catchAllToolset] {
		return nil
	}
	var missing []string
	for name := range want {
		if name == catchAllToolset || have[name] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// policyDirs mirrors the policy search path hitlPolicySource builds (engine.go):
// the resolved .contenox dir first, then $HOME/.contenox, first match wins.
// Duplicated here on purpose so the staleness check needs no engine — and
// deduplicated because beam resolves both to the same directory.
func policyDirs(primaryDir string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	add(primaryDir)
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".contenox"))
	}
	return dirs
}

// readPolicyFile returns the file the policy loader would actually read for
// name: the first one that exists along dirs.
func readPolicyFile(dirs []string, name string) (path string, raw []byte, ok bool) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return p, data, true
	}
	return "", nil, false
}

// stalePolicyPresetNamed reports whether the policy file that WOULD BE LOADED
// for name predates this build's toolsets.
func stalePolicyPresetNamed(name string, dirs []string) (stalePolicyPreset, bool) {
	var shipped string
	for _, p := range HITLPolicyPresets {
		if p.Name == name {
			shipped = p.Content
			break
		}
	}
	if shipped == "" {
		return stalePolicyPreset{}, false
	}
	path, raw, ok := readPolicyFile(dirs, name)
	if !ok {
		// Not on disk yet: the seeder writes this build's copy, so there is
		// nothing stale to report.
		return stalePolicyPreset{}, false
	}
	missing := missingPolicyToolsets([]byte(shipped), raw)
	if len(missing) == 0 {
		return stalePolicyPreset{}, false
	}
	return stalePolicyPreset{
		Name:          name,
		Path:          path,
		Toolsets:      missing,
		DefaultAction: policyDefaultAction(raw),
	}, true
}

// stalePolicyPresets checks every preset this build ships against the copy the
// loader would read for it.
func stalePolicyPresets(dirs []string) []stalePolicyPreset {
	var out []stalePolicyPreset
	for _, p := range HITLPolicyPresets {
		if stale, ok := stalePolicyPresetNamed(p.Name, dirs); ok {
			out = append(out, stale)
		}
	}
	return out
}

// staleFallthrough says what actually happens to a call to an unmentioned
// toolset, in the operator's terms — the approval-card-per-read symptom is the
// "approve" case, and it is the one that made this whole check necessary.
func staleFallthrough(defaultAction string) string {
	switch defaultAction {
	case "", "approve":
		// Empty is the loader's fail-closed default (hitlservice policy.go).
		return "every call stops for approval"
	case "deny":
		return "every call is denied"
	case "allow":
		return "every call runs unreviewed"
	default:
		return fmt.Sprintf("every call falls through to default_action %q", defaultAction)
	}
}

// stalePolicyNotice renders the ONE muted line a surface prints on the way in,
// or "" when there is nothing to say. It stops the moment the file gains the
// rules — by refresh, or by the operator's own hand.
func stalePolicyNotice(name string, dirs []string) string {
	stale, ok := stalePolicyPresetNamed(name, dirs)
	if !ok {
		return ""
	}
	return fmt.Sprintf("policy: %s predates %s — %s · refresh: %s",
		stale.Name, strings.Join(stale.Toolsets, ", "), staleFallthrough(stale.DefaultAction), RefreshPoliciesCommand)
}

// stalePolicyPresetIssues adapts the detector to setupcheck's issue vocabulary
// for `contenox doctor`. Callers pass the same search path the engine's policy
// loader uses (policyDirs).
func stalePolicyPresetIssues(dirs []string) []setupcheck.StalePolicyPreset {
	stale := stalePolicyPresets(dirs)
	if len(stale) == 0 {
		return nil
	}
	out := make([]setupcheck.StalePolicyPreset, 0, len(stale))
	for _, s := range stale {
		out = append(out, setupcheck.StalePolicyPreset{
			Name:     s.Name,
			Path:     s.Path,
			Toolsets: s.Toolsets,
			Effect:   staleFallthrough(s.DefaultAction),
		})
	}
	return out
}

// runRefreshPolicies rewrites the HITL policy presets in ~/.contenox from this
// build and touches NOTHING else — no chains, no config, no database.
//
// This is the only path that replaces a policy file we cannot prove is
// untouched, and it exists precisely so that the automatic path never has to:
// the operator types the verb, so the operator made the decision.
func runRefreshPolicies(out io.Writer) error {
	dir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	if err := writeEmbeddedHITLPolicies(dir, true); err != nil {
		return err
	}
	for _, p := range HITLPolicyPresets {
		fmt.Fprintf(out, "  Refreshed %s\n", filepath.Join(dir, p.Name))
	}
	fmt.Fprintln(out, "HITL policy presets are now this build's. Chains, config and sessions were not touched.")
	return nil
}
