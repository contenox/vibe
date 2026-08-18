package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/mcpworker"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

const (
	mcpWorkerWait = 2 * time.Second
	mcpWorkerPoll = 25 * time.Millisecond
)

// PersistentRepo implements taskengine.ToolsRepo using a single OpenAPI-based protocol.
type PersistentRepo struct {
	localTools   map[string]taskengine.ToolsRepo
	dbInstance   libdb.DBManager
	httpClient   *http.Client
	toolProtocol ToolProtocol
	messenger    libbus.Messenger
	tracker      libtracker.ActivityTracker
}

func NewPersistentRepo(
	localTools map[string]taskengine.ToolsRepo,
	dbInstance libdb.DBManager,
	httpClient *http.Client,
	messenger libbus.Messenger,
	tracker libtracker.ActivityTracker,
) taskengine.ToolsRepo {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	return &PersistentRepo{
		localTools:   localTools,
		dbInstance:   dbInstance,
		httpClient:   httpClient,
		toolProtocol: &OpenAPIToolProtocol{},
		messenger:    messenger,
		tracker:      tracker,
	}
}

func (p *PersistentRepo) insecureClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &http.Client{
		Timeout:   p.httpClient.Timeout,
		Transport: transport,
	}
}

func (p *PersistentRepo) protocolFor(specURL string) ToolProtocol {
	if specURL == "" {
		return p.toolProtocol
	}
	return &OpenAPIToolProtocol{SpecSource: specURL}
}

// Exec executes a tools by name.
func (p *PersistentRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	// Local built-in tools carry their own tracking, so pass through
	// untouched to avoid double-spanning.
	if tools, ok := p.localTools[args.Name]; ok {
		return tools.Exec(ctx, startingTime, input, debug, args)
	}

	// Remote (MCP / HTTP) dispatch is spanned here so an injected exporter sees it.
	reportErr, reportChange, end := p.tracker.Start(ctx, "exec", "remote_tools", "tools", args.Name, "tool", args.ToolName)
	defer end()

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())

	if runtimetypes.IsACPManagedMCPServerName(args.Name) && !acpMCPServerVisible(ctx, args.Name) {
		err := fmt.Errorf("unknown tools: %s", args.Name)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}
	if mcpSrv, err := store.GetMCPServerByName(ctx, args.Name); err == nil {
		out, dt, execErr := p.execMCPTools(ctx, mcpSrv, args, input)
		if execErr != nil {
			reportErr(execErr)
		} else {
			reportChange(args.Name, args.ToolName)
		}
		return out, dt, execErr
	}

	remoteTools, err := store.GetRemoteToolsByName(ctx, args.Name)
	if err != nil {
		err = fmt.Errorf("unknown tools: %s", args.Name)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	out, dt, execErr := p.execRemoteTools(ctx, remoteTools, input, args)
	if execErr != nil {
		reportErr(execErr)
	} else {
		reportChange(args.Name, args.ToolName)
	}
	return out, dt, execErr
}

var _ taskengine.Prechecker = (*PersistentRepo)(nil)

// Precheck forwards to the local tools registered under args.Name; a remote
// provider, or a local one without the seam, raises no static objection.
func (p *PersistentRepo) Precheck(ctx context.Context, input any, args *taskengine.ToolsCall) error {
	if args == nil {
		return nil
	}
	pre, ok := p.localTools[args.Name].(taskengine.Prechecker)
	if !ok {
		return nil
	}
	return pre.Precheck(ctx, input, args)
}

func (p *PersistentRepo) requestWorker(ctx context.Context, subject string, payload []byte) ([]byte, error) {
	deadline := time.Now().Add(mcpWorkerWait)
	for {
		reply, err := p.messenger.Request(ctx, subject, payload)
		if err == nil || !errors.Is(err, libbus.ErrNoResponders) {
			return reply, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		timer := time.NewTimer(mcpWorkerPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *PersistentRepo) execMCPTools(
	ctx context.Context,
	srv *runtimetypes.MCPServer,
	args *taskengine.ToolsCall,
	input any,
) (any, taskengine.DataType, error) {
	// Strip the "toolsname." prefix that taskengine adds.
	toolName := args.ToolName
	if prefix := srv.Name + "."; strings.HasPrefix(toolName, prefix) {
		toolName = strings.TrimPrefix(toolName, prefix)
	}
	if toolName == "" {
		toolName = args.Args["tool"]
	}

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
	// System-level params override any model-provided values.
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

	replyData, err := p.requestWorker(ctx, mcpworker.SubjectExecute(srv.Name), reqPayload)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp tools %q: bus request: %w", srv.Name, err)
	}

	result, err := mcpworker.DecodeToolReply(replyData)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mcp tools %q: %w", srv.Name, err)
	}
	// JSON, so the LLM sees structured data rather than Go's map[key:value] format.
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
	if tools.TimeoutMs <= 0 {
		return nil, taskengine.DataTypeAny, fmt.Errorf("timeout must be positive: %dms", tools.TimeoutMs)
	}

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
	// Strip the tools-name prefix taskengine adds to tool names (e.g.
	// "nws.obs_stations" → "obs_stations" when tools.Name == "nws").
	bareName := args.ToolName
	if prefix := tools.Name + "."; strings.HasPrefix(bareName, prefix) {
		bareName = strings.TrimPrefix(bareName, prefix)
	}

	toolCall := taskengine.ToolCall{
		Function: taskengine.FunctionCall{
			Name:      bareName,
			Arguments: "{}", // Will be replaced
		},
	}

	argumentsMap := map[string]any{}
	if m, ok := input.(map[string]any); ok {
		for k, v := range m {
			argumentsMap[k] = v
		}
	} else if input != nil {
		argumentsMap["input"] = input
	}
	for k, v := range args.Args {
		argumentsMap[k] = v
	}

	argsJSON, err := safeJSONString(argumentsMap)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to prepare tool arguments: %w", err)
	}
	toolCall.Function.Arguments = argsJSON

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(tools.TimeoutMs)*time.Millisecond)
	defer cancel()

	client := p.httpClient
	if tools.InsecureSkipVerify {
		client = p.insecureClient()
	}

	// Spec loaded from SpecURL when set.
	result, dataType, err := p.protocolFor(tools.SpecURL).ExecuteTool(
		timeoutCtx,
		tools.EndpointURL,
		client,
		injectParams,
		toolCall,
	)

	// Auto-login retry on an auth failure, when AuthFlow is configured.
	if err != nil && tools.AuthFlow != nil && IsAuthError(err) {
		newInjects, loginErr := PerformAuthFlow(timeoutCtx, tools, client)
		if loginErr != nil {
			return nil, dataType, fmt.Errorf("execution failed: auth retry failed: %w (original error: %v)", loginErr, err)
		}

		var needsPersist bool
		for k, v := range newInjects {
			injectParams[k] = v
			if v.In == ArgLocationHeader {
				if tools.Headers == nil {
					tools.Headers = make(map[string]string)
				}
				tools.Headers[v.Name] = v.Value
				needsPersist = true
			}
		}

		if needsPersist {
			store := runtimetypes.New(p.dbInstance.WithoutTransaction())
			_ = store.UpdateRemoteTools(ctx, tools)
		}

		result, dataType, err = p.protocolFor(tools.SpecURL).ExecuteTool(
			timeoutCtx,
			tools.EndpointURL,
			client,
			injectParams,
			toolCall,
		)
	}

	if err != nil {
		return nil, dataType, fmt.Errorf("execution failed for tools '%s': %w", tools.Name, err)
	}

	return result, dataType, nil
}

// GetToolsForToolsByName returns the list of tools exposed by the named tools.
func (p *PersistentRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if tools, ok := p.localTools[name]; ok {
		return tools.GetToolsForToolsByName(ctx, name)
	}
	if runtimetypes.IsACPManagedMCPServerName(name) && !acpMCPServerVisible(ctx, name) {
		return nil, fmt.Errorf("unknown tools %q: %w", name, taskengine.ErrToolsNotFound)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())

	// MCP servers route list-tools through the persistent NATS worker.
	if mcpSrv, err := store.GetMCPServerByName(ctx, name); err == nil {
		// SessionID routes the worker to the correct per-session pool.
		sessionID := ""
		if v := ctx.Value(runtimetypes.SessionIDContextKey); v != nil {
			if s, ok := v.(string); ok {
				sessionID = s
			}
		}
		reqPayload, _ := json.Marshal(mcpworker.MCPToolRequest{SessionID: sessionID})
		replyData, err := p.requestWorker(ctx, mcpworker.SubjectListTools(mcpSrv.Name), reqPayload)
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
	tools, err := p.protocolFor(remoteTools.SpecURL).FetchTools(ctx, remoteTools.EndpointURL, injectParams, p.httpClient)
	if err != nil {
		return nil, taskengine.ToolsToolsUnavailable(name, fmt.Errorf("remote tools fetch tools: %w", err))
	}

	return tools, nil
}

// GetSchemasForSupportedTools returns OpenAPI schemas for all remote tools.
func (p *PersistentRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	schemas := make(map[string]*openapi3.T)

	for name, repo := range p.localTools {
		repoSchemas, err := repo.GetSchemasForSupportedTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get schemas for local tools '%s': %w", name, err)
		}
		maps.Copy(schemas, repoSchemas)
	}

	store := runtimetypes.New(p.dbInstance.WithoutTransaction())
	var cursor *time.Time
	const limit = 100

	for {
		page, err := store.ListRemoteTools(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list remote tools: %w", err)
		}

		for _, tools := range page {
			schema, err := p.protocolFor(tools.SpecURL).FetchSchema(ctx, tools.EndpointURL, p.httpClient)
			if err != nil {
				continue // one failing tools doesn't break all
			}
			schemas[tools.Name] = schema
		}

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

	for {
		page, err := store.ListMCPServers(ctx, cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list MCP servers: %w", err)
		}
		for _, s := range page {
			if runtimetypes.IsACPManagedMCPServerName(s.Name) && !acpMCPServerVisible(ctx, s.Name) {
				continue
			}
			names = append(names, s.Name)
		}
		if len(page) < limit {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}

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

func acpMCPServerVisible(ctx context.Context, name string) bool {
	allowlist, ok := taskengine.RuntimeToolsAllowlistFromContext(ctx)
	if !ok {
		return false
	}
	hasStar := false
	hasExact := false
	for _, entry := range allowlist {
		switch {
		case entry == "*":
			hasStar = true
		case strings.HasPrefix(entry, "!"):
			if strings.TrimPrefix(entry, "!") == name {
				return false
			}
		case entry == name:
			hasExact = true
		}
	}
	return hasStar || hasExact
}

func (p *PersistentRepo) mapLocation(in string) ArgLocation {
	switch in {
	case runtimetypes.LocationPath:
		return ArgLocationPath
	case runtimetypes.LocationQuery:
		return ArgLocationQuery
	case runtimetypes.LocationBody:
		return ArgLocationBody
	default:
		return ArgLocationBody
	}
}

func safeJSONString(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("failed to serialize arguments to JSON: %w", err)
	}
	return string(b), nil
}
