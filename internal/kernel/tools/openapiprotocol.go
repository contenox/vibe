package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPIToolProtocol implements ToolProtocol using OpenAPI v3 specs.
// SpecSource, when non-empty, is used as the spec location instead of
// endpointURL+"/openapi.json". Supported formats:
//   - https://... or http://...  — fetched over HTTP
//   - file:///abs/path           — read from local filesystem
type OpenAPIToolProtocol struct {
	SpecSource string // optional; set by remoteprovider when RemoteTools.SpecURL is non-empty
}

func (p *OpenAPIToolProtocol) FetchSchema(ctx context.Context, endpointURL string, httpClient *http.Client) (*openapi3.T, error) {
	specURL := endpointURL + "/openapi.json"
	if p.SpecSource != "" {
		specURL = p.SpecSource
	}

	u, err := url.Parse(specURL)
	if err != nil {
		return nil, fmt.Errorf("invalid spec URL: %w", err)
	}

	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = true

	loader.ReadFromURIFunc = func(loader *openapi3.Loader, url *url.URL) ([]byte, error) {
		if url.Scheme == "file" {
			return os.ReadFile(url.Path)
		}
		if httpClient == nil {
			return nil, fmt.Errorf("no http client configured for URL %s", url.String())
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to fetch OpenAPI spec: %s (status %d)", url.String(), resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	schema, err := loader.LoadFromURI(u)
	if err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI schema: %w", err)
	}

	return schema, nil
}

type ArgLocation int

const (
	ArgLocationQuery ArgLocation = iota
	ArgLocationHeader
	ArgLocationPath
	ArgLocationBody
)

type ParamArg struct {
	Name  string
	Value string
	In    ArgLocation
}

// operationDetails holds the schema information needed to execute a tool.
type operationDetails struct {
	Path      string
	Method    string
	Operation *openapi3.Operation
	PathItem  *openapi3.PathItem
}

// findOperationDetails fetches the OpenAPI schema and finds the specific
// operation that corresponds to the given tool name.
func (p *OpenAPIToolProtocol) findOperationDetails(
	ctx context.Context,
	endpointURL string,
	httpClient *http.Client,
	toolName string,
) (*operationDetails, error) {
	schema, err := p.FetchSchema(ctx, endpointURL, httpClient)
	if err != nil {
		return nil, fmt.Errorf("could not fetch schema for execution: %w", err)
	}

	for path, pathItem := range schema.Paths.Map() {
		for method, operation := range pathItem.Operations() {
			name := p.extractToolName(path, method, operation)
			if name == toolName {
				return &operationDetails{
					Path:      path,
					Method:    method,
					Operation: operation,
					PathItem:  pathItem,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("tool '%s' not found in the OpenAPI schema", toolName)
}

// ExecuteTool performs a tool call by making a corresponding HTTP request.
func (p *OpenAPIToolProtocol) ExecuteTool(
	ctx context.Context,
	endpointURL string,
	httpClient *http.Client,
	injectParams map[string]ParamArg,
	toolCall taskengine.ToolCall,
) (interface{}, taskengine.DataType, error) {
	details, err := p.findOperationDetails(ctx, endpointURL, httpClient, toolCall.Function.Name)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	finalArgs := make(map[string]interface{})
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &finalArgs); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to parse tool arguments: %w", err)
	}
	for _, p := range injectParams {
		finalArgs[p.Name] = p.Value
	}

	finalURL := endpointURL + details.Path
	queryParams := url.Values{}
	headers := http.Header{}

	allParams := append(openapi3.Parameters{}, details.PathItem.Parameters...)
	allParams = append(allParams, details.Operation.Parameters...)
	for _, paramRef := range allParams {
		param := paramRef.Value
		if argVal, ok := finalArgs[param.Name]; ok {
			valStr := fmt.Sprintf("%v", argVal)
			switch param.In {
			case "path":
				finalURL = strings.Replace(finalURL, "{"+param.Name+"}", valStr, 1)
			case "query":
				queryParams.Add(param.Name, valStr)
			}
			delete(finalArgs, param.Name)
		}
	}

	// injectParams apply regardless of what the spec declares.
	for _, injectedParam := range injectParams {
		valStr := fmt.Sprintf("%v", finalArgs[injectedParam.Name])
		switch injectedParam.In {
		case ArgLocationPath:
			finalURL = strings.Replace(finalURL, "{"+injectedParam.Name+"}", valStr, 1)
			delete(finalArgs, injectedParam.Name)
		case ArgLocationQuery:
			queryParams.Set(injectedParam.Name, valStr)
			delete(finalArgs, injectedParam.Name)
		case ArgLocationHeader:
			headers.Set(injectedParam.Name, valStr)
			delete(finalArgs, injectedParam.Name)
		}
	}

	if len(queryParams) > 0 {
		finalURL += "?" + queryParams.Encode()
	}

	var reqBody io.Reader
	if details.Operation.RequestBody != nil && slices.Contains([]string{"POST", "PUT", "PATCH"}, strings.ToUpper(details.Method)) {
		if len(finalArgs) > 0 {
			bodyBytes, err := json.Marshal(finalArgs)
			if err != nil {
				return nil, taskengine.DataTypeAny, fmt.Errorf("failed to marshal request body: %w", err)
			}
			reqBody = bytes.NewBuffer(bodyBytes)
		}
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(details.Method), finalURL, reqBody)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header = headers
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to execute tool request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, taskengine.DataTypeAny, &AuthError{StatusCode: resp.StatusCode, Body: string(responseBody)}
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(responseBody))
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(responseBody) == 0 {
		return nil, taskengine.DataTypeNil, nil
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var result interface{}
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("failed to parse JSON response: %w", err)
		}
		return result, taskengine.DataTypeJSON, nil
	}
	return string(responseBody), taskengine.DataTypeString, nil
}

func (p *OpenAPIToolProtocol) FetchTools(ctx context.Context, endpointURL string, injectParams map[string]ParamArg, httpClient *http.Client) ([]taskengine.Tool, error) {
	schema, err := p.FetchSchema(ctx, endpointURL, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch schema for tools: %w", err)
	}
	if schema == nil {
		return nil, nil
	}

	var tools []taskengine.Tool

	for path, pathItem := range schema.Paths.Map() {
		if pathItem == nil {
			continue
		}

		// Health/readiness/metrics endpoints are never exposed as tools.
		lowerPath := strings.ToLower(strings.TrimSpace(path))
		if lowerPath == "/health" || lowerPath == "/healthz" {
			continue
		}
		if lowerPath == "/ready" || lowerPath == "/readyz" {
			continue
		}
		if lowerPath == "/metrics" {
			continue
		}

		operations := pathItem.Operations()
		for method, operation := range operations {
			switch strings.ToUpper(method) {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue
			}

			name := p.extractToolName(path, method, operation)
			if name == "" {
				continue
			}

			description := operation.Description
			if description == "" {
				description = operation.Summary
			} else {
				description += "/n" + operation.Summary
			}

			parameters, err := p.buildParametersSchema(pathItem, operation, method, injectParams)
			if err != nil {
				continue
			}

			tool := taskengine.Tool{
				Type: "function",
				Function: taskengine.FunctionTool{
					Name:        name,
					Description: description,
					Parameters:  parameters,
				},
			}
			tools = append(tools, tool)
		}
	}

	return tools, nil
}

// extractToolName extracts the tool name using operationId, x-tool-name, or fallback.
func (p *OpenAPIToolProtocol) extractToolName(path, method string, operation *openapi3.Operation) string {
	if operation.OperationID != "" {
		return operation.OperationID
	}

	if toolName, ok := operation.Extensions["x-tool-name"].(string); ok && toolName != "" {
		if !isValidToolName(toolName) {
			return ""
		}
		return toolName
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	baseName := parts[len(parts)-1]
	if baseName == "" && len(parts) > 1 {
		baseName = parts[len(parts)-2]
	}
	return fmt.Sprintf("%s_%s", baseName, strings.ToLower(method))
}

func (p *OpenAPIToolProtocol) buildParametersSchema(
	pathItem *openapi3.PathItem,
	operation *openapi3.Operation,
	method string,
	injectParams map[string]ParamArg,
) (map[string]interface{}, error) {
	properties := make(map[string]interface{})
	required := make([]string, 0)

	mapOA3LocationToArgLocation := func(in string) ArgLocation {
		switch in {
		case "query":
			return ArgLocationQuery
		case "path":
			return ArgLocationPath
		case "header":
			return ArgLocationHeader
		default:
			return -1
		}
	}

	allParams := append(openapi3.Parameters{}, pathItem.Parameters...)
	allParams = append(allParams, operation.Parameters...)

	for _, paramRef := range allParams {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		param := paramRef.Value

		// Hide a parameter the system will inject.
		if injectedParam, ok := injectParams[param.Name]; ok {
			paramLocation := mapOA3LocationToArgLocation(param.In)
			if injectedParam.In == paramLocation {
				continue
			}
		}

		// Only 'path' and 'query' parameters are exposed to the LLM.
		if param.In != "path" && param.In != "query" {
			continue
		}

		if param.Schema != nil && param.Schema.Value != nil {
			schemaJSON, err := param.Schema.Value.MarshalJSON()
			if err != nil {
				continue
			}
			var propSchema map[string]interface{}
			if err := json.Unmarshal(schemaJSON, &propSchema); err == nil {
				properties[param.Name] = propSchema
				if param.Required {
					required = append(required, param.Name)
				}
			}
		}
	}

	// A single-object request body has its properties lifted to the top level.
	if operation.RequestBody != nil && slices.Contains([]string{"POST", "PUT", "PATCH"}, strings.ToUpper(method)) {
		if content := operation.RequestBody.Value.Content; content != nil {
			if jsonContent, ok := content["application/json"]; ok && jsonContent.Schema != nil {
				schema := jsonContent.Schema.Value

				if schema.Type.Is("object") {
					for propName, propSchemaRef := range schema.Properties {
						if injectedParam, ok := injectParams[propName]; ok && injectedParam.In == ArgLocationBody {
							continue
						}

						if propSchemaRef != nil && propSchemaRef.Value != nil {
							schemaJSON, err := propSchemaRef.Value.MarshalJSON()
							if err != nil {
								continue
							}
							var propSchema map[string]interface{}
							if err := json.Unmarshal(schemaJSON, &propSchema); err == nil {
								properties[propName] = propSchema
							}
						}
					}
					required = append(required, schema.Required...)
				}
			}
		}
	}

	if len(properties) == 0 {
		return nil, nil
	}

	finalSchema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}

	if len(required) > 0 {
		slices.Sort(required)
		finalSchema["required"] = slices.Compact(required)
	}

	return finalSchema, nil
}

func isValidToolName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
