package workspaceindex

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// This is a copy of the noise-filter matcher used by localtools' find_files
// (fs_gitignore.go, fs_policy.go) and beamtui's fileaddr package; none of
// those identifiers are exported. It is a noise filter only, never access
// control — containment is vfs's job, enforced separately in walkWorkspace.

// defaultSkipDirNames is a copy of localtools' fallback skip-dir set.
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
// implementation: comments, negation (last match wins), directory-only and
// root-anchored patterns, filepath.Match globs, and a leading "**/" meaning
// "at any depth". Nested .gitignore files, .git/info/exclude,
// core.excludesFile, and mid-pattern "**" are out of scope.
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
	// No separator: pattern applies to any segment; matching one ignores everything below it.
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

// gitignoreFor loads the root's .gitignore. A missing or unreadable file
// yields nil, which Match treats as "nothing is ignored". Unlike
// localtools', this does not cache: a build walks the tree once.
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
