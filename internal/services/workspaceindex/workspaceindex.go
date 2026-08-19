// Package workspaceindex builds and queries a local retrieval index over a
// workspace: `contenox index` fills it, `contenox search` reads it, and a
// local tool exposes the same read to the agent. One local index per
// workspace. Retrieval is hybrid — an FTS5 bm25 leg and a linear cosine leg
// over the same candidates, fused by Reciprocal Rank Fusion (see search.go) —
// and either leg alone still answers, so a workspace with no embedding model
// gets keyword retrieval rather than nothing.
package workspaceindex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// ErrNoIndex is the typed refusal every degraded read returns: this workspace
// has no index yet. It is a first-class result, not a failure — a missing
// index degrades to "run contenox index", never a hard error.
var ErrNoIndex = errors.New("no index for this workspace; run contenox index")

// ErrEmptyQuestion is returned by Query for a blank question. Embedding
// whitespace produces a vector that ranks arbitrary chunks highly, which reads
// as a confident wrong answer.
var ErrEmptyQuestion = errors.New("search question is empty")

// ErrIndexEmpty is the narrower ErrNoIndex: a generation exists but holds no
// chunks, so both retrieval legs have nothing to rank. It wraps ErrNoIndex on
// purpose — every caller that already degrades "no index" to "run contenox
// index" stays correct without changing — while a caller that wants to say
// which of the two happened can test for this one.
var ErrIndexEmpty = fmt.Errorf("%w: the index exists but holds no chunks", ErrNoIndex)

const (
	// embedConcurrencyDefault bounds in-flight embed calls.
	embedConcurrencyDefault = 4

	// writeBatchDefault is how many embedded chunks accumulate before one
	// AppendWorkspaceChunks call; bounded by SQLite's host-parameter limit.
	writeBatchDefault = 64

	// candidateLimitDefault bounds the lexical leg: the top 200 by bm25 are
	// both that leg's ranking and the candidate set the vector leg ranks.
	candidateLimitDefault = 200

	// fullScanLimit bounds the degraded path taken when the lexical leg
	// matches nothing at all; it is the store's own list ceiling, never an
	// unbounded scan.
	fullScanLimit = runtimetypes.MAXLIMIT

	// topKDefault / topKMax bound how many hits a query returns.
	topKDefault = 8
	topKMax     = 50
)

// Embedder is the narrow seam onto embedding generation, letting tests run
// against a deterministic fake with no model and no network.
// NewLLMRepoEmbedder adapts the real thing.
//
// A nil Embedder is legal and means LEXICAL-ONLY: Build writes chunks and
// their FTS5 mirror with no vectors under a Dimension-0 generation, and Query
// ranks on bm25 alone. That is the mode a workspace with no
// `default-embed-model` runs in, and it answers exact-identifier questions
// better than the vector leg does. Configuring a model later cuts over to a
// new generation exactly as any model change does — a lexical-only generation
// is never back-filled with vectors, so it cannot end up half-embedded.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Store is the subset of runtimetypes.Store the index uses;
// runtimetypes.Store satisfies it.
type Store interface {
	CreateWorkspaceIndexConfig(ctx context.Context, cfg *runtimetypes.WorkspaceIndexConfig) error
	GetActiveWorkspaceIndexConfig(ctx context.Context, workspaceID string) (*runtimetypes.WorkspaceIndexConfig, error)
	AppendWorkspaceChunks(ctx context.Context, chunks ...*runtimetypes.WorkspaceChunk) error
	ListWorkspaceIndexedFiles(ctx context.Context, configID string) ([]runtimetypes.WorkspaceIndexedFile, error)
	DeleteWorkspaceChunksForPaths(ctx context.Context, configID string, paths ...string) error
	DeleteWorkspaceChunksForConfig(ctx context.Context, configID string) error
	SearchWorkspaceChunks(ctx context.Context, configID string, match string, limit int) ([]*runtimetypes.WorkspaceChunk, error)
	ScanWorkspaceChunks(ctx context.Context, configID string, limit int) ([]*runtimetypes.WorkspaceChunk, error)
	CountWorkspaceChunks(ctx context.Context, configID string) (int64, error)
}

// Service is the workspace index. Plan reports cost without spending it;
// Build spends it; Query reads; Status reports what exists.
type Service interface {
	// Plan reports what a Build would do — files, chunks, and the number of
	// embed calls — without making any.
	Plan(ctx context.Context, root string, opts BuildOptions) (*BuildPlan, error)

	// Build indexes root into the workspace's index, creating it if needed.
	// Incremental by default; opts.Force rebuilds everything.
	Build(ctx context.Context, root string, opts BuildOptions) (*BuildReport, error)

	// Query returns the topK chunks most similar to question, each a
	// citation (path + line range) with its text, score, and staleness.
	Query(ctx context.Context, workspaceID string, question string, topK int) ([]Hit, error)

	// Status describes the workspace's active index, or ErrNoIndex.
	Status(ctx context.Context, workspaceID string) (*Status, error)
}

// Config is the service's construction-time policy. The zero Config is
// usable except for EmbedModel, which is recorded on every index generation.
type Config struct {
	// EmbedModel / EmbedProvider identify the embedding model behind Embedder.
	EmbedModel    string
	EmbedProvider string
	// ChunkTokens is the per-chunk token budget.
	ChunkTokens int
	// OverlapLines is how many lines each chunk repeats from its
	// predecessor. Zero means the default, not "no overlap".
	OverlapLines int
	// MaxFileBytes is the file-size cap; larger files are counted and skipped.
	MaxFileBytes int64
	// EmbedConcurrency bounds in-flight embed calls.
	EmbedConcurrency int
	// CandidateLimit bounds the lexical leg and, with it, the candidate set
	// the vector leg ranks.
	CandidateLimit int
}

func (c Config) withDefaults() Config {
	if c.ChunkTokens <= 0 {
		c.ChunkTokens = chunkTokensDefault
	}
	if c.OverlapLines < 0 {
		c.OverlapLines = 0
	}
	if c.OverlapLines == 0 {
		c.OverlapLines = overlapLinesDefault
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = maxFileBytesDefault
	}
	if c.EmbedConcurrency <= 0 {
		c.EmbedConcurrency = embedConcurrencyDefault
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = candidateLimitDefault
	}
	return c
}

// BuildOptions scopes one build.
type BuildOptions struct {
	// WorkspaceID scopes the index. Required.
	WorkspaceID string
	// Force rebuilds every file instead of only the changed ones.
	Force bool
	// Progress, when set, is called as the build advances, from the build's
	// own goroutine only.
	Progress func(Progress)
}

// Phase names where a build is.
type Phase string

const (
	PhasePlanning  Phase = "planning"
	PhaseEmbedding Phase = "embedding"
	PhaseDone      Phase = "done"
)

// Progress is one step report. Totals come from the plan, so a caller can
// render a proportion from the first event onward.
type Progress struct {
	Phase       Phase
	Path        string
	Files       int
	FilesTotal  int
	Chunks      int
	ChunksTotal int
	Plan        *BuildPlan
}

// BuildPlan is the honest cost estimate produced before anything is embedded.
type BuildPlan struct {
	WorkspaceID string
	Root        string
	// ConfigID is the existing index generation this build would extend, or
	// "" when it would create one.
	ConfigID      string
	EmbedModel    string
	EmbedProvider string
	// CutOver reports that this build would create a new index generation
	// rather than extend the existing one.
	CutOver bool
	// Files / Chunks describe the whole selected tree.
	Files  int
	Chunks int
	Bytes  int64
	// FilesChanged / ChunksToWrite describe the work: every chunk of every
	// changed file gets written.
	FilesChanged  int
	ChunksToWrite int
	// EmbedCalls is what that work costs at the model: one call per chunk
	// written, or zero for a lexical-only build, which spends nothing.
	EmbedCalls int
	// ChunksReused / FilesDeleted describe what incremental avoids and cleans.
	ChunksReused int
	FilesDeleted int
	// SkippedBinary / SkippedOversize / SkippedGenerated are files selection refused.
	SkippedBinary    int
	SkippedOversize  int
	SkippedGenerated int
}

// BuildReport is what a build actually did.
type BuildReport struct {
	Plan          BuildPlan
	ConfigID      string
	ChunksWritten int
	ChunksDeleted int
	// EmbedCalls counts calls actually made, including the dimension probe
	// a new index generation costs.
	EmbedCalls int
	Duration   time.Duration
}

// Status describes a workspace's active index. Dimension 0 means the
// generation is lexical-only: it was built with no embedding model, so only
// the bm25 leg can rank it.
type Status struct {
	ConfigID      string
	WorkspaceID   string
	Root          string
	EmbedModel    string
	EmbedProvider string
	Dimension     int
	ChunkTokens   int
	ChunkOverlap  int
	Chunks        int64
	CreatedAt     time.Time
}

// Hit is one ranked search result: a citation, its text, its score, and
// whether the file moved underneath it.
type Hit struct {
	Path      string
	StartLine int
	EndLine   int
	Text      string
	// Score is the Reciprocal Rank Fusion score, not a similarity: it is a
	// sum of 1/(rrfK + rank) over the legs that returned this chunk, so its
	// magnitude is meaningful only against the other hits of the same query.
	Score float64
	// Stale means the file's content sha no longer matches what was
	// indexed (or the file is gone). A stale hit is still returned, never
	// silently presented as current.
	Stale bool
}

type service struct {
	store    Store
	embedder Embedder
	tokens   TokenCounter
	cfg      Config
}

// New builds the index service over the store, an embedding seam, and a
// token counter. A nil embedder builds and searches lexical-only (see
// Embedder); it is a supported mode, not a broken wiring.
func New(store Store, embedder Embedder, tokens TokenCounter, cfg Config) Service {
	return &service{
		store:    store,
		embedder: embedder,
		tokens:   tokens,
		cfg:      cfg.withDefaults(),
	}
}

func (s *service) Status(ctx context.Context, workspaceID string) (*Status, error) {
	cfg, err := s.activeConfig(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	n, err := s.store.CountWorkspaceChunks(ctx, cfg.ID)
	if err != nil {
		return nil, err
	}
	return &Status{
		ConfigID:      cfg.ID,
		WorkspaceID:   cfg.WorkspaceID,
		Root:          cfg.Root,
		EmbedModel:    cfg.EmbedModel,
		EmbedProvider: cfg.EmbedProvider,
		Dimension:     cfg.Dimension,
		ChunkTokens:   cfg.ChunkTokens,
		ChunkOverlap:  cfg.ChunkOverlap,
		Chunks:        n,
		CreatedAt:     cfg.CreatedAt,
	}, nil
}

// activeConfig loads the workspace's live index generation, translating "no
// rows" into ErrNoIndex.
func (s *service) activeConfig(ctx context.Context, workspaceID string) (*runtimetypes.WorkspaceIndexConfig, error) {
	cfg, err := s.store.GetActiveWorkspaceIndexConfig(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil, fmt.Errorf("%w (workspace %s)", ErrNoIndex, workspaceID)
		}
		return nil, err
	}
	return cfg, nil
}

func (s *service) Plan(ctx context.Context, root string, opts BuildOptions) (*BuildPlan, error) {
	plan, _, err := s.plan(ctx, root, opts)
	return plan, err
}

// plan walks the tree, chunks it, and diffs it against what is already
// indexed, without embedding anything. It returns the plan plus the
// per-file content shas keyed by path, which Build reuses.
func (s *service) plan(ctx context.Context, root string, opts BuildOptions) (*BuildPlan, map[string]string, error) {
	if strings.TrimSpace(opts.WorkspaceID) == "" {
		return nil, nil, errors.New("workspaceindex: WorkspaceID is required")
	}
	resolvedRoot, err := vfs.ResolveRoot(root)
	if err != nil {
		return nil, nil, fmt.Errorf("workspaceindex: resolve root: %w", err)
	}

	plan := &BuildPlan{
		WorkspaceID:   opts.WorkspaceID,
		Root:          resolvedRoot,
		EmbedModel:    s.cfg.EmbedModel,
		EmbedProvider: s.cfg.EmbedProvider,
	}

	// Which generation would this build write into?
	active, err := s.store.GetActiveWorkspaceIndexConfig(ctx, opts.WorkspaceID)
	switch {
	case err != nil && !errors.Is(err, libdb.ErrNotFound):
		return nil, nil, err
	case err != nil: // no index yet
		plan.CutOver = true
	case s.compatible(active, resolvedRoot):
		plan.ConfigID = active.ID
	default:
		// Incompatible config: cut over to a new generation rather than mix
		// vectors from two models in one table.
		plan.CutOver = true
	}

	// What is already indexed, for the incremental diff.
	indexed := map[string]runtimetypes.WorkspaceIndexedFile{}
	if plan.ConfigID != "" && !opts.Force {
		files, err := s.store.ListWorkspaceIndexedFiles(ctx, plan.ConfigID)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range files {
			indexed[f.Path] = f
		}
	}

	onDisk := make(map[string]string, len(indexed))
	stats, err := walkWorkspace(ctx, resolvedRoot, s.cfg.MaxFileBytes, func(f sourceFile) error {
		chunks, err := chunkFile(ctx, s.tokens, s.cfg.EmbedModel, f, s.cfg.ChunkTokens, s.cfg.OverlapLines)
		if err != nil {
			return err
		}
		onDisk[f.RelPath] = f.SHA
		plan.Chunks += len(chunks)
		if prev, ok := indexed[f.RelPath]; ok && prev.ContentSHA == f.SHA {
			plan.ChunksReused += prev.Chunks
			return nil
		}
		plan.FilesChanged++
		plan.ChunksToWrite += len(chunks)
		if s.embedder != nil {
			plan.EmbedCalls += len(chunks)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	plan.Files = stats.Selected
	plan.Bytes = stats.Bytes
	plan.SkippedBinary = stats.SkippedBinary
	plan.SkippedOversize = stats.SkippedOversize
	plan.SkippedGenerated = stats.SkippedGenerated

	for path := range indexed {
		if _, ok := onDisk[path]; !ok {
			plan.FilesDeleted++
		}
	}
	return plan, onDisk, nil
}

// compatible reports whether an existing index generation may be extended
// by a build under the current config.
func (s *service) compatible(active *runtimetypes.WorkspaceIndexConfig, root string) bool {
	return active.EmbedModel == s.cfg.EmbedModel &&
		active.EmbedProvider == s.cfg.EmbedProvider &&
		active.Root == root &&
		active.ChunkTokens == s.cfg.ChunkTokens &&
		active.ChunkOverlap == s.cfg.OverlapLines
}

func (s *service) Build(ctx context.Context, root string, opts BuildOptions) (*BuildReport, error) {
	started := time.Now()
	plan, onDisk, err := s.plan(ctx, root, opts)
	if err != nil {
		return nil, err
	}
	report := &BuildReport{Plan: *plan, ConfigID: plan.ConfigID}

	// The caller learns the embed-call count before the first call.
	emit(opts.Progress, Progress{
		Phase:       PhasePlanning,
		FilesTotal:  plan.FilesChanged,
		ChunksTotal: plan.ChunksToWrite,
		Plan:        plan,
	})

	indexConfig, err := s.ensureConfig(ctx, plan, opts, report)
	if err != nil {
		return nil, err
	}
	report.ConfigID = indexConfig.ID

	// Forced/new generation clears everything; otherwise only changed/vanished files.
	if opts.Force && !plan.CutOver {
		before, err := s.store.CountWorkspaceChunks(ctx, indexConfig.ID)
		if err != nil {
			return nil, err
		}
		if err := s.store.DeleteWorkspaceChunksForConfig(ctx, indexConfig.ID); err != nil {
			return nil, err
		}
		report.ChunksDeleted = int(before)
	} else if !plan.CutOver {
		stale, staleChunks, err := s.stalePaths(ctx, indexConfig.ID, onDisk)
		if err != nil {
			return nil, err
		}
		if len(stale) > 0 {
			if err := s.store.DeleteWorkspaceChunksForPaths(ctx, indexConfig.ID, stale...); err != nil {
				return nil, err
			}
			report.ChunksDeleted = staleChunks
		}
	}

	// Nothing to write, but the walk above still ran and any deletions already happened.
	if plan.ChunksToWrite == 0 {
		emit(opts.Progress, Progress{Phase: PhaseDone, ChunksTotal: 0, FilesTotal: 0, Plan: plan})
		report.Plan = *plan
		report.Duration = time.Since(started)
		return report, nil
	}

	written, calls, err := s.embedAndWrite(ctx, indexConfig, plan, opts, onDisk)
	report.ChunksWritten = written
	report.EmbedCalls += calls
	report.Plan = *plan
	report.Duration = time.Since(started)
	if err != nil {
		return nil, err
	}
	emit(opts.Progress, Progress{
		Phase:       PhaseDone,
		Files:       plan.FilesChanged,
		FilesTotal:  plan.FilesChanged,
		Chunks:      written,
		ChunksTotal: plan.ChunksToWrite,
		Plan:        plan,
	})
	return report, nil
}

// ensureConfig returns the index generation this build writes into,
// creating one when the workspace has none or when the model/chunking/root
// changed. A new generation needs its vector dimension up front, so one
// probe embed runs first and is counted in the report. A lexical-only build
// (nil embedder) probes nothing and pins Dimension 0.
func (s *service) ensureConfig(ctx context.Context, plan *BuildPlan, opts BuildOptions, report *BuildReport) (*runtimetypes.WorkspaceIndexConfig, error) {
	if !plan.CutOver && plan.ConfigID != "" {
		return s.store.GetActiveWorkspaceIndexConfig(ctx, opts.WorkspaceID)
	}
	dimension := 0
	if s.embedder != nil {
		probe, err := s.embedder.Embed(ctx, "dimension probe")
		if err != nil {
			return nil, fmt.Errorf("workspaceindex: embedding model %q (provider %q) is unusable: %w", s.cfg.EmbedModel, s.cfg.EmbedProvider, err)
		}
		report.EmbedCalls++
		if len(probe) == 0 {
			return nil, fmt.Errorf("workspaceindex: embedding model %q returned a zero-length vector", s.cfg.EmbedModel)
		}
		dimension = len(probe)
	}
	cfg := &runtimetypes.WorkspaceIndexConfig{
		ID:            uuid.NewString(),
		WorkspaceID:   opts.WorkspaceID,
		Root:          plan.Root,
		EmbedModel:    s.cfg.EmbedModel,
		EmbedProvider: s.cfg.EmbedProvider,
		Dimension:     dimension,
		ChunkTokens:   s.cfg.ChunkTokens,
		ChunkOverlap:  s.cfg.OverlapLines,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.store.CreateWorkspaceIndexConfig(ctx, cfg); err != nil {
		return nil, err
	}
	plan.ConfigID = cfg.ID
	return cfg, nil
}

// stalePaths lists the indexed paths whose chunks this build must drop —
// changed or deleted files — and how many chunks they hold.
func (s *service) stalePaths(ctx context.Context, configID string, onDisk map[string]string) ([]string, int, error) {
	indexed, err := s.store.ListWorkspaceIndexedFiles(ctx, configID)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	var chunks int
	for _, f := range indexed {
		sha, present := onDisk[f.Path]
		if !present || sha != f.ContentSHA {
			out = append(out, f.Path)
			chunks += f.Chunks
		}
	}
	sort.Strings(out)
	return out, chunks, nil
}

// embedAndWrite walks the tree a second time, embedding the chunks of
// changed files through a bounded worker pool and writing them in batches.
// The second walk keeps peak memory at one file plus one batch regardless
// of repository size.
func (s *service) embedAndWrite(ctx context.Context, cfg *runtimetypes.WorkspaceIndexConfig, plan *BuildPlan, opts BuildOptions, onDisk map[string]string) (int, int, error) {
	indexed := map[string]string{}
	if !plan.CutOver && !opts.Force {
		files, err := s.store.ListWorkspaceIndexedFiles(ctx, cfg.ID)
		if err != nil {
			return 0, 0, err
		}
		for _, f := range files {
			indexed[f.Path] = f.ContentSHA
		}
	}

	var (
		pending      []Chunk
		written      int
		calls        int
		filesDone    int
		chunksTotal  = plan.ChunksToWrite
		filesChanged = plan.FilesChanged
	)

	// flush embeds and writes everything accumulated, only ever at a file
	// boundary: a file's chunks are written together or not at all, so a
	// cancelled build never leaves a file half-indexed under a sha that
	// reads as done. A failing write deletes every path in the flush before
	// returning.
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		rows, attempted, err := s.embedBatch(ctx, cfg, pending)
		calls += attempted
		if err != nil {
			return err
		}
		for start := 0; start < len(rows); start += writeBatchDefault {
			end := min(start+writeBatchDefault, len(rows))
			if err := s.store.AppendWorkspaceChunks(ctx, rows[start:end]...); err != nil {
				s.discardPartialFlush(ctx, cfg.ID, pending)
				return err
			}
			written += end - start
		}
		pending = pending[:0]
		return nil
	}

	_, err := walkWorkspace(ctx, cfg.Root, s.cfg.MaxFileBytes, func(f sourceFile) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sha, ok := indexed[f.RelPath]; ok && sha == f.SHA {
			return nil // unchanged: its chunks were never deleted
		}
		chunks, err := chunkFile(ctx, s.tokens, s.cfg.EmbedModel, f, s.cfg.ChunkTokens, s.cfg.OverlapLines)
		if err != nil {
			return err
		}
		filesDone++
		emit(opts.Progress, Progress{
			Phase:       PhaseEmbedding,
			Path:        f.RelPath,
			Files:       filesDone,
			FilesTotal:  filesChanged,
			Chunks:      written,
			ChunksTotal: chunksTotal,
		})
		pending = append(pending, chunks...)
		// Flush on the file boundary only — see flush's comment.
		if len(pending) >= writeBatchDefault {
			return flush()
		}
		return nil
	})
	if err != nil {
		return written, calls, err
	}
	if err := flush(); err != nil {
		return written, calls, err
	}
	return written, calls, nil
}

// discardPartialFlush drops the chunks of every path in a failed flush, on
// a context detached from cancellation so cleanup still runs.
func (s *service) discardPartialFlush(ctx context.Context, configID string, pending []Chunk) {
	seen := map[string]struct{}{}
	paths := make([]string, 0, len(pending))
	for _, c := range pending {
		if _, ok := seen[c.Path]; ok {
			continue
		}
		seen[c.Path] = struct{}{}
		paths = append(paths, c.Path)
	}
	if len(paths) == 0 {
		return
	}
	_ = s.store.DeleteWorkspaceChunksForPaths(context.WithoutCancel(ctx), configID, paths...)
}

// embedBatch embeds one batch of chunks through a bounded pool, preserving
// order. A single failure cancels the rest; it returns how many calls were
// attempted even on failure, so a failed build still reports the cost it
// incurred. Under a lexical-only generation it embeds nothing and the rows
// carry no vector — the FTS5 mirror is still written, so the chunks are still
// searchable.
func (s *service) embedBatch(ctx context.Context, cfg *runtimetypes.WorkspaceIndexConfig, chunks []Chunk) ([]*runtimetypes.WorkspaceChunk, int, error) {
	if s.embedder == nil {
		rows := make([]*runtimetypes.WorkspaceChunk, len(chunks))
		for i, c := range chunks {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			rows[i] = newChunkRow(cfg, c, nil)
		}
		return rows, 0, nil
	}
	rows := make([]*runtimetypes.WorkspaceChunk, len(chunks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.EmbedConcurrency)

	var attempted atomic.Int64
	var mu sync.Mutex
	for i, c := range chunks {
		g.Go(func() error {
			attempted.Add(1)
			vec, err := s.embedder.Embed(gctx, c.Text)
			if err != nil {
				return fmt.Errorf("embed %s:%d-%d: %w", c.Path, c.StartLine, c.EndLine, err)
			}
			if len(vec) != cfg.Dimension {
				return fmt.Errorf("%w: %s:%d-%d embedded to %d dimensions, index %s declares %d — the embedding model changed under this index; rebuild with --force",
					runtimetypes.ErrVectorDimensionMismatch, c.Path, c.StartLine, c.EndLine, len(vec), cfg.ID, cfg.Dimension)
			}
			row := newChunkRow(cfg, c, vec)
			mu.Lock()
			rows[i] = row
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, int(attempted.Load()), err
	}
	return rows, int(attempted.Load()), nil
}

// newChunkRow is the one place a chunk becomes a store row, so the vector-bearing
// and lexical-only paths cannot drift in what they record.
func newChunkRow(cfg *runtimetypes.WorkspaceIndexConfig, c Chunk, vec []float32) *runtimetypes.WorkspaceChunk {
	return &runtimetypes.WorkspaceChunk{
		ID:          uuid.NewString(),
		ConfigID:    cfg.ID,
		WorkspaceID: cfg.WorkspaceID,
		Path:        c.Path,
		StartLine:   c.StartLine,
		EndLine:     c.EndLine,
		ContentSHA:  c.SHA,
		Text:        c.Text,
		Vector:      vec,
	}
}

func emit(fn func(Progress), p Progress) {
	if fn != nil {
		fn(p)
	}
}
