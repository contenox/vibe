package localtools_test

import (
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

func TestUnit_HITLWrapper_RecordedAskCarriesTheAgentThatRaisedIt(t *testing.T) {
	inner := &mockInnerTools{}
	policy := newRecordingApprovePolicy()
	w := localtools.NewHITLWrapper(inner, blockUntilCtxDone, policy, nil)

	ctx := hitlservice.WithAgentName(suspendableCtx("call-agent"), "refund-desk")
	_, _, err := w.Exec(ctx, time.Now(), map[string]any{"path": "a.txt"}, false,
		&taskengine.ToolsCall{Name: "local_shell", ToolName: "run_command"})

	var pend *taskengine.ApprovalPendingError
	require.ErrorAs(t, err, &pend)
	require.Len(t, policy.requests, 1)
	require.Equal(t, "refund-desk", policy.requests[0].AgentName,
		"a per-agent subscriber can only see an ask that names its agent")

	anon := newRecordingApprovePolicy()
	wAnon := localtools.NewHITLWrapper(&mockInnerTools{}, blockUntilCtxDone, anon, nil)
	_, _, err = execSuspendableCall(t, wAnon, "call-anon")
	require.ErrorAs(t, err, &pend)
	require.Len(t, anon.requests, 1)
	require.Empty(t, anon.requests[0].AgentName, "no agent bound, nothing invented")
}
