package planservice

import (
	"testing"

	"github.com/contenox/contenox/planstore"
)

func Test_formatPlanOverview(t *testing.T) {
	steps := []*planstore.PlanStep{
		{Ordinal: 1, Description: "  A  "},
		{Ordinal: 2, Description: ""},
	}
	got := formatPlanOverview(steps)
	want := "1. A\n2. (empty step)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func Test_nextStepDescription(t *testing.T) {
	steps := []*planstore.PlanStep{
		{Ordinal: 1, Description: "one"},
		{Ordinal: 2, Description: "two"},
	}
	if nextStepDescription(steps, 1) != "two" {
		t.Fatalf("from 1: %q", nextStepDescription(steps, 1))
	}
	if nextStepDescription(steps, 2) != "(none — this is the final step)" {
		t.Fatalf("from 2: %q", nextStepDescription(steps, 2))
	}
}

func TestNewPlanStepMacroVars(t *testing.T) {
	plan := &planstore.Plan{Goal: "Ship v1"}
	steps := []*planstore.PlanStep{
		{Ordinal: 1, Description: "Draft", ExecutionResult: "ok"},
		{Ordinal: 2, Description: "Review", ExecutionResult: "notes"},
		{Ordinal: 3, Description: "Launch"},
	}
	pending := steps[1]
	v := NewPlanStepMacroVars(plan, steps, pending)
	if v.PlanGoal != "Ship v1" {
		t.Fatalf("PlanGoal: %q", v.PlanGoal)
	}
	if v.StepOrdinal != "2" || v.StepTotal != "3" {
		t.Fatalf("progress: %#v", v)
	}
	if v.CurrentStep != "Review" {
		t.Fatalf("CurrentStep: %q", v.CurrentStep)
	}
	if v.NextStep != "Launch" {
		t.Fatalf("NextStep: %q", v.NextStep)
	}
	if v.PreviousOutput != "ok" {
		t.Fatalf("PreviousOutput: %q", v.PreviousOutput)
	}
	if v.PlanOverview == "" || v.PlanHandover == "" {
		t.Fatalf("missing overview/handover: %#v", v)
	}
	m := v.TemplateVars()
	if m["plan_goal"] != v.PlanGoal || m["previous_output"] != v.PreviousOutput {
		t.Fatalf("TemplateVars mismatch: %#v", m)
	}
	if m["execution_context"] == "" {
		t.Fatal("expected non-empty execution_context")
	}
}
