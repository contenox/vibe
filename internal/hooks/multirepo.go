// internal/hooks/multi_repo.go
package hooks

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

type MultiRepo struct {
	repos []taskengine.HookRepo
}

func NewMultiRepo(repos ...taskengine.HookRepo) *MultiRepo {
	return &MultiRepo{repos: repos}
}

func (m *MultiRepo) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *taskengine.HookCall) (any, taskengine.DataType, error) {
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
	return nil, taskengine.DataTypeAny, fmt.Errorf("no hook repo found for hook %q", args.Name)
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

func (m *MultiRepo) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	out := map[string]*openapi3.T{}
	for _, r := range m.repos {
		schemas, err := r.GetSchemasForSupportedHooks(ctx)
		if err != nil {
			continue
		}
		for k, v := range schemas {
			out[k] = v
		}
	}
	return out, nil
}

func (m *MultiRepo) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	for _, r := range m.repos {
		tools, err := r.GetToolsForHookByName(ctx, name)
		if err == nil && len(tools) > 0 {
			return tools, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", taskengine.ErrHookNotFound, name)
}
