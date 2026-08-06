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
	// The card outlives this call: the run parks, checkpoints, and releases
	// its process, and the ask stays answerable from any terminal. Tell the
	// client how long that lasts and what answers it there.
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

// askRecovery is the durable half of a permission card: when the ask stops
// being answerable, what happens then, and the command that answers it from
// any other process. Marshalled beside approvalflow.Meta's own keys (see
// attachAskRecovery), not as a second envelope, so a client parses one object.
type askRecovery struct {
	// AskID is the durable ask's id — the argument `contenox approvals
	// respond` takes, and the same value as the tool call id on this card.
	AskID string `json:"askId,omitempty"`
	// ExpiresAt is when the ask resolves itself with OnTimeout: the deadline a
	// countdown counts to, RFC3339 UTC.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// OnTimeout is the verdict that applies at ExpiresAt if nobody answers.
	OnTimeout string `json:"onTimeout,omitempty"`
	// RecoveryCommand answers this ask from a terminal, for a client that has
	// lost the card (or a run already checkpointed and released).
	RecoveryCommand string `json:"recoveryCommand,omitempty"`
}

// attachAskRecovery merges the ask's deadline and recovery command into the
// permission request's `_meta` (both the request-level envelope and the tool
// call's copy, which clients read interchangeably).
//
// The deadline is the durable row's ExpiresAt, NOT localtools.ApprovalParkWindow:
// the park window only decides when the run checkpoints and releases its
// process — the card stays answerable across it, and a late verdict still
// resumes the run (localtools deliverLateVerdict). ExpiresAt is the moment
// SweepExpired applies the ask's on-timeout verdict and any answer is refused,
// which is what a countdown must count to.
//
// Nothing is attached unless the row is found under the very id this card
// carries — the non-durable blocking path records none, and a call with no
// engine-minted tool call id gets a uuid-keyed row this side cannot name.
// Printing a recovery command for a row `contenox approvals respond` cannot
// find would be a lie.
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
// already there. Returns base unchanged if either side is not a JSON object,
// so a malformed envelope loses nothing.
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
