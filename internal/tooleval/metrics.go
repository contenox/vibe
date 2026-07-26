package tooleval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// This file computes the navigation-scope metrics INDEPENDENTLY of the toolguidance
// package, from the harness's own trace. That independence is the point: toolguidance
// supplies the live signal to the model and the on/off switch; the harness is the
// external observer that measures whether the signal moved behaviour (toolguidance.go,
// "the harness measures the deltas; this package only supplies the signal and the
// switch"). The definitions below deliberately mirror toolguidance's three rules —
// repeat (identical tool+args), revisit (re-read of a path), scope (distinct paths) —
// so the A/B measures exactly the quantities that package's falsifiable claim names.

// computeMetrics derives the scope/repeat/re-read quantities from a run's trace.
// Malformed calls are excluded from repeat/scope/re-read accounting: a call whose
// arguments never parsed is not navigation the model completed (same guard
// toolguidance applies by short-circuiting on error results before counting).
func computeMetrics(trace []ToolCallRecord) Metrics {
	var m Metrics
	seenCall := map[string]struct{}{}
	seenPath := map[string]struct{}{}
	seenReadPath := map[string]struct{}{}
	files := map[string]struct{}{}
	dirs := map[string]struct{}{}

	for _, t := range trace {
		if !t.ArgsValid {
			continue
		}
		fp := callFingerprint(t)
		if _, ok := seenCall[fp]; ok {
			m.RepeatIdenticalCalls++
		} else {
			seenCall[fp] = struct{}{}
		}

		path := t.Path
		if path != "" {
			if isReadLikeLeaf(t.Tool) {
				if _, ok := seenReadPath[path]; ok {
					m.ReReads++
				} else {
					seenReadPath[path] = struct{}{}
				}
			}
			if isDirLeaf(t.Tool) {
				dirs[path] = struct{}{}
			} else {
				files[path] = struct{}{}
				dirs[dirOf(path)] = struct{}{}
			}
			seenPath[path] = struct{}{}
		}
	}
	m.DistinctPaths = len(seenPath)
	m.DistinctReadPaths = len(seenReadPath)
	return m
}

// callFingerprint is a stable hash of (namespaced tool, canonicalized args), so two
// calls count as "identical" iff their tool and argument set match regardless of key
// ordering — the same notion toolguidance.fingerprint uses.
func callFingerprint(t ToolCallRecord) string {
	name := t.Name
	if name == "" {
		name = t.Provider + "." + t.Tool
	}
	var canon string
	var m map[string]any
	if err := json.Unmarshal([]byte(orEmptyObject(t.RawArgs)), &m); err == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			v, _ := json.Marshal(m[k])
			b.WriteString(k)
			b.WriteByte('=')
			b.Write(v)
			b.WriteByte(0)
		}
		canon = b.String()
	} else {
		canon = t.RawArgs
	}
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(canon))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// pathArgKeys mirrors toolguidance.pathArgKeys: the declared path-carrying argument
// names across the local providers. Same documented limits apply — declared args
// only, first match wins, no filesystem stat (dir-vs-file is inferred from the leaf
// name, not the disk).
var pathArgKeys = []string{"path", "file", "file_path", "filepath", "filename", "dir", "dir_path", "directory", "target"}

// extractPath pulls a best-effort declared path from parsed tool arguments, cleaned
// so "./widget" and "widget" count as one path.
func extractPath(args map[string]any) string {
	for _, k := range pathArgKeys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return filepath.Clean(t)
				}
			}
		}
	}
	return ""
}

// isReadLikeLeaf mirrors toolguidance.isReadLike: a leaf tool name that READS a file,
// so re-read counting stays truthful.
func isReadLikeLeaf(leaf string) bool {
	l := strings.ToLower(leaf)
	if strings.Contains(l, "read") {
		return true
	}
	switch l {
	case "cat", "view", "open", "stat_file":
		return true
	}
	return false
}

// isDirLeaf mirrors toolguidance.isDirTool.
func isDirLeaf(leaf string) bool {
	l := strings.ToLower(leaf)
	return strings.Contains(l, "dir") || strings.Contains(l, "list") || l == "ls"
}

func dirOf(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// orEmptyObject treats an empty/blank argument string as the empty JSON object, so a
// legitimately no-argument tool call is not scored as malformed.
func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
