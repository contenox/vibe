package toolsproviderservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/errdefs"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
	"golang.org/x/sync/errgroup"
)

var (
	ErrInvalidTools = errors.New("invalid remote tools data")
)

var (
	localToolsListConcurrency = 8
	localToolsToolListTimeout = 5 * time.Second
)

// Service defines the interface for managing remote tools and querying tools capabilities.
type Service interface {
	Create(ctx context.Context, tools *runtimetypes.RemoteTools) error
	Get(ctx context.Context, id string) (*runtimetypes.RemoteTools, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.RemoteTools, error)
	Update(ctx context.Context, tools *runtimetypes.RemoteTools) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteTools, error)
	GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error)
	ListLocalTools(ctx context.Context) ([]LocalTools, error)
}

type LocalTools struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Tools       []taskengine.Tool `json:"tools,omitempty"`
	// Source is builtin (in-process), mcp (persisted MCP server), or remote (HTTP tools in DB).
	Source string `json:"source,omitempty"`
	// UnavailableReason is set when tools could not be loaded (e.g. unreachable MCP); other tools still list.
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

type service struct {
	dbInstance    libdb.DBManager
	toolsRegistry taskengine.ToolsProvider
	tracker       libtracker.ActivityTracker
}

// New creates a new service instance. tracker may be nil (no-op tracking).
func New(dbInstance libdb.DBManager, toolsRegistry taskengine.ToolsProvider, tracker libtracker.ActivityTracker) Service {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &service{
		dbInstance:    dbInstance,
		toolsRegistry: toolsRegistry,
		tracker:       tracker,
	}
}

// GetSchemasForSupportedTools delegates the call to the tools registry.
func (s *service) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	if s.toolsRegistry == nil {
		return nil, errors.New("tools registry is not configured for this service")
	}
	return s.toolsRegistry.GetSchemasForSupportedTools(ctx)
}

// ListLocalTools returns all locally registered tools
func (s *service) ListLocalTools(ctx context.Context) ([]LocalTools, error) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "list_tools", "local_tools")
	defer end()

	if s.toolsRegistry == nil {
		err := errors.New("tools registry is not configured for this service")
		reportErr(err)
		return nil, err
	}

	supported, err := s.toolsRegistry.Supports(ctx)
	if err != nil {
		reportErr(err)
		return nil, fmt.Errorf("failed to get supported tools: %w", err)
	}

	localTools := make([]LocalTools, len(supported))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(localToolsListConcurrency)

	for idx, name := range supported {
		idx, name := idx, name
		g.Go(func() error {
			src := s.toolsSource(gctx, name)
			toolsCtx, cancel := context.WithTimeout(gctx, localToolsToolListTimeout)
			defer cancel()

			tools, err := s.toolsRegistry.GetToolsForToolsByName(toolsCtx, name)
			if err != nil {
				reportChange(name, map[string]any{
					"detail": "tools_unavailable",
					"error":  err.Error(),
				})
				localTools[idx] = LocalTools{
					Name:              name,
					Type:              "local",
					Source:            src,
					UnavailableReason: shortenToolsListError(err),
				}
				return nil
			}

			description := ""
			if len(tools) > 0 && tools[0].Function.Description != "" {
				description = tools[0].Function.Description
			}

			localTools[idx] = LocalTools{
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

	return localTools, nil
}

func (s *service) toolsSource(ctx context.Context, name string) string {
	st := runtimetypes.New(s.dbInstance.WithoutTransaction())
	if _, err := st.GetMCPServerByName(ctx, name); err == nil {
		return "mcp"
	}
	if _, err := st.GetRemoteToolsByName(ctx, name); err == nil {
		return "remote"
	}
	return "builtin"
}

// shortenToolsListError produces a short UI-safe message from a tool-listing failure.
func shortenToolsListError(err error) string {
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

func (s *service) Create(ctx context.Context, tools *runtimetypes.RemoteTools) error {
	if err := validate(tools); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateRemoteToolsCount(ctx)
	if err != nil {
		return err
	}
	err = storeInstance.EnforceMaxRowCount(ctx, count)
	if err != nil {
		return err
	}
	return storeInstance.CreateRemoteTools(ctx, tools)
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.RemoteTools, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetRemoteTools(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.RemoteTools, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetRemoteToolsByName(ctx, name)
}

func (s *service) Update(ctx context.Context, tools *runtimetypes.RemoteTools) error {
	if err := validate(tools); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).UpdateRemoteTools(ctx, tools)
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).DeleteRemoteTools(ctx, id)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteTools, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListRemoteTools(ctx, createdAtCursor, limit)
}

func validate(tools *runtimetypes.RemoteTools) error {
	switch {
	case tools.Name == "":
		return fmt.Errorf("%w %w: name is required", ErrInvalidTools, errdefs.ErrUnprocessableEntity)
	case tools.EndpointURL == "":
		return fmt.Errorf("%w %w: endpoint URL is required", ErrInvalidTools, errdefs.ErrUnprocessableEntity)
	case tools.TimeoutMs <= 0:
		return fmt.Errorf("%w %w: timeout must be positive", ErrInvalidTools, errdefs.ErrUnprocessableEntity)
	case tools.SpecURL != "" && !isValidSpecSource(tools.SpecURL):
		return fmt.Errorf("%w %w: spec_url must be an http/https URL or file:///abs/path", ErrInvalidTools, errdefs.ErrUnprocessableEntity)
	}

	// Validate headers if provided
	for key, value := range tools.Headers {
		if key == "" {
			return fmt.Errorf("%w %w: header name cannot be empty", ErrInvalidTools, errdefs.ErrUnprocessableEntity)
		}
		if value == "" {
			return fmt.Errorf("%w %w: header value for %s cannot be empty", ErrInvalidTools, errdefs.ErrUnprocessableEntity, key)
		}
	}

	return nil
}

// isValidSpecSource reports whether s is an acceptable spec source:
// an http/https URL or a file:// URI (absolute path stored by the CLI).
func isValidSpecSource(s string) bool {
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "file://")
}

