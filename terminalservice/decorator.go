package terminalservice

import (
	"context"
	"time"

	"github.com/coder/websocket"
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

func (d *activityTrackerDecorator) Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error) {
	kv := []any{"cols", req.Cols, "rows", req.Rows, "cwd", req.CWD}
	reportErr, reportChange, end := d.tracker.Start(ctx, "create", "terminal_session", kv...)
	defer end()
	out, err := d.svc.Create(ctx, principal, req)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(out.ID, map[string]string{"id": out.ID})
	return out, nil
}

func (d *activityTrackerDecorator) Close(ctx context.Context, principal, id string) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "delete", "terminal_session", "sessionID", id)
	defer end()
	err := d.svc.Close(ctx, principal, id)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange(id, nil)
	return nil
}

func (d *activityTrackerDecorator) CloseAll(ctx context.Context) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "close_all", "terminal_session")
	defer end()
	err := d.svc.CloseAll(ctx)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange("_", map[string]string{"op": "close_all"})
	return nil
}

func (d *activityTrackerDecorator) Attach(ctx context.Context, principal, id string, conn *websocket.Conn) error {
	reportErr, _, end := d.tracker.Start(ctx, "attach", "terminal_session", "sessionID", id)
	defer end()
	err := d.svc.Attach(ctx, principal, id, conn)
	if err != nil {
		reportErr(err)
	}
	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, principal, id string) (*SessionInfo, error) {
	reportErr, _, end := d.tracker.Start(ctx, "read", "terminal_session", "sessionID", id)
	defer end()
	out, err := d.svc.Get(ctx, principal, id)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *activityTrackerDecorator) List(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*SessionInfo, error) {
	reportErr, _, end := d.tracker.Start(ctx, "list", "terminal_session", "limit", limit)
	defer end()
	out, err := d.svc.List(ctx, principal, createdAtCursor, limit)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	return out, nil
}

func (d *activityTrackerDecorator) UpdateGeometry(ctx context.Context, principal, id string, cols, rows int) error {
	reportErr, reportChange, end := d.tracker.Start(ctx, "update", "terminal_session",
		"sessionID", id, "cols", cols, "rows", rows)
	defer end()
	err := d.svc.UpdateGeometry(ctx, principal, id, cols, rows)
	if err != nil {
		reportErr(err)
		return err
	}
	reportChange(id, map[string]any{"cols": cols, "rows": rows})
	return nil
}
