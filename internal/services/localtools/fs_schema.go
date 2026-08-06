package localtools

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ---------------------------------------------------------------------------
// Tool schemas
//
// Descriptions are terse by default. The long-form versions previously shipped
// here ran to ~200 words each across nine tools — plausibly 1,500-2,500 tokens
// serialised, or 8-13% of a 20k-token context window consumed before a single
// byte of file content. That is paid on every single turn.
//
// Almost everything the long form taught (truncation semantics, did-you-mean
// suggestions, severity markers, the read-before-write contract) is re-taught
// by the corresponding error message at the moment it fires, with the concrete
// path and line number filled in. Teaching in the error costs tokens once,
// when relevant; teaching in the schema costs them always.
//
// Set _verbose_tool_descriptions=true in tools_policies.local_fs to restore the
// long form for large-context models.
// ---------------------------------------------------------------------------

type fsToolDoc struct {
	terse   string
	verbose string
}

func (d fsToolDoc) pick(verbose bool) string {
	if verbose && d.verbose != "" {
		return d.verbose
	}
	return d.terse
}

var fsToolDocs = map[string]fsToolDoc{
	"read_file": {
		terse: "Read a text file. Omit start_line/end_line for the whole file; supply them to page through a large one. Refuses binaries. Oversized results are truncated to a head with a notice naming the exact resume line. A full read is required before write_file on an existing file.",
		verbose: "Read a text file. With no start_line/end_line, reads the whole file and returns the raw text. " +
			"Refuses with an error for files that sniff as binary (a NUL byte or a high fraction of invalid UTF-8 in the first ~512 bytes) instead of dumping raw bytes into your context — call stat_file first if unsure, or use shell tools for binaries. " +
			"If you have already read this exact file version this session and it is unchanged on disk, you get a short stub instead of the full content; pass force=true to get the content again. " +
			"NEVER TRUNCATED SILENTLY: if the file is larger than the read/output cap, the result is a line-based HEAD followed by a notice naming the exact next step — 'call read_file with start_line: N'. " +
			"A truncated or ranged read does NOT satisfy the full-file read-before-write prerequisite; only a complete read does. " +
			"Missing paths return a 'Did you mean:' suggestion of similar sibling names. " +
			"Errors carry a severity marker: '(recoverable: adjust parameters and retry)' when a corrected call fixes it, '(fatal: <reason>)' only for a broken environment.",
	},
	"write_file": {
		terse: "Overwrite a file, or create it if absent. Creates parent directories. Returns JSON metadata, not the file bodies. Overwriting an existing file requires a prior full read_file of its current version in this session. Prefer edit_file for a targeted change to an existing file.",
		verbose: "Overwrite a file with new content, or create it if it does not exist. Creates intermediate directories automatically. " +
			"Writes are atomic: a failure partway through leaves the original file intact. " +
			"Returns compact JSON with {path, written, old_bytes, new_bytes, old_sha256, new_sha256}; full old/new file bodies are not returned to the model. " +
			"Modifying an existing file requires a prior read_file call against the same current version in this session; read_file_range is not sufficient for a full-file overwrite. " +
			"Creating a brand-new file requires no prior read. If the file changes between your read and this write, the write is refused rather than clobbering the newer version. " +
			"Prefer edit_file when the change is a targeted replacement within an existing file — it is cheaper and safer than resending the whole file.",
	},
	"edit_file": {
		terse: "Replace old_string with new_string in a file — prefer this over write_file for a targeted modification. old_string must match the current file byte-for-byte (whitespace-exact) and occur exactly once, unless replace_all=true (e.g. renaming an identifier everywhere). Requires a prior read of the current version; never applies a fuzzy match.",
		verbose: "Replace an exact occurrence of old_string with new_string in an existing file, without resending the whole file. " +
			"old_string must match the current on-disk text exactly, whitespace included, and by default must occur exactly once in the file — an ambiguous old_string is refused rather than guessed at, so give it enough surrounding context (a few lines) to be unique. " +
			"Set replace_all=true to replace every occurrence instead, e.g. renaming an identifier across the file. " +
			"old_string and new_string must differ, and old_string must not be empty. " +
			"Requires a prior read_file or read_file_range call against the current file version in this session, and re-verifies the file's hash immediately before writing; if the file changed since your read, the edit is refused rather than clobbering the newer version. " +
			"If old_string is not found, the file is left unchanged and the error says to re-read and retry with the exact current text. " +
			"Returns compact JSON with {path, written, replacements, old_bytes, new_bytes, old_sha256, new_sha256}; full file bodies are not returned. " +
			"Prefer this over write_file whenever the change is a targeted replacement rather than a full rewrite.",
	},
	"list_dir": {
		terse: "List a directory. Set recursive=true for a depth-limited tree. Entries: dirs end with '/', '*' marks executables, files over 1 MiB show a size. Gitignored and high-noise directories are omitted. Long listings are truncated with a resume offset.",
		verbose: "List entries in a directory under the project root. Non-recursive by default: one level, names sorted. Set recursive true for a depth-limited tree (paths relative to the listed directory, dirs end with /). " +
			"Entry names carry ls -F-style hints so you can tell a directory from a text file from an executable binary without a follow-up call: directories end with '/'; a trailing '*' means the executable bit is set; files over 1 MiB get a compact size in parentheses, e.g. 'contenox* (48 MiB)'. " +
			"Paths matched by the repository's .gitignore are omitted, as are high-noise directories (.git, node_modules, .venv, dist, target, ...). Override with the _use_gitignore and _skip_dir_names policy keys. " +
			"Filter returned files by extension with _list_extensions. " +
			"When the listing exceeds the output cap it is truncated with a notice naming the exact offset to resume from — pass that as the offset argument. " +
			"Calling list_dir on something that is not a directory returns an error describing what the path actually is (kind, size, executable/binary flags). " +
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
	"sed": {
		terse: "Literal string replacement in a file (not regex). Replaces exactly one occurrence unless you set all=true or expect_replacements=N. Optional start_line/end_line scope the edit. Requires a prior read of the current version. Never applies a fuzzy match.",
		verbose: "Replace literal occurrences of pattern with replacement in a file (plain string replacement, not regex). " +
			"By default the pattern must occur EXACTLY ONCE — an ambiguous pattern is refused rather than silently rewriting every call site. Set all=true to replace every occurrence, or expect_replacements=N to assert an exact count; a mismatch fails without writing. " +
			"Optional start_line/end_line (1-based, inclusive) confine the edit to a line range, which pairs well with a preceding grep. " +
			"Writes are atomic and the file's hash is re-verified immediately before the write. " +
			"Returns compact JSON with {path, written, changed, replacements, old_bytes, new_bytes, old_sha256, new_sha256}; full file bodies are not returned. " +
			"Requires a prior read_file or read_file_range call against the current file version in this session. " +
			"If the pattern is NOT found, the file is left UNCHANGED and you get the closest actual lines as a suggestion so you can correct the pattern — a fuzzy match is never applied on your behalf.",
	},
	"count_stats": {
		terse:   "Count lines, words, and bytes in a file. Returns \"Lines: N, Words: N, Bytes: N\". Cheap way to size a file before reading it.",
		verbose: "Count lines, words, and bytes in a file. Returns a plain string in the format \"Lines: N, Words: N, Bytes: N\". Useful for checking file size before deciding whether to read_file or read_file_range. Refuses binary files.",
	},
	"read_file_range": {
		terse: "Read a line range from a file (1-based, end_line optional). Works on files of any size. Satisfies the read prerequisite for sed but not for write_file.",
		verbose: "Read a contiguous range of lines from a file (1-based, inclusive end_line optional). Works on files of any size — streamed rather than loaded when over the read cap. " +
			"If the range's output exceeds the output cap it is truncated to a head with a notice naming the exact resume line, never silently. " +
			"This satisfies the read-before-mutate prerequisite for targeted sed edits on the same current file version, but not for write_file full-file overwrites — call read_file (full) before write_file on an existing file. " +
			"Missing paths return a 'Did you mean:' suggestion.",
	},
	"stat_file": {
		terse: "Metadata for a file or directory: {name, size, sizeHuman, modTime, isDir, mode, executable, binary}. Reads at most ~512 bytes. Use before read_file when a path's type is unclear.",
		verbose: "Return metadata for a file or directory. Returns JSON with {name, size (bytes), sizeHuman (e.g. \"48 MiB\"), modTime (RFC3339), isDir (bool), mode (Go permission string, e.g. \"-rwxr-xr-x\"), executable (bool: any executable bit set), binary (bool: best-effort content sniff of the first ~512 bytes — NUL byte or high invalid-UTF-8 density; always false for directories)}. " +
			"Reads at most the first ~512 bytes of file content for the binary check, never the whole file. " +
			"Use this before read_file when unsure whether a path is a directory, a text file, or an executable/binary — a 50 MB binary reports as {isDir:false, executable:true, binary:true, sizeHuman:\"48 MiB\", ...}. " +
			"A missing path returns a 'Did you mean:' suggestion of similar sibling names.",
	},
}

func (h *LocalFSTools) Supports(ctx context.Context) ([]string, error) {
	return []string{
		h.name,
		"read_file", "write_file", "edit_file", "list_dir", "grep", "find_files",
		"sed", "count_stats", "read_file_range", "stat_file",
	}, nil
}

// fsSchemaSpecs is every tool local_fs declares, in Supports order, with the
// OpenAPI component prefix and the response schema for each. The prefixes are
// fixed even though the toolset NAME is configurable (NewLocalFSToolsWith), so
// a second instance under another name publishes the same component names.
func fsSchemaSpecs() []toolSchemaSpec {
	return []toolSchemaSpec{
		{tool: "read_file", component: "LocalFsReadFile", response: fsReadFileResponse},
		{tool: "write_file", component: "LocalFsWriteFile", response: fsWriteFileResponse},
		{tool: "edit_file", component: "LocalFsEditFile", response: fsEditFileResponse},
		{tool: "list_dir", component: "LocalFsListDir", response: fsListDirResponse},
		{tool: "grep", component: "LocalFsGrep", response: fsGrepResponse},
		{tool: "find_files", component: "LocalFsFindFiles", response: fsFindFilesResponse},
		{tool: "sed", component: "LocalFsSed", response: fsSedResponse},
		{tool: "count_stats", component: "LocalFsCountStats", response: fsCountStatsResponse},
		{tool: "read_file_range", component: "LocalFsReadFileRange", response: fsReadFileRangeResponse},
		{tool: "stat_file", component: "LocalFsStatFile", response: fsStatFileResponse},
	}
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// one request/response pair per declared tool. Requests are converted from the
// descriptors GetToolsForToolsByName produces for THIS ctx, so a document
// published under _verbose_tool_descriptions carries the same descriptions the
// model was given. Responses describe what Exec returns, including the
// refusals and no-match messages it returns as results rather than errors.
func (h *LocalFSTools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	declared, err := h.GetToolsForToolsByName(ctx, h.name)
	if err != nil {
		return nil, err
	}
	doc, err := buildToolsetDoc(h.name, "Local Filesystem Tools",
		"Read, search and modify files inside the workspace directory. Every path is contained to that directory, binaries are refused rather than dumped into the transcript, every result is capped and says what it withheld, and modifying an existing file requires having read its current version first.",
		declared, fsSchemaSpecs())
	if err != nil {
		return nil, err
	}
	return map[string]*openapi3.T{h.name: doc}, nil
}

// --- Response schemas ---------------------------------------------------------
// One per tool, written from what its handler in fs.go returns. Where a
// handler answers a refusal or a no-match with a RESULT rather than an error,
// that shape is declared too: it is part of the contract.

// fsUnchangedSchema is FsUnchangedResult, the stub read_file returns instead of
// re-sending content this session has already seen.
func fsUnchangedSchema() *openapi3.SchemaRef {
	return objectSchema(
		"The dedup stub: this session already read this exact version, so the content was not re-sent. Pass force=true to get it again. Reaches the model as a short stub line.",
		map[string]*openapi3.SchemaRef{
			"path":      strSchema("The file, relative to the project root."),
			"sha256":    strSchema("SHA-256 of the content the stub stands in for."),
			"bytes":     intSchema("Size of that content in bytes."),
			"unchanged": boolSchema("Always true on this shape."),
		}, "path", "sha256", "bytes", "unchanged")
}

func fsReadFileResponse() *openapi3.SchemaRef {
	return oneOfSchema("What read_file returns: the file's text, or the dedup stub.",
		strSchema("The file's text. When the output cap bit, a line-based HEAD followed by a notice naming the exact line to resume from — never a silent cut."),
		fsUnchangedSchema())
}

func fsWriteFileResponse() *openapi3.SchemaRef {
	return oneOfSchema("What write_file returns: the write receipt, or a refusal.",
		objectSchema("FsWriteResult: the write happened. Old and new file bodies are NOT returned.",
			map[string]*openapi3.SchemaRef{
				"path":       strSchema("The file that was written, relative to the project root — the same form every other result and error message uses."),
				"written":    boolSchema("Always true on this shape."),
				"old_bytes":  intSchema("Size of the previous content in bytes; 0 when the file was created."),
				"new_bytes":  intSchema("Size of the written content in bytes."),
				"old_sha256": strSchema("SHA-256 of the previous content."),
				"new_sha256": strSchema("SHA-256 of the written content."),
			}, "path", "written", "old_bytes", "new_bytes", "old_sha256", "new_sha256"),
		refusalSchema())
}

func fsEditFileResponse() *openapi3.SchemaRef {
	return oneOfSchema("What edit_file returns: the edit receipt, a refusal, or a message saying the file was left alone.",
		objectSchema("FsEditResult: the replacement was applied.",
			map[string]*openapi3.SchemaRef{
				"path":         strSchema("The file that was written, relative to the project root — the same form every other result and error message uses."),
				"written":      boolSchema("Always true on this shape."),
				"replacements": intSchema("How many occurrences of old_string were replaced; 1 unless replace_all was set."),
				"old_bytes":    intSchema("Size of the previous content in bytes."),
				"new_bytes":    intSchema("Size of the written content in bytes."),
				"old_sha256":   strSchema("SHA-256 of the previous content."),
				"new_sha256":   strSchema("SHA-256 of the written content."),
			}, "path", "written", "replacements", "old_bytes", "new_bytes", "old_sha256", "new_sha256"),
		refusalSchema(),
		strSchema("The file was left UNCHANGED and why: old_string was not found, or it occurs more than once and replace_all was not set. Returned as a result so the call can be corrected and retried."))
}

func fsSedResponse() *openapi3.SchemaRef {
	return oneOfSchema("What sed returns: the edit receipt, a refusal, or a message saying the file was left alone.",
		objectSchema("FsSedResult: the replacement was applied.",
			map[string]*openapi3.SchemaRef{
				"path":         strSchema("The file that was written, relative to the project root — the same form every other result and error message uses."),
				"written":      boolSchema("Always true on this shape."),
				"changed":      boolSchema("Whether the new content differs from the old — false when the replacement equals what it replaced."),
				"replacements": intSchema("How many occurrences of pattern were replaced within the scoped range."),
				"old_bytes":    intSchema("Size of the previous content in bytes."),
				"new_bytes":    intSchema("Size of the written content in bytes."),
				"old_sha256":   strSchema("SHA-256 of the previous content."),
				"new_sha256":   strSchema("SHA-256 of the written content."),
			}, "path", "written", "changed", "replacements", "old_bytes", "new_bytes", "old_sha256", "new_sha256"),
		refusalSchema(),
		strSchema("The file was left UNCHANGED and why: the pattern was not found (with the closest actual lines quoted so it can be corrected), start_line is past the end of the file, expect_replacements did not match the count found, or the pattern is ambiguous and neither all nor expect_replacements was given."))
}

func fsListDirResponse() *openapi3.SchemaRef {
	return strSchema("The entries, one per line: bare names for a one-level listing, paths relative to the listed directory when recursive. A directory ends with '/', a trailing '*' marks the executable bit, and a file over 1 MiB carries a compact size. Gitignored and high-noise paths are omitted. Empty when nothing matched. When the output cap bit, a trailing notice names the offset to resume from.")
}

func fsGrepResponse() *openapi3.SchemaRef {
	return strSchema("The matching lines: \"N: text\" when path is a file, \"relative/path:N: text\" when it is a directory. Empty when nothing matched. When a cap bit, a trailing notice names how many matches are shown, which lines were searched, which cap stopped it and the policy key that raises it, and the line to continue from.")
}

func fsFindFilesResponse() *openapi3.SchemaRef {
	return objectSchema("The matched paths.", map[string]*openapi3.SchemaRef{
		"matches":   arraySchema("Matching files, relative to the searched directory. Empty when nothing matched.", strSchema("One workspace-relative path.")),
		"count":     intSchema("How many paths are in matches."),
		"truncated": boolSchema("True when the result cap stopped the walk. Absent means the listing is complete."),
		"note":      strSchema("Why the listing is not the whole answer — the cap that bit and what to narrow. Absent when nothing was cut."),
	}, "matches", "count")
}

func fsCountStatsResponse() *openapi3.SchemaRef {
	return strSchema("\"Lines: N, Words: N, Bytes: N\" for the file.")
}

func fsReadFileRangeResponse() *openapi3.SchemaRef {
	return strSchema("The requested lines, joined by newlines. Empty when start_line is past the end of the file. When the output cap bit, a notice naming the exact line to resume from is appended — never a silent cut.")
}

func fsStatFileResponse() *openapi3.SchemaRef {
	return objectSchema("Metadata for the path. At most the first ~512 bytes of content are read, for the binary sniff.",
		map[string]*openapi3.SchemaRef{
			"name":       strSchema("The base name."),
			"size":       intSchema("Size in bytes."),
			"sizeHuman":  strSchema("The same size rendered for a reader, e.g. \"48 MiB\"."),
			"modTime":    strSchema("Last modification time, RFC3339."),
			"isDir":      boolSchema("Whether the path is a directory."),
			"mode":       strSchema("The Go permission string, e.g. \"-rwxr-xr-x\"."),
			"executable": boolSchema("Whether any executable bit is set."),
			"binary":     boolSchema("Best-effort content sniff of the first ~512 bytes. Only sniffed for regular files; always false for a directory or other special file."),
		}, "name", "size", "sizeHuman", "modTime", "isDir", "mode", "executable", "binary")
}

func fsTool(name, desc string, props map[string]any, required ...string) taskengine.Tool {
	params := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		params["required"] = required
	}
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        name,
			Description: desc,
			Parameters:  params,
		},
	}
}

func fsProp(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func (h *LocalFSTools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	v := h.verboseToolDescriptions(ctx)
	doc := func(tool string) string { return fsToolDocs[tool].pick(v) }

	allTools := []taskengine.Tool{
		fsTool("read_file", doc("read_file"), map[string]any{
			"path":       fsProp("string", "Path to the file, relative to the project root"),
			"start_line": fsProp("integer", "Optional 1-based first line. Supply the value from a truncation notice to page forward."),
			"end_line":   fsProp("integer", "Optional 1-based last line, inclusive. Defaults to end of file."),
			"force":      fsProp("boolean", "Re-send full content even if this session already read this version."),
		}, "path"),

		fsTool("write_file", doc("write_file"), map[string]any{
			"path":    fsProp("string", "Path to the file"),
			"content": fsProp("string", "New content for the file"),
		}, "path", "content"),

		fsTool("edit_file", doc("edit_file"), map[string]any{
			"path":        fsProp("string", "Path to the file"),
			"old_string":  fsProp("string", "Exact, whitespace-sensitive text to replace; must be unique in the file unless replace_all is set"),
			"new_string":  fsProp("string", "Replacement text"),
			"replace_all": fsProp("boolean", "Replace every occurrence of old_string instead of requiring exactly one"),
		}, "path", "old_string", "new_string"),

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

		fsTool("sed", doc("sed"), map[string]any{
			"path":                fsProp("string", "Path to the file"),
			"pattern":             fsProp("string", "Literal string to replace"),
			"replacement":         fsProp("string", "Replacement string"),
			"all":                 fsProp("boolean", "Replace every occurrence instead of requiring exactly one"),
			"expect_replacements": fsProp("integer", "Assert this exact number of occurrences; the write is refused on mismatch"),
			"start_line":          fsProp("integer", "Confine the edit to lines from here (1-based)"),
			"end_line":            fsProp("integer", "Confine the edit to lines up to here, inclusive"),
		}, "path", "pattern", "replacement"),

		fsTool("count_stats", doc("count_stats"), map[string]any{
			"path": fsProp("string", "Path to the file"),
		}, "path"),

		fsTool("read_file_range", doc("read_file_range"), map[string]any{
			"path":       fsProp("string", "Path to the file"),
			"start_line": fsProp("integer", "Starting line number (1-based, default 1)"),
			"end_line":   fsProp("integer", "Ending line number, inclusive (optional)"),
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
