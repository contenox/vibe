package acpsvc

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
)

func (t *Transport) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	acpSessionID, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return false, libacp.InternalError("acpsvc: no ACP session bound to contenox session " + contenoxSessionID)
	}

	toolCallID := approvalflow.ToolCallID(req)
	// req.PolicyName, req.MatchedRule, and req.Detail are the verdict that
	// actually gated this call (set by localtools.HITLWrapper from
	// hitlservice.EvaluationResult), not re-derived guesses — forward them
	// as-is so the card can say why.
	rpcReq := approvalflow.BuildRequest(req, approvalflow.BuildOptions{
		SessionID:   acpSessionID,
		PolicyName:  req.PolicyName,
		PolicyPath:  t.hitlPolicyPath(req.PolicyName),
		MatchedRule: req.MatchedRule,
		Detail:      req.Detail,
	})

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

// hitlPolicyPath resolves the on-disk path of a named HITL policy for
// display, mirroring how acpPolicySource (contenoxcli/acp_cmd.go) and
// writeEmbeddedHITLPolicies resolve policy files under ContenoxDir. Empty
// name or unset ContenoxDir (e.g. a setup-only transport) yields "".
func (t *Transport) hitlPolicyPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || t.deps.ContenoxDir == "" {
		return ""
	}
	return filepath.Join(t.deps.ContenoxDir, name)
}
