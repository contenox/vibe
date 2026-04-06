package plancompile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/contenox/taskengine"
)

// Compile turns a parsed plan and an executor chain into one linear TaskChainDefinition.
// For each plan step it emits a seed task (noop + prompt template) then a full copy of the
// executor graph with rewritten task IDs and transitions. Successful chain exits from step i
// route to seed_step_{i+1} when i < last step.
func Compile(executor *taskengine.TaskChainDefinition, outChainID string, p *ParsedPlan) (*taskengine.TaskChainDefinition, error) {
	if executor == nil || len(executor.Tasks) == 0 {
		return nil, fmt.Errorf("plancompile: executor chain is empty")
	}
	if p == nil || len(p.Steps) == 0 {
		return nil, fmt.Errorf("plancompile: no steps in parsed plan")
	}
	if strings.TrimSpace(outChainID) == "" {
		return nil, fmt.Errorf("plancompile: chain id is required")
	}

	origIDs := taskIDSet(executor.Tasks)
	if err := validateExecutorEdges(executor, origIDs); err != nil {
		return nil, err
	}

	n := len(p.Steps)
	var outTasks []taskengine.TaskDefinition
	entryID := executor.Tasks[0].ID

	for stepIdx := 1; stepIdx <= n; stepIdx++ {
		stepText := p.Steps[stepIdx-1]
		if err := validateStepText(stepText); err != nil {
			return nil, err
		}

		firstDup := prefixID(stepIdx, entryID)
		seedTmpl := seedPromptTemplate(stepIdx == 1, p.Goal, stepText)

		seed := taskengine.TaskDefinition{
			ID:                fmt.Sprintf("seed_step_%d", stepIdx),
			Description:       fmt.Sprintf("Compiled plan: seed input for step %d", stepIdx),
			Handler:           taskengine.HandleNoop,
			PromptTemplate:    seedTmpl,
			Transition:        linearTransition(firstDup),
			SystemInstruction: "",
		}
		outTasks = append(outTasks, seed)

		for _, t := range executor.Tasks {
			cp, err := cloneTask(&t)
			if err != nil {
				return nil, err
			}
			cp.ID = prefixID(stepIdx, t.ID)

			if cp.InputVar != "" {
				if origIDs[cp.InputVar] {
					cp.InputVar = prefixID(stepIdx, cp.InputVar)
				}
			}

			cp.Transition.OnFailure = remapGoto(cp.Transition.OnFailure, stepIdx, origIDs)

			for j := range cp.Transition.Branches {
				b := &cp.Transition.Branches[j]
				b.Goto = remapStepGoto(b.Goto, stepIdx, origIDs)
			}

			if stepIdx < n {
				cp.Transition = retargetTerminals(cp.Transition, fmt.Sprintf("seed_step_%d", stepIdx+1))
			}

			outTasks = append(outTasks, *cp)
		}
	}

	out := &taskengine.TaskChainDefinition{
		ID:          outChainID,
		Description: fmt.Sprintf("Compiled from plan markdown (%d steps)", n),
		Tasks:       outTasks,
		TokenLimit:  executor.TokenLimit,
		Debug:       executor.Debug,
	}
	return out, nil
}

func taskIDSet(tasks []taskengine.TaskDefinition) map[string]bool {
	m := make(map[string]bool, len(tasks))
	for i := range tasks {
		m[tasks[i].ID] = true
	}
	return m
}

func validateExecutorEdges(ex *taskengine.TaskChainDefinition, ids map[string]bool) error {
	for _, t := range ex.Tasks {
		if t.Transition.OnFailure != "" && !ids[t.Transition.OnFailure] {
			return fmt.Errorf("plancompile: executor task %q on_failure %q is not in the same chain (unsupported)", t.ID, t.Transition.OnFailure)
		}
		for _, b := range t.Transition.Branches {
			if isTerminalGoto(b.Goto) {
				continue
			}
			if !ids[b.Goto] {
				return fmt.Errorf("plancompile: executor task %q branch goto %q is not in the same chain (unsupported)", t.ID, b.Goto)
			}
		}
	}
	return nil
}

func isTerminalGoto(g string) bool {
	g = strings.TrimSpace(g)
	return g == "" || g == taskengine.TermEnd || strings.EqualFold(g, "end")
}

func remapGoto(g string, stepIdx int, origIDs map[string]bool) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return ""
	}
	if origIDs[g] {
		return prefixID(stepIdx, g)
	}
	return g
}

func remapStepGoto(g string, stepIdx int, origIDs map[string]bool) string {
	g = strings.TrimSpace(g)
	if isTerminalGoto(g) {
		return g
	}
	if origIDs[g] {
		return prefixID(stepIdx, g)
	}
	return g
}

// retargetTerminals replaces successful terminal edges with nextSeed when they currently end the chain.
func retargetTerminals(tr taskengine.TaskTransition, nextSeed string) taskengine.TaskTransition {
	out := tr
	out.Branches = append([]taskengine.TransitionBranch(nil), tr.Branches...)
	for i := range out.Branches {
		b := &out.Branches[i]
		if isTerminalGoto(b.Goto) {
			b.Goto = nextSeed
		}
	}
	return out
}

func linearTransition(gotoID string) taskengine.TaskTransition {
	return taskengine.TaskTransition{
		OnFailure: "",
		Branches: []taskengine.TransitionBranch{
			{Operator: taskengine.OpDefault, When: "", Goto: gotoID},
		},
	}
}

func prefixID(step int, id string) string {
	return fmt.Sprintf("s%d__%s", step, id)
}

func cloneTask(t *taskengine.TaskDefinition) (*taskengine.TaskDefinition, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("plancompile: clone task: %w", err)
	}
	var out taskengine.TaskDefinition
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("plancompile: clone task: %w", err)
	}
	return &out, nil
}

func seedPromptTemplate(isFirst bool, goal, stepText string) string {
	g := strings.TrimSpace(goal)
	if isFirst {
		if g == "" {
			return fmt.Sprintf("Current plan step:\n%s", stepText)
		}
		return fmt.Sprintf("Goal:\n%s\n\nCurrent plan step:\n%s", g, stepText)
	}
	if g == "" {
		return fmt.Sprintf("Previous step output:\n{{.previous_output}}\n\nCurrent plan step:\n%s", stepText)
	}
	return fmt.Sprintf("Goal:\n%s\n\nPrevious step output:\n{{.previous_output}}\n\nCurrent plan step:\n%s", g, stepText)
}

func validateStepText(s string) error {
	if strings.Contains(s, "{{") || strings.Contains(s, "}}") {
		return fmt.Errorf("plancompile: step text must not contain {{ or }} (template delimiter conflict)")
	}
	return nil
}
