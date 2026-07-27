package workspaceindex

// Indexer tests. Every one of them runs against a t.TempDir fixture workspace
// and a DETERMINISTIC FAKE EMBEDDER: no model, no network, no ollama. The store
// is the real SQLite one (runtimetypes.SetupStore), so the schema, the FTS5
// mirror and the dimension checks are all exercised for real.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

const fakeDimension = 32

// fakeEmbedder is a hash-derived bag-of-words embedding: deterministic, offline,
// and — crucially for the ranking tests — SIMILAR for texts sharing vocabulary,
// so a planted near-duplicate really does win on cosine rather than by accident.
type fakeEmbedder struct {
	dim    int
	calls  atomic.Int64
	before func(text string) error
}

func newFakeEmbedder() *fakeEmbedder { return &fakeEmbedder{dim: fakeDimension} }

func (f *fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.before != nil {
		if err := f.before(text); err != nil {
			return nil, err
		}
	}
	f.calls.Add(1)
	vec := make([]float32, f.dim)
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum32())%f.dim]++
	}
	return vec, nil
}

func (f *fakeEmbedder) Calls() int { return int(f.calls.Load()) }

// spyStore counts which retrieval path a query took, so "the prefilter was used"
// is asserted rather than assumed.
type spyStore struct {
	Store
	mu       sync.Mutex
	searches int
	scans    int
}

func (s *spyStore) SearchWorkspaceChunks(ctx context.Context, configID, match string, limit int) ([]*runtimetypes.WorkspaceChunk, error) {
	s.mu.Lock()
	s.searches++
	s.mu.Unlock()
	return s.Store.SearchWorkspaceChunks(ctx, configID, match, limit)
}

func (s *spyStore) ScanWorkspaceChunks(ctx context.Context, configID string, limit int) ([]*runtimetypes.WorkspaceChunk, error) {
	s.mu.Lock()
	s.scans++
	s.mu.Unlock()
	return s.Store.ScanWorkspaceChunks(ctx, configID, limit)
}

func (s *spyStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searches, s.scans
}

type harness struct {
	ctx      context.Context
	root     string
	store    runtimetypes.Store
	spy      *spyStore
	embedder *fakeEmbedder
	svc      Service
	ws       string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	ctx, store := runtimetypes.SetupStore(t)
	spy := &spyStore{Store: store}
	emb := newFakeEmbedder()
	if cfg.EmbedModel == "" {
		cfg.EmbedModel = "fake-embed"
	}
	if cfg.EmbedProvider == "" {
		cfg.EmbedProvider = "fake"
	}
	return &harness{
		ctx:      ctx,
		root:     t.TempDir(),
		store:    store,
		spy:      spy,
		embedder: emb,
		svc:      New(spy, emb, ollamatokenizer.NewEstimateTokenizer(), cfg),
		ws:       "ws-test",
	}
}

func (h *harness) write(t *testing.T, rel, content string) {
	t.Helper()
	writeFile(t, h.root, rel, content)
}

func (h *harness) opts(force bool) BuildOptions {
	return BuildOptions{WorkspaceID: h.ws, Force: force}
}

func (h *harness) build(t *testing.T, force bool) *BuildReport {
	t.Helper()
	rep, err := h.svc.Build(h.ctx, h.root, h.opts(force))
	require.NoError(t, err)
	return rep
}

// fixture is a small workspace with enough shape to exercise selection, chunking
// and ranking.
func (h *harness) fixture(t *testing.T) {
	t.Helper()
	h.write(t, "docs/retry.md", "# Retry\n\nRetry backoff is exponential with jitter.\nThe backoff doubles until the ceiling.\n")
	h.write(t, "docs/widgets.md", "# Widgets\n\nThe widget catalogue lists every widget by taxonomy.\n")
	h.write(t, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	h.write(t, ".gitignore", "secrets.txt\n")
	h.write(t, "secrets.txt", "this must never be indexed\n")
}

func TestUnit_Build_FullBuildIndexesTheSelectedTree(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)

	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.Equal(t, 0, h.embedder.Calls(), "Plan must not spend a single embed call — that is the whole point of a pre-count")
	require.Equal(t, 4, plan.Files, "the gitignored file must not be indexed")
	require.Equal(t, plan.Chunks, plan.EmbedCalls, "a first build embeds every chunk exactly once")
	require.True(t, plan.CutOver, "a workspace with no index creates its first generation")
	require.Positive(t, plan.Chunks)

	rep := h.build(t, false)
	require.Equal(t, plan.EmbedCalls, rep.ChunksWritten, "the pre-count must match what was actually written")
	require.Equal(t, plan.EmbedCalls+1, rep.EmbedCalls, "one extra call is the dimension probe, and it is reported")
	require.Equal(t, rep.EmbedCalls, h.embedder.Calls())

	st, err := h.svc.Status(h.ctx, h.ws)
	require.NoError(t, err)
	require.Equal(t, rep.ConfigID, st.ConfigID)
	require.EqualValues(t, rep.ChunksWritten, st.Chunks)
	require.Equal(t, fakeDimension, st.Dimension, "the dimension is discovered from the model, not declared")
	require.Equal(t, "fake-embed", st.EmbedModel)

	// The gitignored file is absent from the index, not merely unranked.
	files, err := h.store.ListWorkspaceIndexedFiles(h.ctx, rep.ConfigID)
	require.NoError(t, err)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	require.NotContains(t, paths, "secrets.txt")
	require.Contains(t, paths, "docs/retry.md")
}

func TestUnit_Build_IncrementalNoOpEmbedsNothing(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	first := h.build(t, false)

	callsAfterFirst := h.embedder.Calls()
	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.Zero(t, plan.EmbedCalls, "nothing changed, so nothing costs anything")
	require.Equal(t, first.ChunksWritten, plan.ChunksReused)
	require.False(t, plan.CutOver)
	require.Equal(t, first.ConfigID, plan.ConfigID, "an unchanged build extends the SAME generation")

	second := h.build(t, false)
	require.Zero(t, second.ChunksWritten)
	require.Zero(t, second.EmbedCalls)
	require.Equal(t, callsAfterFirst, h.embedder.Calls(), "a no-op re-index must not touch the model at all")

	st, err := h.svc.Status(h.ctx, h.ws)
	require.NoError(t, err)
	require.EqualValues(t, first.ChunksWritten, st.Chunks, "a no-op must not lose chunks either")
}

func TestUnit_Build_IncrementalReembedsOnlyTheEditedFile(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	first := h.build(t, false)
	before := h.embedder.Calls()

	h.write(t, "docs/retry.md", "# Retry\n\nRetry backoff is now linear, not exponential.\nThe delay grows by a constant.\n")

	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.Equal(t, 1, plan.FilesChanged, "only the edited file is work")
	require.Positive(t, plan.EmbedCalls)
	require.Less(t, plan.EmbedCalls, first.ChunksWritten, "an incremental build must cost less than a full one")

	rep := h.build(t, false)
	require.Equal(t, plan.EmbedCalls, rep.ChunksWritten)
	require.Equal(t, plan.EmbedCalls, h.embedder.Calls()-before, "only the changed file's chunks are re-embedded")

	// The old text is gone and the new text is present — a stale chunk left
	// behind would be a lie with a valid-looking sha.
	chunks, err := h.store.ScanWorkspaceChunks(h.ctx, rep.ConfigID, 1000)
	require.NoError(t, err)
	var joined string
	for _, c := range chunks {
		joined += c.Text + "\n"
	}
	require.Contains(t, joined, "linear, not exponential")
	require.NotContains(t, joined, "exponential with jitter")
}

func TestUnit_Build_DeletedFileDropsItsChunks(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	first := h.build(t, false)

	require.NoError(t, os.Remove(filepath.Join(h.root, "docs", "widgets.md")))

	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.Equal(t, 1, plan.FilesDeleted)
	require.Zero(t, plan.EmbedCalls, "a deletion costs no embed calls")

	rep := h.build(t, false)
	files, err := h.store.ListWorkspaceIndexedFiles(h.ctx, rep.ConfigID)
	require.NoError(t, err)
	for _, f := range files {
		require.NotEqual(t, "docs/widgets.md", f.Path, "a deleted file must not stay searchable")
	}
	after, err := h.store.CountWorkspaceChunks(h.ctx, rep.ConfigID)
	require.NoError(t, err)
	require.Less(t, int(after), first.ChunksWritten)
}

func TestUnit_Build_ForceRebuildsEverythingInPlace(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	first := h.build(t, false)
	before := h.embedder.Calls()

	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(true))
	require.NoError(t, err)
	require.Equal(t, plan.Chunks, plan.EmbedCalls, "--force re-embeds the whole tree")
	require.Zero(t, plan.ChunksReused)

	rep := h.build(t, true)
	require.Equal(t, first.ConfigID, rep.ConfigID, "a same-model rebuild reuses the generation; only a model change cuts over")
	require.Equal(t, first.ChunksWritten, rep.ChunksWritten)
	require.Equal(t, first.ChunksWritten, rep.ChunksDeleted, "the old chunks are dropped, not duplicated")
	require.Equal(t, plan.EmbedCalls, h.embedder.Calls()-before)

	count, err := h.store.CountWorkspaceChunks(h.ctx, rep.ConfigID)
	require.NoError(t, err)
	require.EqualValues(t, first.ChunksWritten, count, "a rebuild must not double the index")
}

// Changing the embedding model must NOT extend the existing index — vectors from
// two models in one table is the silent corruption create-once immutability
// exists to prevent. A new generation is created and becomes active.
func TestUnit_Build_ModelChangeCutsOverToANewGeneration(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	first := h.build(t, false)

	cutover := New(h.spy, h.embedder, ollamatokenizer.NewEstimateTokenizer(), Config{
		EmbedModel:    "a-different-embed-model",
		EmbedProvider: "fake",
	})
	plan, err := cutover.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.True(t, plan.CutOver)
	require.Empty(t, plan.ConfigID, "a cutover does not extend the old generation")
	require.Equal(t, plan.Chunks, plan.EmbedCalls, "a cutover re-embeds everything")

	rep, err := cutover.Build(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.NotEqual(t, first.ConfigID, rep.ConfigID)

	st, err := cutover.Status(h.ctx, h.ws)
	require.NoError(t, err)
	require.Equal(t, rep.ConfigID, st.ConfigID, "the new generation is active")
	require.Equal(t, "a-different-embed-model", st.EmbedModel)

	// The old generation's chunks are untouched: a cutover is additive, so a
	// rollback is possible until they are explicitly reaped.
	oldCount, err := h.store.CountWorkspaceChunks(h.ctx, first.ConfigID)
	require.NoError(t, err)
	require.EqualValues(t, first.ChunksWritten, oldCount)
}

func TestUnit_Build_CancellationMidBuildStopsAndLeavesNoPartialFile(t *testing.T) {
	h := newHarness(t, Config{})
	// Enough files that cancellation lands mid-build rather than at the end.
	for i := range 40 {
		h.write(t, fmt.Sprintf("docs/f%02d.md", i), strings.Repeat("some indexable prose about retry backoff\n", 30))
	}

	ctx, cancel := context.WithCancel(h.ctx)
	var seen atomic.Int64
	h.embedder.before = func(string) error {
		if seen.Add(1) == 10 {
			cancel()
		}
		return nil
	}

	_, err := h.svc.Build(ctx, h.root, BuildOptions{WorkspaceID: h.ws})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "a cancelled build reports cancellation, not a mystery failure")

	// Whatever survived, every indexed file must be WHOLE: a file recorded with
	// a sha that matches disk but missing chunks would be skipped by every later
	// incremental build, silently and permanently.
	cfg, err := h.store.GetActiveWorkspaceIndexConfig(h.ctx, h.ws)
	require.NoError(t, err)
	indexed, err := h.store.ListWorkspaceIndexedFiles(h.ctx, cfg.ID)
	require.NoError(t, err)

	h.embedder.before = nil
	for _, f := range indexed {
		content, err := os.ReadFile(filepath.Join(cfg.Root, filepath.FromSlash(f.Path)))
		require.NoError(t, err)
		want, err := chunkFile(h.ctx, ollamatokenizer.NewEstimateTokenizer(), "fake-embed",
			sourceFile{RelPath: f.Path, Content: string(content), SHA: fileSHA(content)},
			cfg.ChunkTokens, cfg.ChunkOverlap)
		require.NoError(t, err)
		require.Equal(t, len(want), f.Chunks, "file %s is half-indexed under a matching sha", f.Path)
	}

	// And the build is resumable: a later run finishes the job.
	rep := h.build(t, false)
	require.Positive(t, rep.ChunksWritten)
	plan, err := h.svc.Plan(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	require.Zero(t, plan.EmbedCalls, "after the resumed build, the index is complete")
}

func TestUnit_Build_UnusableEmbeddingModelFailsBeforeSpending(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)
	h.embedder.before = func(string) error { return errors.New("model does not support embeddings") }

	_, err := h.svc.Build(h.ctx, h.root, h.opts(false))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unusable", "the probe must name the model, not fail deep inside a loop")
	require.Equal(t, 0, h.embedder.Calls(), "no chunk was embedded: the probe failed first")

	_, err = h.svc.Status(h.ctx, h.ws)
	require.ErrorIs(t, err, ErrNoIndex, "a failed build must not leave an empty index behind")
}

func TestUnit_Build_RequiresAWorkspaceID(t *testing.T) {
	h := newHarness(t, Config{})
	_, err := h.svc.Build(h.ctx, h.root, BuildOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "WorkspaceID")
}

func TestUnit_Build_ReportsProgressWithTotalsUpFront(t *testing.T) {
	h := newHarness(t, Config{})
	h.fixture(t)

	var phases []Phase
	var planned *BuildPlan
	rep, err := h.svc.Build(h.ctx, h.root, BuildOptions{
		WorkspaceID: h.ws,
		Progress: func(p Progress) {
			phases = append(phases, p.Phase)
			if p.Phase == PhasePlanning {
				planned = p.Plan
			}
		},
	})
	require.NoError(t, err)
	require.Equal(t, PhasePlanning, phases[0], "the cost is reported before the first embed call")
	require.NotNil(t, planned)
	require.Equal(t, rep.ChunksWritten, planned.EmbedCalls)
	require.Equal(t, PhaseDone, phases[len(phases)-1])
	require.Contains(t, phases, PhaseEmbedding)
}
