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

// stringSliceArg reads a repeated argument as a []string, accepting a string,
// a []string or a []any of strings. The string case is SHELL-SPLIT — quotes
// and backslash escapes are interpreted — which is right for a command line
// and wrong for filenames; a paths argument takes pathListArg instead.
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

// pathListArg reads a path-list argument as a []string. It is stringSliceArg
// with one deliberate difference: a lone string is ONE path, never shell-split,
// so a filename may contain spaces, quotes or backslashes and still name the
// file it spells. A blank string yields no paths, leaving "which paths?" to the
// caller's own required-argument error.
func pathListArg(toolName, key string, raw any) ([]string, error) {
	if s, ok := raw.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	return stringSliceArg(toolName, key, raw)
}
