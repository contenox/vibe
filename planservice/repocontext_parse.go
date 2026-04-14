package planservice

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
)

// Defensive limits for explorer output. Same shape and reasoning as
// [validatePlannerStepStrings] in planner_validate.go: reject huge JSON, RSC
// stream leaks, and clearly malformed responses before persisting.
const (
	maxRepoContextBytes      = 64 * 1024 // 64 KiB serialized
	maxRepoContextStringLen  = 4 * 1024  // per-field defense
	maxRepoContextSliceItems = 200       // per array
)

// parseRepoContextRaw extracts a [planstore.RepoContext] from explorer model
// output. It accepts:
//   - a JSON object matching the [planstore.RepoContext] shape (preferred);
//   - the same wrapped in a single ```json code fence.
//
// The returned context has SchemaVersion forced to [planstore.RepoContextSchemaVersion]
// and GeneratedAt stamped to time.Now() if the model omitted it.
func parseRepoContextRaw(raw string) (*planstore.RepoContext, error) {
	trim := strings.TrimSpace(raw)
	if trim == "" {
		return nil, fmt.Errorf("empty explorer output")
	}
	if len(trim) > maxRepoContextBytes {
		return nil, fmt.Errorf("explorer output exceeds max size (%d bytes, max %d)", len(trim), maxRepoContextBytes)
	}
	if looksCorruptedRepoContext(trim) {
		return nil, fmt.Errorf("explorer output looks corrupted (stream or log paste); rerun explore")
	}

	objStr := taskengine.ExtractJSONObject(trim)
	if strings.TrimSpace(objStr) == "" {
		return nil, fmt.Errorf("explorer output did not contain a JSON object: %.500s", raw)
	}
	var rc planstore.RepoContext
	if err := json.Unmarshal([]byte(objStr), &rc); err != nil {
		return nil, fmt.Errorf("explorer output is not valid RepoContext JSON: %w (raw: %.500s)", err, raw)
	}
	if err := validateRepoContext(&rc); err != nil {
		return nil, err
	}
	rc.SchemaVersion = planstore.RepoContextSchemaVersion
	if rc.GeneratedAt.IsZero() {
		rc.GeneratedAt = time.Now().UTC()
	}
	return &rc, nil
}

// validateRepoContext applies bounds to slices and string lengths so an
// adversarial or hallucinating model cannot bloat the seed prompt.
func validateRepoContext(rc *planstore.RepoContext) error {
	rc.RepoRoot = trimWithCap(rc.RepoRoot, maxRepoContextStringLen)
	rc.Languages = capStringSlice(rc.Languages, maxRepoContextSliceItems, maxRepoContextStringLen)
	rc.EntryPoints = capStringSlice(rc.EntryPoints, maxRepoContextSliceItems, maxRepoContextStringLen)
	rc.BuildCommands = capStringSlice(rc.BuildCommands, maxRepoContextSliceItems, maxRepoContextStringLen)
	rc.TestCommands = capStringSlice(rc.TestCommands, maxRepoContextSliceItems, maxRepoContextStringLen)
	rc.Conventions = capStringSlice(rc.Conventions, maxRepoContextSliceItems, maxRepoContextStringLen)
	rc.Caveats = capStringSlice(rc.Caveats, maxRepoContextSliceItems, maxRepoContextStringLen)
	if len(rc.KeyFiles) > maxRepoContextSliceItems {
		rc.KeyFiles = rc.KeyFiles[:maxRepoContextSliceItems]
	}
	for i := range rc.KeyFiles {
		rc.KeyFiles[i].Path = trimWithCap(rc.KeyFiles[i].Path, maxRepoContextStringLen)
		rc.KeyFiles[i].Role = trimWithCap(rc.KeyFiles[i].Role, maxRepoContextStringLen)
	}
	return nil
}

func trimWithCap(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

func capStringSlice(in []string, maxItems, maxItemLen int) []string {
	if len(in) > maxItems {
		in = in[:maxItems]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = trimWithCap(s, maxItemLen)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// looksCorruptedRepoContext detects framework build streams pasted into the
// explorer output. Same detector as [plannerStepLooksCorrupted] but applied to
// the whole document.
func looksCorruptedRepoContext(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "__next_f") || strings.Contains(lower, "self.__next_f") {
		return true
	}
	if len(s) >= 800 {
		n := strings.Count(s, "\\")
		if n*3 > len(s) {
			return true
		}
	}
	return false
}

// renderRepoContextBlock turns a persisted RepoContext JSON into a compact
// bullet block suitable for inlining into a seed prompt via {{var:repo_context}}.
// Returns the empty string when raw is empty or unparseable so the seed prompt
// degrades gracefully (current behavior preserved).
func renderRepoContextBlock(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var rc planstore.RepoContext
	if err := json.Unmarshal([]byte(raw), &rc); err != nil {
		return ""
	}
	var b strings.Builder
	if rc.RepoRoot != "" {
		fmt.Fprintf(&b, "- repo_root: %s\n", rc.RepoRoot)
	}
	writeBulletLine := func(label string, items []string) {
		items = capStringSlice(items, 50, 240)
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "- %s: %s\n", label, strings.Join(items, ", "))
	}
	writeBulletLine("languages", rc.Languages)
	writeBulletLine("entry_points", rc.EntryPoints)
	writeBulletLine("build", rc.BuildCommands)
	writeBulletLine("test", rc.TestCommands)
	if len(rc.Conventions) > 0 {
		b.WriteString("- conventions:\n")
		for _, c := range capStringSlice(rc.Conventions, 30, 400) {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	if len(rc.KeyFiles) > 0 {
		b.WriteString("- key_files:\n")
		for i, kf := range rc.KeyFiles {
			if i >= 30 {
				break
			}
			path := trimWithCap(kf.Path, 240)
			role := trimWithCap(kf.Role, 240)
			if path == "" {
				continue
			}
			if role == "" {
				fmt.Fprintf(&b, "  - %s\n", path)
				continue
			}
			fmt.Fprintf(&b, "  - %s — %s\n", path, role)
		}
	}
	if len(rc.Caveats) > 0 {
		b.WriteString("- caveats:\n")
		for _, c := range capStringSlice(rc.Caveats, 20, 400) {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
