package eventmappingservice

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/vibe/eventstore"
	libdb "github.com/contenox/vibe/libdbexec"
)

var (
	ErrInvalidMapping   = errors.New("invalid mapping configuration")
	ErrInvalidParameter = errors.New("invalid parameter")
)

// Service defines the interface for managing event mapping configurations
type Service interface {
	CreateMapping(ctx context.Context, config *eventstore.MappingConfig) error
	GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error)
	UpdateMapping(ctx context.Context, config *eventstore.MappingConfig) error
	DeleteMapping(ctx context.Context, path string) error
	ListMappings(ctx context.Context) ([]*eventstore.MappingConfig, error)
}

type service struct {
	store eventstore.Store
}

// New creates a new mapping service
func New(db libdb.DBManager) Service {
	exec := db.WithoutTransaction()
	store := eventstore.New(exec)
	return &service{store: store}
}

// validateMapping validates the mapping configuration
func (s *service) validateMapping(config *eventstore.MappingConfig) error {
	if config == nil {
		return fmt.Errorf("%w: config cannot be nil", ErrInvalidMapping)
	}
	if config.Path == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidMapping)
	}
	if config.EventType == "" {
		return fmt.Errorf("%w: event_type is required", ErrInvalidMapping)
	}
	if config.EventSource == "" {
		return fmt.Errorf("%w: event_source is required", ErrInvalidMapping)
	}
	if config.AggregateType == "" {
		return fmt.Errorf("%w: aggregate_type is required", ErrInvalidMapping)
	}
	if config.Version <= 0 {
		return fmt.Errorf("%w: version must be > 0, got %d", ErrInvalidMapping, config.Version)
	}
	return nil
}

// CreateMapping implements Service
func (s *service) CreateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	if err := s.validateMapping(config); err != nil {
		return err
	}

	return s.store.CreateMapping(ctx, config)
}

// GetMapping implements Service
func (s *service) GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidParameter)
	}

	config, err := s.store.GetMapping(ctx, path)
	if err != nil {
		return nil, err
	}
	return config, nil
}

// UpdateMapping implements Service
func (s *service) UpdateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	if err := s.validateMapping(config); err != nil {
		return err
	}

	return s.store.UpdateMapping(ctx, config)
}

// DeleteMapping implements Service
func (s *service) DeleteMapping(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidParameter)
	}

	return s.store.DeleteMapping(ctx, path)
}

// ListMappings implements Service
func (s *service) ListMappings(ctx context.Context) ([]*eventstore.MappingConfig, error) {
	return s.store.ListMappings(ctx)
}
