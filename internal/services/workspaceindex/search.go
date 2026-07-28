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

	"github.com/contenox/beam/internal/store/runtimetypes"
)

// Query answers a natural-language question with ranked citations. FTS5
// narrows (bm25 over chunk text, capped at CandidateLimit) and vectors rank
// (cosine over just those candidates), so scoring never scans the whole
// index. Returns ErrNoIndex when the workspace has none; a question sharing
// no term with the corpus falls back to a bounded scan instead of nothing.
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

	candidates, err := s.store.SearchWorkspaceChunks(ctx, cfg.ID, ftsMatchQuery(question), s.cfg.CandidateLimit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		if candidates, err = s.store.ScanWorkspaceChunks(ctx, cfg.ID, fullScanLimit); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return []Hit{}, nil
	}

	qvec, err := s.embedder.Embed(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("workspaceindex: embed question: %w", err)
	}
	if len(qvec) != cfg.Dimension {
		return nil, fmt.Errorf("%w: the question embedded to %d dimensions but index %s holds %d — the embedding model changed; rebuild with `contenox index --force`",
			runtimetypes.ErrVectorDimensionMismatch, len(qvec), cfg.ID, cfg.Dimension)
	}

	scored := make([]Hit, 0, len(candidates))
	for _, c := range candidates {
		scored = append(scored, Hit{
			Path:      c.Path,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Text:      c.Text,
			Score:     cosine(qvec, c.Vector),
		})
	}
	// Ties break on location so the same question twice gives the same answer.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		if scored[i].Path != scored[j].Path {
			return scored[i].Path < scored[j].Path
		}
		return scored[i].StartLine < scored[j].StartLine
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}

	markStale(cfg.Root, candidates, scored)
	return scored, nil
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

// ftsMatchQuery turns a natural-language question into an FTS5 MATCH
// expression. Every term is extracted as a run of letters/digits and
// double-quoted, so nothing typed is interpreted as FTS5 syntax. Terms are
// OR-ed: this stage narrows, precision is the vector stage's job.
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
