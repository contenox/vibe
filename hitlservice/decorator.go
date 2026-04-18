package hitlservice

import (
	"context"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/taskengine"
)

type activityTrackerDecorator struct {
	svc     Service
	tracker libtracker.ActivityTracker
}

// WithActivityTracker wraps a Service with activity logging.
func WithActivityTracker(svc Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{svc: svc, tracker: tracker}
}

var _ Service = (*activityTrackerDecorator)(nil)

func (d *activityTrackerDecorator) Evaluate(ctx context.Context, hookName, toolName string, args map[string]any) (EvaluationResult, error) {
	reportErr, _, end := d.tracker.Start(ctx, "evaluate", "hitl_policy", "hook", hookName, "tool", toolName)
	defer end()
	result, err := d.svc.Evaluate(ctx, hookName, toolName, args)
	if err != nil {
		reportErr(err)
		return EvaluationResult{}, err
	}
	return result, nil
}

func (d *activityTrackerDecorator) RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error) {
	reportErr, _, end := d.tracker.Start(ctx, "request", "hitl_approval", "hook", req.HookName, "tool", req.ToolName)
	defer end()
	ok, err := d.svc.RequestApproval(ctx, req, sink)
	if err != nil {
		reportErr(err)
		return false, err
	}
	return ok, nil
}

func (d *activityTrackerDecorator) Respond(approvalID string, approved bool) bool {
	return d.svc.Respond(approvalID, approved)
}
