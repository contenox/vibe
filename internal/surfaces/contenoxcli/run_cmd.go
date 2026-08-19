package contenoxcli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/spf13/cobra"
)

// defaultRunAgent is the preseeded run.md declaration, shaped to answer a program rather than a person.
const defaultRunAgent = "run"

const defaultRunTimeout = 30 * time.Minute

var runCmd = &cobra.Command{
	Use:   `run [agent] "<task>"`,
	Short: "Carry out one task and print the agent's report on stdout, for a caller that is a program.",
	Long: `Carry out one stated task and report back as data, for CI, a script, or anything
else that is not a person at a keyboard.

The task is fired as a mission at a declared agent — the preseeded 'run' agent
unless you name another — and this command blocks until the mission reaches a
terminal status. Stdout carries the agent's final report and nothing else;
progress, the mission id and the outcome go to stderr, so a pipe reads clean.
There is no prompt, no spinner and no follow-up question: a task either finishes
or stops, and the exit status says which.

Exit status is 0 when the mission lands, and non-zero when it derails, gets
stuck, is abandoned, or the wait times out (--timeout).

The fleet is embedded IN-PROCESS: the dispatched unit is a child subprocess of
this command and is torn down when it exits, so nothing keeps running after the
command returns. The unit runs unattended inside an envelope — --policy, or the
default-mission-policy config — which is what bounds what the task may touch.

` + toolGrantLine + `

` + askWaitLine + ` Nobody is watching a run like this, so
an envelope that asks is an envelope that holds it: the ask is a durable row and
the run blocks on that row until someone answers with 'contenox approvals
respond' — which releases it and lets the task finish — or until the wait runs
out and the on-timeout verdict (deny) applies. If --timeout ends the command
first, the run is checkpointed beside the still-pending ask, so answering it
afterwards resumes the work rather than losing it.

The mission record and every report survive in the local store, so a run whose
output was discarded can still be read back with 'contenox mission show'.

Piped stdin is the material the task is about. A dispatch carries one intent and
nothing else, so the piped body travels inside it, appended to the task between
'--- begin piped stdin ---' and '--- end piped stdin ---'. Saying the task as one
sentence lets you drop the word 'run' entirely: 'contenox "<task>"' is this
command, and a pipe into it is this command too.

Examples:
  contenox run "regenerate the API docs from the openapi spec"
  contenox run reviewer "check the last commit for regressions"
  contenox run "summarise what npm audit wants upgraded" --timeout 5m
  git diff | contenox run "review this and say what I should check myself"

Quote the task as ONE argument. See declared agents with 'contenox agent list'.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runRun,
}

func init() {
	runCmd.Flags().String("policy", "", "Mission envelope: the HITL policy bounding the unit (default: the default-mission-policy config)")
	runCmd.Flags().Duration("timeout", defaultRunTimeout, "Maximum time to wait for a terminal status before tearing the unit down")
	rootCmd.AddCommand(runCmd)
}

const (
	stdinBodyOpen  = "--- begin piped stdin ---"
	stdinBodyClose = "--- end piped stdin ---"
)

// attachPipedStdin puts the piped body inside the task text, delimited: a
// dispatch carries an intent and no second channel to hang an attachment on.
func attachPipedStdin(task, body string) string {
	body = strings.Trim(body, "\n")
	if strings.TrimSpace(body) == "" {
		return task
	}
	return task + "\n\n" + stdinBodyOpen + "\n" + body + "\n" + stdinBodyClose
}

// runTarget reads `run [agent] "<task>"`: one argument is the task alone.
func runTarget(args []string) (agent, task string) {
	if len(args) == 1 {
		return defaultRunAgent, strings.TrimSpace(args[0])
	}
	return strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
}

func runRun(cmd *cobra.Command, args []string) error {
	agent, task := runTarget(args)
	if task == "" {
		return fmt.Errorf("the task is empty: contenox run %q \"<task>\"", agent)
	}
	if body, piped := pipedStdin(); piped {
		task = attachPipedStdin(task, body)
	}

	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	policy, _ := cmd.Flags().GetString("policy")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	outcome, err := fireMissionAndWait(cmd, missionFireSpec{
		agent:   agent,
		intent:  task,
		policy:  policy,
		timeout: timeout,
		narrate: stderr,
	})
	if err != nil {
		return err
	}

	writeRunReport(stdout, finalMissionReport(outcome.reports))
	fmt.Fprintln(stderr, missionOutcomeLine(outcome.mission))
	fmt.Fprintf(stderr, "Full detail: contenox mission reports %s\n", outcome.mission.ID)
	if outcome.mission.Status != missionservice.StatusLanded {
		return &exitError{1}
	}
	return nil
}

// finalMissionReport prefers the newest result over the newest report of any kind.
func finalMissionReport(reports []*missionservice.Report) *missionservice.Report {
	for _, r := range reports {
		if r.Kind == missionservice.ReportKindResult {
			return r
		}
	}
	if len(reports) > 0 {
		return reports[0]
	}
	return nil
}

func writeRunReport(w io.Writer, r *missionservice.Report) {
	if r == nil {
		return
	}
	fmt.Fprintln(w, r.Summary)
	if detail := strings.TrimSpace(r.Detail); detail != "" {
		fmt.Fprintln(w, detail)
	}
	if len(r.Refs) > 0 {
		fmt.Fprintf(w, "refs: %s\n", strings.Join(r.Refs, ", "))
	}
}
