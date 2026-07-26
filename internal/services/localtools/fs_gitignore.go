package localtools

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// .gitignore filtering
//
// Basename skip-lists can only omit noise someone enumerated in advance. The
// directories that actually flood an agent's context are project-specific:
// scratch dirs, build output, vendored comparison checkouts, local model
// caches, browser-automation logs. Every one of those is already declared in
// .gitignore, so reading it fixes the whole class instead of one instance.
//
// This is a deliberately partial implementation of gitignore semantics. It
// covers what matters for suppressing noise in a listing:
//
//   - comments (#) and blank lines
//   - negation (!pattern), last matching rule wins
//   - directory-only patterns (pattern/)
//   - root-anchored patterns (/pattern)
//   - glob patterns via filepath.Match, matched against the basename when the
//     pattern has no slash, and against the root-relative path when it does
//   - a leading "**/" is treated as "match at any depth"
//
// Not covered: nested .gitignore files in subdirectories, .git/info/exclude,
// the global core.excludesFile, and "**" appearing mid-pattern. Those affect
// what git tracks; they do not meaningfully change whether a listing is
// readable. This is a noise filter, never an access control — containment is
// vfs.Contain's job and denial is _denied_path_substrings' job.
// ---------------------------------------------------------------------------

type ignoreRule struct {
	pattern string // normalised, slash-separated, no leading/trailing marker chars
	negate  bool
	dirOnly bool
	rooted  bool // pattern was anchored with a leading slash
	hasSep  bool // pattern contains a separator, so match against the full rel path
}

type ignoreMatcher struct {
	rules []ignoreRule
}

// Match reports whether rel (a slash-separated path relative to the ignore
// file's directory) is ignored. Later rules override earlier ones, matching
// git's own last-match-wins behaviour.
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
		line := sc.Text()
		// Trailing whitespace is insignificant unless backslash-escaped;
		// we do not support the escape, which is vanishingly rare.
		line = strings.TrimRight(line, " \t")
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
		// "**/foo" means "foo at any depth", which is this matcher's default
		// for separator-free patterns.
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

// ---------------------------------------------------------------------------
// Caching
//
// A listing calls Match once per entry, and a recursive walk of a large tree
// makes thousands of those calls. Re-reading and re-parsing .gitignore each
// time would dominate the walk, so matchers are cached per root and revalidated
// against the file's mtime and size.
// ---------------------------------------------------------------------------

type ignoreCacheEntry struct {
	matcher *ignoreMatcher
	modTime time.Time
	size    int64
	loaded  time.Time
}

var (
	ignoreCacheMu sync.RWMutex
	ignoreCache   = map[string]ignoreCacheEntry{}
)

// ignoreCacheTTL bounds how long a cached matcher is trusted even when mtime
// and size are unchanged, so a same-second rewrite is picked up eventually.
const ignoreCacheTTL = 5 * time.Second

// gitignoreFor loads (or returns cached) ignore rules for the given absolute
// root. A missing .gitignore yields a nil matcher, which Match treats as
// "nothing is ignored".
func gitignoreFor(absRoot string) *ignoreMatcher {
	path := filepath.Join(absRoot, ".gitignore")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}

	ignoreCacheMu.RLock()
	entry, ok := ignoreCache[absRoot]
	ignoreCacheMu.RUnlock()
	if ok &&
		entry.modTime.Equal(info.ModTime()) &&
		entry.size == info.Size() &&
		time.Since(entry.loaded) < ignoreCacheTTL {
		return entry.matcher
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := parseIgnoreFile(data)

	ignoreCacheMu.Lock()
	ignoreCache[absRoot] = ignoreCacheEntry{
		matcher: m,
		modTime: info.ModTime(),
		size:    info.Size(),
		loaded:  time.Now(),
	}
	ignoreCacheMu.Unlock()
	return m
}

// entryFilter bundles every "should this path appear in a listing" decision so
// list_dir and find_files share one implementation and cannot drift apart.
type entryFilter struct {
	skipDirs   map[string]bool
	allowExts  map[string]bool
	ignore     *ignoreMatcher
	deniedSubs []string
}

// skip reports whether an entry should be omitted. rel is the path relative to
// the workspace root, using forward slashes.
func (f entryFilter) skip(rel string, base string, isDir bool) bool {
	if isDir && f.skipDirs[base] {
		return true
	}
	if !isDir && f.allowExts != nil && !f.allowExts[strings.ToLower(filepath.Ext(base))] {
		return true
	}
	for _, sub := range f.deniedSubs {
		if strings.Contains(rel, sub) {
			return true
		}
	}
	if f.ignore != nil && f.ignore.Match(rel, isDir) {
		return true
	}
	return false
}
