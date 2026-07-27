package workspaceindex

// Search tests. Same offline harness as the indexer tests: a real SQLite store
// (so FTS5 and the dimension checks are exercised) and a deterministic
// hash-derived fake embedder — no model, no network.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// searchFixture plants a near-duplicate of the question in one file and
// plausible-but-different prose everywhere else.
func searchFixture(t *testing.T, h *harness) {
	t.Helper()
	h.write(t, "docs/retry.md", "Retry backoff is explained here: the delay doubles on every attempt until a ceiling.\n")
	h.write(t, "docs/widgets.md", "The widget catalogue enumerates every widget by taxonomy and colour.\n")
	h.write(t, "docs/deploy.md", "Deployment rolls out one region at a time and waits for health checks.\n")
	h.write(t, "src/server.go", "package server\n\nfunc Serve() error { return nil }\n")
	h.build(t, false)
}

func TestUnit_Query_RanksThePlantedNearDuplicateFirst(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	hits, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "docs/retry.md", hits[0].Path, "the chunk that nearly restates the question must win")
	require.Positive(t, hits[0].Score)
	require.Contains(t, hits[0].Text, "Retry backoff is explained here")
	require.False(t, hits[0].Stale, "an untouched file is not stale")

	// A hit is a CITATION: the line range must name real lines of the file.
	require.GreaterOrEqual(t, hits[0].StartLine, 1)
	require.GreaterOrEqual(t, hits[0].EndLine, hits[0].StartLine)

	// Scores descend.
	for i := 1; i < len(hits); i++ {
		require.GreaterOrEqual(t, hits[i-1].Score, hits[i].Score, "hits must be returned in rank order")
	}
}

// The lexical prefilter is the primary path: when the question shares terms with
// the corpus, FTS5 narrows and the bounded fallback scan is never reached.
func TestUnit_Query_UsesTheLexicalPrefilter(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	before, beforeScans := h.spy.counts()
	hits, err := h.svc.Query(h.ctx, h.ws, "retry backoff ceiling", 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits)

	searches, scans := h.spy.counts()
	require.Equal(t, before+1, searches, "the FTS5 prefilter must run")
	require.Equal(t, beforeScans, scans, "a matching prefilter must not fall back to a scan")
}

// A question sharing no term with the corpus is exactly what semantic search is
// for, so an empty prefilter falls back to a BOUNDED scan rather than returning
// nothing.
func TestUnit_Query_FallsBackWhenThePrefilterMatchesNothing(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	_, beforeScans := h.spy.counts()
	hits, err := h.svc.Query(h.ctx, h.ws, "zzqqxx unrelatedtoken", 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "an unmatched prefilter must still rank semantically, not return nothing")

	_, scans := h.spy.counts()
	require.Equal(t, beforeScans+1, scans, "the bounded fallback scan must be the path taken")
}

// A hit whose file changed underneath is a lie. It is still returned — the text
// may be what the caller wants — but never as if it were current.
func TestUnit_Query_MarksStaleHitsAfterAnEdit(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	fresh, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.False(t, fresh[0].Stale)

	// Edit the file WITHOUT re-indexing.
	h.write(t, "docs/retry.md", "Retry backoff is explained here: the delay doubles on every attempt until a ceiling.\nA new line changes the file's sha.\n")

	stale, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.Equal(t, "docs/retry.md", stale[0].Path)
	require.True(t, stale[0].Stale, "a hit whose file moved must be marked, never silently returned")

	// Re-indexing clears it.
	h.build(t, false)
	current, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.False(t, current[0].Stale)
}

func TestUnit_Query_DeletedFileIsStaleNotAbsent(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)
	require.NoError(t, os.Remove(filepath.Join(h.root, "docs", "retry.md")))

	hits, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.Equal(t, "docs/retry.md", hits[0].Path)
	require.True(t, hits[0].Stale, "a hit whose file is gone must be marked stale, not reported as current")
}

// No index for the workspace degrades to a typed error the caller renders as
// "run contenox index" — never a hard failure, because retrieval is optional.
func TestUnit_Query_EmptyIndexDegradesToErrNoIndex(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.svc.Query(h.ctx, h.ws, "anything at all", 5)
	require.ErrorIs(t, err, ErrNoIndex)
	require.Contains(t, err.Error(), "contenox index", "the error must tell the operator what to run")

	_, err = h.svc.Status(h.ctx, h.ws)
	require.ErrorIs(t, err, ErrNoIndex)

	// An index that exists but holds nothing is a different, quieter case: no
	// hits, no error.
	h.write(t, "empty.md", "\n")
	_, err = h.svc.Build(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	hits, err := h.svc.Query(h.ctx, h.ws, "anything at all", 5)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestUnit_Query_TopKBounds(t *testing.T) {
	h := newHarness(t, Config{})
	for i := range 60 {
		h.write(t, fmt.Sprintf("docs/f%02d.md", i), fmt.Sprintf("Document %d explains retry backoff behaviour in its own words.\n", i))
	}
	h.build(t, false)

	hits, err := h.svc.Query(h.ctx, h.ws, "retry backoff", 3)
	require.NoError(t, err)
	require.Len(t, hits, 3)

	hits, err = h.svc.Query(h.ctx, h.ws, "retry backoff", 0)
	require.NoError(t, err)
	require.Len(t, hits, topKDefault, "topK <= 0 means the default, not zero results")

	hits, err = h.svc.Query(h.ctx, h.ws, "retry backoff", -5)
	require.NoError(t, err)
	require.Len(t, hits, topKDefault)

	hits, err = h.svc.Query(h.ctx, h.ws, "retry backoff", 10_000)
	require.NoError(t, err)
	require.Len(t, hits, topKMax, "an absurd topK is clamped, not rejected")
}

func TestUnit_Query_EmptyQuestionIsRefused(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	for _, q := range []string{"", "   ", "\n\t"} {
		_, err := h.svc.Query(h.ctx, h.ws, q, 5)
		require.ErrorIs(t, err, ErrEmptyQuestion, "embedding whitespace would rank arbitrary chunks confidently")
	}
}

// Querying an index whose vectors came from a different-width model must fail
// loudly. Scoring a 16-dimension question against 32-dimension chunks would
// silently return nonsense.
func TestUnit_Query_DimensionMismatchIsLoud(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	narrow := &fakeEmbedder{dim: 16}
	other := New(h.spy, narrow, ollamatokenizer.NewEstimateTokenizer(), Config{
		EmbedModel:    "fake-embed",
		EmbedProvider: "fake",
	})
	_, err := other.Query(h.ctx, h.ws, "retry backoff", 3)
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch)
	require.Contains(t, err.Error(), "--force", "the error must name the fix")
}

func TestUnit_FTSMatchQuery_QuotesEveryTermAndDropsNoise(t *testing.T) {
	require.Equal(t, `"retry" OR "backoff"`, ftsMatchQuery("retry backoff"))
	require.Equal(t, `"retry" OR "backoff"`, ftsMatchQuery("Retry, backoff!"), "terms are folded and deduplicated")
	require.Equal(t, `"retry"`, ftsMatchQuery("retry retry a b"), "single-character terms are dropped")

	// Nothing a user types may be interpreted as FTS5 syntax.
	got := ftsMatchQuery(`what does NOT * "quoted" mean`)
	require.NotContains(t, strings.ReplaceAll(got, `"`, ""), "*")
	require.Contains(t, got, `"not"`)
	require.Contains(t, got, `"quoted"`)
	require.Empty(t, ftsMatchQuery("!!! ??"), "a question with no usable term yields no expression")
}

func TestUnit_Cosine(t *testing.T) {
	require.InDelta(t, 1.0, cosine([]float32{1, 0}, []float32{2, 0}), 1e-9, "direction, not magnitude")
	require.InDelta(t, 0.0, cosine([]float32{1, 0}, []float32{0, 1}), 1e-9)
	require.InDelta(t, -1.0, cosine([]float32{1, 0}, []float32{-1, 0}), 1e-9)
	require.Zero(t, cosine([]float32{0, 0}, []float32{1, 1}), "a zero vector scores 0, never NaN")
	require.Zero(t, cosine([]float32{1}, []float32{1, 1}), "mismatched lengths score 0 rather than panicking")
}

func TestUnit_Query_RespectsContextCancellation(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	_, err := h.svc.Query(ctx, h.ws, "retry backoff", 3)
	require.Error(t, err)
}
