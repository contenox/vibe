package contenoxcli

import (
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func lookupCommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range rootCmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("no %q command is registered", name)
	return nil
}

// `run` is the program-caller shape, so it has to be a real subcommand and a
// reserved name: an unreserved one would fall through to cobra as unknown.
func TestUnit_RunCommandIsWired(t *testing.T) {
	cmd := lookupCommand(t, "run")
	require.True(t, reservedSubcommands["run"])
	require.True(t, firstNonFlagIsReserved([]string{"run", "do the thing"}))

	require.NotNil(t, cmd.Flags().Lookup("timeout"), "a script must be able to bound the wait")
	require.NotNil(t, cmd.Flags().Lookup("policy"), "a script must be able to pin the envelope")
	require.Nil(t, cmd.Flags().Lookup("wait"),
		"run always waits: a caller that is a program has nowhere to detach to")

	timeout, err := cmd.Flags().GetDuration("timeout")
	require.NoError(t, err)
	require.Equal(t, defaultRunTimeout, timeout)

	require.Error(t, cmd.Args(cmd, []string{}), "a task is required")
	require.NoError(t, cmd.Args(cmd, []string{"do the thing"}))
	require.NoError(t, cmd.Args(cmd, []string{"reviewer", "do the thing"}))
	require.Error(t, cmd.Args(cmd, []string{"do", "the", "thing"}),
		"an unquoted task must fail loudly rather than be read as an agent name plus a task")
}

// The default agent is the preseeded run.md declaration, whose whole shape is
// answering a program; naming one is the two-argument form.
func TestUnit_RunTarget_DefaultsToTheRunAgent(t *testing.T) {
	agent, task := runTarget([]string{" ship it "})
	require.Equal(t, defaultRunAgent, agent)
	require.Equal(t, "ship it", task)

	agent, task = runTarget([]string{"reviewer", "check the last commit"})
	require.Equal(t, "reviewer", agent)
	require.Equal(t, "check the last commit", task)
}

func TestUnit_RunCommand_EmptyTaskIsRefusedBeforeAnythingIsFired(t *testing.T) {
	cmd := &cobra.Command{Use: "run", Args: cobra.RangeArgs(1, 2), RunE: runRun}
	cmd.Flags().String("policy", "", "")
	cmd.Flags().Duration("timeout", time.Minute, "")
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"   "})
	require.Error(t, cmd.Execute())
}

// Stdout is the report and only the report: a caller pipes it somewhere. The
// verdict a unit files last is what it means, even when progress lands after it.
func TestUnit_RunReport_IsTheResultAndNothingElse(t *testing.T) {
	reports := []*missionservice.Report{
		{Kind: missionservice.ReportKindProgress, Summary: "still going"},
		{Kind: missionservice.ReportKindResult, Summary: "upgraded 3 packages", Detail: "lodash, axios, vite", Refs: []string{"package.json"}},
		{Kind: missionservice.ReportKindFinding, Summary: "found a pinned transitive dep"},
	}
	final := finalMissionReport(reports)
	require.NotNil(t, final)
	require.Equal(t, missionservice.ReportKindResult, final.Kind)

	var out strings.Builder
	writeRunReport(&out, final)
	require.Equal(t, "upgraded 3 packages\nlodash, axios, vite\nrefs: package.json\n", out.String())

	// No result filed: the newest report is still the best answer available.
	require.Equal(t, "still going", finalMissionReport(reports[:1]).Summary)
	require.Nil(t, finalMissionReport(nil))

	out.Reset()
	writeRunReport(&out, nil)
	require.Empty(t, out.String(), "a mission that filed nothing prints nothing, not a placeholder")
}
