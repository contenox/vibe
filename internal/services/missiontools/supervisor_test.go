package missiontools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/stretchr/testify/require"
)

type fakeSupervisor struct {
	missions []*missionservice.Mission
	reports  map[string][]*missionservice.Report
}

func (f *fakeSupervisor) MissionsFiredBy(_ context.Context, parentSessionID string, _ int) ([]*missionservice.Mission, error) {
	out := []*missionservice.Mission{}
	for _, m := range f.missions {
		if m.ParentSessionID == parentSessionID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeSupervisor) ListReports(_ context.Context, missionID string, _ int) ([]*missionservice.Report, error) {
	return f.reports[missionID], nil
}

type fakeResolver struct {
	pending  map[string][]missiontools.PendingAsk
	answered map[string]string
}

func (f *fakeResolver) PendingAsks(_ context.Context, missionID string) ([]missiontools.PendingAsk, error) {
	return f.pending[missionID], nil
}

func (f *fakeResolver) AnswerAsAgent(_ context.Context, askID, text string) error {
	if f.answered == nil {
		f.answered = map[string]string{}
	}
	f.answered[askID] = text
	return nil
}

func supervisorFixture(t *testing.T) (context.Context, taskengine.ToolsRepo, *fakeResolver) {
	t.Helper()
	ctx, svc, _ := setup(t)
	sup := &fakeSupervisor{
		missions: []*missionservice.Mission{
			{ID: "m-mine", AgentName: "chain-acp", Intent: "explain the moat", Status: missionservice.StatusOpen, ParentSessionID: "cnx-parent"},
			{ID: "m-other", AgentName: "chain-acp", Intent: "someone else's", Status: missionservice.StatusOpen, ParentSessionID: "cnx-stranger"},
		},
		reports: map[string][]*missionservice.Report{
			"m-mine": {{Kind: missionservice.ReportKindProgress, Summary: "read the README"}},
		},
	}
	res := &fakeResolver{pending: map[string][]missiontools.PendingAsk{
		"m-mine":  {{AskID: "ask-mine", MissionID: "m-mine", Question: "which project?"}},
		"m-other": {{AskID: "ask-other", MissionID: "m-other", Question: "not yours"}},
	}}
	return ctx, missiontools.New(svc, nil, missiontools.WithSupervision(sup, res)), res
}

// TestUnit_Supervisor_ToolsUnlockForAFiringSessionOnly pins that the supervisor/unit/neither surfaces gate on separate facts.
func TestUnit_Supervisor_ToolsUnlockForAFiringSessionOnly(t *testing.T) {
	ctx, tools, _ := supervisorFixture(t)
	repo := tools.(interface {
		GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error)
	})

	plain, err := repo.GetToolsForToolsByName(ctx, missiontools.ToolsProviderName)
	require.NoError(t, err)
	require.Empty(t, plain, "an ordinary chat session is offered no mission tools at all")

	supervisor, err := repo.GetToolsForToolsByName(missiontools.WithParentSessionID(ctx, "cnx-parent"), missiontools.ToolsProviderName)
	require.NoError(t, err)
	names := toolNames(supervisor)
	require.ElementsMatch(t, []string{missiontools.ToolNameListMissions, missiontools.ToolNameAnswer}, names,
		"a firing session supervises: it looks and it answers — it does not report or finish")

	unit, err := repo.GetToolsForToolsByName(missiontools.WithMissionID(ctx, "m-mine"), missiontools.ToolsProviderName)
	require.NoError(t, err)
	require.NotContains(t, toolNames(unit), missiontools.ToolNameAnswer,
		"a unit answers nothing — it asks")
}

// TestUnit_Supervisor_ListShowsOwnMissionsAndWhatWaitsOnYou pins that the list shows own missions, their reports, and the askId waiting.
func TestUnit_Supervisor_ListShowsOwnMissionsAndWhatWaitsOnYou(t *testing.T) {
	ctx, tools, _ := supervisorFixture(t)

	out, _, err := tools.Exec(missiontools.WithParentSessionID(ctx, "cnx-parent"), time.Now(), nil, false,
		&taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameListMissions})
	require.NoError(t, err)

	text, ok := out.(string)
	require.True(t, ok)
	require.Contains(t, text, "m-mine")
	require.Contains(t, text, "explain the moat")
	require.Contains(t, text, "read the README", "the unit's reports are what 'how is it going' means")
	require.Contains(t, text, "ask-mine", "the answer handle must be reachable without a second tool")
	require.NotContains(t, text, "m-other", "a supervisor sees ITS missions, never another session's")
}

// TestUnit_Supervisor_AnswerReachesTheUnit pins that an answer reaches the waiting unit.
func TestUnit_Supervisor_AnswerReachesTheUnit(t *testing.T) {
	ctx, tools, res := supervisorFixture(t)

	out, _, err := tools.Exec(missiontools.WithParentSessionID(ctx, "cnx-parent"), time.Now(), nil, false,
		&taskengine.ToolsCall{
			Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameAnswer,
			Args: map[string]string{"askId": "ask-mine", "answer": "the runtime repo, docs/ only"},
		})
	require.NoError(t, err)
	require.Contains(t, out.(string), "ask-mine")
	require.Equal(t, "the runtime repo, docs/ only", res.answered["ask-mine"])
}

// TestUnit_Supervisor_AnswerRefusesAnotherSessionsUnit pins that guessing another session's ask id is refused.
func TestUnit_Supervisor_AnswerRefusesAnotherSessionsUnit(t *testing.T) {
	ctx, tools, res := supervisorFixture(t)

	_, _, err := tools.Exec(missiontools.WithParentSessionID(ctx, "cnx-parent"), time.Now(), nil, false,
		&taskengine.ToolsCall{
			Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameAnswer,
			Args: map[string]string{"askId": "ask-other", "answer": "meddling"},
		})
	require.Error(t, err)
	require.Empty(t, res.answered)
}

// TestUnit_Supervisor_ToolsRefuseWithoutASupervisingSession pins that Exec (not just listing) refuses without a supervising session.
func TestUnit_Supervisor_ToolsRefuseWithoutASupervisingSession(t *testing.T) {
	ctx, tools, _ := supervisorFixture(t)
	_, _, err := tools.Exec(ctx, time.Now(), nil, false,
		&taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameListMissions})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fired missions")
}

func toolNames(tools []taskengine.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, strings.TrimSpace(tool.Function.Name))
	}
	return out
}
