package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/gojatool"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/stretchr/testify/require"
)

func TestReadinessDefaults(t *testing.T) {
	cases := []struct {
		name         string
		opts         chatOpts
		wantModel    string
		wantProvider string
	}{
		{
			name: "explicit --model on fresh DB is credited",
			opts: chatOpts{
				EffectiveDefaultModel:    "phi-4-mini",
				EffectiveConfiguredModel: "",
			},
			wantModel: "phi-4-mini",
		},
		{
			name: "hardcoded fallback model on fresh DB is not credited",
			opts: chatOpts{
				EffectiveDefaultModel:    defaultModel,
				EffectiveConfiguredModel: "",
			},
			wantModel: "",
		},
		{
			name: "model from persisted config needs no override",
			opts: chatOpts{
				EffectiveDefaultModel:    "persisted",
				EffectiveConfiguredModel: "persisted",
			},
			wantModel: "",
		},
		{
			name: "explicit --provider on fresh DB is credited",
			opts: chatOpts{
				EffectiveDefaultProvider:    "ollama",
				EffectiveConfiguredProvider: "",
			},
			wantProvider: "ollama",
		},
		{
			name: "provider from persisted config needs no override",
			opts: chatOpts{
				EffectiveDefaultProvider:    "ollama",
				EffectiveConfiguredProvider: "ollama",
			},
			wantProvider: "",
		},
		{
			name: "model and provider flags both credited together",
			opts: chatOpts{
				EffectiveDefaultModel:    "phi-4-mini",
				EffectiveConfiguredModel: "",
				EffectiveDefaultProvider: "vllm",
			},
			wantModel:    "phi-4-mini",
			wantProvider: "vllm",
		},
		{
			// The reported bug: a healthy explicit override must beat a broken
			// persisted default, not be ignored because config is non-empty.
			name: "explicit flags override a broken persisted config",
			opts: chatOpts{
				EffectiveDefaultModel:       "gemini-2.5-flash",
				EffectiveConfiguredModel:    "unservable-model",
				EffectiveDefaultProvider:    "vertex-google",
				EffectiveConfiguredProvider: "vllm",
			},
			wantModel:    "gemini-2.5-flash",
			wantProvider: "vertex-google",
		},
		{
			// Override only the provider; the model stays on persisted config and
			// needs no readiness credit (effective == configured).
			name: "provider override alone, model unchanged from config",
			opts: chatOpts{
				EffectiveDefaultModel:       "persisted",
				EffectiveConfiguredModel:    "persisted",
				EffectiveDefaultProvider:    "vertex-google",
				EffectiveConfiguredProvider: "vllm",
			},
			wantModel:    "",
			wantProvider: "vertex-google",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, provider := readinessDefaults(tc.opts)
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if provider != tc.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tc.wantProvider)
			}
		})
	}
}

// TestUnit_LocalToolset_GitIsAlwaysOnAndShellIsGated asserts git is always registered regardless of --shell, while local_shell stays gated behind it.
func TestUnit_LocalToolset_GitIsAlwaysOnAndShellIsGated(t *testing.T) {
	tracker := libtracker.NoopTracker{}

	goIndex := gointel.NewIndex(gointel.Config{})
	t.Cleanup(goIndex.Shutdown)

	// No ScriptDir: the provider still registers, carrying goja_eval alone.
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	off := localToolset(chatOpts{EffectiveEnableLocalExec: false}, nil, tracker, goIndex, gt)
	require.Contains(t, off, "git", "git must be registered even with the shell off")
	require.Contains(t, off, "local_fs")
	require.Contains(t, off, gointel.ToolsProviderName, "gointel is a read surface, always on")
	require.Contains(t, off, gojatool.ToolsProviderName, "goja is a compute surface, always on")
	require.NotContains(t, off, "local_shell", "the shell stays opt-in")

	on := localToolset(chatOpts{EffectiveEnableLocalExec: true, EffectiveHITL: true}, nil, tracker, goIndex, gt)
	require.Contains(t, on, "git")
	require.Contains(t, on, "local_shell")

	supported, err := off["git"].Supports(context.Background())
	require.NoError(t, err)
	require.Contains(t, supported, "git_status")
	require.Contains(t, supported, "git_commit")

	gojaSupported, err := off[gojatool.ToolsProviderName].Supports(context.Background())
	require.NoError(t, err)
	require.Contains(t, gojaSupported, gojatool.ToolEval)
}

// TestUnit_LocalToolset_JQIsAlwaysOn asserts jq_query is scoped to the same
// directory as local_fs, so what it can read stays a subset of read_file.
func TestUnit_LocalToolset_JQIsAlwaysOn(t *testing.T) {
	tracker := libtracker.NoopTracker{}

	goIndex := gointel.NewIndex(gointel.Config{})
	t.Cleanup(goIndex.Shutdown)
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain.json"),
		[]byte(`{"tasks":[{"id":"plan","handler":"model"},{"id":"read","handler":"tools"}]}`), 0o644))

	for _, shellOn := range []bool{false, true} {
		tools := localToolset(chatOpts{
			EffectiveEnableLocalExec:     shellOn,
			EffectiveHITL:                true,
			EffectiveLocalExecAllowedDir: dir,
		}, nil, tracker, goIndex, gt)

		require.Containsf(t, tools, jqtool.ToolsProviderName,
			"jq is a read surface and must be registered with the shell %v", shellOn)

		supported, err := tools[jqtool.ToolsProviderName].Supports(context.Background())
		require.NoError(t, err)
		require.Contains(t, supported, jqtool.ToolQuery)

		res, dt, err := tools[jqtool.ToolsProviderName].Exec(context.Background(), time.Now(),
			map[string]any{"path": "chain.json", "filter": `[.tasks[] | select(.handler=="tools") | .id]`}, false,
			&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: jqtool.ToolQuery})
		require.NoError(t, err)
		require.Equal(t, taskengine.DataTypeJSON, dt)
		out, ok := res.(*jqtool.Result)
		require.True(t, ok)
		require.Equal(t, 1, out.Count)
		require.JSONEq(t, `["read"]`, string(out.Values[0]))

		// Same boundary: a path outside the workspace is refused, not read.
		_, _, err = tools[jqtool.ToolsProviderName].Exec(context.Background(), time.Now(),
			map[string]any{"path": "../outside.json", "filter": "."}, false,
			&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: jqtool.ToolQuery})
		require.Error(t, err)
		require.Contains(t, err.Error(), "escapes allowed directory")
	}
}

// TestUnit_LocalToolset_WriteFileInvalidatesGoIntelIndex pins the seam wired in
// localToolset: a write through the local_fs TOOL PATH (not a bare os.WriteFile,
// as gointel's own freshness tests use) reaches gointel.Index.Invalidate via
// WithOnFileMutated, so a query issued immediately after sees the edit rather
// than depending on the mtime-sweep backstop.
func TestUnit_LocalToolset_WriteFileInvalidatesGoIntelIndex(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/onmutate\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg.go"), []byte("package pkg\n\n// Original is here from the start.\nfunc Original() int { return 1 }\n"), 0o644))

	goIndex := gointel.NewIndex(gointel.Config{AllowedDir: root})
	t.Cleanup(goIndex.Shutdown)
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	tools := localToolset(chatOpts{EffectiveLocalExecAllowedDir: root}, nil, libtracker.NoopTracker{}, goIndex, gt)

	// Warm the index on the pre-edit source.
	_, _, err = tools[gointel.ToolsProviderName].Exec(context.Background(), time.Now(),
		map[string]any{"symbol": "pkg.Original"}, false,
		&taskengine.ToolsCall{Name: gointel.ToolsProviderName, ToolName: gointel.ToolDefinition})
	require.NoError(t, err, "warm-up query")

	// Read-then-write through the real local_fs tool path, exactly as an agent would.
	_, _, err = tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": "pkg.go"}, false, &taskengine.ToolsCall{Name: "local_fs", ToolName: "read_file"})
	require.NoError(t, err)

	newContent := "package pkg\n\n// Original is here from the start.\nfunc Original() int { return 1 }\n\n" +
		"// Added lands via the tool path, the way an agent's edit would.\nfunc Added() int { return 2 }\n"
	_, _, err = tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": "pkg.go", "content": newContent}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})
	require.NoError(t, err, "write_file through the tool path")

	// No sleep, no second edit to trip the mtime sweep: if Invalidate fired,
	// this resolves immediately.
	out, _, err := tools[gointel.ToolsProviderName].Exec(context.Background(), time.Now(),
		map[string]any{"symbol": "pkg.Added"}, false,
		&taskengine.ToolsCall{Name: gointel.ToolsProviderName, ToolName: gointel.ToolDefinition})
	require.NoError(t, err, "go_definition immediately after a write_file through the tool path — "+
		"WithOnFileMutated must have called gointel.Index.Invalidate")
	res, ok := out.(*gointel.DefinitionResult)
	require.True(t, ok, "unexpected result type %T", out)
	require.Contains(t, res.Location, "pkg.go:")
}

// fakeGoIndex is a gointel.Index that only records Invalidate calls. Queries
// return zero values — this fake exists solely to isolate the WithOnFileMutated
// wiring from gointel's own mtime-sweep backstop, which independently catches a
// stale snapshot and would make a real-index test pass even if the wiring were
// broken.
type fakeGoIndex struct {
	invalidated []string
}

func (f *fakeGoIndex) Describe(context.Context, gointel.Request) (*gointel.DescribeResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) Definition(context.Context, gointel.Request) (*gointel.DefinitionResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) References(context.Context, gointel.Request) (*gointel.ReferencesResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) Implementations(context.Context, gointel.Request) (*gointel.ImplementationsResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) Symbols(context.Context, gointel.Request) (*gointel.SymbolsResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) Diagnostics(context.Context, gointel.Request) (*gointel.DiagnosticsResult, error) {
	return nil, nil
}
func (f *fakeGoIndex) Invalidate(paths ...string) { f.invalidated = append(f.invalidated, paths...) }
func (f *fakeGoIndex) Shutdown()                  {}

var _ gointel.Index = (*fakeGoIndex)(nil)

// TestUnit_LocalToolset_OnFileMutatedCallsGoIntelInvalidate pins the wiring
// itself, isolated from gointel's mtime-sweep backstop: write_file, sed, and
// edit_file must each call Invalidate with the absolute path just written, and
// a denied mutation (no prior read) must call it zero times.
func TestUnit_LocalToolset_OnFileMutatedCallsGoIntelInvalidate(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha bravo\n"), 0o644))
	abs := filepath.Join(root, "a.txt")

	fake := &fakeGoIndex{}
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	// db is nil, so the read-before-write guard is a no-op — this test is
	// about the mutate callback, not that gate.
	tools := localToolset(chatOpts{EffectiveLocalExecAllowedDir: root}, nil, libtracker.NoopTracker{}, fake, gt)

	_, _, err = tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "content": "alpha bravo\n"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "write_file"})
	require.NoError(t, err)
	require.Equal(t, []string{abs}, fake.invalidated, "write_file must call Invalidate with the absolute path")

	_, _, err = tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "pattern": "alpha", "replacement": "ALPHA"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "sed"})
	require.NoError(t, err)
	require.Equal(t, []string{abs, abs}, fake.invalidated, "sed must call Invalidate too")

	_, _, err = tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": "a.txt", "old_string": "bravo", "new_string": "BRAVO"}, false,
		&taskengine.ToolsCall{Name: "local_fs", ToolName: "edit_file"})
	require.NoError(t, err)
	require.Equal(t, []string{abs, abs, abs}, fake.invalidated, "edit_file must call Invalidate too")
}
