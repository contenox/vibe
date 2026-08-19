package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
)

// walkGuard re-applies the vfs control-plane denylist to every path a recursive
// walk reaches. resolveTarget only contains the walk root, so a control-plane
// directory nested under the allowed root (the project's own .contenox holding
// the relay token) would otherwise be descended, listed, grepped and read.
type walkGuard struct {
	controlPlane []string
}

func (h *LocalFSBrowseTools) walkGuardFor() walkGuard {
	return walkGuard{controlPlane: vfs.ControlPlaneDenied()}
}

// deny reports whether an absolute path reached during a walk must be withheld
// because it resolves at or under the runtime control plane (symlinks resolved).
func (g walkGuard) deny(absPath string) bool {
	if len(g.controlPlane) == 0 {
		return false
	}
	_, hit := vfs.WithinControlPlane(g.controlPlane, absPath)
	return hit
}

func (h *LocalFSBrowseTools) entryFilterFor(ctx context.Context) entryFilter {
	f := entryFilter{
		skipDirs:   h.skipDirNamesFromPolicy(ctx),
		allowExts:  h.listExtensionsFromPolicy(ctx),
		deniedSubs: h.deniedSubstringsFromPolicy(ctx),
	}
	if h.useGitignoreFromPolicy(ctx) {
		if base, err := h.absAllowedDir(ctx); err == nil {
			f.ignore = gitignoreFor(base)
		}
	}
	return f
}

func (h *LocalFSBrowseTools) listDir(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, _ := argString(args, "path")
	if path == "" {
		path = "."
	}
	listRootArg := filepath.Clean(path)

	absRoot, display, st, err := h.resolveTarget(ctx, "list_dir", listRootArg)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if !st.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef(
			"%s: list_dir: %s is not a directory: %s",
			h.name, display, describePathForError(absRoot, st),
		)
	}

	recursive, _ := argBool(args, "recursive")
	policyMaxDepth := h.maxListDepthFromPolicy(ctx)
	reqDepth := 1
	if recursive {
		reqDepth = 3
		if v, ok := argInt(args, "max_depth"); ok && v >= 1 {
			reqDepth = v
		}
		if reqDepth > policyMaxDepth {
			reqDepth = policyMaxDepth
		}
	}

	offset := 0
	if v, ok := argInt(args, "offset"); ok && v > 0 {
		offset = v
	}

	budget, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		budget = 0
	}
	// Leave headroom for the truncation notice, so appending it cannot push
	// the result over the cap.
	if budget > 1024 {
		budget -= 512
	}

	filter := h.entryFilterFor(ctx)
	// gitignore matching bases relative paths on the workspace root, not the
	// listed subdirectory.
	baseRoot, baseErr := h.absAllowedDir(ctx)
	if baseErr != nil {
		baseRoot = absRoot
	}

	c := &listCollector{
		budget:  budget,
		offset:  offset,
		maxScan: h.maxListEntriesScannedFromPolicy(ctx),
	}
	guard := h.walkGuardFor()

	if recursive {
		if err := h.walkListDir(ctx, listRootArg, absRoot, baseRoot, "", 1, reqDepth, filter, guard, c); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	} else {
		if err := h.listOneLevel(absRoot, baseRoot, filter, guard, c); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	}

	out := strings.Join(c.out, "\n")
	if c.truncated {
		if out != "" {
			out += "\n"
		}
		out += fmt.Sprintf(
			"%s: list_dir truncated — showed %d entries starting at offset %d; output capped at %d bytes. To continue call list_dir with offset: %d (same path, recursive, and max_depth). %s",
			h.name, len(c.out), offset, budget, c.nextOffset(), severityRecoverable)
	}
	if len(c.out) == 0 && !c.truncated {
		return "", taskengine.DataTypeString, nil
	}
	return out, taskengine.DataTypeString, nil
}

func (h *LocalFSBrowseTools) listOneLevel(absRoot, baseRoot string, filter entryFilter, guard walkGuard, c *listCollector) error {
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return fmt.Errorf("%s: failed to read directory: %w", h.name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !c.visit() {
			return nil
		}
		abs := filepath.Join(absRoot, e.Name())
		if guard.deny(abs) {
			continue
		}
		relFromBase := e.Name()
		if r, relErr := filepath.Rel(baseRoot, abs); relErr == nil {
			relFromBase = filepath.ToSlash(r)
		}
		if filter.skip(relFromBase, e.Name(), e.IsDir()) {
			continue
		}
		if e.IsDir() {
			if !c.add(e.Name() + "/") {
				return nil
			}
			continue
		}
		name := e.Name()
		if info, infoErr := e.Info(); infoErr == nil {
			name += fileEntrySuffix(info)
		}
		if !c.add(name) {
			return nil
		}
	}
	return nil
}

func (h *LocalFSBrowseTools) walkListDir(
	ctx context.Context,
	listRootArg, curAbs, baseRoot, relFromListRoot string,
	depth, maxDepth int,
	filter entryFilter,
	guard walkGuard,
	c *listCollector,
) error {
	entries, err := os.ReadDir(curAbs)
	if err != nil {
		// An unreadable subdirectory should not abort a listing that is
		// otherwise useful; the top-level call already verified the root.
		if depth > 1 {
			return nil
		}
		return fmt.Errorf("%s: failed to read directory: %w", h.name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !c.visit() {
			return nil
		}

		rel := e.Name()
		if relFromListRoot != "" {
			rel = filepath.ToSlash(filepath.Join(relFromListRoot, e.Name()))
		}

		userPath := rel
		if listRootArg != "" && listRootArg != "." {
			userPath = filepath.ToSlash(filepath.Join(listRootArg, rel))
		}

		childAbs := filepath.Join(curAbs, e.Name())
		if guard.deny(childAbs) {
			continue
		}
		relFromBase := userPath
		if r, relErr := filepath.Rel(baseRoot, childAbs); relErr == nil {
			relFromBase = filepath.ToSlash(r)
		}

		if filter.skip(relFromBase, e.Name(), e.IsDir()) {
			continue
		}

		if e.IsDir() {
			if !c.add(userPath + "/") {
				return nil
			}
			if depth >= maxDepth {
				continue
			}
			if err := h.walkListDir(ctx, listRootArg, childAbs, baseRoot, rel, depth+1, maxDepth, filter, guard, c); err != nil {
				return err
			}
			if c.truncated {
				return nil
			}
			continue
		}

		name := userPath
		if info, infoErr := e.Info(); infoErr == nil {
			name += fileEntrySuffix(info)
		}
		if !c.add(name) {
			return nil
		}
	}
	return nil
}

func (h *LocalFSBrowseTools) grep(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: path required for grep", h.name)
	}
	pattern, ok := argString(args, "pattern")
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: pattern required for grep", h.name)
	}
	if len(pattern) > 8192 {
		return nil, taskengine.DataTypeAny, recoverablef("%s: grep: pattern exceeds 8192 characters", h.name)
	}

	useRegex, _ := argBool(args, "regex")
	var re *regexp.Regexp
	if useRegex {
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, taskengine.DataTypeAny, recoverablef("%s: grep: invalid regex: %v", h.name, err)
		}
	}

	absPath, display, info, err := h.resolveTarget(ctx, "grep", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		out, err := h.grepDir(ctx, absPath, display, pattern, useRegex, re)
		if err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return out, taskengine.DataTypeString, nil
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary("grep", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: failed to read file: %w", h.name, err)
	}

	lines := strings.Split(string(content), "\n")
	start, end := grepLineRange(args, len(lines))
	maxMatches := h.maxGrepMatchesFromPolicy(ctx)
	budget, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		budget = 0
	}
	if budget > 1024 {
		budget -= 512
	}

	var (
		matches   []string
		size      int64
		truncated bool
		hitCap    bool // stopped on _max_grep_matches rather than the byte budget
		lastLine  = end
	)
	for lineNo := start; lineNo <= end; lineNo++ {
		if lineNo < 1 || lineNo > len(lines) {
			continue
		}
		line := lines[lineNo-1]
		var matched bool
		if useRegex {
			matched = re.MatchString(line)
		} else {
			matched = strings.Contains(line, pattern)
		}
		if !matched {
			continue
		}
		entry := fmt.Sprintf("%d: %s", lineNo, line)
		if budget > 0 && size+int64(len(entry)+1) > budget {
			truncated = true
			lastLine = lineNo - 1
			break
		}
		matches = append(matches, entry)
		size += int64(len(entry) + 1)
		if len(matches) >= maxMatches {
			truncated = true
			hitCap = true
			lastLine = lineNo
			break
		}
	}

	out := strings.Join(matches, "\n")
	if truncated {
		if out != "" {
			out += "\n"
		}
		noun := "matches"
		if len(matches) == 1 {
			noun = "match"
		}
		reason := fmt.Sprintf("output capped at %d bytes (raise %s or %s)", budget, h.policyKey("_max_output_bytes"), h.policyKey("_model_context_tokens"))
		if hitCap {
			reason = fmt.Sprintf("hit the %d-match cap (raise %s)", maxMatches, h.policyKey("_max_grep_matches"))
		}
		out += fmt.Sprintf(
			"%s: grep truncated — %d %s shown, searched lines %d-%d of %d; %s. Narrow the pattern or continue with start_line: %d. %s",
			h.name, len(matches), noun, start, lastLine, len(lines), reason, lastLine+1, severityRecoverable)
	}
	return out, taskengine.DataTypeString, nil
}

func (h *LocalFSBrowseTools) grepDir(ctx context.Context, absRoot, display, pattern string, useRegex bool, re *regexp.Regexp) (string, error) {
	filter := h.entryFilterFor(ctx)
	guard := h.walkGuardFor()
	baseRoot, baseErr := h.absAllowedDir(ctx)
	if baseErr != nil {
		baseRoot = absRoot
	}
	maxDepth := h.maxFindDepthFromPolicy(ctx)
	maxScan := h.maxListEntriesScannedFromPolicy(ctx)
	readLimit, unlimitedRead := h.maxReadBytesFromPolicy(ctx)

	maxMatches := h.maxGrepMatchesFromPolicy(ctx)
	if maxMatches > dirGrepMaxMatches {
		maxMatches = dirGrepMaxMatches
	}
	budget, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	if unlimitedOut {
		budget = 0
	}
	if budget > 1024 {
		budget -= 512
	}

	var (
		matches   []string
		size      int64
		scanned   int
		truncated bool
		hitCap    bool
	)

	walkErr := filepath.WalkDir(absRoot, func(walkPath string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if truncated {
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(absRoot, walkPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		relFromBase := rel
		if r, e := filepath.Rel(baseRoot, walkPath); e == nil {
			relFromBase = filepath.ToSlash(r)
		}

		if guard.deny(walkPath) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if filter.skip(relFromBase, d.Name(), true) {
				return filepath.SkipDir
			}
			if strings.Count(rel, "/")+1 > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}

		scanned++
		if maxScan > 0 && scanned > maxScan {
			truncated = true
			return filepath.SkipAll
		}
		if filter.skip(relFromBase, d.Name(), false) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		if !unlimitedRead && info.Size() > readLimit {
			return nil
		}
		if binary, sniffErr := sniffBinaryFile(walkPath); sniffErr != nil || binary {
			return nil
		}
		content, readErr := os.ReadFile(walkPath)
		if readErr != nil {
			return nil
		}

		for lineNo, line := range strings.Split(string(content), "\n") {
			var matched bool
			if useRegex {
				matched = re.MatchString(line)
			} else {
				matched = strings.Contains(line, pattern)
			}
			if !matched {
				continue
			}
			entry := fmt.Sprintf("%s:%d: %s", rel, lineNo+1, line)
			if budget > 0 && size+int64(len(entry)+1) > budget {
				truncated = true
				return filepath.SkipAll
			}
			matches = append(matches, entry)
			size += int64(len(entry) + 1)
			if len(matches) >= maxMatches {
				truncated = true
				hitCap = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("%s: grep: %w", h.name, walkErr)
	}

	out := strings.Join(matches, "\n")
	if truncated {
		if out != "" {
			out += "\n"
		}
		noun := "matches"
		if len(matches) == 1 {
			noun = "match"
		}
		reason := fmt.Sprintf("output capped at %d bytes (raise %s or %s)", budget, h.policyKey("_max_output_bytes"), h.policyKey("_model_context_tokens"))
		if hitCap {
			reason = fmt.Sprintf("hit the %d-match directory-search cap", maxMatches)
		}
		out += fmt.Sprintf(
			"%s: grep truncated — %d %s shown under %s; %s. Narrow the pattern or search a subdirectory. %s",
			h.name, len(matches), noun, display, reason, severityRecoverable)
	}
	return out, nil
}

func (h *LocalFSBrowseTools) findFiles(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	pattern, ok := argString(args, "pattern")
	if !ok || pattern == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: pattern required for find_files", h.name)
	}
	rootArg, _ := argString(args, "path")
	if rootArg == "" {
		rootArg = "."
	}

	absRoot, display, info, err := h.resolveTarget(ctx, "find_files", filepath.Clean(rootArg))
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if !info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("%s: find_files: %s is not a directory", h.name, display)
	}

	patternHasSlash := strings.ContainsRune(pattern, '/')
	hasDoubleStar := strings.Contains(pattern, "**")
	filter := h.entryFilterFor(ctx)
	guard := h.walkGuardFor()
	maxResults := h.maxFindResultsFromPolicy(ctx)
	maxDepth := h.maxFindDepthFromPolicy(ctx)
	baseRoot, baseErr := h.absAllowedDir(ctx)
	if baseErr != nil {
		baseRoot = absRoot
	}

	var matches []string
	truncated := false

	walkErr := filepath.WalkDir(absRoot, func(walkPath string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if truncated {
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(absRoot, walkPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		relFromBase := rel
		if r, e := filepath.Rel(baseRoot, walkPath); e == nil {
			relFromBase = filepath.ToSlash(r)
		}

		if guard.deny(walkPath) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if filter.skip(relFromBase, d.Name(), true) {
				return filepath.SkipDir
			}
			if strings.Count(rel, "/")+1 > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if filter.skip(relFromBase, d.Name(), false) {
			return nil
		}

		var matched bool
		switch {
		case hasDoubleStar:
			// "**" crosses "/" boundaries, so it matches the whole relative
			// path, never just the basename.
			matched = doubleStarGlobMatch(pattern, rel)
		case patternHasSlash:
			matched, _ = filepath.Match(pattern, rel)
		default:
			matched, _ = filepath.Match(pattern, d.Name())
		}
		if matched {
			matches = append(matches, rel)
			if len(matches) >= maxResults {
				truncated = true
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: find_files: %w", h.name, walkErr)
	}
	if matches == nil {
		matches = []string{}
	}

	type findResult struct {
		Matches   []string `json:"matches"`
		Count     int      `json:"count"`
		Truncated bool     `json:"truncated,omitempty"`
		Note      string   `json:"note,omitempty"`
	}
	res := findResult{Matches: matches, Count: len(matches), Truncated: truncated}
	if truncated {
		res.Note = fmt.Sprintf("capped at %d results; narrow the pattern or search a subdirectory", maxResults)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: find_files marshal: %w", h.name, err)
	}
	s := string(out)
	if err := h.checkToolOutputLimit(ctx, "find_files", s); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return s, taskengine.DataTypeJSON, nil
}

func (h *LocalFSBrowseTools) countStats(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: path required for count_stats", h.name)
	}

	absPath, display, info, err := h.resolveTarget(ctx, "count_stats", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("%s: count_stats: %s is a directory", h.name, display)
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary("count_stats", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: failed to read file: %w", h.name, err)
	}

	text := string(content)
	lineCount := countTextLines(text)
	if len(content) > 0 && content[len(content)-1] == '\n' {
		lineCount--
	}
	result := fmt.Sprintf("Lines: %d, Words: %d, Bytes: %d", lineCount, len(strings.Fields(text)), len(content))
	if err := h.checkToolOutputLimit(ctx, "count_stats", result); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return result, taskengine.DataTypeString, nil
}

func (h *LocalFSBrowseTools) statFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: path required for stat_file", h.name)
	}

	absPath, _, info, err := h.resolveTarget(ctx, "stat_file", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	// binary is only meaningful, and only sniffed, for regular files.
	binary := false
	if info.Mode().IsRegular() {
		if b, sniffErr := sniffBinaryFile(absPath); sniffErr == nil {
			binary = b
		}
	}

	result := map[string]any{
		"name":       info.Name(),
		"size":       info.Size(),
		"sizeHuman":  humanSize(info.Size()),
		"modTime":    info.ModTime().Format(time.RFC3339),
		"isDir":      info.IsDir(),
		"mode":       info.Mode().String(),
		"executable": isExecutable(info),
		"binary":     binary,
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: marshal stat: %w", h.name, err)
	}
	out := string(b)
	if err := h.checkToolOutputLimit(ctx, "stat_file", out); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return out, taskengine.DataTypeJSON, nil
}
