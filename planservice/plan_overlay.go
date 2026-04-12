package planservice

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/planstore"
)

// PlanStepMacroVars holds values for MacroEnv {{var:…}} expansion in the compiled noop seed
// (see plancompile.seedPromptTemplate). Use [NewPlanStepMacroVars] then [PlanStepMacroVars.TemplateVars]
// for taskengine.MergeTemplateVars.
type PlanStepMacroVars struct {
	PlanGoal       string
	PlanOverview   string
	StepOrdinal    string
	StepTotal      string
	CurrentStep    string
	NextStep       string
	PlanHandover   string
	PreviousOutput string
}

// NewPlanStepMacroVars fills macro vars for the pending plan step (goal, full plan, next step, handover, etc.).
func NewPlanStepMacroVars(plan *planstore.Plan, steps []*planstore.PlanStep, pending *planstore.PlanStep) PlanStepMacroVars {
	if plan == nil || pending == nil {
		return PlanStepMacroVars{}
	}
	return PlanStepMacroVars{
		PlanGoal:       strings.TrimSpace(plan.Goal),
		PlanOverview:   formatPlanOverview(steps),
		StepOrdinal:    strconv.Itoa(pending.Ordinal),
		StepTotal:      strconv.Itoa(len(steps)),
		CurrentStep:    strings.TrimSpace(pending.Description),
		NextStep:       nextStepDescription(steps, pending.Ordinal),
		PlanHandover:   planHandoverText(pending.Ordinal),
		PreviousOutput: previousStepOutput(steps, pending.Ordinal),
	}
}

// TemplateVars maps field names to MacroEnv keys expected by the seed prompt.
func (v PlanStepMacroVars) TemplateVars() map[string]string {
	if v.StepOrdinal == "" {
		return map[string]string{}
	}
	return map[string]string{
		"plan_goal":       v.PlanGoal,
		"plan_overview":   v.PlanOverview,
		"step_ordinal":    v.StepOrdinal,
		"step_total":      v.StepTotal,
		"current_step":    v.CurrentStep,
		"next_step":       v.NextStep,
		"plan_handover":   v.PlanHandover,
		"previous_output": v.PreviousOutput,
	}
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
