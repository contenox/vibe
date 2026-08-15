package taskengine

import (
	"context"
	"strings"
)

func resolveToolsNames(ctx context.Context, allowlist []string, provider ToolsProvider) ([]string, error) {
	all, err := provider.Supports(ctx)
	if err != nil {
		return nil, err
	}

	taskSet := applyAllowlist(allowlist, all)

	runtime, runtimeAttached := RuntimeToolsAllowlistFromContext(ctx)
	if !runtimeAttached {
		return taskSet, nil
	}
	// nil runtime allowlist means no restriction; an empty slice denies all — see WithRuntimeToolsAllowlist.
	if runtime == nil {
		return taskSet, nil
	}
	runtimeSet := applyAllowlist(runtime, all)

	permitted := make(map[string]struct{}, len(runtimeSet))
	for _, n := range runtimeSet {
		permitted[n] = struct{}{}
	}
	result := make([]string, 0, len(taskSet))
	for _, n := range taskSet {
		if _, ok := permitted[n]; ok {
			result = append(result, n)
		}
	}
	return result, nil
}

func applyAllowlist(allowlist []string, all []string) []string {
	if len(allowlist) == 0 {
		return []string{}
	}

	hasStar := false
	exact := make(map[string]struct{})
	excluded := make(map[string]struct{})
	for _, entry := range allowlist {
		if entry == "*" {
			hasStar = true
		} else if strings.HasPrefix(entry, "!") {
			excluded[strings.TrimPrefix(entry, "!")] = struct{}{}
		} else {
			exact[entry] = struct{}{}
		}
	}

	result := make([]string, 0, len(all))
	for _, name := range all {
		if _, skip := excluded[name]; skip {
			continue
		}
		if hasStar && !isDeclarationScopedToolset(name) {
			result = append(result, name)
			continue
		}
		if _, ok := exact[name]; ok {
			result = append(result, name)
		}
	}
	return result
}

// declarationScopedPrefix mirrors runtimetypes.DeclaredToolNamePrefix, which
// the kernel may not import: it is store-free by design. Kept a bare literal
// rather than a dependency, and pinned by a test in that package.
const declarationScopedPrefix = "decl-"

// isDeclarationScopedToolset reports a toolset one agent declaration brought
// with it. A wildcard means "every tool this machine offers"; a source private
// to another agent is not that, so "*" never reaches one. The agent that
// declared it names it exactly, which its emitted chain does.
func isDeclarationScopedToolset(name string) bool {
	return strings.HasPrefix(name, declarationScopedPrefix)
}

// ExportedApplyAllowlist is a test-only export of applyAllowlist.
func ExportedApplyAllowlist(allowlist []string, all []string) []string {
	return applyAllowlist(allowlist, all)
}

// ExportedResolveToolsNames is a test-only export of resolveToolsNames.
func ExportedResolveToolsNames(ctx context.Context, allowlist []string, provider ToolsProvider) ([]string, error) {
	return resolveToolsNames(ctx, allowlist, provider)
}
