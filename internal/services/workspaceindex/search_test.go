package workspaceindex

// Search tests share the indexer harness: real SQLite store, deterministic fake embedder.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// affinityEmbedder embeds every text as a 2-D unit vector whose cosine against
// the question is exactly the affinity scripted for it. A hash-of-words fake
// cannot state "the vector leg prefers the wrong chunk" as a fact; this can.
type affinityEmbedder struct {
	question string
	affinity func(text string) float64
}

func (e *affinityEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	a := 1.0
	if text != e.question {
		a = e.affinity(text)
	}
	return []float32{float32(a), float32(math.Sqrt(1 - a*a))}, nil
}

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

	require.GreaterOrEqual(t, hits[0].StartLine, 1)
	require.GreaterOrEqual(t, hits[0].EndLine, hits[0].StartLine)

	for i := 1; i < len(hits); i++ {
		require.GreaterOrEqual(t, hits[i-1].Score, hits[i].Score, "hits must be returned in rank order")
	}
}

// TestUnit_Query_UsesTheLexicalPrefilter pins that a matching question narrows via FTS5 and never reaches the bounded fallback scan.
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

// TestUnit_Query_FallsBackWhenThePrefilterMatchesNothing pins that an empty prefilter falls back to a bounded scan rather than returning nothing.
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

// TestUnit_Query_MarksStaleHitsAfterAnEdit pins that a hit whose file changed underneath is marked, never silently returned as current.
func TestUnit_Query_MarksStaleHitsAfterAnEdit(t *testing.T) {
	h := newHarness(t, Config{})
	searchFixture(t, h)

	fresh, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.False(t, fresh[0].Stale)

	h.write(t, "docs/retry.md", "Retry backoff is explained here: the delay doubles on every attempt until a ceiling.\nA new line changes the file's sha.\n")

	stale, err := h.svc.Query(h.ctx, h.ws, "where is retry backoff explained", 1)
	require.NoError(t, err)
	require.Equal(t, "docs/retry.md", stale[0].Path)
	require.True(t, stale[0].Stale, "a hit whose file moved must be marked, never silently returned")

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

// TestUnit_Query_EmptyIndexDegradesToErrNoIndex pins that a missing index degrades to ErrNoIndex, never a hard failure.
func TestUnit_Query_EmptyIndexDegradesToErrNoIndex(t *testing.T) {
	h := newHarness(t, Config{})

	_, err := h.svc.Query(h.ctx, h.ws, "anything at all", 5)
	require.ErrorIs(t, err, ErrNoIndex)
	require.Contains(t, err.Error(), "contenox index", "the error must tell the operator what to run")

	_, err = h.svc.Status(h.ctx, h.ws)
	require.ErrorIs(t, err, ErrNoIndex)

	// An index that exists but holds nothing says so: an empty hit list would
	// read as "the workspace does not contain that", which is a different claim.
	h.write(t, "empty.md", "\n")
	_, err = h.svc.Build(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	_, err = h.svc.Query(h.ctx, h.ws, "anything at all", 5)
	require.ErrorIs(t, err, ErrIndexEmpty, "an empty generation must be distinguishable from a query that matched nothing")
	require.ErrorIs(t, err, ErrNoIndex, "and must stay matchable by every caller that already degrades ErrNoIndex")

	// A populated index that simply matches nothing is NOT the empty case.
	h.write(t, "docs/retry.md", "Retry backoff is explained here.\n")
	_, err = h.svc.Build(h.ctx, h.root, h.opts(false))
	require.NoError(t, err)
	hits, err := h.svc.Query(h.ctx, h.ws, "zzqqxx unrelatedtoken", 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "the vector leg still ranks the bounded scan")
}

// TestUnit_Query_ExactIdentifierOutranksTheSemanticDecoy is the reason this
// retrieval is hybrid: the vector leg is scripted to prefer prose that restates
// the question in other words, and rank fusion must still put the file that
// literally declares the symbol first.
func TestUnit_Query_ExactIdentifierOutranksTheSemanticDecoy(t *testing.T) {
	const (
		question = "where is ResolveHITLApprovalWithinBound called"
		target   = "internal/hitl/resolve.go"
		decoy    = "docs/approvals.md"
	)

	h := newHarness(t, Config{})
	h.write(t, target, "package hitl\n\nfunc ResolveHITLApprovalWithinBound(ctx context.Context) error {\n\treturn nil\n}\n")
	h.write(t, decoy, strings.Repeat(
		"This document explains where a pending human approval is resolved, how the bound on it is applied, and what happens when the approval flow is called again later.\n", 6))
	// Filler that shares only the question's common words, so the lexical leg
	// has a realistic field to rank the decoy against.
	fillers := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for i, name := range fillers {
		h.write(t, fmt.Sprintf("docs/fill%02d.md", i), fmt.Sprintf("Where the %s rollout is called out, the runbook is authoritative.\n", name))
	}

	// The vector leg's verdict: the prose decoy, not the declaration. Ranked by
	// cosine alone — which is what this retrieval did before it was hybrid —
	// the answer to "where is X called" is a document that never mentions X.
	scripted := &affinityEmbedder{question: question, affinity: func(text string) float64 {
		switch {
		case strings.Contains(text, "pending human approval"):
			return 0.99
		case strings.Contains(text, "func ResolveHITLApprovalWithinBound"):
			return 0.85
		}
		for i, name := range fillers {
			if strings.Contains(text, name+" rollout") {
				return 0.35 - 0.03*float64(i)
			}
		}
		return 0.10
	}}
	h.svc = New(h.spy, scripted, ollamatokenizer.NewEstimateTokenizer(), Config{EmbedModel: "fake-embed", EmbedProvider: "fake"})
	h.build(t, false)

	cfg, err := h.store.GetActiveWorkspaceIndexConfig(h.ctx, h.ws)
	require.NoError(t, err)
	candidates, err := h.store.SearchWorkspaceChunks(h.ctx, cfg.ID, ftsMatchQuery(question), 200)
	require.NoError(t, err)
	require.Len(t, candidates, 1+1+len(fillers), "every document shares at least one term with the question")

	// Premise 1: the lexical leg finds the declaration by its rare token.
	require.Equal(t, target, candidates[0].Path, "bm25's IDF must put the exact identifier first")
	lexical := rankByOrder(candidates)

	// Premise 2: the vector leg gets it wrong, and by how much.
	qvec, err := scripted.Embed(h.ctx, question)
	require.NoError(t, err)
	vector := rankByCosine(qvec, candidates)
	byPath := map[string]*runtimetypes.WorkspaceChunk{}
	for _, c := range candidates {
		byPath[c.Path] = c
	}
	require.Equal(t, 1, vector[byPath[decoy].ID], "the decoy must be the vector leg's own answer")
	require.Greater(t, vector[byPath[target].ID], 1, "and the declaration must not be")
	require.Greater(t, lexical[byPath[decoy].ID], vector[byPath[target].ID],
		"the fixture is only meaningful while the decoy is lexically worse than the target is semantically")

	// The fusion's verdict: the declaration, not the prose about it.
	hits, err := h.svc.Query(h.ctx, h.ws, question, 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, target, hits[0].Path, "rank fusion must not let a confident vector leg bury an exact identifier")
	require.Contains(t, hits[0].Text, "ResolveHITLApprovalWithinBound")
	require.Greater(t, hits[0].Score, hits[1].Score)
}

// TestUnit_Query_LexicalOnlyIndexAnswersWithNoEmbeddingModel pins the degraded
// mode: no embedding model at all, and search still works.
func TestUnit_Query_LexicalOnlyIndexAnswersWithNoEmbeddingModel(t *testing.T) {
	h := newHarness(t, Config{})
	h.svc = New(h.spy, nil, ollamatokenizer.NewEstimateTokenizer(), Config{})
	h.write(t, "docs/retry.md", "Retry backoff is explained here: the delay doubles on every attempt until a ceiling.\n")
	h.write(t, "docs/widgets.md", "The widget catalogue enumerates every widget by taxonomy and colour.\n")

	rep := h.build(t, false)
	require.Zero(t, rep.EmbedCalls, "a lexical-only build spends nothing at the model, not even the dimension probe")
	require.Positive(t, rep.ChunksWritten)

	st, err := h.svc.Status(h.ctx, h.ws)
	require.NoError(t, err)
	require.Zero(t, st.Dimension, "dimension 0 is how a lexical-only generation declares itself")

	hits, err := h.svc.Query(h.ctx, h.ws, "retry backoff ceiling", 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "docs/retry.md", hits[0].Path)
	require.Positive(t, hits[0].Score)
	require.False(t, hits[0].Stale)

	// Staleness flagging is a property of the hit, not of the vector leg.
	h.write(t, "docs/retry.md", "Retry backoff is explained here: the delay doubles on every attempt until a ceiling.\nAnd one more line.\n")
	hits, err = h.svc.Query(h.ctx, h.ws, "retry backoff ceiling", 3)
	require.NoError(t, err)
	require.True(t, hits[0].Stale, "a hybrid hit must still report that its file moved")

	// With no vector leg the bounded scan would be an unranked list, so a
	// question sharing no term answers with nothing rather than with noise.
	_, scansBefore := h.spy.counts()
	none, err := h.svc.Query(h.ctx, h.ws, "zzqqxx unrelatedtoken", 3)
	require.NoError(t, err)
	require.Empty(t, none)
	_, scansAfter := h.spy.counts()
	require.Equal(t, scansBefore, scansAfter, "there is nothing to rank a scan with, so it is not taken")
}

// TestUnit_RRF_FusesRanksAndDegradesToOneLeg pins the fusion arithmetic and the
// tie-break, independently of any store or embedder.
func TestUnit_RRF_FusesRanksAndDegradesToOneLeg(t *testing.T) {
	require.InDelta(t, 1.0/61.0, rrf(1), 1e-12, "k=60 plus a 1-based rank")
	require.InDelta(t, 1.0/70.0, rrf(10), 1e-12)
	require.Zero(t, rrf(0), "a leg that did not return the chunk contributes nothing, not a rank-0 bonus")

	chunks := []*runtimetypes.WorkspaceChunk{
		{ID: "a", Path: "a.go"}, {ID: "b", Path: "b.go"}, {ID: "c", Path: "c.go"},
	}
	lexical := map[string]int{"a": 1, "b": 5}
	vector := map[string]int{"b": 1, "a": 5, "c": 2}

	fused := fuse(chunks, lexical, vector)
	require.Equal(t, []string{"a.go", "b.go", "c.go"}, hitPaths(fused))
	require.InDelta(t, 1.0/61.0+1.0/65.0, fused[0].Score, 1e-12)
	require.InDelta(t, fused[0].Score, fused[1].Score, 1e-12, "a symmetric rank swap ties on score")
	require.InDelta(t, 1.0/62.0, fused[2].Score, 1e-12, "a chunk only one leg returned scores from that leg alone")

	// One leg absent degrades to the other leg's own order, not to no order.
	require.Equal(t, []string{"b.go", "c.go", "a.go"}, hitPaths(fuse(chunks, nil, vector)))
	require.Equal(t, []string{"a.go", "b.go", "c.go"}, hitPaths(fuse(chunks, lexical, nil)),
		"a chunk no leg returned sorts last, by location")
}

func hitPaths(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
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

// TestUnit_Query_DimensionMismatchIsLoud pins that querying with a different-width model fails loudly instead of scoring nonsense.
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
