package plancompile

import (
	"testing"

	"github.com/contenox/contenox/taskengine"
)

func TestCompile_linearChainRetargets(t *testing.T) {
	executor := &taskengine.TaskChainDefinition{
		ID: "exec",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "a",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: "do {{.input}}",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
		TokenLimit: 100,
	}

	p := &ParsedPlan{
		Goal:  "g",
		Steps: []string{"one", "two"},
	}
	out, err := Compile(executor, "compiled", p)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "compiled" {
		t.Fatalf("id: %q", out.ID)
	}
	if len(out.Tasks) != 2*(1+len(executor.Tasks)) {
		t.Fatalf("tasks: %d", len(out.Tasks))
	}
	// seed_step_1 -> s1__a -> seed_step_2 -> s2__a
	if out.Tasks[0].ID != "seed_step_1" {
		t.Fatalf("first task %q", out.Tasks[0].ID)
	}
	if out.Tasks[1].ID != "s1__a" {
		t.Fatalf("second task %q", out.Tasks[1].ID)
	}
	// terminal from step 1 should jump to seed_step_2
	br := out.Tasks[1].Transition.Branches
	if len(br) != 1 || br[0].Goto != "seed_step_2" {
		t.Fatalf("step1 terminal goto: %#v", br)
	}
	// last step should still end
	br2 := out.Tasks[3].Transition.Branches
	if len(br2) != 1 || br2[0].Goto != taskengine.TermEnd {
		t.Fatalf("step2 terminal goto: %#v", br2)
	}
}

func TestCompile_executorLoop(t *testing.T) {
	// Minimal loop: a -> b -> a, end from a
	executor := &taskengine.TaskChainDefinition{
		ID: "loop-exec",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "a",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "x", Goto: "b"},
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
			{
				ID:       "b",
				Handler:  taskengine.HandleNoop,
				InputVar: "a",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "a"},
					},
				},
			},
		},
	}
	p := &ParsedPlan{Goal: "g", Steps: []string{"s1", "s2"}}
	out, err := Compile(executor, "c", p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tasks[1].ID != "s1__a" {
		t.Fatalf("got %q", out.Tasks[1].ID)
	}
	// s1__a: equals x -> s1__b; default -> seed_step_2
	if out.Tasks[1].Transition.Branches[0].Goto != "s1__b" {
		t.Fatalf("a->b: %#v", out.Tasks[1].Transition.Branches[0])
	}
	// s1__b loops to s1__a
	if out.Tasks[2].Transition.Branches[0].Goto != "s1__a" {
		t.Fatalf("b->a: %#v", out.Tasks[2].Transition.Branches[0])
	}
}

func TestExtractStepChain_retargetsToEnd(t *testing.T) {
	executor := &taskengine.TaskChainDefinition{
		ID: "exec",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "a",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: "do {{.input}}",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
		TokenLimit: 100,
	}
	p := &ParsedPlan{Goal: "g", Steps: []string{"one", "two"}}
	full, err := Compile(executor, "compiled", p)
	if err != nil {
		t.Fatal(err)
	}

	s1, err := ExtractStepChain(full, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != "compiled__step_1" {
		t.Fatalf("id: %q", s1.ID)
	}
	if len(s1.Tasks) != 2 {
		t.Fatalf("step1 tasks: %d", len(s1.Tasks))
	}
	br := s1.Tasks[1].Transition.Branches
	if len(br) != 1 || br[0].Goto != taskengine.TermEnd {
		t.Fatalf("step1 terminal should end chain, got %#v", br)
	}

	s2, err := ExtractStepChain(full, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Tasks) != 2 {
		t.Fatalf("step2 tasks: %d", len(s2.Tasks))
	}
	br2 := s2.Tasks[1].Transition.Branches
	if len(br2) != 1 || br2[0].Goto != taskengine.TermEnd {
		t.Fatalf("step2 terminal: %#v", br2)
	}
}

func TestExtractStepChain_invalidStep(t *testing.T) {
	executor := &taskengine.TaskChainDefinition{
		ID: "exec",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "a",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: "x",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
	p := &ParsedPlan{Goal: "g", Steps: []string{"only"}}
	full, err := Compile(executor, "c", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractStepChain(full, 0); err == nil {
		t.Fatal("expected error for step 0")
	}
	if _, err := ExtractStepChain(full, 99); err == nil {
		t.Fatal("expected error for missing step")
	}
}
