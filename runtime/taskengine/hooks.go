package taskengine

import (
	"context"
	"strings"
)

// resolveHookNames returns the effective set of hook names for a task based on its allowlist.
//
// Semantics:
//   - nil allowlist  → all names from provider.Supports() (field was absent; backward compat)
//   - []             → empty set (field explicitly set to empty; no hooks)
//   - ["*"]          → all names from provider.Supports()
//   - ["a","b"]      → intersection of the named entries with Supports()
//   - ["*","!name"]  → all from Supports() minus the excluded names
//
// Entries starting with "!" are exclusions and may only be combined with "*".
// Unknown exact names (not returned by Supports) are silently ignored.
//
// If a runtime allowlist is attached to ctx via WithRuntimeHookAllowlist, the
// task-resolved set is intersected with the runtime-resolved set (stricter
// wins). Exclusions on either side are honored. Absent runtime allowlist keeps
// the task allowlist unchanged (fully backward compatible).
func resolveHookNames(ctx context.Context, allowlist []string, provider HookProvider) ([]string, error) {
	all, err := provider.Supports(ctx)
	if err != nil {
		return nil, err
	}

	taskSet := applyAllowlist(allowlist, all)

	runtime, runtimeAttached := RuntimeHookAllowlistFromContext(ctx)
	if !runtimeAttached {
		return taskSet, nil
	}
	runtimeSet := applyAllowlist(runtime, all)

	// Intersect: a hook is available iff both sides permit it.
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

// applyAllowlist resolves a single allowlist against the full set of supported
// hook names per the grammar documented on resolveHookNames.
func applyAllowlist(allowlist []string, all []string) []string {
	if allowlist == nil {
		return all
	}
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
		if hasStar {
			result = append(result, name)
			continue
		}
		if _, ok := exact[name]; ok {
			result = append(result, name)
		}
	}
	return result
}

// ExportedResolveHookNames is a test-only export of resolveHookNames.
func ExportedResolveHookNames(ctx context.Context, allowlist []string, provider HookProvider) ([]string, error) {
	return resolveHookNames(ctx, allowlist, provider)
}

