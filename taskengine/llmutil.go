package taskengine

import (
	"strings"
)

// StripCodeFences removes leading and trailing Markdown code fences from LLM
// output. It handles ` ``` `, ` ```json `, ` ```javascript ` etc.
func StripCodeFences(s string) string { return stripCodeFences(s) }

// ExtractJSONArray scans s for the outermost [...] block and returns it.
// It first strips code fences, then skips any preamble text the LLM may have
// placed before the JSON array to be robust to inconsistent model output.
func ExtractJSONArray(s string) string {
	s = strings.TrimSpace(StripCodeFences(s))
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end < 0 || end <= start {
		return s // return as-is; caller will get a descriptive unmarshal error
	}
	return s[start : end+1]
}

// ExtractJSONObject scans s for the outermost {...} block and returns it.
// It strips code fences first, same spirit as ExtractJSONArray.
func ExtractJSONObject(s string) string {
	s = strings.TrimSpace(StripCodeFences(s))
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}
