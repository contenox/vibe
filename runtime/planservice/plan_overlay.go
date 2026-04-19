package planservice

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/runtime/planstore"
)

// PlanStepMacroVars holds values for MacroEnv {{var:…}} expansion in the compiled noop seed
// (see plancompile.seedPromptTemplate). Use [NewPlanStepMacroVars] then [PlanStepMacroVars.TemplateVars]
// for taskengine.MergeTemplateVars.
//
// PreviousOutput / PreviousHandover / PreviousArtifacts / PreviousCaveats are all
// rendered from the prior completed step's persisted Summary JSON. When the
// prior step's Summary is empty (legacy row or fallback path persisted only
// ExecutionResult), PreviousOutput falls back to the legacy string so existing
// plans keep working.
type PlanStepMacroVars struct {
	PlanGoal           string
	PlanOverview       string
	PlanProgress       string
	StepOrdinal        string
	StepTotal          string
	CurrentStep        string
	NextStep           string
	PlanHandover       string
	PreviousOutput     string
	PreviousHandover   string
	PreviousArtifacts  string
	PreviousCaveats    string
	PriorFailureDetail string
	// RepoContext is the rendered bullet block from the plan's persisted
	// RepoContextJSON (see [planstore.RepoContext] / chain-plan-explorer.json).
	// Empty when the plan was created without --explore.
	RepoContext string
}

// NewPlanStepMacroVars fills macro vars for the pending plan step (goal, full plan, next step, handover, etc.).
func NewPlanStepMacroVars(plan *planstore.Plan, steps []*planstore.PlanStep, pending *planstore.PlanStep) PlanStepMacroVars {
	if plan == nil || pending == nil {
		return PlanStepMacroVars{}
	}
	prev := priorStepSummary(steps, pending.Ordinal)
	return PlanStepMacroVars{
		PlanGoal:           strings.TrimSpace(plan.Goal),
		PlanOverview:       formatPlanOverview(steps),
		PlanProgress:       formatPlanProgress(steps, pending.Ordinal),
		StepOrdinal:        strconv.Itoa(pending.Ordinal),
		StepTotal:          strconv.Itoa(len(steps)),
		CurrentStep:        strings.TrimSpace(pending.Description),
		NextStep:           nextStepDescription(steps, pending.Ordinal),
		PlanHandover:       planHandoverText(pending.Ordinal),
		PreviousOutput:     prev.output,
		PreviousHandover:   prev.handover,
		PreviousArtifacts:  prev.artifacts,
		PreviousCaveats:    prev.caveats,
		PriorFailureDetail: strings.TrimSpace(pending.LastFailureSummary),
		RepoContext:        renderRepoContextBlock(plan.RepoContextJSON),
	}
}

// TemplateVars maps field names to MacroEnv keys expected by the seed prompt.
// Each summarizer-chain task that uses any of these macro keys must find them
// populated in ctx (MergeTemplateVars overlay), so we ALWAYS include every key
// — empty string when the prior step didn't populate it — to avoid "template
// var not set" errors on variants of the summarizer chain.
func (v PlanStepMacroVars) TemplateVars() map[string]string {
	if v.StepOrdinal == "" {
		return map[string]string{}
	}
	return map[string]string{
		"plan_goal":            v.PlanGoal,
		"plan_overview":        v.PlanOverview,
		"plan_progress":        v.PlanProgress,
		"step_ordinal":         v.StepOrdinal,
		"step_total":           v.StepTotal,
		"current_step":         v.CurrentStep,
		"next_step":            v.NextStep,
		"plan_handover":        v.PlanHandover,
		"previous_output":      v.PreviousOutput,
		"previous_handover":    v.PreviousHandover,
		"previous_artifacts":   v.PreviousArtifacts,
		"previous_caveats":     v.PreviousCaveats,
		"prior_failure_detail": v.PriorFailureDetail,
		// execution_context is static copy for the seed prompt so the executor
		// sees task-engine boundaries (hooks-only) without relying on the model's persona alone.
		"execution_context": planExecutionContextBlock(),
		// repo_context is the rendered bullet block from chain-plan-explorer.json
		// output. Always present (may be empty) so seed templates referencing
		// {{var:repo_context}} never fail with "template var not set".
		"repo_context": v.RepoContext,
	}
}

// planExecutionContextBlock is injected into compiled plan seed prompts via {{var:execution_context}}.
func planExecutionContextBlock() string {
	return "Execution boundary: you run inside the Contenox task engine. Only registered hooks and tools exist (filesystem, shell, git, MCP, HTTP/OpenAPI as configured). There is no separate product UI or browser unless provided by a tool. If work needs a human-only action, say so briefly and stop; do not claim it is done."
}

// priorSummaryView is the set of rendered strings extracted from a prior step's
// persisted Summary JSON (schema: planstore.SummaryDoc). When the prior step's
// Summary is empty, output falls back to ExecutionResult for backwards compat.
type priorSummaryView struct {
	output    string // summary field, or fallback to ExecutionResult
	handover  string // handover_for_next field
	artifacts string // rendered bullet list of artifacts[]
	caveats   string // rendered bullet list of caveats[]
}

func priorStepSummary(steps []*planstore.PlanStep, pendingOrdinal int) priorSummaryView {
	if pendingOrdinal <= 1 {
		return priorSummaryView{}
	}
	want := pendingOrdinal - 1
	var prev *planstore.PlanStep
	for _, st := range steps {
		if st.Ordinal == want {
			prev = st
			break
		}
	}
	if prev == nil {
		return priorSummaryView{}
	}
	if strings.TrimSpace(prev.Summary) == "" {
		// Legacy row or fallback path: ExecutionResult is all we have.
		return priorSummaryView{output: strings.TrimSpace(prev.ExecutionResult)}
	}
	var doc planstore.SummaryDoc
	if err := json.Unmarshal([]byte(prev.Summary), &doc); err != nil {
		return priorSummaryView{output: strings.TrimSpace(prev.ExecutionResult)}
	}
	return priorSummaryView{
		output:    strings.TrimSpace(doc.Summary),
		handover:  strings.TrimSpace(doc.HandoverForNext),
		artifacts: renderArtifacts(doc.Artifacts),
		caveats:   renderCaveats(doc.Caveats),
	}
}

func renderArtifacts(artifacts []planstore.SummaryArtifact) string {
	if len(artifacts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range artifacts {
		kind := strings.TrimSpace(a.Kind)
		ref := strings.TrimSpace(a.Ref)
		note := strings.TrimSpace(a.Note)
		switch {
		case kind != "" && ref != "" && note != "":
			fmt.Fprintf(&b, "- [%s] %s — %s\n", kind, ref, note)
		case kind != "" && ref != "":
			fmt.Fprintf(&b, "- [%s] %s\n", kind, ref)
		case ref != "":
			fmt.Fprintf(&b, "- %s\n", ref)
		case note != "":
			fmt.Fprintf(&b, "- %s\n", note)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderCaveats(caveats []string) string {
	if len(caveats) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range caveats {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", c)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatPlanProgress renders a rolling view of completed / skipped / failed
// steps up to (but not including) pendingOrdinal. Each completed step with a
// Summary contributes its one-line summary; steps without a Summary fall back
// to ExecutionResult. Steps still pending or beyond pendingOrdinal are omitted.
func formatPlanProgress(steps []*planstore.PlanStep, pendingOrdinal int) string {
	if len(steps) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range steps {
		if s.Ordinal >= pendingOrdinal {
			break
		}
		marker := "•"
		switch s.Status {
		case planstore.StepStatusCompleted:
			marker = "✓"
		case planstore.StepStatusFailed:
			marker = "✗"
		case planstore.StepStatusSkipped:
			marker = "⏭"
		}
		desc := strings.TrimSpace(s.Description)
		snippet := priorStepOneLine(s)
		if snippet == "" {
			fmt.Fprintf(&b, "%d. %s %s\n", s.Ordinal, marker, desc)
			continue
		}
		fmt.Fprintf(&b, "%d. %s %s — %s\n", s.Ordinal, marker, desc, snippet)
	}
	return strings.TrimRight(b.String(), "\n")
}

// priorStepOneLine extracts a one-line narrative from a step's persisted
// Summary JSON, falling back to the first line of ExecutionResult when the
// JSON is absent or malformed.
func priorStepOneLine(s *planstore.PlanStep) string {
	if strings.TrimSpace(s.Summary) != "" {
		var doc planstore.SummaryDoc
		if err := json.Unmarshal([]byte(s.Summary), &doc); err == nil {
			line := strings.TrimSpace(doc.Summary)
			if line != "" {
				return firstLine(line)
			}
		}
	}
	return firstLine(strings.TrimSpace(s.ExecutionResult))
}

func firstLine(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func formatPlanOverview(steps []*planstore.PlanStep) string {
	if len(steps) == 0 {
		return "(no steps)"
	}
	var b strings.Builder
	for _, s := range steps {
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = "(empty step)"
		}
		fmt.Fprintf(&b, "%d. %s\n", s.Ordinal, desc)
	}
	return strings.TrimSpace(b.String())
}

func nextStepDescription(steps []*planstore.PlanStep, currentOrdinal int) string {
	want := currentOrdinal + 1
	for _, s := range steps {
		if s.Ordinal != want {
			continue
		}
		d := strings.TrimSpace(s.Description)
		if d == "" {
			return "(empty step)"
		}
		return d
	}
	return "(none — this is the final step)"
}

func planHandoverText(stepOrdinal int) string {
	if stepOrdinal <= 1 {
		return "You are executing the first step of this plan. Work only on the current step. " +
			"When it is complete, end your reply with a line containing exactly ===STEP_DONE===."
	}
	return "The prior step is finished; use the previous step output only as context. " +
		"Do not redo completed work. Execute only the current step. " +
		"When it is complete, end your reply with a line containing exactly ===STEP_DONE===."
}
