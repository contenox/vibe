// internal/tools/multi_repo.go
package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

type MultiRepo struct {
	repos []taskengine.ToolsRepo
}

func NewMultiRepo(repos ...taskengine.ToolsRepo) *MultiRepo {
	return &MultiRepo{repos: repos}
}

func (m *MultiRepo) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	for _, r := range m.repos {
		supported, err := r.Supports(ctx)
		if err != nil {
			continue
		}
		for _, name := range supported {
			if name == args.Name {
				return r.Exec(ctx, startingTime, input, debug, args)
			}
		}
	}
	return nil, taskengine.DataTypeAny, fmt.Errorf("no tools repo found for tools %q", args.Name)
}

func (m *MultiRepo) Supports(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range m.repos {
		names, err := r.Supports(ctx)
		if err != nil {
			continue
		}
		for _, n := range names {
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func (m *MultiRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	out := map[string]*openapi3.T{}
	for _, r := range m.repos {
		schemas, err := r.GetSchemasForSupportedTools(ctx)
		if err != nil {
			continue
		}
		for k, v := range schemas {
			out[k] = v
		}
	}
	return out, nil
}

func (m *MultiRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	for _, r := range m.repos {
		tools, err := r.GetToolsForToolsByName(ctx, name)
		if err == nil && len(tools) > 0 {
			return tools, nil
		}
		if errors.Is(err, taskengine.ErrToolsToolsUnavailable) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %q", taskengine.ErrToolsNotFound, name)
}
