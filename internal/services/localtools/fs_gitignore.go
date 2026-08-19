package localtools

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

// Match reports whether rel (a slash-separated path relative to the ignore file's directory) is ignored, later rules overriding earlier ones per git's last-match-wins behaviour.
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

	// No separator: the pattern applies to any path segment, and matching one ignores everything below it.
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
		// Trailing whitespace is insignificant unless backslash-escaped; that escape is not supported (vanishingly rare).
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
		// "**/foo" means "foo at any depth", already this matcher's default for separator-free patterns.
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

const ignoreCacheTTL = 5 * time.Second

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

type entryFilter struct {
	skipDirs   map[string]bool
	allowExts  map[string]bool
	ignore     *ignoreMatcher
	deniedSubs []string
}

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
