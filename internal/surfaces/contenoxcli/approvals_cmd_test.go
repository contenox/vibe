package contenoxcli

import (
	"bytes"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_ApprovalsCommandIsReserved(t *testing.T) {
	require.True(t, reservedSubcommands["approvals"], `"approvals" must be reserved so it dispatches as a subcommand`)
}

func TestUnit_RenderApprovalsTable(t *testing.T) {
	now := time.Now().UTC()

	var empty bytes.Buffer
	require.NoError(t, renderApprovalsTable(&empty, nil, now))
	require.Contains(t, empty.String(), "No pending asks", "an empty inbox renders empty, it does not fail")

	missionID := "m-42"
	diff := "-old\n+new"
	rows := []*runtimetypes.HITLApproval{
		{
			ID: "ask-1", ToolsName: "local_shell", ToolName: "exec", ArgsSummary: "rm -rf ./build",
			State:     runtimetypes.HITLApprovalPending,
			CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(28 * time.Minute),
			Diff: &diff,
		},
		{
			ID: "ask-2", ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
			ArgsSummary: "which project did you mean?", MissionID: &missionID,
			State:     runtimetypes.HITLApprovalPending,
			CreatedAt: now.Add(-30 * time.Second), ExpiresAt: now.Add(29 * time.Minute),
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderApprovalsTable(&buf, rows, now))
	out := buf.String()
	for _, want := range []string{
		"ID", "KIND", "TOOL", "SUMMARY", "MISSION", "AGE", "EXPIRES-IN",
		"ask-1", "permission", "local_shell.exec", "rm -rf ./build", "2m", "28m",
		"ask-2", "question", "which project did you mean?", "m-42",
		"-old\n+new",
		"approvals respond",
	} {
		require.Contains(t, out, want)
	}
}
