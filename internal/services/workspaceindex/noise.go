package workspaceindex

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// DUPLICATED NOISE FILTER — extraction pending
//
// Everything below is a copy of the matcher the agent's own local_fs find_files
// uses: internal/services/localtools/fs_gitignore.go (ignoreRule, ignoreMatcher,
// parseIgnoreFile) and internal/services/localtools/fs_policy.go's
// defaultSkipDirNames. Not one of those identifiers is exported, so there is
// nothing to import today — internal/surfaces/beamtui/comp/fileaddr/noise.go is
// a second copy of the same code, for the same reason.
//
// TODO(blueprint beam-tui.md section 3, item 9 — "Extract the gitignore/skip-dirs
// matcher"): that item exists precisely because @-mention completion and
// find_files MUST filter identically, or the human's list and the agent's list
// disagree about which files exist. The index is now a THIRD party to that
// agreement — a semantic search that indexes files the agent cannot see, or
// misses files it can, is worse than no search — so the extraction is now
// overdue rather than merely pending. Where the shared matcher lands (its own
// package, or folded into vfs/localfileservice) is still an open design
// decision. When it lands, DELETE this file and call it; there will be three
// copies to delete, which is the point.
//
// Scope note, unchanged from both originals: this is a NOISE filter, never
// access control. Containment is vfs's job, and walkWorkspace runs every
// candidate through vfs.Contain for exactly that reason.
// ---------------------------------------------------------------------------

// defaultSkipDirNames is a verbatim copy of localtools' fallback set of
// directory basenames omitted from listings. See the extraction TODO above.
var defaultSkipDirNames = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "bower_components": true, "Pods": true,
	".venv": true, "venv": true, "env": true, "__pycache__": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true, ".tox": true,
	".next": true, ".nuxt": true, ".turbo": true, ".parcel-cache": true,
	"dist": true, "build": true, "out": true, "target": true, "coverage": true,
	".cache": true, ".gradle": true, ".terraform": true,
	"vendor":  true,
	".idea":   true,
	".vscode": true,
}

// skipDir reports whether a directory basename is noise.
func skipDir(base string) bool { return defaultSkipDirNames[base] }

// ignoreRule and ignoreMatcher mirror localtools' partial gitignore
// implementation: comments, negation (last match wins), directory-only patterns,
// root-anchored patterns, filepath.Match globs matched against the basename when
// the pattern has no slash and against the root-relative path when it does, and
// a leading "**/" meaning "at any depth". Nested .gitignore files,
// .git/info/exclude, core.excludesFile, and mid-pattern "**" are out of scope
// there and therefore out of scope here.
type ignoreRule struct {
	pattern string
	negate  bool
	dirOnly bool
	rooted  bool
	hasSep  bool
}

type ignoreMatcher struct {
	rules []ignoreRule
}

// Match reports whether rel (slash-separated, relative to the ignore file's
// directory) is ignored. Later rules override earlier ones, as git does.
func (m *ignoreMatcher) Match(rel string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 || rel == "" || rel == "." {
		return false
	}
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.matches(rel) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (r ignoreRule) matches(rel string) bool {
	if r.hasSep || r.rooted {
		if ok, _ := filepath.Match(r.pattern, rel); ok {
			return true
		}
		// A rule matching a directory also covers everything beneath it.
		if strings.HasPrefix(rel, r.pattern+"/") {
			return true
		}
		if !r.rooted {
			// Unanchored multi-segment patterns may match at any depth.
			for i := 0; i < len(rel); i++ {
				if rel[i] != '/' {
					continue
				}
				suffix := rel[i+1:]
				if ok, _ := filepath.Match(r.pattern, suffix); ok {
					return true
				}
				if strings.HasPrefix(suffix, r.pattern+"/") {
					return true
				}
			}
		}
		return false
	}
	// No separator: the pattern applies to any path segment, and matching a
	// segment ignores everything below it.
	for _, seg := range strings.Split(rel, "/") {
		if ok, _ := filepath.Match(r.pattern, seg); ok {
			return true
		}
	}
	return false
}

func parseIgnoreFile(data []byte) *ignoreMatcher {
	m := &ignoreMatcher{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.rooted = true
			line = strings.TrimPrefix(line, "/")
		}
		// "**/foo" means "foo at any depth", this matcher's default for
		// separator-free patterns.
		line = strings.TrimPrefix(line, "**/")
		if line == "" {
			continue
		}
		r.pattern = line
		r.hasSep = strings.Contains(line, "/")
		m.rules = append(m.rules, r)
	}
	return m
}

// gitignoreFor loads the root's .gitignore. A missing or unreadable file yields
// nil, which Match treats as "nothing is ignored".
//
// Like fileaddr's copy and unlike localtools', this does not cache: an index
// build walks the tree once and then does thousands of times more work per file
// embedding it, so a matcher cache would buy microseconds in exchange for
// staleness rules nobody asked for.
func gitignoreFor(absRoot string) *ignoreMatcher {
	path := filepath.Join(absRoot, ".gitignore")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseIgnoreFile(data)
}
