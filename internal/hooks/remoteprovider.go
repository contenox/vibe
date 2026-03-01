package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// PersistentRepo implements taskengine.HookRepo using a single OpenAPI-based protocol.
type PersistentRepo struct {
	localHooks   map[string]taskengine.HookRepo
	dbInstance   libdb.DBManager
	httpClient   *http.Client
	toolProtocol ToolProtocol
}

func NewPersistentRepo(
	localHooks map[string]taskengine.HookRepo,
	dbInstance libdb.DBManager,
	httpClient *http.Client,
) taskengine.HookRepo {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &PersistentRepo{
		localHooks:   localHooks,
		dbInstance:   dbInstance,
		httpClient:   httpClient,
		toolProtocol: &OpenAPIToolProtocol{},
	}
}

// Exec executes a hook by name.
func (p *PersistentRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	// Check local hooks first
	if hook, ok := p.localHooks[args.Name]; ok {
		return hook.Exec(ctx, startingTime, input, debug, args)
	}

	// Fetch remote hook
	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	remoteHook, err := store.GetRemoteHookByName(ctx, args.Name)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("unknown hook: %s", args.Name)
	}

	return p.execRemoteHook(ctx, remoteHook, input, args)
}

func (p *PersistentRepo) execRemoteHook(
	ctx context.Context,
	hook *runtimetypes.RemoteHook,
	input any,
	args *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	// Validate hook
	if hook.TimeoutMs <= 0 {
		return nil, taskengine.DataTypeAny, fmt.Errorf("timeout must be positive: %dms", hook.TimeoutMs)
	}

	// Build injection map from hook.Properties
	injectParams := make(map[string]ParamArg)
	if hook.Properties.Name != "" {
		loc := p.mapLocation(hook.Properties.In)
		injectParams[hook.Properties.Name] = ParamArg{
			Name:  hook.Properties.Name,
			Value: fmt.Sprintf("%v", hook.Properties.Value),
			In:    loc,
		}
	}
	for k, v := range hook.Headers {
		injectParams[k] = ParamArg{
			Name:  k,
			Value: fmt.Sprintf("%v", v),
			In:    ArgLocationHeader,
		}
	}
	// Strip the hook-name prefix that taskengine adds to tool names
	// (e.g. "nws.obs_stations" → "obs_stations" when hook.Name == "nws").
	bareName := args.ToolName
	if prefix := hook.Name + "."; strings.HasPrefix(bareName, prefix) {
		bareName = strings.TrimPrefix(bareName, prefix)
	}

	// Construct ToolCall
	toolCall := taskengine.ToolCall{
		Function: taskengine.FunctionCall{
			Name:      bareName,
			Arguments: "{}", // Will be replaced
		},
	}

	// Merge input into arguments
	argumentsMap := map[string]any{"input": input}
	for k, v := range args.Args {
		argumentsMap[k] = v
	}

	// Serialize arguments safely
	argsJSON, err := safeJSONString(argumentsMap)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to prepare tool arguments: %w", err)
	}
	toolCall.Function.Arguments = argsJSON

	// Set timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(hook.TimeoutMs)*time.Millisecond)
	defer cancel()

	// Execute via OpenAPI protocol
	result, dataType, err := p.toolProtocol.ExecuteTool(
		timeoutCtx,
		hook.EndpointURL,
		p.httpClient,
		injectParams,
		toolCall,
	)
	if err != nil {
		return nil, dataType, fmt.Errorf("execution failed for hook '%s': %w", hook.Name, err)
	}

	return result, dataType, nil
}

// GetToolsForHookByName returns the list of tools exposed by the remote hook.
func (p *PersistentRepo) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	// Check local hooks first
	if hook, ok := p.localHooks[name]; ok {
		return hook.GetToolsForHookByName(ctx, name)
	}

	// Fetch remote hook
	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	remoteHook, err := store.GetRemoteHookByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("unknown hook %q: %w", name, taskengine.ErrHookNotFound)
	}

	injectParams := make(map[string]ParamArg)
	if remoteHook.Properties.Name != "" {
		loc := p.mapLocation(remoteHook.Properties.In)
		injectParams[remoteHook.Properties.Name] = ParamArg{
			Name:  remoteHook.Properties.Name,
			Value: fmt.Sprintf("%v", remoteHook.Properties.Value),
			In:    loc,
		}
	}
	for k, v := range remoteHook.Headers {
		injectParams[k] = ParamArg{
			Name:  k,
			Value: fmt.Sprintf("%v", v),
			In:    ArgLocationHeader,
		}
	}
	tools, err := p.toolProtocol.FetchTools(ctx, remoteHook.EndpointURL, injectParams, p.httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tools for hook '%s': %w", name, err)
	}

	return tools, nil
}

// GetSchemasForSupportedHooks returns OpenAPI schemas for all remote hooks.
func (p *PersistentRepo) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	schemas := make(map[string]*openapi3.T)

	// Local hooks have no schema (for now)
	for name, repo := range p.localHooks {
		repoSchemas, err := repo.GetSchemasForSupportedHooks(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get schemas for local hook '%s': %w", name, err)
		}
		maps.Copy(schemas, repoSchemas)
	}

	// Fetch and process remote hooks page by page
	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	var cursor *time.Time
	const limit = 100

	for {
		page, err := store.ListRemoteHooks(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list remote hooks: %w", err)
		}

		// Process this page immediately
		for _, hook := range page {
			schema, err := p.toolProtocol.FetchSchema(ctx, hook.EndpointURL, p.httpClient)
			if err != nil {
				// Optionally log here (e.g., via p.logger.Warn(...)) in real implementation
				continue // Graceful: one failing hook doesn't break all
			}
			schemas[hook.Name] = schema // Store the *openapi3.T directly
		}

		// Break if this is the last page
		if len(page) < limit {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}

	return schemas, nil
}

// Supports returns a list of all hook names (local + remote).
func (p *PersistentRepo) Supports(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(p.localHooks))

	// Add local hooks
	for name := range p.localHooks {
		names = append(names, name)
	}

	// Add remote hooks page by page
	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	var cursor *time.Time
	const limit = 100

	for {
		page, err := store.ListRemoteHooks(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list remote hooks: %w", err)
		}

		for _, hook := range page {
			names = append(names, hook.Name)
		}

		if len(page) < limit {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}

	return names, nil
}

// --- Helper Functions ---

func (p *PersistentRepo) mapLocation(in string) ArgLocation {
	switch in {
	case runtimetypes.LocationPath:
		return ArgLocationPath
	case runtimetypes.LocationQuery:
		return ArgLocationQuery
	case runtimetypes.LocationBody:
		return ArgLocationBody
	default:
		return ArgLocationBody // default fallback
	}
}

func safeJSONString(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("failed to serialize arguments to JSON: %w", err)
	}
	return string(b), nil
}
