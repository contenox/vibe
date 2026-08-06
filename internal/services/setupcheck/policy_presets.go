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

// TrustedBinaryIssueCode is the issue code for a policy whose
// trusted_binaries declarations no longer describe this host. Deliberately
// absent from blockingIssue's list: an entry that stopped matching costs an
// approval card, never a refusal to run.
const TrustedBinaryIssueCode = "hitl_trusted_binaries_drift"

// TrustedBinaryDrift names one policy file together with the declaration
// findings for it. The caller (the CLI, which knows the policy search path)
// does the detection; this package only knows how to report it.
type TrustedBinaryDrift struct {
	Path string `json:"path"`
	// Findings are one rendered line per entry that is missing, mismatched,
	// unreadable, or unreachable.
	Findings []string `json:"findings"`
}

// AddTrustedBinaryIssue appends a warning for declarations that no longer
// match this host, returning the Result unchanged when nothing drifted. The
// runtime's own answer for a drifted entry is a refusal (the allow is
// withdrawn and the call asks a human), so this only ever explains an
// otherwise puzzling approval card and names the verb that fixes it.
func AddTrustedBinaryIssue(r Result, drift []TrustedBinaryDrift, refreshCommand string) Result {
	parts := make([]string, 0, len(drift))
	for _, d := range drift {
		if len(d.Findings) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", d.Path, strings.Join(d.Findings, "; ")))
	}
	if len(parts) == 0 {
		return r
	}
	// Fresh slice, since callers hold the Result by value.
	issues := make([]Issue, 0, len(r.Issues)+1)
	issues = append(issues, r.Issues...)
	issues = append(issues, Issue{
		Code:     TrustedBinaryIssueCode,
		Severity: "warning",
		Category: CategoryPolicy,
		Message: fmt.Sprintf("HITL trusted-binary declaration(s) no longer describe this host: %s. "+
			"Calls naming those binaries are refused an allow and stop for approval instead. "+
			"Re-declare only after verifying the change was a legitimate upgrade.",
			strings.Join(parts, " | ")),
		CLICommand: refreshCommand,
	})
	r.Issues = issues
	return r
}

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
// more often, never a reason to refuse to run. Each stale file is named by
// full path with its default_action fall-through; a policy never gates tool
// visibility (the chain's tools allowlist does).
func AddStalePolicyPresetIssue(r Result, stale []StalePolicyPreset, refreshCommand string) Result {
	if len(stale) == 0 {
		return r
	}
	parts := make([]string, 0, len(stale))
	for _, s := range stale {
		if len(s.Toolsets) == 0 {
			continue
		}
		path := s.Path
		if path == "" {
			path = s.Name
		}
		part := fmt.Sprintf("%s predates toolsets %s — calls to them fall to this file's default_action", path, strings.Join(s.Toolsets, ", "))
		if s.Effect != "" {
			part += fmt.Sprintf(" (%s)", s.Effect)
		}
		parts = append(parts, part)
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
			"The tools stay visible to the model — a policy only sets the review posture, never availability. "+
			"Your envelope is never overwritten automatically — refresh the shipped presets when you are ready.",
			strings.Join(parts, "; ")),
		CLICommand: refreshCommand,
	})
	r.Issues = issues
	return r
}
