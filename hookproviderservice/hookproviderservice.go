package hookproviderservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/apiframework"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

var (
	ErrInvalidHook = errors.New("invalid remote hook data")
)

// Service defines the interface for managing remote hooks and querying hook capabilities.
type Service interface {
	Create(ctx context.Context, hook *runtimetypes.RemoteHook) error
	Get(ctx context.Context, id string) (*runtimetypes.RemoteHook, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.RemoteHook, error)
	Update(ctx context.Context, hook *runtimetypes.RemoteHook) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteHook, error)
	GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error)
	ListLocalHooks(ctx context.Context) ([]LocalHook, error)
}

type LocalHook struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Tools       []taskengine.Tool `json:"tools,omitempty"`
}

type service struct {
	dbInstance   libdb.DBManager
	hookRegistry taskengine.HookProvider
}

// New creates a new service instance.
func New(dbInstance libdb.DBManager, hookRegistry taskengine.HookProvider) Service {
	return &service{
		dbInstance:   dbInstance,
		hookRegistry: hookRegistry,
	}
}

// GetSchemasForSupportedHooks delegates the call to the hook registry.
func (s *service) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	if s.hookRegistry == nil {
		return nil, errors.New("hook registry is not configured for this service")
	}
	return s.hookRegistry.GetSchemasForSupportedHooks(ctx)
}

// ListLocalHooks returns all locally registered hooks
func (s *service) ListLocalHooks(ctx context.Context) ([]LocalHook, error) {
	if s.hookRegistry == nil {
		return nil, errors.New("hook registry is not configured for this service")
	}

	supported, err := s.hookRegistry.Supports(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get supported hooks: %w", err)
	}

	localHooks := make([]LocalHook, 0, len(supported))
	for _, name := range supported {
		tools, err := s.hookRegistry.GetToolsForHookByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get tool for hook %s %w", name, err)
		}

		description := ""
		if len(tools) > 0 && tools[0].Function.Description != "" {
			description = tools[0].Function.Description
		}

		localHooks = append(localHooks, LocalHook{
			Name:        name,
			Description: description,
			Type:        "local",
			Tools:       tools,
		})
	}

	return localHooks, nil
}

func (s *service) Create(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	if err := validate(hook); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateRemoteHookCount(ctx)
	if err != nil {
		return err
	}
	err = storeInstance.EnforceMaxRowCount(ctx, count)
	if err != nil {
		return err
	}
	return storeInstance.CreateRemoteHook(ctx, hook)
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.RemoteHook, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetRemoteHook(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.RemoteHook, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetRemoteHookByName(ctx, name)
}

func (s *service) Update(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	if err := validate(hook); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).UpdateRemoteHook(ctx, hook)
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).DeleteRemoteHook(ctx, id)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteHook, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListRemoteHooks(ctx, createdAtCursor, limit)
}

func validate(hook *runtimetypes.RemoteHook) error {
	switch {
	case hook.Name == "":
		return fmt.Errorf("%w %w: name is required", ErrInvalidHook, apiframework.ErrUnprocessableEntity)
	case hook.EndpointURL == "":
		return fmt.Errorf("%w %w: endpoint URL is required", ErrInvalidHook, apiframework.ErrUnprocessableEntity)
	case hook.TimeoutMs <= 0:
		return fmt.Errorf("%w %w: timeout must be positive", ErrInvalidHook, apiframework.ErrUnprocessableEntity)
	}

	// Validate headers if provided
	for key, value := range hook.Headers {
		if key == "" {
			return fmt.Errorf("%w %w: header name cannot be empty", ErrInvalidHook, apiframework.ErrUnprocessableEntity)
		}
		if value == "" {
			return fmt.Errorf("%w %w: header value for %s cannot be empty", ErrInvalidHook, apiframework.ErrUnprocessableEntity, key)
		}
	}

	return nil
}
