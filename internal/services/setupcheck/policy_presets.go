package setupcheck

import (
	"fmt"
	"strings"
)

// CategoryPolicy groups issues about the HITL policy envelope on disk (the
// files behind hitl-policy-name), as distinct from model/backend readiness.
const CategoryPolicy = "policy"

// StalePolicyPresetsCode is the issue code for a policy file that predates the
// toolsets this build ships. It is deliberately absent from blockingIssue's
// list: an envelope that asks for approval too often is annoying, never a
// reason to refuse to run.
const StalePolicyPresetsCode = "hitl_policy_presets_stale"

// StalePolicyPreset names one policy file on disk together with the shipped
// toolsets it has no rule for. The caller (the CLI, which owns the embedded
// presets) does the detection; this package only knows how to report it.
type StalePolicyPreset struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	// Toolsets are the toolset names the file never mentions.
	Toolsets []string `json:"toolsets"`
	// Effect describes, in the operator's terms, what those toolsets get
	// instead of a rule (e.g. "every call stops for approval").
	Effect string `json:"effect,omitempty"`
}

// AddStalePolicyPresetIssue appends a WARNING for policy files that predate this
// build's toolsets, and returns the updated Result. With nothing stale it
// returns the Result unchanged.
//
// Warning, never error, and never a blocking code: the runtime is perfectly
// usable with a stale envelope — every affected call just stops to ask. The
// operator's file is theirs, so the only correct move is to name the toolsets
// and the verb that refreshes the shipped presets, then get out of the way.
func AddStalePolicyPresetIssue(r Result, stale []StalePolicyPreset, refreshCommand string) Result {
	if len(stale) == 0 {
		return r
	}
	parts := make([]string, 0, len(stale))
	for _, s := range stale {
		if len(s.Toolsets) == 0 {
			continue
		}
		effect := s.Effect
		if effect == "" {
			effect = "those calls fall through to default_action"
		}
		parts = append(parts, fmt.Sprintf("%s has no rule for %s (%s)", s.Name, strings.Join(s.Toolsets, ", "), effect))
	}
	if len(parts) == 0 {
		return r
	}

	// Fresh slice: callers hold the Result by value, and appending in place
	// would write into their backing array (same reason OverlayEffectiveDefaults
	// rebuilds).
	issues := make([]Issue, 0, len(r.Issues)+1)
	issues = append(issues, r.Issues...)
	issues = append(issues, Issue{
		Code:     StalePolicyPresetsCode,
		Severity: "warning",
		Category: CategoryPolicy,
		Message: fmt.Sprintf("HITL policy preset(s) on disk predate this build's toolsets: %s. "+
			"Your envelope is never overwritten automatically — refresh the shipped presets when you are ready.",
			strings.Join(parts, "; ")),
		CLICommand: refreshCommand,
	})
	r.Issues = issues
	return r
}
