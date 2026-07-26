package localtools

import (
	"fmt"
	"sort"
	"strings"
)

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}

	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)
	sort.Strings(allowed)
	return fmt.Errorf("%s: unknown argument(s): %s (allowed: %s)",
		toolName, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

func stringSliceArg(toolName, key string, raw any) ([]string, error) {
	switch v := raw.(type) {
	case string:
		return splitShellArgs(v), nil
	case []string:
		return v, nil
	case []any:
		out := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s: %s[%d] must be a string, got %T", toolName, key, i, item)
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: %s must be a string or array of strings, got %T", toolName, key, raw)
	}
}
