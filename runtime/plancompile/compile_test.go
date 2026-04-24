package plancompile

import (
	"testing"

	"github.com/contenox/contenox/runtime/taskengine"
)

// minimalSummarizer returns a valid summarizer chain fixture shaped like the
// shipped chain-step-summarizer.json: one chat LLM, one persist tools (with
// ok/invalid branches), one repair LLM, a second persist tools, and a fallback
// tools. Uses the symbolic references (SummarizerRefExecTerminal,
// SummarizerRefNextStep) that plancompile.Compile rewrites per step.
func minimalSummarizer() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: "sum",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:       "summarize",
				Handler:  taskengine.HandleChatCompletion,
				InputVar: SummarizerRefExecTerminal,
				Transition: taskengine.TaskTransition{
					OnFailure: "fallback",
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "persist"},
					},
				},
			},
			{
				ID:      "persist",
				Handler: taskengine.HandleTools,
				Tools:    &taskengine.ToolsCall{Name: "plan_summary", ToolName: "persist"},
				Transition: taskengine.TaskTransition{
					OnFailure: "fallback",
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "ok", Goto: SummarizerRefNextStep},
						{Operator: taskengine.OpEquals, When: "invalid", Goto: "repair"},
						{Operator: taskengine.OpDefault, Goto: "fallback"},
					},
				},
			},
			{
				ID:       "repair",
				Handler:  taskengine.HandleChatCompletion,
				InputVar: "summarize",
				Transition: taskengine.TaskTransition{
					OnFailure: "fallback",
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "persist_repair"},
					},
				},
			},
			{
				ID:      "persist_repair",
				Handler: taskengine.HandleTools,
				Tools:    &taskengine.ToolsCall{Name: "plan_summary", ToolName: "persist"},
				Transition: taskengine.TaskTransition{
					OnFailure: "fallback",
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "ok", Goto: SummarizerRefNextStep},
						{Operator: taskengine.OpDefault, Goto: "fallback"},
					},
				},
			},
			{
				ID:       "fallback",
				Handler:  taskengine.HandleTools,
				InputVar: SummarizerRefExecTerminal,
				Tools:     &taskengine.ToolsCall{Name: "plan_summary", ToolName: "fallback"},
				Transition: taskengine.TaskTransition{
					OnFailure: SummarizerRefNextStep,
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: SummarizerRefNextStep},
					},
				},
			},
		},
	}
}

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
	summarizer := minimalSummarizer()

	p := &ParsedPlan{
		Goal:  "g",
		Steps: []string{"one", "two"},
	}
	out, err := Compile(executor, summarizer, "compiled", p)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "compiled" {
		t.Fatalf("id: %q", out.ID)
	}
	// Per step: seed (1) + executor (len) + exec_done (1) + summarizer (len)
	perStep := 1 + len(executor.Tasks) + 1 + len(summarizer.Tasks)
	if got, want := len(out.Tasks), 2*perStep; got != want {
		t.Fatalf("tasks: got %d want %d", got, want)
	}

	// Expected order for step 1: seed_step_1, s1__a, s1__exec_done, s1__sum__*
	if out.Tasks[0].ID != "seed_step_1" {
		t.Fatalf("tasks[0] = %q", out.Tasks[0].ID)
	}
	if out.Tasks[1].ID != "s1__a" {
		t.Fatalf("tasks[1] = %q", out.Tasks[1].ID)
	}
	if out.Tasks[2].ID != "s1__exec_done" {
		t.Fatalf("tasks[2] = %q", out.Tasks[2].ID)
	}
	if out.Tasks[3].ID != "s1__sum__summarize" {
		t.Fatalf("tasks[3] = %q (expected s1__sum__summarize)", out.Tasks[3].ID)
	}

	// Executor's terminal routes into s1__exec_done (not seed_step_2 anymore).
	execBranches := out.Tasks[1].Transition.Branches
	if len(execBranches) != 1 || execBranches[0].Goto != "s1__exec_done" {
		t.Fatalf("executor terminal should route to s1__exec_done, got %#v", execBranches)
	}

	// Summarizer's ok branch should now target seed_step_2 after rewrite.
	persist := out.Tasks[4]
	if persist.ID != "s1__sum__persist" {
		t.Fatalf("tasks[4] = %q", persist.ID)
	}
	var okGoto string
	for _, b := range persist.Transition.Branches {
		if b.When == "ok" {
			okGoto = b.Goto
		}
	}
	if okGoto != "seed_step_2" {
		t.Fatalf("step1 persist ok -> %q, want seed_step_2", okGoto)
	}

	// Sibling-reference rewrite: persist's invalid branch should point at s1__sum__repair.
	var invalidGoto string
	for _, b := range persist.Transition.Branches {
		if b.When == "invalid" {
			invalidGoto = b.Goto
		}
	}
	if invalidGoto != "s1__sum__repair" {
		t.Fatalf("step1 persist invalid -> %q, want s1__sum__repair", invalidGoto)
	}

	// Final-step summarizer terminal should end the chain.
	step2PersistIdx := perStep + 4 // seed2 + s2__a + exec_done_2 + summarize = offset 4 within step 2
	step2Persist := out.Tasks[step2PersistIdx]
	if step2Persist.ID != "s2__sum__persist" {
		t.Fatalf("tasks[%d] = %q (expected s2__sum__persist)", step2PersistIdx, step2Persist.ID)
	}
	for _, b := range step2Persist.Transition.Branches {
		if b.When == "ok" && b.Goto != taskengine.TermEnd {
			t.Fatalf("step2 persist ok should be TermEnd for final step, got %q", b.Goto)
		}
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
	out, err := Compile(executor, minimalSummarizer(), "c", p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tasks[1].ID != "s1__a" {
		t.Fatalf("got %q", out.Tasks[1].ID)
	}
	// s1__a: equals x -> s1__b; default (executor terminal) now -> s1__exec_done
	if out.Tasks[1].Transition.Branches[0].Goto != "s1__b" {
		t.Fatalf("a->b: %#v", out.Tasks[1].Transition.Branches[0])
	}
	if out.Tasks[1].Transition.Branches[1].Goto != "s1__exec_done" {
		t.Fatalf("a default should retarget to s1__exec_done, got %#v", out.Tasks[1].Transition.Branches[1])
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
	full, err := Compile(executor, minimalSummarizer(), "compiled", p)
	if err != nil {
		t.Fatal(err)
	}

	perStep := 1 + len(executor.Tasks) + 1 + 5

	s1, err := ExtractStepChain(full, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != "compiled__step_1" {
		t.Fatalf("id: %q", s1.ID)
	}
	if len(s1.Tasks) != perStep {
		t.Fatalf("step1 tasks: got %d want %d", len(s1.Tasks), perStep)
	}
	// ExtractStepChain rewrites persist's "ok" from seed_step_2 back to TermEnd for a single-step subgraph.
	var persist *taskengine.TaskDefinition
	for i := range s1.Tasks {
		if s1.Tasks[i].ID == "s1__sum__persist" {
			persist = &s1.Tasks[i]
			break
		}
	}
	if persist == nil {
		t.Fatal("missing s1__sum__persist in extracted step 1")
	}
	for _, b := range persist.Transition.Branches {
		if b.When == "ok" && b.Goto != taskengine.TermEnd {
			t.Fatalf("extracted step1 persist ok should be TermEnd, got %q", b.Goto)
		}
	}

	s2, err := ExtractStepChain(full, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Tasks) != perStep {
		t.Fatalf("step2 tasks: got %d want %d", len(s2.Tasks), perStep)
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
	full, err := Compile(executor, minimalSummarizer(), "c", p)
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

func TestCompile_rejectsEmptySummarizer(t *testing.T) {
	executor := &taskengine.TaskChainDefinition{
		ID: "exec",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "a",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
	p := &ParsedPlan{Goal: "g", Steps: []string{"only"}}
	if _, err := Compile(executor, nil, "c", p); err == nil {
		t.Fatal("expected error when summarizer is nil")
	}
	empty := &taskengine.TaskChainDefinition{ID: "sum", Tasks: nil}
	if _, err := Compile(executor, empty, "c", p); err == nil {
		t.Fatal("expected error when summarizer has no tasks")
	}
}
