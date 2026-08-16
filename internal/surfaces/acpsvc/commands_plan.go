package acpsvc

import (
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/services/missiontools"
)

// missionStartToolRef is the bare function name the model sees in its tool list.
func missionStartToolRef() string { return missiontools.ToolNameStartMission }

const planCommandName = "plan"

const planUsageLine = "usage: /plan <what you want done>"

// parsePlanCommand recognises `/plan <goal>` and returns the goal. /plan is not
// a command handler: it expands into an instruction and takes the ordinary
// prompt path, because the verb needs the model and its tools.
func parsePlanCommand(input string) (string, bool) {
	name, args, ok := parseCommand(input)
	if !ok || name != planCommandName {
		return "", false
	}
	return strings.TrimSpace(args), true
}

// planPreamble is the instruction `/plan` expands into.
func planPreamble(goal string) string {
	tool := missionStartToolRef()
	return fmt.Sprintf(`Plan the following work, then carry it out one step at a time using subagents.

GOAL: %s

Run it like this:

1. State the plan first: a short, ordered list of steps. Each step must be one self-contained unit of work — something a fresh agent could do knowing nothing but what you write down.
2. Work through the steps IN ORDER. For each one, call %s with an "intent" that fully describes that step. The subagent shares none of this conversation: restate the goal, the exact paths, and what a finished step looks like.
3. Read each subagent's reports when it returns. If it landed, move to the next step. If it derailed or got stuck, STOP and tell me what happened — do not push on past a failed step.
4. When every step is done, summarise what actually changed.

Run the steps as subagents rather than doing them yourself. If a step turns out to be trivial or already done, say so and skip it rather than starting a subagent for nothing.`, goal, tool)
}

func planPreambleForMissingFleet() error {
	return fmt.Errorf("/plan needs subagents, which this session cannot start: it requires a configured model and the in-process fleet. Configure a model with `contenox config set default-model …` and run /plan from your editor session")
}
