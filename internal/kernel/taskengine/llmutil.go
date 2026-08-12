package taskengine

import (
	"strings"
)

func stripCodeFences(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	trimmed = trimmed[3:]
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[idx+1:]
	} else {
		return strings.TrimSpace(trimmed)
	}
	if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimSpace(trimmed)
}

func StripCodeFences(s string) string { return stripCodeFences(s) }

// ExtractJSONArray strips code fences then scans s for the outermost [...] block and returns it.
func ExtractJSONArray(s string) string {
	s = strings.TrimSpace(StripCodeFences(s))
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

// ExtractJSONObject strips code fences then scans s for the outermost {...} block and returns it.
func ExtractJSONObject(s string) string {
	s = strings.TrimSpace(StripCodeFences(s))
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}
