package contenoxcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/setupcheck"
)

// RefreshPoliciesCommand rewrites the preset copies an operator already has and
// nothing else.
const RefreshPoliciesCommand = "contenox init --refresh-policies"

const catchAllToolset = "*"

type stalePolicyPreset struct {
	Name string
	Path string
	// Toolsets are the shipped toolset names this file never mentions, sorted.
	Toolsets []string
	// DefaultAction is the on-disk file's default_action; empty means the loader's
	// fail-closed default.
	DefaultAction string
}

func policyToolsets(raw []byte) (map[string]bool, bool) {
	var doc struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	// An unrecognized field shape reports "no claim" for the whole document.
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
			// A wildcard on both axes covers every call.
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

func policyDefaultAction(raw []byte) string {
	var doc struct {
		DefaultAction string `json:"default_action"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.DefaultAction)
}

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

// policyDirs is the envelope search path, strongest first: the resolved
// .contenox dir, then ~/.contenox, then the envelopes rendered from agent
// declarations, so a hand-written envelope shadows a rendered one.
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
	// Joined only onto a named dir: filepath.Join("", ".generated") is relative.
	addGenerated := func(dir string) {
		if dir == "" {
			return
		}
		add(filepath.Join(dir, agentdecl.GeneratedDirName))
	}
	home, homeErr := os.UserHomeDir()
	globalDir := ""
	if homeErr == nil {
		globalDir = filepath.Join(home, ".contenox")
	}
	add(primaryDir)
	add(globalDir)
	addGenerated(primaryDir)
	addGenerated(globalDir)
	return dirs
}

// operatorPolicyDirs is policyDirs without the rendered halves: the two places
// a policy file belongs to whoever put it there. Staleness and the refresh verb
// speak only about those — a file under .generated is the transpiler's, and
// judging or rewriting one would report the envelope's own render as an
// operator's stale copy and then clobber it with a preset.
func operatorPolicyDirs(primaryDir string) []string {
	all := policyDirs(primaryDir)
	out := make([]string, 0, len(all))
	for _, dir := range all {
		if filepath.Base(dir) == agentdecl.GeneratedDirName {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// isRenderedPolicyPath reports whether a resolved policy file is the
// transpiler's rather than an operator's.
func isRenderedPolicyPath(path string) bool {
	return filepath.Base(filepath.Dir(path)) == agentdecl.GeneratedDirName
}

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

func stalePolicyPresets(dirs []string, gated map[string]bool) []stalePolicyPreset {
	var out []stalePolicyPreset
	for _, p := range HITLPolicyPresets {
		if stale, ok := stalePolicyPresetNamed(p.Name, dirs, gated); ok {
			out = append(out, stale)
		}
	}
	return out
}

func staleFallthrough(defaultAction string) string {
	switch defaultAction {
	case "", "approve":
		// Empty is the loader's fail-closed default.
		return "every call stops for approval"
	case "deny":
		return "every call is denied"
	case "allow":
		return "every call runs unreviewed"
	default:
		return fmt.Sprintf("every call falls through to default_action %q", defaultAction)
	}
}

func stalePolicyNotice(name string, dirs []string, gated map[string]bool) string {
	stale, ok := stalePolicyPresetNamed(name, dirs, gated)
	if !ok {
		return ""
	}
	return fmt.Sprintf("policy: %s predates %s — calls to them fall to its default_action (%s); the tools stay visible to the model · refresh: %s",
		stale.Path, strings.Join(stale.Toolsets, ", "), staleFallthrough(stale.DefaultAction), RefreshPoliciesCommand)
}

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

// refreshPoliciesOnSearchPath rewrites the preset copies already on the search
// path. Nothing is created: a directory that holds no preset is served by the
// rendered envelopes behind it.
func refreshPoliciesOnSearchPath(out io.Writer, primaryDir string) error {
	for _, dir := range operatorPolicyDirs(primaryDir) {
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

func runRefreshPolicies(out io.Writer, primaryDir string, gated map[string]bool) error {
	if err := refreshPoliciesOnSearchPath(out, primaryDir); err != nil {
		return err
	}
	if stale := stalePolicyPresets(operatorPolicyDirs(primaryDir), gated); len(stale) > 0 {
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
