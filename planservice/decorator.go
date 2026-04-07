package planservice

import (
	"context"
	"strconv"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/planstore"
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

type planActivityView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func planView(p *planstore.Plan) *planActivityView {
	if p == nil {
		return nil
	}
	return &planActivityView{ID: p.ID, Name: p.Name, Status: string(p.Status)}
}

func (d *activityTrackerDecorator) New(ctx context.Context, goal string, plannerChain *taskengine.TaskChainDefinition) (*planstore.Plan, []*planstore.PlanStep, string, error) {
	chainID := ""
	if plannerChain != nil {
		chainID = plannerChain.ID
	}
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "plan", "plannerChainID", chainID)
	defer end()
	p, steps, md, err := d.svc.New(ctx, goal, plannerChain)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	if p != nil {
		reportChange(p.ID, planView(p))
	}
	return p, steps, md, nil
}

func (d *activityTrackerDecorator) Replan(ctx context.Context, plannerChain *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error) {
	chainID := ""
	if plannerChain != nil {
		chainID = plannerChain.ID
	}
	reportErr, reportChange, end := d.tracker.Start(ctx, "replan", "plan", "plannerChainID", chainID)
	defer end()
	steps, md, err := d.svc.Replan(ctx, plannerChain)
	if err != nil {
		reportErr(err)
		return nil, "", err
	}
	if len(steps) > 0 {
		reportChange(steps[0].PlanID, map[string]string{"op": "replan"})
	}
	return steps, md, nil
}

func (d *activityTrackerDecorator) Next(ctx context.Context, args Args, executorChain *taskengine.TaskChainDefinition) (string, string, error) {
	execID := ""
	if executorChain != nil {
		execID = executorChain.ID
	}
	reportErr, reportChange, end := d.tracker.Start(ctx, "next", "plan_step",
		"executorChainID", execID, "withShell", args.WithShell, "withAuto", args.WithAuto)
	defer end()
	r1, r2, err := d.svc.Next(ctx, args, executorChain)
	if err != nil {
		reportErr(err)
		return r1, r2, err
	}
	if p, _, aerr := d.svc.Active(ctx); aerr == nil && p != nil {
		reportChange(p.ID, map[string]string{"op": "next"})
	}
	return r1, r2, nil
}

func (d *activityTrackerDecorator) Retry(ctx context.Context, ordinal int) (string, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "retry", "plan_step", "ordinal", ordinal)
	defer end()
	md, err := d.svc.Retry(ctx, ordinal)
	if err != nil {
		reportErr(err)
		return "", err
	}
	if p, _, aerr := d.svc.Active(ctx); aerr == nil && p != nil {
		reportChange(p.ID, map[string]string{"op": "retry", "ordinal": strconv.Itoa(ordinal)})
	}
	return md, nil
}

func (d *activityTrackerDecorator) Skip(ctx context.Context, ordinal int) (string, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "skip", "plan_step", "ordinal", ordinal)
	defer end()
	md, err := d.svc.Skip(ctx, ordinal)
	if err != nil {
		reportErr(err)
		return "", err
	}
	if p, _, aerr := d.svc.Active(ctx); aerr == nil && p != nil {
		reportChange(p.ID, map[string]string{"op": "skip", "ordinal": strconv.Itoa(ordinal)})
	}
	return md, nil
}

func (d *activityTrackerDecorator) Active(ctx context.Context) (*planstore.Plan, []*planstore.PlanStep, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "plan", "scope", "active")
	defer end()
	p, steps, err := d.svc.Active(ctx)
	if err != nil {
		reportErr(err)
		return nil, nil, err
	}
	return p, steps, nil
}

func (d *activityTrackerDecorator) Show(ctx context.Context) (string, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "plan", "scope", "show")
	defer end()
	md, err := d.svc.Show(ctx)
	if err != nil {
		reportErr(err)
		return "", err
	}
	return md, nil
}

func (d *activityTrackerDecorator) List(ctx context.Context) ([]*planstore.Plan, error) {
	reportErr, _, end := d.tracker.Start(ctx, "list", "plan")
	defer end()
	out, err := d.svc.List(ctx)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *activityTrackerDecorator) SetActive(ctx context.Context, planName string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "set_active", "plan", "planName", planName)
	defer end()
	err := d.svc.SetActive(ctx, planName)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange(planName, map[string]string{"op": "set_active", "name": planName})
	return nil
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, planName string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "delete", "plan", "planName", planName)
	defer end()
	err := d.svc.Delete(ctx, planName)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange(planName, nil)
	return nil
}

func (d *activityTrackerDecorator) Clean(ctx context.Context) (int, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "clean", "plan")
	defer end()
	n, err := d.svc.Clean(ctx)
	if err != nil {
		reportErr(err)
		return 0, err
	}
	reportChange(strconv.Itoa(n), map[string]int{"removed": n})
	return n, nil
}
