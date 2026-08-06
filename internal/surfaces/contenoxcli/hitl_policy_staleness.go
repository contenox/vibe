package contenoxcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/setupcheck"
)

// RefreshPoliciesCommand rewrites the shipped HITL policy presets and nothing
// else — unlike `contenox init --force`, which also overwrites every chain
// file in ~/.contenox.
const RefreshPoliciesCommand = "contenox init --refresh-policies"

// catchAllToolset is the value a rule uses to match every toolset. The
// evaluator treats an EMPTY tools field the same way (policy.go: `r.Tools == ""
// || r.Tools == "*"`), so both map onto this marker here.
const catchAllToolset = "*"

// stalePolicyPreset is a policy file on disk that carries no rule for a
// toolset the same-named preset in this build rules on — i.e. the envelope
// predates that toolset. The file itself is never rewritten; this record lets
// a surface say so instead of the operator discovering it one approval card
// at a time.
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
// mention, and whether the document could be read at all. Parsing is
// deliberately tolerant (minimal shape, per-rule error skipping) rather than
// the strict loader's: a file that fails to parse reports ok=false, so every
// caller stays silent rather than risk a wrong warning about someone's
// security boundary.
func policyToolsets(raw []byte) (map[string]bool, bool) {
	var doc struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	// An unexpected field shape means this rule isn't understood, and it might
	// be the very rule covering the toolset in question — so the whole
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
			// A wildcard on both axes covers every call; one pinned to a
			// single tool name covers only that name.
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

// missingPolicyToolsets returns the toolsets `shipped` writes rules for that
// `onDisk` mentions nowhere at all — a rule that denies a toolset, or a
// catch-all rule, still counts as mentioning it. An unparseable file reports
// no claim. Names in gated are skipped: a toolset invisible this invocation
// (betaGatedToolsets) cannot make an envelope stale.
func missingPolicyToolsets(shipped, onDisk []byte, gated map[string]bool) []string {
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
		if name == catchAllToolset || have[name] || gated[name] {
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
// for name predates this build's toolsets, ignoring gated (invisible) ones.
func stalePolicyPresetNamed(name string, dirs []string, gated map[string]bool) (stalePolicyPreset, bool) {
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
	missing := missingPolicyToolsets([]byte(shipped), raw, gated)
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
func stalePolicyPresets(dirs []string, gated map[string]bool) []stalePolicyPreset {
	var out []stalePolicyPreset
	for _, p := range HITLPolicyPresets {
		if stale, ok := stalePolicyPresetNamed(p.Name, dirs, gated); ok {
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
// rules — by refresh, or by the operator's own hand. The line names the file
// by full path and states the effect as that file's default_action
// fall-through; a policy never gates tool visibility (the chain's tools
// allowlist does).
func stalePolicyNotice(name string, dirs []string, gated map[string]bool) string {
	stale, ok := stalePolicyPresetNamed(name, dirs, gated)
	if !ok {
		return ""
	}
	return fmt.Sprintf("policy: %s predates %s — calls to them fall to its default_action (%s); the tools stay visible to the model · refresh: %s",
		stale.Path, strings.Join(stale.Toolsets, ", "), staleFallthrough(stale.DefaultAction), RefreshPoliciesCommand)
}

// stalePolicyPresetIssues adapts the detector to setupcheck's issue vocabulary
// for `contenox doctor`. Callers pass the same search path the engine's policy
// loader uses (policyDirs).
func stalePolicyPresetIssues(dirs []string, gated map[string]bool) []setupcheck.StalePolicyPreset {
	stale := stalePolicyPresets(dirs, gated)
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

// refreshPoliciesOnSearchPath rewrites the shipped presets in every directory
// the policy loader consults for primaryDir (policyDirs): ~/.contenox gets
// every preset; a dir ahead of it only the presets it already holds, so a
// shadowing copy is refreshed without widening the shadow. Each written path
// is printed to out. A non-home write failure is a warning, not an abort:
// home must still refresh, and stalePolicyPresets names the survivor.
func refreshPoliciesOnSearchPath(out io.Writer, primaryDir string) error {
	home, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	for _, dir := range policyDirs(primaryDir) {
		if dir == home {
			// A failure at home is fatal: no copy resolves behind it.
			if err := writeEmbeddedHITLPolicies(dir, true); err != nil {
				return err
			}
			for _, p := range HITLPolicyPresets {
				fmt.Fprintf(out, "  Refreshed %s\n", filepath.Join(dir, p.Name))
			}
			continue
		}
		written, refreshErr := refreshExistingHITLPolicies(dir)
		for _, path := range written {
			fmt.Fprintf(out, "  Refreshed %s\n", path)
		}
		if refreshErr != nil {
			fmt.Fprintf(out, "  warning: %v\n", refreshErr)
		}
	}
	return nil
}

// runRefreshPolicies rewrites the HITL policy presets along the loader's whole
// search path and touches nothing else — no chains, no config, no database. It
// is the only path that replaces a policy file that cannot be proven
// untouched. primaryDir is the resolved workspace .contenox, whose copies
// shadow ~/.contenox (policyDirs order). gated toolsets are skipped in the
// residual report, as everywhere the detector runs.
func runRefreshPolicies(out io.Writer, primaryDir string, gated map[string]bool) error {
	if err := refreshPoliciesOnSearchPath(out, primaryDir); err != nil {
		return err
	}
	// The detector, not the writer, earns the closing line: a copy that
	// survived the rewrite is still the one the loader reads.
	if stale := stalePolicyPresets(policyDirs(primaryDir), gated); len(stale) > 0 {
		for _, s := range stale {
			fmt.Fprintf(out, "  Still stale: %s predates %s — %s\n",
				s.Path, strings.Join(s.Toolsets, ", "), staleFallthrough(s.DefaultAction))
		}
		fmt.Fprintln(out, "The loader still reads these copies; edit or remove them, then re-run the refresh.")
		return nil
	}
	fmt.Fprintln(out, "HITL policy presets are now this build's. Chains, config and sessions were not touched.")
	return nil
}
