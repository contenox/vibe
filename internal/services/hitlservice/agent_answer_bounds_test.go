package hitlservice_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeMissions resolves every mission id to one fixed envelope name.
type fakeMissions struct {
	policy string
	err    error
}

func (f fakeMissions) Get(_ context.Context, id string) (*missionservice.Mission, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &missionservice.Mission{ID: id, HITLPolicyName: f.policy}, nil
}

func boundsFixture(t *testing.T, envelope string) (hitlservice.Service, runtimetypes.Store, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "envelope.json"), []byte(envelope), 0o644))
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "t.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")
	return svc, store, "envelope.json"
}

func attentionRow(missionID string) *runtimetypes.HITLApproval {
	now := time.Now().UTC()
	row := &runtimetypes.HITLApproval{
		ID: uuid.NewString(), ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "q", OnTimeout: string(hitlservice.ActionDeny), State: runtimetypes.HITLApprovalPending,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if missionID != "" {
		row.MissionID = &missionID
	}
	return row
}

// TestUnit_EnforceAgentAnswerBounds_GrantWithBudgetPasses pins the pass path.
func TestUnit_EnforceAgentAnswerBounds_GrantWithBudgetPasses(t *testing.T) {
	svc, _, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	err := hitlservice.EnforceAgentAnswerBounds(context.Background(), fakeMissions{policy: policy}, svc, attentionRow("m-1"))
	require.NoError(t, err)
}

// TestUnit_EnforceAgentAnswerBounds_RefusalsAreTyped pins every refusal as
// *AgentAnswerBoundsError — the branch both surfaces (CLI respond and the
// oracle driver) share.
func TestUnit_EnforceAgentAnswerBounds_RefusalsAreTyped(t *testing.T) {
	t.Run("no mission on the ask", func(t *testing.T) {
		svc, _, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true}}`)
		err := hitlservice.EnforceAgentAnswerBounds(context.Background(), fakeMissions{policy: policy}, svc, attentionRow(""))
		require.Error(t, err)
		require.True(t, hitlservice.IsAgentAnswerRefusal(err))
		require.Contains(t, err.Error(), "belongs to no mission")
	})

	t.Run("envelope forbids", func(t *testing.T) {
		svc, _, policy := boundsFixture(t, `{"default_action":"deny","rules":[]}`)
		err := hitlservice.EnforceAgentAnswerBounds(context.Background(), fakeMissions{policy: policy}, svc, attentionRow("m-1"))
		require.Error(t, err)
		require.True(t, hitlservice.IsAgentAnswerRefusal(err))
		require.Contains(t, err.Error(), "does not allow agent answers")
	})

	t.Run("bound spent, counted on durable records", func(t *testing.T) {
		svc, store, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":1}}`)
		ctx := context.Background()
		spent := attentionRow("m-1")
		require.NoError(t, store.CreateHITLApproval(ctx, spent))
		require.NoError(t, svc.AnswerAsAgentNamed(ctx, spent.ID, "oracle", "first answer"))

		err := hitlservice.EnforceAgentAnswerBounds(ctx, fakeMissions{policy: policy}, svc, attentionRow("m-1"))
		require.Error(t, err)
		require.True(t, hitlservice.IsAgentAnswerRefusal(err))
		require.Contains(t, err.Error(), "spent its agent-answer bound (1 of 1)")
	})

	t.Run("mission unreadable", func(t *testing.T) {
		svc, _, _ := boundsFixture(t, `{"default_action":"deny","rules":[]}`)
		err := hitlservice.EnforceAgentAnswerBounds(context.Background(), fakeMissions{err: context.DeadlineExceeded}, svc, attentionRow("m-1"))
		require.Error(t, err)
		require.True(t, hitlservice.IsAgentAnswerRefusal(err))
		require.Contains(t, err.Error(), "cannot be read")
	})
}
