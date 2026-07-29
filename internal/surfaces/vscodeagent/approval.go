package vscodeagent

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libacp"
	"github.com/google/uuid"
)

type ApprovalBroker struct {
	request      func(context.Context, libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
	activePolicy func(context.Context) hitlPolicyRef
	sessionID    func(context.Context) string
}

func NewApprovalBroker(
	request func(context.Context, libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error),
	activePolicy func(context.Context) hitlPolicyRef,
	sessionID func(context.Context) string,
) *ApprovalBroker {
	return &ApprovalBroker{
		request:      request,
		activePolicy: activePolicy,
		sessionID:    sessionID,
	}
}

func (b *ApprovalBroker) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	if b == nil || b.request == nil {
		return false, fmt.Errorf("vscodeagent: approval broker is not configured")
	}
	if req.ToolCallID == "" {
		req.ToolCallID = uuid.NewString()
	}

	var policy hitlPolicyRef
	if b.activePolicy != nil {
		policy = b.activePolicy(ctx)
	}
	sessionID := ""
	if b.sessionID != nil {
		sessionID = b.sessionID(ctx)
	}
	rpcReq := approvalflow.BuildRequest(req, approvalflow.BuildOptions{
		SessionID:  libacp.SessionID(sessionID),
		PolicyName: policy.Name,
		PolicyPath: policy.Path,
		Detail:     req.Detail,
	})
	resp, err := b.request(ctx, rpcReq)
	if err != nil {
		return false, err
	}
	switch resp.Outcome.Outcome {
	case libacp.PermissionOutcomeCancelled:
		return false, context.Canceled
	case libacp.PermissionOutcomeSelected:
		return resp.Outcome.OptionID == approvalflow.OptionAllow, nil
	default:
		return false, nil
	}
}
