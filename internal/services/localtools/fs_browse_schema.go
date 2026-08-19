package localtools

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

var fsBrowseToolDocs = map[string]fsToolDoc{
	"list_dir": {
		terse: "List a directory. Set recursive=true for a depth-limited tree. Entries: dirs end with '/', '*' marks executables, files over 1 MiB show a size. Gitignored and high-noise directories are omitted. Long listings are truncated with a resume offset.",
		verbose: "List entries in a directory under the project root. Non-recursive by default: one level, names sorted. Set recursive true for a depth-limited tree (paths relative to the listed directory, dirs end with /). " +
			"Entry names carry ls -F-style hints so you can tell a directory from a text file from an executable binary without a follow-up call: directories end with '/'; a trailing '*' means the executable bit is set; files over 1 MiB get a compact size in parentheses, e.g. 'contenox* (48 MiB)'. " +
			"Paths matched by the repository's .gitignore are omitted, as are high-noise directories (.git, node_modules, .venv, dist, target, ...). Override with the _use_gitignore and _skip_dir_names policy keys. " +
			"Filter returned files by extension with _list_extensions. " +
			"When the listing exceeds the output cap it is truncated with a notice naming the exact offset to resume from — pass that as the offset argument. " +
			"Calling list_dir on something that is not a directory returns an error describing what the path actually is (kind, size, executable flag). " +
			"A missing path returns a 'Did you mean:' suggestion of similar sibling names.",
	},
	"grep": {
		terse: "Search for a pattern. Literal substring by default; set regex=true for RE2. path may be a file or directory; directories search recursively (skipping binaries, .gitignore'd and high-noise paths), capped at 100 matches and the usual output-byte cap. Single-file matches print as 'N: text'; directory matches as 'path:N: text'. start_line/end_line only apply to a single file.",
		verbose: "Search for a pattern. Default: literal substring match. Set regex true for a Go RE2 regular expression matched per line. " +
			"path may be a file or a directory: a directory searches every text file beneath it recursively, applying the same .gitignore and high-noise-directory filtering as list_dir, and silently skipping binaries and unreadable files rather than aborting the search. " +
			"Optional start_line and end_line (1-based, inclusive) limit a single-file search to a line range; they are ignored in directory mode. " +
			"Output: matching lines as 'N: text' for a single file, or 'relative/path:N: text' when searching a directory. " +
			"Directory mode is capped at 100 matches (regardless of _max_grep_matches, which is sized for one file) and the same output-byte cap as everything else in this toolset; when either cap is hit, the matches found so far are returned with a notice to narrow the pattern or search a subdirectory. Refuses binary files.",
	},
	"find_files": {
		terse: "Find files by glob under the project root. filepath.Match syntax ('*.go'), plus '**' to span any number of directories (e.g. 'src/**/*.ts'). Pattern matches the basename unless it contains a slash. Returns JSON {matches, count, truncated}. Gitignored paths are skipped.",
		verbose: "Find files by name pattern under the project root. Uses Go filepath.Match glob syntax: * matches any sequence of non-separator characters, ? matches one character, [range] matches a character class. " +
			"** additionally matches zero or more whole path segments, crossing directory boundaries — e.g. \"src/**/*.ts\" finds every .ts file under src at any depth, including directly in src itself. " +
			"Without a slash in the pattern, the pattern is matched against the file basename only (e.g. \"*.go\" finds all Go files anywhere in the tree). With a slash (including \"**\"), the pattern is matched against the relative path. " +
			"Returns JSON: {matches: [...], count: N, truncated: true|false}. Results are capped at 200 by default (policy: _max_find_results) and depth is bounded. " +
			"Gitignored paths, high-noise directories, and paths excluded by policy are skipped.",
	},
	"count_stats": {
		terse:   "Count lines, words, and bytes in a file. Returns \"Lines: N, Words: N, Bytes: N\". Cheap way to size a file before reading it.",
		verbose: "Count lines, words, and bytes in a file. Returns a plain string in the format \"Lines: N, Words: N, Bytes: N\". Useful for sizing a file before deciding how much of it to read. Refuses binary files.",
	},
	"stat_file": {
		terse: "Metadata for a file or directory: {name, size, sizeHuman, modTime, isDir, mode, executable, binary}. Reads at most ~512 bytes. Use before reading when a path's type is unclear.",
		verbose: "Return metadata for a file or directory. Returns JSON with {name, size (bytes), sizeHuman (e.g. \"48 MiB\"), modTime (RFC3339), isDir (bool), mode (Go permission string, e.g. \"-rwxr-xr-x\"), executable (bool: any executable bit set), binary (bool: best-effort content sniff of the first ~512 bytes — NUL byte or high invalid-UTF-8 density; always false for directories)}. " +
			"Reads at most the first ~512 bytes of file content for the binary check, never the whole file. " +
			"Use this before reading when unsure whether a path is a directory, a text file, or an executable/binary — a 50 MB binary reports as {isDir:false, executable:true, binary:true, sizeHuman:\"48 MiB\", ...}. " +
			"A missing path returns a 'Did you mean:' suggestion of similar sibling names.",
	},
}

func (h *LocalFSBrowseTools) Supports(ctx context.Context) ([]string, error) {
	return []string{
		h.name,
		"list_dir", "grep", "find_files", "count_stats", "stat_file",
	}, nil
}

func fsBrowseSchemaSpecs() []toolSchemaSpec {
	return []toolSchemaSpec{
		{tool: "list_dir", component: "LocalFsListDir", response: fsListDirResponse},
		{tool: "grep", component: "LocalFsGrep", response: fsGrepResponse},
		{tool: "find_files", component: "LocalFsFindFiles", response: fsFindFilesResponse},
		{tool: "count_stats", component: "LocalFsCountStats", response: fsCountStatsResponse},
		{tool: "stat_file", component: "LocalFsStatFile", response: fsStatFileResponse},
	}
}

func (h *LocalFSBrowseTools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	declared, err := h.GetToolsForToolsByName(ctx, h.name)
	if err != nil {
		return nil, err
	}
	doc, err := buildToolsetDoc(h.name, "Local Filesystem Browse Tools",
		"List, search and inspect files inside the workspace directory, read-only. Every path is contained to that directory, gitignored and high-noise paths are omitted, binaries are refused rather than dumped into the transcript, and every result is capped and says what it withheld.",
		declared, fsBrowseSchemaSpecs())
	if err != nil {
		return nil, err
	}
	return map[string]*openapi3.T{h.name: doc}, nil
}

func (h *LocalFSBrowseTools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	v := h.verboseToolDescriptions(ctx)
	doc := func(tool string) string { return fsBrowseToolDocs[tool].pick(v) }

	allTools := []taskengine.Tool{
		fsTool("list_dir", doc("list_dir"), map[string]any{
			"path":      fsProp("string", "Directory path relative to the project root (default: .)"),
			"recursive": fsProp("boolean", "List subdirectories up to max_depth"),
			"max_depth": fsProp("integer", "Maximum depth below the listed path when recursive (default 3)"),
			"offset":    fsProp("integer", "Skip this many entries. Supply the value from a truncation notice to continue."),
		}),

		fsTool("grep", doc("grep"), map[string]any{
			"path":       fsProp("string", "Path to a file or a directory, relative to the project root. A directory searches recursively."),
			"pattern":    fsProp("string", "Substring to find, or a regex when regex is true"),
			"regex":      fsProp("boolean", "Treat pattern as a Go RE2 regular expression"),
			"start_line": fsProp("integer", "First line to search (1-based, default 1); single-file search only"),
			"end_line":   fsProp("integer", "Last line to search, inclusive (default: end of file); single-file search only"),
		}, "path", "pattern"),

		fsTool("find_files", doc("find_files"), map[string]any{
			"pattern": fsProp("string", "Glob matched against the basename (e.g. \"*.go\"), or against the relative path when it contains a slash or \"**\" (e.g. \"src/**/*.ts\")"),
			"path":    fsProp("string", "Directory to search from, relative to the project root (default: project root)"),
		}, "pattern"),

		fsTool("count_stats", doc("count_stats"), map[string]any{
			"path": fsProp("string", "Path to the file"),
		}, "path"),

		fsTool("stat_file", doc("stat_file"), map[string]any{
			"path": fsProp("string", "Path to the file or directory"),
		}, "path"),
	}

	if name == h.name {
		return allTools, nil
	}
	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("unknown tools tool: %s", name)
}
