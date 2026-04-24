package tools

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
	"github.com/contenox/contenox/runtime/mcpworker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// PersistentRepo implements taskengine.ToolsRepo using a single OpenAPI-based protocol.
type PersistentRepo struct {
	localTools   map[string]taskengine.ToolsRepo
	dbInstance   libdb.DBManager
	httpClient   *http.Client
	toolProtocol ToolProtocol
	messenger    libbus.Messenger
}

func NewPersistentRepo(
	localTools map[string]taskengine.ToolsRepo,
	dbInstance libdb.DBManager,
	httpClient *http.Client,
	messenger libbus.Messenger,
) taskengine.ToolsRepo {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &PersistentRepo{
		localTools:   localTools,
		dbInstance:   dbInstance,
		httpClient:   httpClient,
		toolProtocol: &OpenAPIToolProtocol{},
		messenger:    messenger,
	}
}

// Exec executes a tools by name.
func (p *PersistentRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	// 1. Check local built-in tools first.
	if tools, ok := p.localTools[args.Name]; ok {
		return tools.Exec(ctx, startingTime, input, debug, args)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())

	// 2. Check MCP servers from DB (transient connection per call).
	if mcpSrv, err := store.GetMCPServerByName(ctx, args.Name); err == nil {
		return p.execMCPTools(ctx, mcpSrv, args, input)
	}

	// 3. Fall back to HTTP remote tools from DB.
	remoteTools, err := store.GetRemoteToolsByName(ctx, args.Name)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("unknown tools: %s", args.Name)
	}

	return p.execRemoteTools(ctx, remoteTools, input, args)
}

// execMCPTools routes a tool call to the persistent session worker via NATS.
func (p *PersistentRepo) execMCPTools(
	ctx context.Context,
	srv *runtimetypes.MCPServer,
	args *taskengine.ToolsCall,
	input any,
) (any, taskengine.DataType, error) {
	// Determine tool name: strip "toolsname." prefix that taskengine adds.
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
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp tools %q: encode request: %w", srv.Name, err)
	}

	replyData, err := p.messenger.Request(ctx, mcpworker.SubjectExecute(srv.Name), reqPayload)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp tools %q: nats request: %w", srv.Name, err)
	}

	result, err := mcpworker.DecodeToolReply(replyData)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp tools %q: %w", srv.Name, err)
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

func (p *PersistentRepo) execRemoteTools(
	ctx context.Context,
	tools *runtimetypes.RemoteTools,
	input any,
	args *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	// Validate tools
	if tools.TimeoutMs <= 0 {
		return nil, taskengine.DataTypeAny, fmt.Errorf("timeout must be positive: %dms", tools.TimeoutMs)
	}

	// Build injection map from tools.Properties
	injectParams := make(map[string]ParamArg)
	if tools.Properties.Name != "" {
		loc := p.mapLocation(tools.Properties.In)
		injectParams[tools.Properties.Name] = ParamArg{
			Name:  tools.Properties.Name,
			Value: fmt.Sprintf("%v", tools.Properties.Value),
			In:    loc,
		}
	}
	for k, v := range tools.Headers {
		injectParams[k] = ParamArg{
			Name:  k,
			Value: fmt.Sprintf("%v", v),
			In:    ArgLocationHeader,
		}
	}
	// Strip the tools-name prefix that taskengine adds to tool names
	// (e.g. "nws.obs_stations" → "obs_stations" when tools.Name == "nws").
	bareName := args.ToolName
	if prefix := tools.Name + "."; strings.HasPrefix(bareName, prefix) {
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
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(tools.TimeoutMs)*time.Millisecond)
	defer cancel()

	// Execute via OpenAPI protocol
	result, dataType, err := p.toolProtocol.ExecuteTool(
		timeoutCtx,
		tools.EndpointURL,
		p.httpClient,
		injectParams,
		toolCall,
	)
	if err != nil {
		return nil, dataType, fmt.Errorf("execution failed for tools '%s': %w", tools.Name, err)
	}

	return result, dataType, nil
}

// GetToolsForToolsByName returns the list of tools exposed by the named tools.
func (p *PersistentRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	// 1. Local tools.
	if tools, ok := p.localTools[name]; ok {
		return tools.GetToolsForToolsByName(ctx, name)
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
			return nil, taskengine.ToolsToolsUnavailable(name, fmt.Errorf("mcp list-tools request: %w", err))
		}
		mcpTools, err := mcpworker.DecodeListToolsReply(replyData)
		if err != nil {
			return nil, taskengine.ToolsToolsUnavailable(name, err)
		}
		tools := make([]taskengine.Tool, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, mcpToolToTaskTool(mcpSrv.Name, t, mcpSrv.InjectParams))
		}
		return tools, nil
	}

	// 3. HTTP remote tools from DB.
	remoteTools, err := store.GetRemoteToolsByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("unknown tools %q: %w", name, taskengine.ErrToolsNotFound)
	}

	injectParams := make(map[string]ParamArg)
	if remoteTools.Properties.Name != "" {
		loc := p.mapLocation(remoteTools.Properties.In)
		injectParams[remoteTools.Properties.Name] = ParamArg{
			Name:  remoteTools.Properties.Name,
			Value: fmt.Sprintf("%v", remoteTools.Properties.Value),
			In:    loc,
		}
	}
	for k, v := range remoteTools.Headers {
		injectParams[k] = ParamArg{
			Name:  k,
			Value: fmt.Sprintf("%v", v),
			In:    ArgLocationHeader,
		}
	}
	tools, err := p.toolProtocol.FetchTools(ctx, remoteTools.EndpointURL, injectParams, p.httpClient)
	if err != nil {
		return nil, taskengine.ToolsToolsUnavailable(name, fmt.Errorf("remote tools fetch tools: %w", err))
	}

	return tools, nil
}

// GetSchemasForSupportedTools returns OpenAPI schemas for all remote tools.
func (p *PersistentRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	schemas := make(map[string]*openapi3.T)

	// Local tools have no schema (for now)
	for name, repo := range p.localTools {
		repoSchemas, err := repo.GetSchemasForSupportedTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get schemas for local tools '%s': %w", name, err)
		}
		maps.Copy(schemas, repoSchemas)
	}

	// Fetch and process remote tools page by page
	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	var cursor *time.Time
	const limit = 100

	for {
		page, err := store.ListRemoteTools(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list remote tools: %w", err)
		}

		// Process this page immediately
		for _, tools := range page {
			schema, err := p.toolProtocol.FetchSchema(ctx, tools.EndpointURL, p.httpClient)
			if err != nil {
				// Optionally log here (e.g., via p.logger.Warn(...)) in real implementation
				continue // Graceful: one failing tools doesn't break all
			}
			schemas[tools.Name] = schema // Store the *openapi3.T directly
		}

		// Break if this is the last page
		if len(page) < limit {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}

	return schemas, nil
}

// Supports returns a list of all tools names (local + MCP + remote).
func (p *PersistentRepo) Supports(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(p.localTools))
	for name := range p.localTools {
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

	// HTTP remote tools
	cursor = nil
	for {
		page, err := store.ListRemoteTools(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list remote tools: %w", err)
		}
		for _, tools := range page {
			names = append(names, tools.Name)
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
