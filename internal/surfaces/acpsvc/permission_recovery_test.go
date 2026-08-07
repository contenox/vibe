package acpsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// newRecoveryTestTransport is a Transport over a real store, the only thing
// attachAskRecovery reads.
func newRecoveryTestTransport(t *testing.T) (*Transport, libdb.DBManager) {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "recovery-acp.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Transport{deps: Deps{DB: db, ContenoxDir: "/home/op/.contenox"}}, db
}

func permissionRequestFor(askID string) libacp.RequestPermissionRequest {
	return approvalflow.BuildRequest(hitlservice.ApprovalRequest{
		ToolCallID: askID,
		ToolsName:  "local_fs",
		ToolName:   "write_file",
		Args:       map[string]any{"path": "/tmp/x"},
		PolicyName: "hitl-policy-strict.json",
	}, approvalflow.BuildOptions{SessionID: "sess-1", PolicyName: "hitl-policy-strict.json"})
}

// TestUnit_AttachAskRecovery_CarriesDeadlineAndRecoveryCommand pins what a
// parked permission card needs to stay actionable: the ask's own deadline (so
// a client can count down), the verdict that lands at it, and the command that
// answers it from any other process. The existing approvalflow envelope must
// survive the merge — clients parse one object, not two.
func TestUnit_AttachAskRecovery_CarriesDeadlineAndRecoveryCommand(t *testing.T) {
	tr, db := newRecoveryTestTransport(t)
	expires := time.Now().UTC().Add(42 * time.Minute).Truncate(time.Second)
	require.NoError(t, runtimetypes.New(db.WithoutTransaction()).CreateHITLApproval(context.Background(), &runtimetypes.HITLApproval{
		ID:          "ask-park-1",
		ToolsName:   "local_fs",
		ToolName:    "write_file",
		ArgsSummary: "/tmp/x",
		State:       runtimetypes.HITLApprovalPending,
		OnTimeout:   "deny",
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   expires,
	}))

	req := permissionRequestFor("ask-park-1")
	tr.attachAskRecovery(context.Background(), &req, "ask-park-1")

	for _, raw := range []json.RawMessage{req.Meta, req.ToolCall.Meta} {
		var rec askRecovery
		require.NoError(t, json.Unmarshal(raw, &rec))
		require.Equal(t, "ask-park-1", rec.AskID)
		require.Equal(t, expires.Format(time.RFC3339), rec.ExpiresAt, "the countdown must run to the ask's own expiry, not the park window")
		require.Equal(t, "deny", rec.OnTimeout)
		require.Equal(t, "contenox approvals respond ask-park-1 --approve|--deny", rec.RecoveryCommand)

		meta, ok := approvalflow.ParseMeta(raw)
		require.True(t, ok, "the policy envelope must survive the merge")
		require.Equal(t, "hitl-policy-strict.json", meta.PolicyName)
		require.Equal(t, "local_fs", meta.ToolsName)
	}
}

// TestUnit_AttachAskRecovery_SaysNothingWithoutADurableRow pins the honesty
// rule: with no durable row behind the ask (the non-durable blocking path),
// naming a recovery command would point at a row `approvals respond` cannot
// find, so nothing is attached and the envelope is untouched.
func TestUnit_AttachAskRecovery_SaysNothingWithoutADurableRow(t *testing.T) {
	tr, _ := newRecoveryTestTransport(t)
	req := permissionRequestFor("ask-not-recorded")
	before := string(req.Meta)

	tr.attachAskRecovery(context.Background(), &req, "ask-not-recorded")
	require.Equal(t, before, string(req.Meta))

	var rec askRecovery
	require.NoError(t, json.Unmarshal(req.Meta, &rec))
	require.Empty(t, rec.RecoveryCommand)
	require.Empty(t, rec.ExpiresAt)

	// A transport with no store at all must not panic or invent a command.
	noStore := &Transport{}
	noStore.attachAskRecovery(context.Background(), &req, "ask-not-recorded")
	require.Equal(t, before, string(req.Meta))
}
