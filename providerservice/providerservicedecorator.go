package providerservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/libtracker"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) SetProviderConfig(ctx context.Context, providerType string, replace bool, config *runtimestate.ProviderConfig) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"set",
		"provider_config",
		"provider_type", providerType,
		"replace", replace,
	)
	defer endFn()

	err := d.service.SetProviderConfig(ctx, providerType, replace, config)
	if err != nil {
		reportErrFn(err)
	} else {
		var configMasked runtimestate.ProviderConfig
		configMasked.Type = config.Type
		configMasked.APIKey = "********"
		reportChangeFn(providerType, configMasked)
	}

	return err
}

func (d *activityTrackerDecorator) GetProviderConfig(ctx context.Context, providerType string) (*runtimestate.ProviderConfig, error) {
	// reportErrFn, _, endFn := d.tracker.Start(
	// 	ctx,
	// 	"get",
	// 	"provider_config",
	// 	"provider_type", providerType,
	// )
	// defer endFn()

	config, err := d.service.GetProviderConfig(ctx, providerType)
	// if err != nil {
	// 	reportErrFn(err)
	// }
	return config, err
}

func (d *activityTrackerDecorator) DeleteProviderConfig(ctx context.Context, providerType string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"provider_config",
		"provider_type", providerType,
	)
	defer endFn()

	err := d.service.DeleteProviderConfig(ctx, providerType)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(providerType, nil)
	}

	return err
}

func (d *activityTrackerDecorator) ListProviderConfigs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimestate.ProviderConfig, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"provider_configs",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	configs, err := d.service.ListProviderConfigs(ctx, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}
	return configs, err
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)
