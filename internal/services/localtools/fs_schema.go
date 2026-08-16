package localtools

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

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
			"An audio file (wav, mp3, m4a, ogg, flac — detected from its bytes, not its extension) is consumed by the tool and answered with a transcript from the configured audio model (config key: default-audio-model), prefixed with a one-line notice; without a configured audio model, or over the _max_audio_bytes cap, audio is refused with the key or cap named. " +
			"Refuses with an error for other files that sniff as binary (a NUL byte or a high fraction of invalid UTF-8 in the first ~512 bytes) instead of dumping raw bytes into your context — use shell tools for binaries. " +
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
	"read_file_range": {
		terse: "Read a line range from a file (1-based, end_line optional). Works on files of any size. Satisfies the read prerequisite for sed but not for write_file.",
		verbose: "Read a contiguous range of lines from a file (1-based, inclusive end_line optional). Works on files of any size — streamed rather than loaded when over the read cap. " +
			"If the range's output exceeds the output cap it is truncated to a head with a notice naming the exact resume line, never silently. " +
			"This satisfies the read-before-mutate prerequisite for targeted sed edits on the same current file version, but not for write_file full-file overwrites — call read_file (full) before write_file on an existing file. " +
			"Missing paths return a 'Did you mean:' suggestion.",
	},
}

func (h *LocalFSTools) Supports(ctx context.Context) ([]string, error) {
	return []string{
		h.name,
		"read_file", "write_file", "edit_file", "sed", "read_file_range",
	}, nil
}

func fsSchemaSpecs() []toolSchemaSpec {
	return []toolSchemaSpec{
		{tool: "read_file", component: "LocalFsReadFile", response: fsReadFileResponse},
		{tool: "write_file", component: "LocalFsWriteFile", response: fsWriteFileResponse},
		{tool: "edit_file", component: "LocalFsEditFile", response: fsEditFileResponse},
		{tool: "sed", component: "LocalFsSed", response: fsSedResponse},
		{tool: "read_file_range", component: "LocalFsReadFileRange", response: fsReadFileRangeResponse},
	}
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// one request/response pair per declared tool.
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
	return oneOfSchema("What read_file returns: the file's text, an audio transcript, or the dedup stub.",
		strSchema("The file's text. When the output cap bit, a line-based HEAD followed by a notice naming the exact line to resume from — never a silent cut. For a supported audio file (wav, mp3, m4a, ogg, flac), a transcript from the configured audio model instead, prefixed with a one-line notice naming the file, its type, and its size."),
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

		fsTool("sed", doc("sed"), map[string]any{
			"path":                fsProp("string", "Path to the file"),
			"pattern":             fsProp("string", "Literal string to replace"),
			"replacement":         fsProp("string", "Replacement string"),
			"all":                 fsProp("boolean", "Replace every occurrence instead of requiring exactly one"),
			"expect_replacements": fsProp("integer", "Assert this exact number of occurrences; the write is refused on mismatch"),
			"start_line":          fsProp("integer", "Confine the edit to lines from here (1-based)"),
			"end_line":            fsProp("integer", "Confine the edit to lines up to here, inclusive"),
		}, "path", "pattern", "replacement"),

		fsTool("read_file_range", doc("read_file_range"), map[string]any{
			"path":       fsProp("string", "Path to the file"),
			"start_line": fsProp("integer", "Starting line number (1-based, default 1)"),
			"end_line":   fsProp("integer", "Ending line number, inclusive (optional)"),
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
