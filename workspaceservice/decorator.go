package workspaceservice

import (
	"context"
	"time"

	"github.com/contenox/contenox/libtracker"
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

// workspaceActivityView is a reduced DTO for activity streams (omits absolute path).
type workspaceActivityView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	VfsPath string `json:"vfsPath"`
}

func activityView(w *WorkspaceDTO) *workspaceActivityView {
	if w == nil {
		return nil
	}
	return &workspaceActivityView{ID: w.ID, Name: w.Name, VfsPath: w.VfsPath}
}

func (d *activityTrackerDecorator) Create(ctx context.Context, principal string, in CreateInput) (*WorkspaceDTO, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "workspace",
		"name", in.Name)
	defer end()
	out, err := d.svc.Create(ctx, principal, in)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(out.ID, activityView(out))
	return out, nil
}

func (d *activityTrackerDecorator) Get(ctx context.Context, principal, id string) (*WorkspaceDTO, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "workspace", "workspaceID", id)
	defer end()
	out, err := d.svc.Get(ctx, principal, id)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *activityTrackerDecorator) List(ctx context.Context, principal string, cursor *time.Time, limit int) ([]*WorkspaceDTO, error) {
	reportErr, _, end := d.tracker.Start(ctx, "list", "workspace", "limit", limit)
	defer end()
	out, err := d.svc.List(ctx, principal, cursor, limit)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *activityTrackerDecorator) Update(ctx context.Context, principal, id string, in UpdateInput) (*WorkspaceDTO, error) {
	reportErr, reportChange, end := d.tracker.Start(ctx, "update", "workspace",
		"workspaceID", id, "name", in.Name)
	defer end()
	out, err := d.svc.Update(ctx, principal, id, in)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(out.ID, activityView(out))
	return out, nil
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, principal, id string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "delete", "workspace", "workspaceID", id)
	defer end()
	err := d.svc.Delete(ctx, principal, id)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange(id, nil)
	return nil
}
