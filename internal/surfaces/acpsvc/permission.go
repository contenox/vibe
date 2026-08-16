package acpsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
)

func (t *Transport) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	acpSessionID, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return false, libacp.InternalError("this tool call needs approval, but no editor session is attached to answer it — answer it from a terminal with `contenox approvals list`")
	}

	toolCallID := approvalflow.ToolCallID(req)
	rpcReq := approvalflow.BuildRequest(req, approvalflow.BuildOptions{
		SessionID:   acpSessionID,
		PolicyName:  req.PolicyName,
		PolicyPath:  t.hitlPolicyPath(req.PolicyName),
		MatchedRule: req.MatchedRule,
		Detail:      req.Detail,
	})
	t.attachAskRecovery(ctx, &rpcReq, toolCallID)

	t.markPermissionPending(acpSessionID, toolCallID)
	defer t.clearPermissionPending(acpSessionID, toolCallID)

	reportErr, reportChange, end := t.tracker().Start(ctx, "hitl", "acp_permission", "tool_call_id", toolCallID)
	defer end()

	resp, err := t.conn.RequestPermission(ctx, rpcReq)
	if err != nil {
		ctxErr := ""
		if e := ctx.Err(); e != nil {
			ctxErr = e.Error()
		}
		reportChange("rpc_error", err.Error())
		reportChange("ctx_err", ctxErr)
		reportErr(err)
		return false, err
	}
	reportChange("outcome", string(resp.Outcome.Outcome))
	reportChange("option_id", resp.Outcome.OptionID)
	switch resp.Outcome.Outcome {
	case libacp.PermissionOutcomeCancelled:
		return false, context.Canceled
	case libacp.PermissionOutcomeSelected:
		return resp.Outcome.OptionID == approvalflow.OptionAllow, nil
	}
	return false, nil
}

// askRecovery is the durable half of a permission card: when the ask stops being
// answerable, what happens then, and the command that answers it elsewhere.
type askRecovery struct {
	AskID           string `json:"askId,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	OnTimeout       string `json:"onTimeout,omitempty"`
	RecoveryCommand string `json:"recoveryCommand,omitempty"`
}

// attachAskRecovery merges the ask's deadline and recovery command into the
// permission request's `_meta`. The deadline is the durable row's ExpiresAt, not
// localtools.ApprovalParkWindow, and nothing is attached unless the row is found
// under the id this card carries.
func (t *Transport) attachAskRecovery(ctx context.Context, rpcReq *libacp.RequestPermissionRequest, askID string) {
	if t.deps.DB == nil || strings.TrimSpace(askID) == "" {
		return
	}
	row, err := runtimetypes.New(t.deps.DB.WithoutTransaction()).GetHITLApproval(ctx, askID)
	if err != nil || row == nil {
		return
	}
	rec := askRecovery{
		AskID:           askID,
		OnTimeout:       row.OnTimeout,
		RecoveryCommand: "contenox approvals respond " + askID + " --approve|--deny",
	}
	if !row.ExpiresAt.IsZero() {
		rec.ExpiresAt = row.ExpiresAt.UTC().Format(time.RFC3339)
	}
	rpcReq.Meta = mergeMetaFields(rpcReq.Meta, rec)
	rpcReq.ToolCall.Meta = mergeMetaFields(rpcReq.ToolCall.Meta, rec)
}

// mergeMetaFields adds extra's fields to a `_meta` object, keeping every key
// already there, and returns base unchanged if either side is not a JSON object.
func mergeMetaFields(base json.RawMessage, extra any) json.RawMessage {
	fields := map[string]json.RawMessage{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &fields); err != nil {
			return base
		}
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return base
	}
	added := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &added); err != nil {
		return base
	}
	for k, v := range added {
		fields[k] = v
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return base
	}
	return out
}

// hitlPolicyPath resolves the on-disk path of a named HITL policy for display.
func (t *Transport) hitlPolicyPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || t.deps.ContenoxDir == "" {
		return ""
	}
	return filepath.Join(t.deps.ContenoxDir, name)
}
