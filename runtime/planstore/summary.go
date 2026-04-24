package planstore

// SummaryDoc is the typed contract emitted by the summarizer chain and persisted
// in PlanStep.Summary (as JSON). Required fields: Outcome (enum), Summary,
// HandoverForNext. Artifacts and Caveats are optional (may be empty arrays).
//
// This contract is shared between:
//
//   - localtools.PlanSummaryTools (validates + writes it on success, rejects
//     invalid output so the DAG's repair branch can try again).
//   - planservice.NewPlanStepMacroVars (reads the prior step's Summary to
//     populate {{var:previous_output}} / {{var:previous_handover}} /
//     {{var:previous_artifacts}} / {{var:previous_caveats}} for the next
//     step's seed prompt).
//
// Kept in planstore — not in planservice or localtools — to avoid either side
// owning the contract the other depends on. Adding a required field here is a
// breaking change (requires migration of persisted rows); adding an optional
// field is safe.
type SummaryDoc struct {
	Outcome         string            `json:"outcome"`
	Summary         string            `json:"summary"`
	Artifacts       []SummaryArtifact `json:"artifacts"`
	HandoverForNext string            `json:"handover_for_next"`
	Caveats         []string          `json:"caveats"`
}

// SummaryArtifact is one entry in SummaryDoc.Artifacts. All fields optional
// individually; a well-formed entry typically has Kind and Ref.
type SummaryArtifact struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// ValidOutcomes locks the outcome enum. Kept as a function (not a package var)
// to avoid data-race concerns if a caller ever mutates a returned map.
func ValidOutcomes() map[string]struct{} {
	return map[string]struct{}{
		"success": {},
		"partial": {},
		"blocked": {},
	}
}
