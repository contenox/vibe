package affinitygroupservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/runtimetypes"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Create(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"group",
		"name", group.Name,
		"purposeType", group.PurposeType,
	)
	defer endFn()

	err := d.service.Create(ctx, group)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(group.ID, map[string]interface{}{
			"name":        group.Name,
			"purposeType": group.PurposeType,
		})
	}

	return err
}

func (d *activityTrackerDecorator) GetByID(ctx context.Context, id string) (*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"group",
		"groupID", id,
	)
	defer endFn()

	group, err := d.service.GetByID(ctx, id)
	if err != nil {
		reportErrFn(err)
	}

	return group, err
}

func (d *activityTrackerDecorator) GetByName(ctx context.Context, name string) (*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"group",
		"name", name,
	)
	defer endFn()

	group, err := d.service.GetByName(ctx, name)
	if err != nil {
		reportErrFn(err)
	}

	return group, err
}

func (d *activityTrackerDecorator) Update(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"group",
		"groupID", group.ID,
		"name", group.Name,
	)
	defer endFn()

	err := d.service.Update(ctx, group)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(group.ID, map[string]interface{}{
			"name":        group.Name,
			"purposeType": group.PurposeType,
		})
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"group",
		"groupID", id,
	)
	defer endFn()

	err := d.service.Delete(ctx, id)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(id, nil)
	}

	return err
}

func (d *activityTrackerDecorator) ListAll(ctx context.Context) ([]*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(ctx, "list", "groups")
	defer endFn()

	groups, err := d.service.ListAll(ctx)
	if err != nil {
		reportErrFn(err)
	}

	return groups, err
}

func (d *activityTrackerDecorator) ListByPurpose(ctx context.Context, purpose string, createdAtCursor *time.Time, limit int) ([]*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"groups-by-purpose",
		"purpose", purpose,
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	groups, err := d.service.ListByPurpose(ctx, purpose, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return groups, err
}

func (d *activityTrackerDecorator) AssignBackend(ctx context.Context, groupID, backendID string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"assign",
		"backend-to-group",
		"groupID", groupID,
		"backendID", backendID,
	)
	defer endFn()

	err := d.service.AssignBackend(ctx, groupID, backendID)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(groupID, map[string]interface{}{
			"backendID": backendID,
		})
	}

	return err
}

func (d *activityTrackerDecorator) RemoveBackend(ctx context.Context, groupID, backendID string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"remove",
		"backend-from-group",
		"groupID", groupID,
		"backendID", backendID,
	)
	defer endFn()

	err := d.service.RemoveBackend(ctx, groupID, backendID)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(groupID, map[string]interface{}{
			"backendID": backendID,
		})
	}

	return err
}

func (d *activityTrackerDecorator) ListBackends(ctx context.Context, groupID string) ([]*runtimetypes.Backend, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"group-backends",
		"groupID", groupID,
	)
	defer endFn()

	backends, err := d.service.ListBackends(ctx, groupID)
	if err != nil {
		reportErrFn(err)
	}

	return backends, err
}

func (d *activityTrackerDecorator) ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"groups-for-backend",
		"backendID", backendID,
	)
	defer endFn()

	groups, err := d.service.ListAffinityGroupsForBackend(ctx, backendID)
	if err != nil {
		reportErrFn(err)
	}

	return groups, err
}

func (d *activityTrackerDecorator) AssignModel(ctx context.Context, groupID, modelID string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"assign",
		"model-to-group",
		"groupID", groupID,
		"modelID", modelID,
	)
	defer endFn()

	err := d.service.AssignModel(ctx, groupID, modelID)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(groupID, map[string]interface{}{
			"modelID": modelID,
		})
	}

	return err
}

func (d *activityTrackerDecorator) RemoveModel(ctx context.Context, groupID, modelID string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"remove",
		"model-from-group",
		"groupID", groupID,
		"modelID", modelID,
	)
	defer endFn()

	err := d.service.RemoveModel(ctx, groupID, modelID)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(groupID, map[string]interface{}{
			"modelID": modelID,
		})
	}

	return err
}

func (d *activityTrackerDecorator) ListModels(ctx context.Context, groupID string) ([]*runtimetypes.Model, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"group-models",
		"groupID", groupID,
	)
	defer endFn()

	models, err := d.service.ListModels(ctx, groupID)
	if err != nil {
		reportErrFn(err)
	}

	return models, err
}

func (d *activityTrackerDecorator) ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*runtimetypes.AffinityGroup, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"groups-for-model",
		"modelID", modelID,
	)
	defer endFn()

	groups, err := d.service.ListAffinityGroupsForModel(ctx, modelID)
	if err != nil {
		reportErrFn(err)
	}

	return groups, err
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)
