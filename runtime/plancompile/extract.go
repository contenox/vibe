package plancompile

import (
	"fmt"
	"strings"

	"github.com/contenox/contenox/runtime/taskengine"
)

// ExtractStepChain returns the subgraph for a single plan step from a chain produced by [Compile].
// It includes seed_step_k and all tasks with IDs prefixed s{k}__, and rewrites transitions that
// jump to seed_step_{k+1} so the subgraph terminates at the end of the step instead.
func ExtractStepChain(full *taskengine.TaskChainDefinition, stepIndex1Based int) (*taskengine.TaskChainDefinition, error) {
	if full == nil || len(full.Tasks) == 0 {
		return nil, fmt.Errorf("plancompile: ExtractStepChain: empty chain")
	}
	k := stepIndex1Based
	if k < 1 {
		return nil, fmt.Errorf("plancompile: ExtractStepChain: step index must be >= 1")
	}

	seedID := fmt.Sprintf("seed_step_%d", k)
	stepPref := fmt.Sprintf("s%d__", k)
	nextSeed := fmt.Sprintf("seed_step_%d", k+1)

	var picked []taskengine.TaskDefinition
	for i := range full.Tasks {
		t := full.Tasks[i]
		if t.ID == seedID || strings.HasPrefix(t.ID, stepPref) {
			cp, err := cloneTask(&t)
			if err != nil {
				return nil, err
			}
			picked = append(picked, *cp)
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("plancompile: ExtractStepChain: no tasks for step %d", k)
	}
	if picked[0].ID != seedID {
		return nil, fmt.Errorf("plancompile: ExtractStepChain: expected first task %q, got %q", seedID, picked[0].ID)
	}

	for i := range picked {
		picked[i].Transition = retargetNextSeedToEnd(picked[i].Transition, nextSeed)
	}

	out := &taskengine.TaskChainDefinition{
		ID:          fmt.Sprintf("%s__step_%d", full.ID, k),
		Description: fmt.Sprintf("Step %d slice of %s", k, full.ID),
		Tasks:       picked,
		TokenLimit:  full.TokenLimit,
		Debug:       full.Debug,
	}
	return out, nil
}

func retargetNextSeedToEnd(tr taskengine.TaskTransition, nextSeed string) taskengine.TaskTransition {
	out := tr
	out.Branches = append([]taskengine.TransitionBranch(nil), tr.Branches...)
	for i := range out.Branches {
		b := &out.Branches[i]
		if strings.TrimSpace(b.Goto) == nextSeed {
			b.Goto = taskengine.TermEnd
		}
	}
	if strings.TrimSpace(out.OnFailure) == nextSeed {
		out.OnFailure = ""
	}
	return out
}
