package jqtool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainFixture is a miniature contenox chain file, queried by every semantics assertion below.
const chainFixture = `{
  "id": "acp-session",
  "description": "the coding chain",
  "tasks": [
    {"id": "plan",    "handler": "model",  "retries": 2},
    {"id": "read",    "handler": "tools",  "retries": 0},
    {"id": "edit",    "handler": "tools",  "retries": 1},
    {"id": "respond", "handler": "model",  "retries": 0}
  ],
  "token_limit": 9007199254740993
}`

const yamlFixture = `id: acp-session
tasks:
  - id: plan
    handler: model
  - id: read
    handler: tools
enabled: true
`

// --- helpers -----------------------------------------------------------------

func newTools(t *testing.T, dir string) taskengine.ToolsRepo {
	t.Helper()
	return jqtool.NewTools(dir)
}

func exec(t *testing.T, tools taskengine.ToolsRepo, args map[string]any) (*jqtool.Result, error) {
	t.Helper()
	res, dt, err := tools.Exec(context.Background(), time.Now(), args, false,
		&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: jqtool.ToolQuery})
	if err != nil {
		return nil, err
	}
	require.Equal(t, taskengine.DataTypeJSON, dt,
		"jq_query must return DataTypeJSON so the engine serialises the declared schema and nothing else")
	out, ok := res.(*jqtool.Result)
	require.Truef(t, ok, "jq_query returned %T, not *jqtool.Result", res)
	return out, nil
}

func mustExec(t *testing.T, tools taskengine.ToolsRepo, args map[string]any) *jqtool.Result {
	t.Helper()
	res, err := exec(t, tools, args)
	require.NoError(t, err)
	return res
}

// values renders the emitted values as the compact-JSON lines a reader sees.
func values(res *jqtool.Result) []string {
	out := make([]string, 0, len(res.Values))
	for _, v := range res.Values {
		out = append(out, string(v))
	}
	return out
}

func writeFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// --- jq semantics ------------------------------------------------------------

func TestUnit_JQTool_SemanticsOverAChainFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)
	tools := newTools(t, dir)

	cases := []struct {
		name   string
		filter string
		want   []string
	}{
		{
			name:   "select",
			filter: `.tasks[] | select(.handler=="tools") | .id`,
			want:   []string{`"read"`, `"edit"`},
		},
		{
			name:   "project",
			filter: `.tasks[] | {id, handler}`,
			// Pinned: gojq.Marshal sorts object keys, so output order is stable.
			want: []string{
				`{"handler":"model","id":"plan"}`,
				`{"handler":"tools","id":"read"}`,
				`{"handler":"tools","id":"edit"}`,
				`{"handler":"model","id":"respond"}`,
			},
		},
		{
			name:   "map",
			filter: `[.tasks[] | .retries] | add`,
			want:   []string{`3`},
		},
		{
			name:   "keys",
			filter: `keys`,
			want:   []string{`["description","id","tasks","token_limit"]`},
		},
		{
			name:   "length",
			filter: `.tasks | length`,
			want:   []string{`4`},
		},
		{
			name:   "group_by",
			filter: `[.tasks[] | .handler] | group_by(.) | map({(.[0]): length}) | add`,
			want:   []string{`{"model":2,"tools":2}`},
		},
		{
			// Pinned: gojq is fed json.Number, so a 64-bit id survives the round trip.
			name:   "big integers keep their precision",
			filter: `.token_limit`,
			want:   []string{`9007199254740993`},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := mustExec(t, tools, map[string]any{"path": "chain.json", "filter": tc.filter})
			assert.Equal(t, tc.want, values(res))
			assert.Equal(t, len(tc.want), res.Count)
			assert.False(t, res.Truncated)
			assert.Equal(t, "json", res.Format)
			assert.Equal(t, "chain.json", res.Source, "the source must be the workspace-relative path, never the host absolute one")
		})
	}
}

func TestUnit_JQTool_BothInputSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)
	tools := newTools(t, dir)

	fromPath := mustExec(t, tools, map[string]any{"path": "chain.json", "filter": `[.tasks[].id]`})
	fromInline := mustExec(t, tools, map[string]any{"input": chainFixture, "filter": `[.tasks[].id]`})

	assert.Equal(t, values(fromPath), values(fromInline), "the same document must answer the same whichever source carried it")
	assert.Equal(t, `["plan","read","edit","respond"]`, values(fromPath)[0])
	assert.Contains(t, fromInline.Source, "inline")

	// An already-decoded object rather than a string is accepted too.
	asValue := mustExec(t, tools, map[string]any{
		"input":  map[string]any{"tasks": []any{map[string]any{"id": "plan"}}},
		"filter": `[.tasks[].id]`,
	})
	assert.Equal(t, []string{`["plan"]`}, values(asValue))
}

func TestUnit_JQTool_YAMLInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.yaml", yamlFixture)
	writeFixture(t, dir, "noext", yamlFixture)
	tools := newTools(t, dir)

	t.Run("by extension", func(t *testing.T) {
		res := mustExec(t, tools, map[string]any{"path": "chain.yaml", "filter": `[.tasks[].handler]`})
		assert.Equal(t, []string{`["model","tools"]`}, values(res))
		assert.Equal(t, "yaml", res.Format)
	})

	t.Run("sniffed with no extension", func(t *testing.T) {
		res := mustExec(t, tools, map[string]any{"path": "noext", "filter": `.enabled`})
		assert.Equal(t, []string{`true`}, values(res))
		assert.Equal(t, "yaml", res.Format, "the result must state the parser that was actually used")
	})

	t.Run("inline yaml", func(t *testing.T) {
		res := mustExec(t, tools, map[string]any{"input": yamlFixture, "filter": `.id`})
		assert.Equal(t, []string{`"acp-session"`}, values(res))
		assert.Equal(t, "yaml", res.Format)
	})

	t.Run("explicit format wins over the extension", func(t *testing.T) {
		writeFixture(t, dir, "mislabelled.json", yamlFixture)
		_, err := exec(t, tools, map[string]any{"path": "mislabelled.json", "filter": `.id`})
		require.Error(t, err, "a .json file that is not JSON must be reported, not silently reparsed")
		assert.Contains(t, err.Error(), "not valid json")

		res := mustExec(t, tools, map[string]any{"path": "mislabelled.json", "filter": `.id`, "format": "yaml"})
		assert.Equal(t, []string{`"acp-session"`}, values(res))
	})

	t.Run("multi-document yaml runs the filter once per document", func(t *testing.T) {
		writeFixture(t, dir, "multi.yaml", "kind: Service\n---\nkind: Deployment\n")
		res := mustExec(t, tools, map[string]any{"path": "multi.yaml", "filter": `.kind`})
		assert.Equal(t, []string{`"Service"`, `"Deployment"`}, values(res))
		assert.Equal(t, 2, res.Documents)
	})

	t.Run("yaml shapes json does not have are normalized", func(t *testing.T) {
		writeFixture(t, dir, "shapes.yaml", "1: one\ntrue: yes-key\nwhen: 2026-07-27T10:00:00Z\n")
		res := mustExec(t, tools, map[string]any{"path": "shapes.yaml", "filter": `keys`})
		assert.Equal(t, []string{`["1","true","when"]`}, values(res))

		res = mustExec(t, tools, map[string]any{"path": "shapes.yaml", "filter": `.when`})
		assert.Equal(t, []string{`"2026-07-27T10:00:00Z"`}, values(res),
			"a YAML timestamp must arrive as an RFC3339 string the jq date builtins accept")
	})
}

func TestUnit_JQTool_JSONLinesIsAStream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "events.jsonl", "{\"lvl\":\"info\"}\n{\"lvl\":\"error\"}\n{\"lvl\":\"info\"}\n")
	tools := newTools(t, dir)

	res := mustExec(t, tools, map[string]any{"path": "events.jsonl", "filter": `select(.lvl=="error") | .lvl`})
	assert.Equal(t, []string{`"error"`}, values(res))
	assert.Equal(t, 3, res.Documents)
}

func TestUnit_JQTool_MutuallyExclusiveInputSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)
	tools := newTools(t, dir)

	_, err := exec(t, tools, map[string]any{"path": "chain.json", "input": chainFixture, "filter": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EITHER path OR input",
		"both sources at once must be refused, never silently resolved to one of them")

	_, err = exec(t, tools, map[string]any{"filter": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input source is required")
	assert.Contains(t, err.Error(), "recoverable")
}

func TestUnit_JQTool_EmptyResultIsAnAnswer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)
	tools := newTools(t, dir)

	res := mustExec(t, tools, map[string]any{"path": "chain.json", "filter": `.tasks[] | select(.handler=="nope")`})
	require.NoError(t, nil)
	assert.Equal(t, 0, res.Count)
	assert.Empty(t, values(res))
	assert.False(t, res.Truncated, "an empty result is not a truncated one")
	assert.Contains(t, res.Note, "matched nothing")

	// The zero-value Values field must marshal as [] and not null: a consumer
	// iterating the result should not have to special-case a missing array.
	blob, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"values":[]`)
}

// --- the capability boundary --------------------------------------------------

// TestUnit_JQTool_NoAmbientCapabilities pins that no filter can reach outside the document it was handed.
func TestUnit_JQTool_NoAmbientCapabilities(t *testing.T) {
	// No t.Parallel: t.Setenv requires the secret to be really present in this process's environment.
	t.Setenv("JQTOOL_TEST_SECRET", "super-secret-api-key")
	tools := newTools(t, t.TempDir())

	t.Run("env is empty", func(t *testing.T) {
		for _, filter := range []string{`$ENV`, `env`} {
			res := mustExec(t, tools, map[string]any{"input": `{}`, "filter": filter})
			require.Equal(t, []string{`{}`}, values(res),
				"%s must be an empty object: a filter must never be able to read this process's environment", filter)
			assert.NotContains(t, strings.Join(values(res), ""), "super-secret")
		}
	})

	t.Run("input/inputs are refused at compile time", func(t *testing.T) {
		for _, filter := range []string{`input`, `[inputs]`} {
			_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": filter})
			require.Errorf(t, err, "%s must not compile", filter)
			assert.Contains(t, err.Error(), "does not compile")
		}
	})

	t.Run("modules cannot be loaded", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": `include "/etc/passwd"; .`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compile")
	})
}

// --- the bounds matrix --------------------------------------------------------

// TestUnit_JQTool_DeadlineBoundsNonTerminatingFilters pins that a non-terminating filter is stopped by the clock, not eventually.
func TestUnit_JQTool_DeadlineBoundsNonTerminatingFilters(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())

	cases := []struct {
		name   string
		filter string
	}{
		{"infinite recursion", `def f: f; f`},
		{"compute bomb", `[range(100000000)] | length`},
		{"unbounded repeat", `[repeat(.)] | length`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const budget = 300 * time.Millisecond
			start := time.Now()
			_, err := exec(t, tools, map[string]any{
				"input":       `{"a":1}`,
				"filter":      tc.filter,
				"deadline_ms": 300,
			})
			elapsed := time.Since(start)

			require.Error(t, err, "%s must be stopped, not answered", tc.filter)
			assert.Contains(t, err.Error(), "did not finish within its",
				"the deadline refusal has its own teaching shape, distinct from a type error")
			assert.Contains(t, err.Error(), "recurses with no base case",
				"the message must name the two ways a filter fails to terminate")
			assert.GreaterOrEqual(t, elapsed, budget/2,
				"stopping far too early would mean the deadline is not what stopped it")
			// Generous upper bound: this asserts bounded, not precise.
			assert.Less(t, elapsed, 10*time.Second,
				"the %v deadline must bound %s; it took %v", budget, tc.name, elapsed)
		})
	}
}

func TestUnit_JQTool_DeadlineIsClampedNotRefused(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())

	// A huge value clamps to the ceiling; a nonsense one takes the default.
	res := mustExec(t, tools, map[string]any{"input": `{"a":1}`, "filter": `.a`, "deadline_ms": 600000})
	assert.Equal(t, []string{`1`}, values(res))

	res = mustExec(t, tools, map[string]any{"input": `{"a":1}`, "filter": `.a`, "deadline_ms": -5})
	assert.Equal(t, []string{`1`}, values(res))
}

func TestUnit_JQTool_OversizeFileIsRefusedFromTheStat(t *testing.T) {
	t.Parallel()
	if testing.Short() && runtime.GOOS == "windows" {
		t.Skip("large temp file on windows CI")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	f, err := os.Create(path)
	require.NoError(t, err)
	// Sparse: the refusal reads the stat size, so 8 MB need not be written.
	require.NoError(t, f.Truncate(jqtool.MaxInputBytes+1))
	require.NoError(t, f.Close())

	_, err = exec(t, newTools(t, dir), map[string]any{"path": "huge.json", "filter": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input cap")
	assert.Contains(t, err.Error(), "recoverable")
}

func TestUnit_JQTool_OversizeInlineInputIsRefused(t *testing.T) {
	t.Parallel()
	huge := `"` + strings.Repeat("x", jqtool.MaxInputBytes) + `"`
	_, err := exec(t, newTools(t, t.TempDir()), map[string]any{"input": huge, "filter": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inline input is")
	assert.Contains(t, err.Error(), "pass `path` instead")
}

func TestUnit_JQTool_UnboundedEmissionIsCappedWithAMarker(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())

	// range(100000) emits far more values than the default cap.
	res := mustExec(t, tools, map[string]any{"input": `null`, "filter": `range(100000)`})
	assert.Equal(t, 200, res.Count, "the default results cap must hold")
	assert.True(t, res.Truncated)
	assert.Contains(t, res.Note, "TRUNCATED")
	assert.Contains(t, res.Note, "value cap")

	// `max` raises it, and is itself clamped to the ceiling rather than refused.
	res = mustExec(t, tools, map[string]any{"input": `null`, "filter": `range(100000)`, "max": 10})
	assert.Equal(t, 10, res.Count)
	assert.True(t, res.Truncated)

	res = mustExec(t, tools, map[string]any{"input": `null`, "filter": `range(100000)`, "max": 999999})
	assert.Equal(t, 5000, res.Count, "max must clamp to the ceiling, not be refused")
	assert.True(t, res.Truncated)
}

func TestUnit_JQTool_OutputCapTruncatesWithAMarker(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())

	// ~1 KB per value: the byte cap bites well before the 200-value count cap.
	res := mustExec(t, tools, map[string]any{
		"input":  `null`,
		"filter": `range(100) | ("x" * 1000)`,
	})
	assert.True(t, res.Truncated)
	assert.Contains(t, res.Note, "byte output cap")
	assert.Less(t, res.Count, 200, "the byte cap, not the count cap, must be what stopped this")
	assert.Greater(t, res.Count, 0)

	total := 0
	for _, v := range res.Values {
		total += len(v)
	}
	assert.LessOrEqual(t, total, jqtool.MaxOutputBytes)
}

func TestUnit_JQTool_OneEnormousValueIsRefusedWithAdvice(t *testing.T) {
	t.Parallel()
	_, err := exec(t, newTools(t, t.TempDir()), map[string]any{
		"input":  `null`,
		"filter": `"x" * 100000`,
	})
	require.Error(t, err, "a single value over the output cap must be refused, not returned as an invalid JSON prefix")
	assert.Contains(t, err.Error(), "output cap")
	assert.Contains(t, err.Error(), "| keys", "the refusal must name the narrowing filters that would work")
}

// --- containment --------------------------------------------------------------

func TestUnit_JQTool_ContainmentRefusesEscapes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(inner, 0o755))
	writeFixture(t, inner, "chain.json", chainFixture)
	// A secret OUTSIDE the workspace, reachable only by escaping it.
	outside := filepath.Join(root, "secrets.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"token":"leaked"}`), 0o644))

	tools := newTools(t, inner)

	// The in-bounds control: containment is a boundary, not a lockout.
	res := mustExec(t, tools, map[string]any{"path": "chain.json", "filter": `.id`})
	assert.Equal(t, []string{`"acp-session"`}, values(res))

	escapes := map[string]string{
		"traversal":          "../secrets.json",
		"nested traversal":   "a/b/../../../secrets.json",
		"absolute elsewhere": outside,
	}
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(outside, filepath.Join(inner, "link.json")))
		escapes["symlink out of the workspace"] = "link.json"
		require.NoError(t, os.Symlink(root, filepath.Join(inner, "up")))
		escapes["symlinked directory"] = "up/secrets.json"
	}
	if runtime.GOOS != "windows" {
		escapes["absolute /etc/passwd"] = "/etc/passwd"
	}

	for name, path := range escapes {
		name, path := name, path
		t.Run(name, func(t *testing.T) {
			_, err := exec(t, tools, map[string]any{"path": path, "filter": "."})
			require.Errorf(t, err, "%s must be refused", path)
			assert.Truef(t, isEscape(err), "%s must be refused with the containment sentinel, got: %v", path, err)
			assert.NotContains(t, err.Error(), "leaked", "a containment refusal must never carry the contents it refused")
		})
	}
}

func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "escapes allowed directory")
}

func TestUnit_JQTool_NoWorkspaceRootRefusesPathButNotInline(t *testing.T) {
	t.Parallel()
	tools := jqtool.NewTools("")

	_, err := exec(t, tools, map[string]any{"path": "chain.json", "filter": "."})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no workspace root",
		"the refusal must be FATAL and name how a root is supplied; no path spelling can fix it")
	assert.Contains(t, err.Error(), "--local-exec-allowed-dir")
	assert.Contains(t, err.Error(), "`input`", "and it must name the source that still works")

	// Inline still answers: the tool is degraded, not dead.
	res := mustExec(t, tools, map[string]any{"input": `{"a":1}`, "filter": `.a`})
	assert.Equal(t, []string{`1`}, values(res))
}

func TestUnit_JQTool_PolicyAllowedDirOverridesTheConstructor(t *testing.T) {
	t.Parallel()
	ctor := t.TempDir()
	policy := t.TempDir()
	writeFixture(t, ctor, "chain.json", `{"who":"constructor"}`)
	writeFixture(t, policy, "chain.json", `{"who":"policy"}`)

	tools := jqtool.NewTools(ctor)
	ctx := taskengine.WithToolsArgs(context.Background(), jqtool.ToolsProviderName,
		map[string]string{"_allowed_dir": policy})
	res, _, err := tools.Exec(ctx, time.Now(), map[string]any{"path": "chain.json", "filter": ".who"}, false,
		&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: jqtool.ToolQuery})
	require.NoError(t, err)
	assert.Equal(t, []string{`"policy"`}, values(res.(*jqtool.Result)),
		"tools_policies._allowed_dir must scope this tool exactly as it scopes local_fs")
}

// --- hostile arguments ---------------------------------------------------------

func TestUnit_JQTool_HostileArgs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)
	tools := newTools(t, dir)

	t.Run("a 10KB filter is refused before parsing", func(t *testing.T) {
		filter := "." + strings.Repeat(" | .", 2500) // ~10 KB
		require.Greater(t, len(filter), 10_000)
		_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": filter})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "over the")
		assert.Contains(t, err.Error(), "byte cap")
		assert.Less(t, len(err.Error()), 1000, "the refusal must not echo the whole filter back")
	})

	t.Run("an empty filter is refused with an example", func(t *testing.T) {
		for _, filter := range []any{"", "   ", nil} {
			_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": filter})
			require.Errorf(t, err, "filter %v", filter)
			assert.Contains(t, err.Error(), "filter is required")
		}
	})

	t.Run("NUL and bidi in a path are escaped in the echo", func(t *testing.T) {
		for _, path := range []string{"chain\x00.json", "chain\u202e.json", "\u0000\u202d\u202e"} {
			_, err := exec(t, tools, map[string]any{"path": path, "filter": "."})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "\x00", "a NUL must be escaped, never embedded in a tool result")
			assert.NotContains(t, err.Error(), "\u202e", "a bidi override must be escaped, never embedded")
		}
	})

	// gojq's lexer stops at a NUL, so an unrefused one would silently truncate the filter.
	t.Run("a NUL in the filter is refused, not silently truncated", func(t *testing.T) {
		res, err := exec(t, tools, map[string]any{"input": `{"a":1}`, "filter": ".\x00[garbage"})
		require.Error(t, err, "a NUL-truncated filter must be refused; got result %+v", res)
		assert.Contains(t, err.Error(), "non-printing character")
		assert.NotContains(t, err.Error(), "\x00")
	})

	t.Run("a bidi override in the filter is refused", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": ".\u202e["})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "\u202e")
	})

	t.Run("a multi-line filter is ordinary", func(t *testing.T) {
		res := mustExec(t, tools, map[string]any{
			"input":  chainFixture,
			"filter": ".tasks[]\n  | select(.handler==\"tools\")\n  | .id",
		})
		assert.Equal(t, []string{`"read"`, `"edit"`}, values(res))
	})

	t.Run("a long path is clamped in the echo", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"path": strings.Repeat("z", 10_000), "filter": "."})
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 1000, "a 10 KB path must not come back as a 10 KB error")
	})

	t.Run("NaN and 1e30 for max take the documented path, never int overflow", func(t *testing.T) {
		for _, max := range []any{
			1e30, -1e30, "not-a-number", true,
			json.Number("1e30"), json.Number("NaN"),
		} {
			res, err := exec(t, tools, map[string]any{"input": `null`, "filter": `range(100000)`, "max": max})
			require.NoErrorf(t, err, "max=%v must clamp, not fail", max)
			assert.LessOrEqualf(t, res.Count, 5000, "max=%v must never exceed the ceiling", max)
			assert.Greaterf(t, res.Count, 0, "max=%v must never produce a negative or zero cap", max)
		}
	})

	t.Run("NaN for deadline_ms takes the default", func(t *testing.T) {
		res := mustExec(t, tools, map[string]any{"input": `{"a":1}`, "filter": `.a`, "deadline_ms": 1e30})
		assert.Equal(t, []string{`1`}, values(res))
	})

	t.Run("unknown arguments are refused by name", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": ".", "fillter": "x", "recursive": true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown argument(s)")
		assert.Contains(t, err.Error(), "fillter")
		assert.Contains(t, err.Error(), "recursive")
		assert.Contains(t, err.Error(), "allowed: deadline_ms, filter, format, input, max, path")
	})

	t.Run("an unknown argument NAME is clamped and sanitised", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{
			"filter":                           ".",
			"input":                            `{}`,
			strings.Repeat("q", 5000) + "\x00": "x",
		})
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 1000)
		assert.NotContains(t, err.Error(), "\x00")
	})

	t.Run("an unknown tool names what this toolset provides", func(t *testing.T) {
		_, _, err := tools.Exec(context.Background(), time.Now(), map[string]any{}, false,
			&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: "jq_write\x00"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), jqtool.ToolQuery)
		assert.NotContains(t, err.Error(), "\x00")
	})

	t.Run("an unknown format is refused with the two that work", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": `{}`, "filter": ".", "format": "toml"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown format")
		assert.Contains(t, err.Error(), "\"json\"")
	})
}

// --- malformed documents --------------------------------------------------------

func TestUnit_JQTool_MalformedDocuments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tools := newTools(t, dir)

	t.Run("malformed json", func(t *testing.T) {
		writeFixture(t, dir, "broken.json", `{"tasks": [ {"id": "plan"`)
		_, err := exec(t, tools, map[string]any{"path": "broken.json", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid json")
		assert.Contains(t, err.Error(), "problem with the DOCUMENT",
			"a bad document must not read like a bad filter — they have different fixes")
	})

	t.Run("malformed yaml", func(t *testing.T) {
		writeFixture(t, dir, "broken.yaml", "a:\n  - x\n b: [unclosed\n")
		_, err := exec(t, tools, map[string]any{"path": "broken.yaml", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid yaml")
	})

	t.Run("an empty file is a document problem, not a silent null", func(t *testing.T) {
		writeFixture(t, dir, "empty.json", "")
		_, err := exec(t, tools, map[string]any{"path": "empty.json", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("an unparseable inline string names both parsers it tried", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": "{[}", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "json")
		assert.Contains(t, err.Error(), "yaml")
	})

	t.Run("a missing file is refused by workspace-relative name", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"path": "nope.json", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nope.json")
		assert.Contains(t, err.Error(), "does not exist")
		assert.NotContains(t, err.Error(), dir, "errors address the model in workspace-relative paths, not host absolute ones")
	})

	t.Run("a directory is refused", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir"), 0o755))
		_, err := exec(t, tools, map[string]any{"path": "subdir", "filter": "."})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a directory")
	})
}

// --- the three error shapes ------------------------------------------------------

// TestUnit_JQTool_ThreeErrorShapes pins that parse, compile and runtime failures each name a different fix.
func TestUnit_JQTool_ThreeErrorShapes(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())

	t.Run("parse: the text is not jq", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": chainFixture, "filter": `.rules[ | select(`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid jq syntax")
		assert.Contains(t, err.Error(), "Fix the PROGRAM")
	})

	t.Run("compile: it is jq but names something absent", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": chainFixture, "filter": `.tasks | mapp(.id)`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not compile")
		assert.Contains(t, err.Error(), "SYNTAX is fine")
	})

	t.Run("compile: an unbound variable", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": chainFixture, "filter": `.tasks[] | select(.id == $wanted)`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not compile")
	})

	t.Run("runtime: the program and the document disagree", func(t *testing.T) {
		_, err := exec(t, tools, map[string]any{"input": chainFixture, "filter": `.tasks.id`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed on this document")
		assert.Contains(t, err.Error(), "disagree about shape")
		assert.Contains(t, err.Error(), "keys")
	})

	t.Run("every failure carries a severity marker", func(t *testing.T) {
		for _, filter := range []string{`.rules[ |`, `mapp(.)`, `.tasks.id`} {
			_, err := exec(t, tools, map[string]any{"input": chainFixture, "filter": filter})
			require.Error(t, err)
			assert.Containsf(t, err.Error(), "recoverable", "filter %q must be marked recoverable-by-correction", filter)
		}
	})
}

// --- the ToolsRepo contract -------------------------------------------------------

func TestUnit_JQTool_ToolsRepoContract(t *testing.T) {
	t.Parallel()
	tools := newTools(t, t.TempDir())
	ctx := context.Background()

	supported, err := tools.Supports(ctx)
	require.NoError(t, err)
	assert.Contains(t, supported, jqtool.ToolsProviderName)
	assert.Contains(t, supported, jqtool.ToolQuery)

	schemas, err := tools.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	assert.Empty(t, schemas, "jq is a local toolset: the model-facing contract is GetToolsForToolsByName")

	declared, err := tools.GetToolsForToolsByName(ctx, jqtool.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, declared, 1)
	fn := declared[0].Function
	assert.Equal(t, jqtool.ToolQuery, fn.Name)

	assert.Contains(t, fn.Description, "NEVER writes")
	assert.Contains(t, fn.Description, "goja_eval")
	assert.Contains(t, fn.Description, "not a pipe")

	schema, ok := fn.Parameters.(map[string]any)
	require.True(t, ok)
	params, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	for _, arg := range []string{"filter", "path", "input", "format", "max", "deadline_ms"} {
		assert.Containsf(t, params, arg, "the schema must declare %s", arg)
	}
	assert.Equal(t, []string{"filter"}, schema["required"])

	byName, err := tools.GetToolsForToolsByName(ctx, jqtool.ToolQuery)
	require.NoError(t, err)
	require.Len(t, byName, 1)

	_, err = tools.GetToolsForToolsByName(ctx, "jq_write")
	require.Error(t, err)
}

func TestUnit_JQTool_DeclarativeArgsOnTheCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", chainFixture)

	// Arguments carried on the ToolsCall rather than in the chain input.
	res, _, err := jqtool.NewTools(dir).Exec(context.Background(), time.Now(), "some chat history", false,
		&taskengine.ToolsCall{
			Name:     jqtool.ToolsProviderName,
			ToolName: jqtool.ToolQuery,
			Args:     map[string]string{"path": "chain.json", "filter": ".id"},
		})
	require.NoError(t, err)
	assert.Equal(t, []string{`"acp-session"`}, values(res.(*jqtool.Result)))
}

func TestUnit_JQTool_NilToolsCallIsRefused(t *testing.T) {
	t.Parallel()
	_, _, err := jqtool.NewTools(t.TempDir()).Exec(context.Background(), time.Now(), map[string]any{}, false, nil)
	require.Error(t, err)
}

// TestUnit_JQTool_DocumentStreamCapIsNeverSilent pins the document-count truncation marker (the third cap, after count and bytes).
func TestUnit_JQTool_DocumentStreamCapIsNeverSilent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 1200; i++ {
		b.WriteString("{\"n\":1}\n")
	}
	writeFixture(t, dir, "many.jsonl", b.String())
	tools := newTools(t, dir)

	res := mustExec(t, tools, map[string]any{"path": "many.jsonl", "filter": `.n`, "max": 5000})
	assert.Equal(t, 1000, res.Documents, "the document stream must stop at the documented cap")
	assert.Contains(t, res.Note, "TRUNCATED")
	assert.Contains(t, res.Note, "documents")

	// Exactly at the cap is a COMPLETE answer and must not claim truncation.
	var exact strings.Builder
	for i := 0; i < 1000; i++ {
		exact.WriteString("{\"n\":1}\n")
	}
	writeFixture(t, dir, "exact.jsonl", exact.String())
	res = mustExec(t, tools, map[string]any{"path": "exact.jsonl", "filter": `.n`, "max": 5000})
	assert.Equal(t, 1000, res.Documents)
	assert.NotContains(t, res.Note, "documents", "an input of exactly the cap is complete, not truncated")
}
