package contenoxcli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// recordingPublisher captures the subjects a mission producer announces on.
type recordingPublisher struct {
	mu       sync.Mutex
	subjects []string
}

func (p *recordingPublisher) Publish(_ context.Context, subject string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subject)
	return nil
}

func (p *recordingPublisher) seen(subject string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.subjects {
		if s == subject {
			return true
		}
	}
	return false
}

// recordingAsker stands in for missionAttentionAsker: it answers, and counts.
type recordingAsker struct {
	answer string
	asks   []missiontools.AttentionAsk
}

func (a *recordingAsker) RaiseAttention(_ context.Context, ask missiontools.AttentionAsk) (string, error) {
	a.asks = append(a.asks, ask)
	return a.answer, nil
}

// TestUnit_LocalToolset_MissionToolsCarryTheAskerAndThePublisher pins that
// localToolset hands its mission service and attention asker to the provider it
// registers, rather than minting a bare durable-only one.
func TestUnit_LocalToolset_MissionToolsCarryTheAskerAndThePublisher(t *testing.T) {
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "resume-tools.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pub := &recordingPublisher{}
	missions := missionservice.New(db, missionservice.WithEventPublisher(pub))
	m := &missionservice.Mission{Intent: "resolve ambiguity", AgentName: "unit", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))

	asker := &recordingAsker{answer: "the main branch"}
	tools := localToolset(chatOpts{}, db, libtracker.NoopTracker{}, missions, missiontools.WithAttentionAsker(asker))
	repo := tools[missiontools.ToolsProviderName]
	require.NotNil(t, repo, "the resume path needs the mission tools registered")

	missionCtx := missiontools.WithMissionID(ctx, m.ID)
	out, _, err := repo.Exec(missionCtx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameAskAttention,
		Args: map[string]string{"summary": "which branch should I work on?", "detail": "two match"},
	})
	require.NoError(t, err)
	require.Equal(t, "the main branch", out, "the asker's answer IS the tool result")
	require.Len(t, asker.asks, 1, "the question reached the asker instead of being self-answered")
	require.Equal(t, m.ID, asker.asks[0].MissionID)

	reports, err := missions.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	for _, r := range reports {
		require.NotEqualf(t, missionservice.ReportKindBlocker, r.Kind,
			"an asker-less provider downgrades the question to this blocker (%q)", r.Summary)
	}

	_, _, err = repo.Exec(missionCtx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameFinish,
		Args: map[string]string{"status": string(missionservice.StatusLanded), "reason": "done"},
	})
	require.NoError(t, err)
	require.True(t, pub.seen(missionservice.StatusChangedSubject),
		"a resumed mission_finish must announce its terminal transition, or the unit subprocess is never reaped")
}
