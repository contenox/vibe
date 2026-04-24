package tools

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// SimpleRepo holds a map of locally registered tools.
type SimpleRepo struct {
	tools map[string]taskengine.ToolsRepo
}

func NewSimpleProvider(tools map[string]taskengine.ToolsRepo) taskengine.ToolsRepo {
	return &SimpleRepo{
		tools: tools,
	}
}

func (m *SimpleRepo) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	args *taskengine.ToolsCall,
) (any, taskengine.DataType, error) {
	if tools, ok := m.tools[args.Name]; ok {
		return tools.Exec(ctx, startingTime, input, debug, args)
	}
	return nil, taskengine.DataTypeAny, fmt.Errorf("unknown tools type: %s", args.Name)
}

// Supports returns a list of all tools names registered in the internal map.
func (m *SimpleRepo) Supports(ctx context.Context) ([]string, error) {
	supported := make([]string, 0, len(m.tools))
	for k := range m.tools {
		supported = append(supported, k)
	}
	return supported, nil
}

// GetSchemasForSupportedTools aggregates the schemas from all registered tools.
func (m *SimpleRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	allSchemas := make(map[string]*openapi3.T)

	// Iterate through each registered tools implementation.
	for toolsName, toolsImpl := range m.tools {
		// Get the schemas provided by this specific tools's implementation.
		implSchemas, err := toolsImpl.GetSchemasForSupportedTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting schema for tools '%s': %w", toolsName, err)
		}

		// Merge the returned schemas into our main map.
		maps.Copy(allSchemas, implSchemas)
	}
	return allSchemas, nil
}

func (m *SimpleRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if tools, ok := m.tools[name]; ok {
		return tools.GetToolsForToolsByName(ctx, name)
	}
	return nil, fmt.Errorf("unknown tools type %q: %w", name, taskengine.ErrToolsNotFound)
}

var _ taskengine.ToolsRepo = (*SimpleRepo)(nil)
