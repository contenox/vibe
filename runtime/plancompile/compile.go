package plancompile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/contenox/runtime/taskengine"
)

// Summarizer chain symbolic references — plancompile rewrites these per-step when
// cloning the operator-supplied summarizer chain into the compiled plan DAG:
//
//   - SummarizerRefExecTerminal: use as a task's InputVar (or Transition.Goto) to
//     refer to the executor's terminal output for the current step. Compile
//     inserts an `exec_done_{N}` noop between the executor's success terminals and
//     the summarizer subgraph, and rewrites this sentinel to that noop's ID.
//   - SummarizerRefNextStep: use as a Transition.Goto to refer to the next step's
//     seed (or the chain's end for the final step).
const (
	SummarizerRefExecTerminal = "__exec_terminal__"
	SummarizerRefNextStep     = "__next_step__"
)

// Compile turns a parsed plan, an executor chain, and a summarizer chain into
// one linear TaskChainDefinition. For each plan step it emits:
//
//  1. `seed_step_{N}` (HandleNoop) seeding the prompt template with plan macro vars;
//  2. a full copy of the executor graph with `s{N}__` prefixed task IDs;
//  3. `exec_done_{N}` (HandleNoop) splicing the executor's success terminals into the summarizer;
//  4. a full copy of the summarizer graph with `s{N}__sum__` prefixed IDs, symbolic
//     references (SummarizerRefExecTerminal, SummarizerRefNextStep) rewritten.
//
// Successful summarizer terminals route to `seed_step_{N+1}` when N < last step,
// or to TermEnd for the final step. Matches the state-machine pattern in
// enterprise/site/public/images/about/1763497730022.jpeg: every LLM role is a
// typed node in the graph, not imperative glue in a service.
func Compile(
	executor *taskengine.TaskChainDefinition,
	summarizer *taskengine.TaskChainDefinition,
	outChainID string,
	p *ParsedPlan,
) (*taskengine.TaskChainDefinition, error) {
	if executor == nil || len(executor.Tasks) == 0 {
		return nil, fmt.Errorf("plancompile: executor chain is empty")
	}
	if summarizer == nil || len(summarizer.Tasks) == 0 {
		return nil, fmt.Errorf("plancompile: summarizer chain is empty")
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

	sumIDs := taskIDSet(summarizer.Tasks)
	if err := validateSummarizerEdges(summarizer, sumIDs); err != nil {
		return nil, err
	}

	n := len(p.Steps)
	var outTasks []taskengine.TaskDefinition
	entryID := executor.Tasks[0].ID
	sumEntryID := summarizer.Tasks[0].ID

	for stepIdx := 1; stepIdx <= n; stepIdx++ {
		stepText := p.Steps[stepIdx-1]
		if err := validateStepText(stepText); err != nil {
			return nil, err
		}

		firstDup := prefixID(stepIdx, entryID)
		seedTmpl := seedPromptTemplate()

		seed := taskengine.TaskDefinition{
			ID:                fmt.Sprintf("seed_step_%d", stepIdx),
			Description:       fmt.Sprintf("Compiled plan: seed input for step %d", stepIdx),
			Handler:           taskengine.HandleNoop,
			PromptTemplate:    seedTmpl,
			Transition:        linearTransition(firstDup),
			SystemInstruction: "",
		}
		outTasks = append(outTasks, seed)

		execDoneID := prefixID(stepIdx, "exec_done")
		sumEntryFullID := sumPrefixID(stepIdx, sumEntryID)

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

			// Executor success terminals flow into the summarizer via exec_done_{N},
			// not directly into the next step. The summarizer subgraph is what
			// routes onward to seed_step_{N+1} (or end).
			cp.Transition = retargetTerminals(cp.Transition, execDoneID)

			outTasks = append(outTasks, *cp)
		}

		// exec_done_{N}: identity noop that captures the executor's terminal
		// output under a predictable ID. Both the summarizer's entry task and
		// the fallback tools task bind their InputVar to this node (via
		// SummarizerRefExecTerminal), so each sees the raw executor ChatHistory.
		execDone := taskengine.TaskDefinition{
			ID:          execDoneID,
			Description: fmt.Sprintf("Compiled plan: capture executor terminal for step %d", stepIdx),
			Handler:     taskengine.HandleNoop,
			Transition:  linearTransition(sumEntryFullID),
		}
		outTasks = append(outTasks, execDone)

		// Next-step target for summarizer terminals: next seed, or end for final step.
		nextTarget := fmt.Sprintf("seed_step_%d", stepIdx+1)
		if stepIdx == n {
			nextTarget = taskengine.TermEnd
		}

		for _, t := range summarizer.Tasks {
			cp, err := cloneTask(&t)
			if err != nil {
				return nil, err
			}
			cp.ID = sumPrefixID(stepIdx, t.ID)

			// Rewrite InputVar: symbolic exec-terminal reference wins; otherwise
			// bind references to sibling summarizer tasks to their prefixed IDs.
			cp.InputVar = rewriteSummarizerRef(cp.InputVar, stepIdx, sumIDs, execDoneID, nextTarget)

			cp.Transition.OnFailure = rewriteSummarizerRef(cp.Transition.OnFailure, stepIdx, sumIDs, execDoneID, nextTarget)
			for j := range cp.Transition.Branches {
				b := &cp.Transition.Branches[j]
				b.Goto = rewriteSummarizerRef(b.Goto, stepIdx, sumIDs, execDoneID, nextTarget)
			}

			// Unmarked terminal branches (empty / "end" / TermEnd) of the
			// summarizer subgraph route to the next step, matching the executor's
			// retargetTerminals discipline from the previous implementation.
			cp.Transition = retargetTerminals(cp.Transition, nextTarget)

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

// sumPrefixID namespaces summarizer-task IDs under the s{N}__sum__ subtree so
// ExtractStepChain (which matches on s{N}__*) picks them up as part of step N's
// slice, and they cannot collide with executor task IDs.
func sumPrefixID(step int, id string) string {
	return fmt.Sprintf("s%d__sum__%s", step, id)
}

// rewriteSummarizerRef resolves symbolic references within a cloned summarizer
// task's InputVar / Transition.Goto / Transition.OnFailure to concrete per-step
// IDs. Handles:
//
//   - SummarizerRefExecTerminal → execDoneID (the exec_done_{N} noop).
//   - SummarizerRefNextStep     → nextTarget (seed_step_{N+1} or TermEnd).
//   - exact match of a sibling summarizer task ID → prefixed sumPrefixID form.
//   - terminal / end / empty → returned unchanged (retargetTerminals handles those).
//   - anything else (including executor task IDs if the operator ever referenced
//     them explicitly) → returned unchanged.
func rewriteSummarizerRef(ref string, stepIdx int, sumIDs map[string]bool, execDoneID, nextTarget string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	switch ref {
	case SummarizerRefExecTerminal:
		return execDoneID
	case SummarizerRefNextStep:
		return nextTarget
	}
	if isTerminalGoto(ref) {
		return ref
	}
	if sumIDs[ref] {
		return sumPrefixID(stepIdx, ref)
	}
	return ref
}

// validateSummarizerEdges ensures every in-chain summarizer transition and
// OnFailure target refers to either a sibling summarizer task, a summarizer
// symbolic ref (SummarizerRefExecTerminal / SummarizerRefNextStep), or a
// terminal ("end" / TermEnd / empty). Mirrors validateExecutorEdges discipline:
// compile-time rejection of broken chains instead of runtime surprises.
func validateSummarizerEdges(sm *taskengine.TaskChainDefinition, ids map[string]bool) error {
	isOK := func(g string) bool {
		g = strings.TrimSpace(g)
		if g == "" || isTerminalGoto(g) {
			return true
		}
		if g == SummarizerRefExecTerminal || g == SummarizerRefNextStep {
			return true
		}
		return ids[g]
	}
	for _, t := range sm.Tasks {
		if t.Transition.OnFailure != "" && !isOK(t.Transition.OnFailure) {
			return fmt.Errorf("plancompile: summarizer task %q on_failure %q is not in the same chain (unsupported)", t.ID, t.Transition.OnFailure)
		}
		for _, b := range t.Transition.Branches {
			if !isOK(b.Goto) {
				return fmt.Errorf("plancompile: summarizer task %q branch goto %q is not in the same chain (unsupported)", t.ID, b.Goto)
			}
		}
		// InputVar may reference a sibling or the exec-terminal sentinel; other
		// values (e.g. "input", "previous_output", or a caller-owned variable
		// name) are left untouched by rewriteSummarizerRef and thus always valid.
		// Nothing to enforce here.
	}
	return nil
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

// seedPromptTemplate returns the noop seed PromptTemplate for every compiled step.
// Runtime strings (goal, full plan, current/next step, handover, previous output) come from
// planservice via taskengine.MergeTemplateVars / MacroEnv {{var:…}} — not from compile-time text.
func seedPromptTemplate() string {
	return `## Plan execution context

**Goal:** {{var:plan_goal}}

**Full plan:**
{{var:plan_overview}}

**Progress:** step {{var:step_ordinal}} of {{var:step_total}}

**Engine boundary:**
{{var:execution_context}}

**Repo context (from explorer; may be empty):**
{{var:repo_context}}

**Current step (execute only this now):**
{{var:current_step}}

**Next step (orientation only — do not start until the next plan run):**
{{var:next_step}}

{{var:plan_handover}}

**Previous step output (context; may be empty):**
{{var:previous_output}}
`
}

func validateStepText(s string) error {
	if strings.Contains(s, "{{") || strings.Contains(s, "}}") {
		return fmt.Errorf("plancompile: step text must not contain {{ or }} (template delimiter conflict)")
	}
	return nil
}
