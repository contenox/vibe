package setupcheck

import (
	"fmt"
	"strings"
)

// CategoryPolicy groups issues about the HITL policy envelope on disk (the
// files behind hitl-policy-name), as distinct from model/backend readiness.
const CategoryPolicy = "policy"

// StalePolicyPresetsCode is the issue code for a policy file that predates
// the toolsets this build ships. Deliberately absent from blockingIssue's list.
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

// AddStalePolicyPresetIssue appends a warning for policy files that predate
// this build's toolsets, returning the Result unchanged when nothing is
// stale. Never a blocking code: a stale envelope just asks for approval
// more often, never a reason to refuse to run.
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

	// Fresh slice, since callers hold the Result by value.
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
