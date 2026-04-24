package hookproviderservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/errdefs"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
	"golang.org/x/sync/errgroup"
)

var (
	ErrInvalidHook = errors.New("invalid remote hook data")
)

var (
	localHookListConcurrency = 8
	localHookToolListTimeout = 5 * time.Second
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
	// Source is builtin (in-process), mcp (persisted MCP server), or remote (HTTP hook in DB).
	Source string `json:"source,omitempty"`
	// UnavailableReason is set when tools could not be loaded (e.g. unreachable MCP); other hooks still list.
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

type service struct {
	dbInstance   libdb.DBManager
	hookRegistry taskengine.HookProvider
	tracker      libtracker.ActivityTracker
}

// New creates a new service instance. tracker may be nil (no-op tracking).
func New(dbInstance libdb.DBManager, hookRegistry taskengine.HookProvider, tracker libtracker.ActivityTracker) Service {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &service{
		dbInstance:   dbInstance,
		hookRegistry: hookRegistry,
		tracker:      tracker,
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
	reportErr, reportChange, end := s.tracker.Start(ctx, "list_tools", "local_hooks")
	defer end()

	if s.hookRegistry == nil {
		err := errors.New("hook registry is not configured for this service")
		reportErr(err)
		return nil, err
	}

	supported, err := s.hookRegistry.Supports(ctx)
	if err != nil {
		reportErr(err)
		return nil, fmt.Errorf("failed to get supported hooks: %w", err)
	}

	localHooks := make([]LocalHook, len(supported))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(localHookListConcurrency)

	for idx, name := range supported {
		idx, name := idx, name
		g.Go(func() error {
			src := s.hookSource(gctx, name)
			hookCtx, cancel := context.WithTimeout(gctx, localHookToolListTimeout)
			defer cancel()

			tools, err := s.hookRegistry.GetToolsForHookByName(hookCtx, name)
			if err != nil {
				reportChange(name, map[string]any{
					"detail": "tools_unavailable",
					"error":  err.Error(),
				})
				localHooks[idx] = LocalHook{
					Name:              name,
					Type:              "local",
					Source:            src,
					UnavailableReason: shortenHookListError(err),
				}
				return nil
			}

			description := ""
			if len(tools) > 0 && tools[0].Function.Description != "" {
				description = tools[0].Function.Description
			}

			localHooks[idx] = LocalHook{
				Name:        name,
				Description: description,
				Type:        "local",
				Source:      src,
				Tools:       tools,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		reportErr(err)
		return nil, err
	}

	return localHooks, nil
}

func (s *service) hookSource(ctx context.Context, name string) string {
	st := runtimetypes.New(s.dbInstance.WithoutTransaction())
	if _, err := st.GetMCPServerByName(ctx, name); err == nil {
		return "mcp"
	}
	if _, err := st.GetRemoteHookByName(ctx, name); err == nil {
		return "remote"
	}
	return "builtin"
}

// shortenHookListError produces a short UI-safe message from a tool-listing failure.
func shortenHookListError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	const max = 200
	if len(msg) > max {
		return msg[:max-3] + "..."
	}
	return msg
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
		return fmt.Errorf("%w %w: name is required", ErrInvalidHook, errdefs.ErrUnprocessableEntity)
	case hook.EndpointURL == "":
		return fmt.Errorf("%w %w: endpoint URL is required", ErrInvalidHook, errdefs.ErrUnprocessableEntity)
	case hook.TimeoutMs <= 0:
		return fmt.Errorf("%w %w: timeout must be positive", ErrInvalidHook, errdefs.ErrUnprocessableEntity)
	}

	// Validate headers if provided
	for key, value := range hook.Headers {
		if key == "" {
			return fmt.Errorf("%w %w: header name cannot be empty", ErrInvalidHook, errdefs.ErrUnprocessableEntity)
		}
		if value == "" {
			return fmt.Errorf("%w %w: header value for %s cannot be empty", ErrInvalidHook, errdefs.ErrUnprocessableEntity, key)
		}
	}

	return nil
}
