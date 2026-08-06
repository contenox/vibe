package workspaceindex

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// rrfK damps how much a leg's top ranks are worth in Reciprocal Rank Fusion:
// a chunk's fused score is the sum over legs of 1/(rrfK + rank). 60 is the
// constant RRF was published with (Cormack, Clarke & Buettcher, SIGIR 2009)
// and the value hybrid-retrieval implementations have inherited since. It is
// NOT tuned here: tuning it against this repository's own corpus would fit
// this corpus and generalise to no user's.
//
// The two legs are weighted EQUALLY. RRF's point is that a rank is comparable
// across legs while a bm25 score and a cosine are not; introducing a weight
// would re-import exactly the incomparability the fusion exists to avoid, and
// there is no held-out corpus here against which a weight could be justified.
// The one asymmetry that IS encoded is a tie-break, not a weight: see fuse.
const rrfK = 60

// Query answers a question with ranked citations by HYBRID retrieval. Two legs
// rank the same candidates independently — a lexical leg (FTS5 bm25 over chunk
// text, capped at CandidateLimit) and a vector leg (cosine over those
// candidates) — and Reciprocal Rank Fusion combines them. The legs fail
// differently, which is the whole point: an embedding of a rare identifier
// (ResolveHITLApprovalWithinBound, CONTENOX_ACP_CHAIN_PATH) is near-noise
// while bm25's IDF finds it exactly, and prose that answers the question in
// other words is invisible to bm25 while the vector leg ranks it.
//
// Either leg alone still answers. With no embedding model the index is
// lexical-only (config Dimension 0) and the lexical leg ranks by itself; a
// question sharing no term with the corpus leaves the lexical leg empty and
// falls back to a bounded scan the vector leg ranks. Returns ErrNoIndex when
// the workspace has none and ErrIndexEmpty when the generation holds nothing —
// silence would read as "the answer is not in this codebase".
func (s *service) Query(ctx context.Context, workspaceID string, question string, topK int) ([]Hit, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, ErrEmptyQuestion
	}
	topK = clampTopK(topK)

	cfg, err := s.activeConfig(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	vectorLeg := s.embedder != nil && cfg.Dimension > 0

	lexical, err := s.store.SearchWorkspaceChunks(ctx, cfg.ID, ftsMatchQuery(question), s.cfg.CandidateLimit)
	if err != nil {
		return nil, err
	}
	// The bounded scan exists to give the vector leg something to rank when
	// the lexical leg matched nothing; with no vector leg it would just be an
	// unranked list, so it is not taken.
	candidates := lexical
	if len(candidates) == 0 && vectorLeg {
		if candidates, err = s.store.ScanWorkspaceChunks(ctx, cfg.ID, fullScanLimit); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return s.explainNoCandidates(ctx, cfg)
	}

	var vectorRank map[string]int
	if vectorLeg {
		qvec, err := s.embedder.Embed(ctx, question)
		if err != nil {
			return nil, fmt.Errorf("workspaceindex: embed question: %w", err)
		}
		if len(qvec) != cfg.Dimension {
			return nil, fmt.Errorf("%w: the question embedded to %d dimensions but index %s holds %d — the embedding model changed; rebuild with `contenox index --force`",
				runtimetypes.ErrVectorDimensionMismatch, len(qvec), cfg.ID, cfg.Dimension)
		}
		vectorRank = rankByCosine(qvec, candidates)
	}

	scored := fuse(candidates, rankByOrder(lexical), vectorRank)
	if len(scored) > topK {
		scored = scored[:topK]
	}

	markStale(cfg.Root, candidates, scored)
	return scored, nil
}

// explainNoCandidates separates "this workspace was never indexed" from
// "nothing in the index matched". Both return zero hits, but only one of them
// is the user's to fix, and a model cannot tell them apart from an empty list.
func (s *service) explainNoCandidates(ctx context.Context, cfg *runtimetypes.WorkspaceIndexConfig) ([]Hit, error) {
	n, err := s.store.CountWorkspaceChunks(ctx, cfg.ID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w (workspace %s, index %s)", ErrIndexEmpty, cfg.WorkspaceID, cfg.ID)
	}
	return []Hit{}, nil
}

// rankByOrder assigns 1-based ranks to an already-ordered leg, keyed by chunk
// id. The store returns the lexical leg in bm25 order, so its position IS its
// rank — the raw bm25 value is never fused, only its rank (see rrfK).
func rankByOrder(chunks []*runtimetypes.WorkspaceChunk) map[string]int {
	out := make(map[string]int, len(chunks))
	for i, c := range chunks {
		out[c.ID] = i + 1
	}
	return out
}

// rankByCosine ranks the candidate set by cosine against the question, ties
// broken on location so the same question twice produces the same ranks.
func rankByCosine(qvec []float32, candidates []*runtimetypes.WorkspaceChunk) map[string]int {
	order := make([]*runtimetypes.WorkspaceChunk, len(candidates))
	copy(order, candidates)
	sim := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		sim[c.ID] = cosine(qvec, c.Vector)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if sim[order[i].ID] != sim[order[j].ID] {
			return sim[order[i].ID] > sim[order[j].ID]
		}
		return beforeByLocation(order[i], order[j])
	})
	return rankByOrder(order)
}

// fuse combines the legs by Reciprocal Rank Fusion: score = Σ 1/(rrfK + rank),
// a leg that did not return the chunk contributing nothing. A missing leg
// therefore degrades to the other leg's own order rather than to no order.
//
// Ties are broken first on the LEXICAL rank. Two legs that disagree by a
// symmetric rank swap produce identical fused scores, and there the chunk with
// literal evidence is the better answer — that is the failure this fusion
// exists to fix. Location breaks what remains, so a query is reproducible.
func fuse(candidates []*runtimetypes.WorkspaceChunk, lexicalRank, vectorRank map[string]int) []Hit {
	type ranked struct {
		chunk   *runtimetypes.WorkspaceChunk
		score   float64
		lexical int
	}
	order := make([]ranked, 0, len(candidates))
	for _, c := range candidates {
		order = append(order, ranked{
			chunk:   c,
			score:   rrf(lexicalRank[c.ID]) + rrf(vectorRank[c.ID]),
			lexical: lexicalRank[c.ID],
		})
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].score != order[j].score {
			return order[i].score > order[j].score
		}
		if order[i].lexical != order[j].lexical {
			return beforeByRank(order[i].lexical, order[j].lexical)
		}
		return beforeByLocation(order[i].chunk, order[j].chunk)
	})

	hits := make([]Hit, 0, len(order))
	for _, r := range order {
		hits = append(hits, Hit{
			Path:      r.chunk.Path,
			StartLine: r.chunk.StartLine,
			EndLine:   r.chunk.EndLine,
			Text:      r.chunk.Text,
			Score:     r.score,
		})
	}
	return hits
}

// rrf is one leg's contribution. Rank 0 means the leg did not return this
// chunk at all, which contributes nothing rather than a rank-0 bonus.
func rrf(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1 / float64(rrfK+rank)
}

// beforeByRank orders two 1-based ranks best-first, treating 0 ("absent from
// this leg") as worse than any rank rather than as the best one.
func beforeByRank(a, b int) bool {
	switch {
	case a <= 0:
		return false
	case b <= 0:
		return true
	}
	return a < b
}

// beforeByLocation is the final, total tie-break: path then start line.
func beforeByLocation(a, b *runtimetypes.WorkspaceChunk) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.StartLine < b.StartLine
}

// clampTopK bounds the result count instead of rejecting an out-of-range one: a
// caller asking for 10000 hits wants "as many as you have", and every returned
// hit costs a staleness check.
func clampTopK(topK int) int {
	if topK <= 0 {
		return topKDefault
	}
	if topK > topKMax {
		return topKMax
	}
	return topK
}

// markStale re-derives each returned hit's file sha and flags the hit when
// the content moved since indexing, or the file no longer exists. Only the
// returned paths are hashed (at most topK distinct files); candidates that
// lost the ranking are never touched.
func markStale(root string, candidates []*runtimetypes.WorkspaceChunk, hits []Hit) {
	indexedSHA := make(map[string]string, len(candidates))
	for _, c := range candidates {
		indexedSHA[c.Path] = c.ContentSHA
	}
	current := make(map[string]string, len(hits))
	for i := range hits {
		path := hits[i].Path
		sha, seen := current[path]
		if !seen {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err == nil {
				sha = fileSHA(content)
			}
			current[path] = sha
		}
		hits[i].Stale = sha == "" || sha != indexedSHA[path]
	}
}

// ftsMatchQuery turns a question into an FTS5 MATCH expression. Every term is
// extracted as a run of letters/digits and double-quoted, so nothing typed is
// interpreted as FTS5 syntax. Terms are OR-ed rather than AND-ed so the leg
// keeps recall; precision comes from bm25's IDF, which is what makes a rare
// identifier outrank the common words surrounding it in the question.
//
// An identifier written in one run (CONTENOX_ACP_CHAIN_PATH splits on the
// underscores, ResolveHITLApprovalWithinBound does not) tokenizes the same way
// on both sides here and in unicode61, so an exact symbol matches exactly.
func ftsMatchQuery(question string) string {
	fields := strings.FieldsFunc(question, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(fields))
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ToLower(f)
		// One-character terms match almost everything and defeat the prefilter.
		if len([]rune(f)) < 2 {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		terms = append(terms, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

// cosine is the similarity between two equal-length vectors, in [-1, 1]. A
// zero-magnitude vector scores 0 against everything rather than producing NaN,
// which would sort unpredictably.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
