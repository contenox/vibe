package enginesvc

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

// execRecorder is a minimal ToolsRepo that records which side of the exemption
// router a call landed on.
type execRecorder struct {
	taskengine.ToolsRepo
	hits *[]string
	tag  string
}

func (e execRecorder) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	*e.hits = append(*e.hits, e.tag)
	return "ok", taskengine.DataTypeString, nil
}

// TestUnit_HITLExempt_MissionProviderBypassesGate pins the deadlock fix the
// first live discovered-chain unit exposed: mission tools are the attention
// channel and must never route through the HITL gate (a unit's mission_report
// once raised an approval ask FOR ITS OWN REPORT under default_action:approve
// and hung until --wait timed out). The exemption is structural — no policy
// file can reintroduce the deadlock by omission — and every other provider
// stays gated.
func TestUnit_HITLExempt_MissionProviderBypassesGate(t *testing.T) {
	var hits []string
	gated := execRecorder{hits: &hits, tag: "gated"}
	raw := execRecorder{hits: &hits, tag: "raw"}
	repo := hitlExemptProviders(gated, raw, "mission")

	now := time.Now()
	_, _, err := repo.Exec(context.Background(), now, nil, false, &taskengine.ToolsCall{Name: "mission", ToolName: "mission_report"})
	require.NoError(t, err)
	_, _, err = repo.Exec(context.Background(), now, nil, false, &taskengine.ToolsCall{Name: "local_fs", ToolName: "read_file"})
	require.NoError(t, err)
	_, _, err = repo.Exec(context.Background(), now, nil, false, nil)
	require.NoError(t, err)

	require.Equal(t, []string{"raw", "gated", "gated"}, hits,
		"mission provider bypasses the gate; every other call — including a nil ToolsCall — stays gated")
}
