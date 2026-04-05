package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/mcpworker"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// PersistentRepo implements taskengine.HookRepo using a single OpenAPI-based protocol.
type PersistentRepo struct {
	localHooks   map[string]taskengine.HookRepo
	dbInstance   libdb.DBManager
	httpClient   *http.Client
	toolProtocol ToolProtocol
	messenger    libbus.Messenger
}

func NewPersistentRepo(
	localHooks map[string]taskengine.HookRepo,
	dbInstance libdb.DBManager,
	httpClient *http.Client,
	messenger libbus.Messenger,
) taskengine.HookRepo {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &PersistentRepo{
		localHooks:   localHooks,
		dbInstance:   dbInstance,
		httpClient:   httpClient,
		toolProtocol: &OpenAPIToolProtocol{},
		messenger:    messenger,
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
	// 1. Check local built-in hooks first.
	if hook, ok := p.localHooks[args.Name]; ok {
		return hook.Exec(ctx, startingTime, input, debug, args)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())

	// 2. Check MCP servers from DB (transient connection per call).
	if mcpSrv, err := store.GetMCPServerByName(ctx, args.Name); err == nil {
		return p.execMCPHook(ctx, mcpSrv, args, input)
	}

	// 3. Fall back to HTTP remote hooks from DB.
	remoteHook, err := store.GetRemoteHookByName(ctx, args.Name)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("unknown hook: %s", args.Name)
	}

	return p.execRemoteHook(ctx, remoteHook, input, args)
}

// execMCPHook routes a tool call to the persistent session worker via NATS.
func (p *PersistentRepo) execMCPHook(
	ctx context.Context,
	srv *runtimetypes.MCPServer,
	args *taskengine.HookCall,
	input any,
) (any, taskengine.DataType, error) {
	// Determine tool name: strip "hookname." prefix that taskengine adds.
	toolName := args.ToolName
	if prefix := srv.Name + "."; strings.HasPrefix(toolName, prefix) {
		toolName = strings.TrimPrefix(toolName, prefix)
	}
	if toolName == "" {
		toolName = args.Args["tool"]
	}

	// Merge model args first, then inject system params (injected values always win).
	toolArgs := map[string]any{}
	if m, ok := input.(map[string]any); ok {
		for k, v := range m {
			toolArgs[k] = v
		}
	} else if input != nil {
		toolArgs["input"] = input
	}
	for k, v := range args.Args {
		toolArgs[k] = v
	}
	// Inject system-level params — these override any model-provided values.
	for k, v := range srv.InjectParams {
		toolArgs[k] = v
	}

	sessionID := ""
	if v := ctx.Value(runtimetypes.SessionIDContextKey); v != nil {
		if s, ok := v.(string); ok {
			sessionID = s
		}
	}

	reqPayload, err := json.Marshal(mcpworker.MCPToolRequest{
		SessionID: sessionID,
		Tool:      toolName,
		Args:      toolArgs,
	})
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp hook %q: encode request: %w", srv.Name, err)
	}

	replyData, err := p.messenger.Request(ctx, mcpworker.SubjectExecute(srv.Name), reqPayload)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp hook %q: nats request: %w", srv.Name, err)
	}

	result, err := mcpworker.DecodeToolReply(replyData)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp hook %q: %w", srv.Name, err)
	}
	// Always return JSON so the LLM sees structured data, not Go's map[key:value] format.
	if result != nil {
		if s, ok := result.(string); ok {
			return s, taskengine.DataTypeString, nil
		}
		if b, err := json.Marshal(result); err == nil {
			return string(b), taskengine.DataTypeString, nil
		}
	}
	return "", taskengine.DataTypeString, nil
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

// GetToolsForHookByName returns the list of tools exposed by the named hook.
func (p *PersistentRepo) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	// 1. Local hooks.
	if hook, ok := p.localHooks[name]; ok {
		return hook.GetToolsForHookByName(ctx, name)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())

	// 2. MCP servers — route list-tools through persistent NATS worker.
	if mcpSrv, err := store.GetMCPServerByName(ctx, name); err == nil {
		// Extract SessionID so the worker routes to the correct per-session pool.
		sessionID := ""
		if v := ctx.Value(runtimetypes.SessionIDContextKey); v != nil {
			if s, ok := v.(string); ok {
				sessionID = s
			}
		}
		reqPayload, _ := json.Marshal(mcpworker.MCPToolRequest{SessionID: sessionID})
		replyData, err := p.messenger.Request(ctx, mcpworker.SubjectListTools(mcpSrv.Name), reqPayload)
		if err != nil {
			return nil, taskengine.HookToolsUnavailable(name, fmt.Errorf("mcp list-tools request: %w", err))
		}
		mcpTools, err := mcpworker.DecodeListToolsReply(replyData)
		if err != nil {
			return nil, taskengine.HookToolsUnavailable(name, err)
		}
		tools := make([]taskengine.Tool, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, mcpToolToTaskTool(mcpSrv.Name, t, mcpSrv.InjectParams))
		}
		return tools, nil
	}

	// 3. HTTP remote hooks from DB.
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
		return nil, taskengine.HookToolsUnavailable(name, fmt.Errorf("remote hook fetch tools: %w", err))
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

// Supports returns a list of all hook names (local + MCP + remote).
func (p *PersistentRepo) Supports(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(p.localHooks))
	for name := range p.localHooks {
		names = append(names, name)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	var cursor *time.Time
	const limit = 100

	// MCP servers
	for {
		page, err := store.ListMCPServers(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list MCP servers: %w", err)
		}
		for _, s := range page {
			names = append(names, s.Name)
		}
		if len(page) < limit {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}

	// HTTP remote hooks
	cursor = nil
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
