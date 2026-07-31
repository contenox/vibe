package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/services/gointel"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/services/workspaceindex"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// ─── the rig ────────────────────────────────────────────────────────────────

// fakeEmbedder is a deterministic bag-of-words embedder: no model, no
// network, no provider. The counter is mutex-guarded because the code under
// test embeds concurrently (workspaceindex.embedBatch's bounded pool), so an
// unsynchronised int here would be a data race in every test that builds an
// index.
type fakeEmbedder struct {
	mu     sync.Mutex
	nCalls int
	// fail, when non-nil, makes every call fail: the unusable-embedding-model path.
	fail error
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	f.nCalls++
	f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	vec := make([]float32, 16)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(word))
		vec[h.Sum32()%16] += 1
	}
	vec[0] += 0.01 // never an all-zero vector, which scores 0 against everything
	return vec, nil
}

// calls reports how many embeds were made, under the same lock the writer takes.
func (f *fakeEmbedder) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nCalls
}

// newIndexTestDeps builds the two verbs' dependencies over a real SQLite
// file and a fake embedder, so the flow under test is production end to end
// minus the provider.
func newIndexTestDeps(t *testing.T) (*indexDeps, *fakeEmbedder) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "index.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	emb := &fakeEmbedder{}
	return &indexDeps{
		WorkspaceID: "ws-index-test",
		Model:       "fake-embed",
		Provider:    "testprovider",
		Svc: workspaceindex.New(
			runtimetypes.New(db.WithoutTransaction()),
			emb,
			ollamatokenizer.NewEstimateTokenizer(),
			workspaceindex.Config{EmbedModel: "fake-embed", EmbedProvider: "testprovider"},
		),
	}, emb
}

// newIndexTestTree writes a small workspace worth indexing.
func newIndexTestTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"README.md":         "# Project\n\nThis project retries failed calls.\n",
		"docs/retry.md":     "# Retry backoff\n\nThe retry backoff doubles after every failure, capped at sixty seconds.\nOperators tune it with the backoff config key.\n",
		"config/app.yaml":   "backoff: exponential\nceiling_seconds: 60\n",
		"internal/serve.go": "package internal\n\n// Serve starts the listener.\nfunc Serve() {}\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return dir
}

// indexTestCmd is testCobraCmd plus captured streams.
func indexTestCmd(t *testing.T, stdin string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := testCobraCmd()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, out, errOut
}

// ─── registration ───────────────────────────────────────────────────────────

func TestUnit_IndexAndSearchCommandsAreReserved(t *testing.T) {
	// Reserved so `contenox index` dispatches as a subcommand instead of being
	// injected as chat input. Kept in lockstep with scripts/verify_cli_help.sh.
	require.True(t, reservedSubcommands["index"], `"index" must be reserved`)
	require.True(t, reservedSubcommands["search"], `"search" must be reserved`)
}

func TestUnit_LocalToolset_RegistersWorkspaceSearchAlwaysOn(t *testing.T) {
	goIndex := gointel.NewIndex(gointel.Config{AllowedDir: t.TempDir()})
	t.Cleanup(goIndex.Shutdown)
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	// Registered with the shell OFF, like gointel and git: it is a read.
	tools := localToolset(chatOpts{EffectiveEnableLocalExec: false}, nil, libtracker.NoopTracker{}, goIndex, gt)
	repo, ok := tools[searchtool.ToolsProviderName]
	require.True(t, ok, "workspace_search is a read surface and must always be registered")

	supported, err := repo.Supports(context.Background())
	require.NoError(t, err)
	require.Contains(t, supported, searchtool.ToolSearch)

	// Unbound (no engine yet, or no embedding model resolved): must degrade to
	// the runnable instruction, never panic or answer as if empty.
	out, _, err := repo.Exec(context.Background(), time.Now(),
		map[string]any{"question": "where is retry backoff"}, false,
		&taskengine.ToolsCall{Name: searchtool.ToolSearch})
	require.NoError(t, err, "an unbound querier must not be an error")
	require.Contains(t, mustJSON(t, out), "contenox index")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// TestUnit_SeededPolicies_AllowWorkspaceSearch asserts both shipped policy presets allow workspace_search, while the no-file fallback does not and asks instead.
func TestUnit_SeededPolicies_AllowWorkspaceSearch(t *testing.T) {
	ctx := context.Background()
	args := map[string]any{"question": "where is retry backoff", "top_k": 5}

	fresh := t.TempDir()
	require.NoError(t, writeEmbeddedHITLPolicies(fresh, false))
	for _, name := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(fresh), testTenant, nopKV{}, libtracker.NoopTracker{}, name)
		r, err := svc.Evaluate(ctx, searchtool.ToolsProviderName, searchtool.ToolSearch, args)
		require.NoError(t, err)
		require.Equal(t, hitlservice.ActionAllow, r.Action, "%s must allow workspace_search: it is a read", name)
	}

	// No policy file anywhere: fail-closed, everything asks, including this read.
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err := svc.Evaluate(ctx, searchtool.ToolsProviderName, searchtool.ToolSearch, args)
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionApprove, r.Action, "the no-file fallback must ask — allow tiers live only in seeded, readable policy files")
}

// ─── directory resolution ───────────────────────────────────────────────────

func TestUnit_ResolveWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveWorkspaceDir(dir)
	require.NoError(t, err)
	require.Equal(t, dir, got)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	got, err = resolveWorkspaceDir("")
	require.NoError(t, err)
	require.Equal(t, cwd, got)

	// A path that names a FILE is refused in beam's exact words, so a typo is
	// answered the same way by every verb that takes a workspace directory.
	file := filepath.Join(dir, "notadir.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err = resolveWorkspaceDir(file)
	require.ErrorContains(t, err, "is not a directory")

	_, err = resolveWorkspaceDir(filepath.Join(dir, "missing"))
	require.ErrorContains(t, err, "is not a directory")
}

func TestUnit_ContenoxDirForWorkspace(t *testing.T) {
	// A real workspace marker is found by walking UP from the given directory,
	// not from the process's cwd — that is what lets `index ~/x` and `search
	// --dir ~/x` agree on which index they mean.
	root := t.TempDir()
	marker := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(marker, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(marker, "workspace.id"), []byte("{}"), 0o644))
	nested := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	got, err := contenoxDirForWorkspace(nil, nested)
	require.NoError(t, err)
	require.Equal(t, marker, got)

	// No marker anywhere above: the directory's own .contenox is the answer.
	bare := t.TempDir()
	got, err = contenoxDirForWorkspace(nil, bare)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bare, ".contenox"), got)

	// --data-dir still wins, as everywhere else in the CLI.
	cmd := testCobraCmd()
	explicit := t.TempDir()
	require.NoError(t, cmd.Root().PersistentFlags().Set("data-dir", explicit))
	got, err = contenoxDirForWorkspace(cmd, nested)
	require.NoError(t, err)
	require.Equal(t, explicit, got)
}

// ─── rendering ──────────────────────────────────────────────────────────────

func TestUnit_Commafy(t *testing.T) {
	for in, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 7065: "7,065", 1270: "1,270", 1234567: "1,234,567", -4321: "-4,321"} {
		require.Equal(t, want, commafy(in))
	}
}

func TestUnit_RenderIndexPlan_PricesTheBuildBeforeItRuns(t *testing.T) {
	var buf bytes.Buffer
	renderIndexPlan(&buf, &workspaceindex.BuildPlan{
		WorkspaceID: "ws-1", Root: "/src/project",
		Files: 1270, Chunks: 7065, EmbedCalls: 7065, CutOver: true,
		SkippedBinary: 12, SkippedOversize: 3,
	}, "nomic-embed-text", "ollama")
	out := buf.String()

	// The one line the whole confirmation rests on.
	require.Contains(t, out, "1,270 files → 7,065 chunks → 7,065 embed calls against nomic-embed-text · ollama")
	require.Contains(t, out, "ws-1")
	require.Contains(t, out, "/src/project")
	// "N files indexed" must never be mistaken for "the whole tree".
	require.Contains(t, out, "Skipped 12 binary, 3 oversized and 0 generated file(s).")
	require.Contains(t, out, "new index generation")

	// Incremental: chunks and embed calls DIFFER, and both are shown.
	buf.Reset()
	renderIndexPlan(&buf, &workspaceindex.BuildPlan{
		WorkspaceID: "ws-1", Root: "/src/project", ConfigID: "cfg-1",
		Files: 1270, Chunks: 7065, EmbedCalls: 42, ChunksReused: 7023, FilesDeleted: 4,
	}, "nomic-embed-text", "ollama")
	out = buf.String()
	require.Contains(t, out, "7,065 chunks → 42 embed calls")
	require.Contains(t, out, "Reusing 7,023 chunk(s)")
	require.Contains(t, out, "Dropping the chunks of 4 file(s)")
	require.NotContains(t, out, "new index generation")
}

func TestUnit_RenderProgressLine(t *testing.T) {
	line := renderProgressLine(workspaceindex.Progress{
		Phase: workspaceindex.PhaseEmbedding,
		Path:  "internal/services/workspaceindex/workspaceindex.go",
		Files: 12, FilesTotal: 1270, Chunks: 340, ChunksTotal: 7065,
	})
	require.Contains(t, line, "12/1,270 files")
	require.Contains(t, line, "340/7,065 chunks")
	require.NotContains(t, line, "\n", "the status line is rewritten in place; a newline would spam scrollback")
	require.LessOrEqual(t, len([]rune(line)), 120)

	// Planning and done say nothing here: the cost line and the report already
	// cover them on stdout.
	require.Empty(t, renderProgressLine(workspaceindex.Progress{Phase: workspaceindex.PhasePlanning}))
	require.Empty(t, renderProgressLine(workspaceindex.Progress{Phase: workspaceindex.PhaseDone}))
}

func TestUnit_TruncateMiddle_KeepsBothEnds(t *testing.T) {
	got := truncateMiddle("internal/services/workspaceindex/workspaceindex.go", 20)
	require.Len(t, []rune(got), 20)
	require.True(t, strings.HasPrefix(got, "interna"), got)
	require.True(t, strings.HasSuffix(got, ".go"), got)
	require.Equal(t, "short.go", truncateMiddle("short.go", 20))
}

func TestUnit_RenderSearchHits(t *testing.T) {
	t.Run("empty says so honestly", func(t *testing.T) {
		var buf bytes.Buffer
		renderSearchHits(&buf, "where is the flux capacitor", nil)
		out := buf.String()
		require.Contains(t, out, `No match for "where is the flux capacitor"`)
		require.Contains(t, out, "contenox index")
		require.Contains(t, out, "snapshot")
	})

	t.Run("citation, score and capped snippet", func(t *testing.T) {
		var buf bytes.Buffer
		renderSearchHits(&buf, "backoff", []workspaceindex.Hit{{
			Path: "docs/retry.md", StartLine: 10, EndLine: 24, Score: 0.8123,
			Text: strings.Join([]string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8"}, "\n"),
		}})
		out := buf.String()
		require.Contains(t, out, "docs/retry.md:10-24  0.812")
		require.Contains(t, out, "    l1\n")
		require.Contains(t, out, "    l6\n")
		require.NotContains(t, out, "    l7\n")
		require.Contains(t, out, "+2 line(s) not shown")
	})

	t.Run("stale hits are labelled where they are read", func(t *testing.T) {
		var buf bytes.Buffer
		renderSearchHits(&buf, "backoff", []workspaceindex.Hit{
			{Path: "a.md", StartLine: 1, EndLine: 2, Score: 0.9, Text: "current"},
			{Path: "b.md", StartLine: 3, EndLine: 4, Score: 0.7, Text: "moved", Stale: true},
		})
		out := buf.String()
		require.Contains(t, out, "b.md:3-4  0.700  [STALE: file changed since indexing]")
		require.NotContains(t, out, "a.md:1-2  0.900  [STALE")
		require.Contains(t, out, "1 of 2 hit(s) are stale")
	})
}

func TestUnit_ConfirmSpend(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {"  YES \n", true}, {"n\n", false}, {"\n", false}, {"maybe\n", false}} {
		cmd, out, _ := indexTestCmd(t, tc.in)
		got, err := confirmSpend(cmd, "Make 10 embedding call(s)?")
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "input %q", tc.in)
		require.Contains(t, out.String(), "[y/N]")
	}

	// A closed stdin is NOT a yes. An accidental pipe must never start thousands
	// of billed calls.
	cmd, _, _ := indexTestCmd(t, "")
	_, err := confirmSpend(cmd, "spend?")
	require.ErrorContains(t, err, "--yes")
}

// ─── the flow, against a real store ─────────────────────────────────────────

func TestUnit_IndexCmd_PlanThenBuildWithYes(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	tree := newIndexTestTree(t)
	cmd, out, _ := indexTestCmd(t, "")

	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))
	text := out.String()

	// The price is printed BEFORE anything is spent, and it names the model.
	require.Contains(t, text, "embed calls against fake-embed · testprovider")
	require.Contains(t, text, "4 files →")
	require.Contains(t, text, "Indexed in")
	require.Contains(t, text, "chunk(s) written")
	require.Contains(t, text, `contenox search "your question"`)
	require.NotContains(t, text, "[y/N]", "--yes must skip the confirmation, not answer it")
	require.Greater(t, emb.calls(), 4, "every chunk plus the dimension probe is one embed call")

	// A second run is incremental: nothing changed, so nothing is embedded.
	before := emb.calls()
	cmd2, out2, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd2, deps, tree, false, true))
	require.Contains(t, out2.String(), "Already current")
	require.Equal(t, before, emb.calls(), "an unchanged tree must cost nothing")

	// And the index it built actually answers.
	cmd3, out3, _ := indexTestCmd(t, "")
	require.NoError(t, runSearchWith(ctx, cmd3, deps, "retry backoff", 3, false))
	require.Contains(t, out3.String(), "docs/retry.md:")
}

func TestUnit_IndexCmd_RefusedConfirmationSpendsNothing(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	tree := newIndexTestTree(t)

	cmd, out, _ := indexTestCmd(t, "n\n")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, false))
	text := out.String()
	require.Contains(t, text, "[y/N]")
	require.Contains(t, text, "Cancelled; nothing was embedded.")
	require.Zero(t, emb.calls(), "declining must not make a single embedding call")

	// Nothing was written, so the workspace still has no index at all.
	err := runSearchWith(ctx, cmd, deps, "retry backoff", 0, false)
	require.ErrorContains(t, err, "no index for this workspace")
}

func TestUnit_IndexCmd_WarnsWhenTheChatModelIsStandingInAsTheEmbedder(t *testing.T) {
	ctx := context.Background()
	deps, _ := newIndexTestDeps(t)
	deps.EmbedFallback = true
	cmd, _, errOut := indexTestCmd(t, "")

	require.NoError(t, runIndexWith(ctx, cmd, deps, newIndexTestTree(t), false, true))
	require.Contains(t, errOut.String(), "default-embed-model")
	require.Contains(t, errOut.String(), "most chat models cannot")
}

func TestUnit_SearchCmd_NoIndexIsARunnableInstruction(t *testing.T) {
	ctx := context.Background()
	deps, _ := newIndexTestDeps(t)
	cmd, _, _ := indexTestCmd(t, "")

	err := runSearchWith(ctx, cmd, deps, "anything at all", 0, false)
	require.EqualError(t, err, "no index for this workspace — run: contenox index")

	// An empty question is answered with the example, not a stack of jargon.
	err = runSearchWith(ctx, cmd, deps, "   ", 0, false)
	require.ErrorContains(t, err, "contenox search")
}

func TestUnit_SearchCmd_JSONShape(t *testing.T) {
	ctx := context.Background()
	deps, _ := newIndexTestDeps(t)
	tree := newIndexTestTree(t)
	cmd, _, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))

	cmd2, out, _ := indexTestCmd(t, "")
	require.NoError(t, runSearchWith(ctx, cmd2, deps, "retry backoff", 3, true))

	var hits []workspaceindex.Hit
	require.NoError(t, json.Unmarshal(out.Bytes(), &hits), "output: %s", out.String())
	require.NotEmpty(t, hits)
	require.LessOrEqual(t, len(hits), 3, "--top must bound the JSON too")
	for _, h := range hits {
		require.NotEmpty(t, h.Path)
		require.Positive(t, h.StartLine)
		require.GreaterOrEqual(t, h.EndLine, h.StartLine)
		require.NotEmpty(t, h.Text)
	}
	// The raw field names are the scripting contract.
	require.Contains(t, out.String(), `"Path"`)
	require.Contains(t, out.String(), `"StartLine"`)

	// An empty result is [], never null: a script that iterates must not have to
	// special-case it.
	cmd3, emptyOut, _ := indexTestCmd(t, "")
	require.NoError(t, runSearchWith(ctx, cmd3, deps, "zzzzz", 3, true))
	var empty []workspaceindex.Hit
	require.NoError(t, json.Unmarshal(emptyOut.Bytes(), &empty))
	require.NotContains(t, emptyOut.String(), "null")
}
