// Package workspaceindex builds and queries a local semantic index over a
// workspace: `contenox index` fills it, `contenox search` reads it, and a local
// tool exposes the same read to the agent.
//
// It composes seams that already exist rather than introducing any of its own
// (docs/development/blueprints/workspace-index.md, ratified 2026-07-27 —
// "this should map 1:1 into everything we already do including the store
// interfaces etc, there is ZERO new invention here"):
//
//   - embeddings — llmrepo's Embed, behind the one-method Embedder seam below
//   - persistence — runtimetypes' store interface, over the same SQLite file
//     everything else uses (workspace_index_configs, workspace_chunks, and the
//     FTS5 mirror)
//   - containment — vfs.Contain, on every walked candidate
//   - file selection — the gitignore/skip-dir matcher find_files and
//     @-completion share (noise.go, with the extraction TODO both other copies
//     carry)
//   - token budgeting — ollamatokenizer's estimator, the one acpsvc budgets with
//
// What it is NOT: a vector database, a service to run, multi-tenant, or a
// document-sync product. One local index per workspace. V1 accepts a linear
// cosine scan over an FTS5-narrowed candidate set; ANN is a later optimization
// gated on measured pain.
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

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// ErrNoIndex is the typed refusal every degraded read returns: this workspace
// has no index yet. It is a first-class result, not a failure — the blueprint's
// rule is that a missing index degrades to "run contenox index", never to a hard
// error, because many local setups have no embedding model pulled at all.
var ErrNoIndex = errors.New("no index for this workspace; run contenox index")

// ErrEmptyQuestion is returned by Query for a blank question. Embedding
// whitespace produces a vector that ranks arbitrary chunks highly, which reads
// as a confident wrong answer.
var ErrEmptyQuestion = errors.New("search question is empty")

const (
	// embedConcurrencyDefault bounds in-flight embed calls. Local Ollama
	// serializes embedding requests anyway, so a larger pool buys nothing
	// there; against a remote provider four concurrent calls keep the pipe busy
	// without looking like an attack from a laptop.
	embedConcurrencyDefault = 4

	// writeBatchDefault is how many embedded chunks accumulate before one
	// AppendWorkspaceChunks call. Bounded by the store's own per-call ceiling
	// (SQLite's host-parameter limit); this also bounds how much embedded text
	// is held in memory at once.
	writeBatchDefault = 64

	// candidateLimitDefault is how many chunks the FTS5 lexical prefilter hands
	// to the cosine stage. Large enough that the right chunk is nearly always
	// inside it, small enough that ranking is a scan of hundreds rather than of
	// the whole index — the "FTS5 narrows, vectors rank" split.
	candidateLimitDefault = 200

	// fullScanLimit bounds the degraded path taken ONLY when the lexical
	// prefilter matched nothing at all — a question sharing no term with the
	// corpus, which is precisely the case semantic search exists for. It is the
	// store's own list ceiling rather than a number invented here, so this can
	// never be an unbounded table scan. An index larger than this answers such
	// questions from a prefix of itself, and THAT is the measured pain that
	// would justify ANN.
	fullScanLimit = runtimetypes.MAXLIMIT

	// topKDefault / topKMax bound how many hits a query returns. topKMax exists
	// because every returned hit costs a staleness check (one file read), and
	// because fifty citations is already past what any caller reads.
	topKDefault = 8
	topKMax     = 50
)

// Embedder is the narrow seam onto embedding generation: one method, one
// direction, no model manager. The index depends on this and never on
// llmrepo's concrete manager, which is what lets every test in this package run
// against a deterministic fake with no model and no network. NewLLMRepoEmbedder
// adapts the real thing.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Store is the subset of runtimetypes.Store the index uses. runtimetypes.Store
// satisfies it, so production wiring passes the real store unchanged; the
// narrowing documents exactly which rows this service can touch.
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

// Service is the workspace index. Plan answers "what would this cost" without
// spending anything; Build spends it; Query reads; Status reports what exists.
type Service interface {
	// Plan reports what a Build would do — files, chunks, and above all the
	// number of embed calls — WITHOUT making a single one. The blueprint's
	// cost-honesty rule: one embed call per chunk is the dominant cost of this
	// feature, and the operator sees the number before it is spent.
	Plan(ctx context.Context, root string, opts BuildOptions) (*BuildPlan, error)

	// Build indexes root into the workspace's index, creating it if needed.
	// Incremental by default: only files whose content sha changed are
	// re-embedded, and files that disappeared drop their chunks. opts.Force
	// rebuilds everything.
	Build(ctx context.Context, root string, opts BuildOptions) (*BuildReport, error)

	// Query returns the topK chunks most similar to question, each a citation
	// (path + line range) with its text, its score, and whether the file has
	// changed under the index since it was written.
	Query(ctx context.Context, workspaceID string, question string, topK int) ([]Hit, error)

	// Status describes the workspace's active index, or ErrNoIndex.
	Status(ctx context.Context, workspaceID string) (*Status, error)
}

// Config is the service's construction-time policy. Every field has a documented
// default; the zero Config is usable except for EmbedModel, which names what
// produced the vectors and is recorded on every index generation.
type Config struct {
	// EmbedModel / EmbedProvider identify the embedding model behind Embedder.
	// They are recorded on the index config so a later build can tell whether
	// it may extend an index or must cut over to a new one.
	EmbedModel    string
	EmbedProvider string
	// ChunkTokens is the per-chunk token budget (see ChunkTokensForContext).
	ChunkTokens int
	// OverlapLines is how many lines each chunk repeats from its predecessor.
	// Zero means the default, not "no overlap": a struct field cannot tell the
	// two apart, and an index with no overlap loses every passage that straddles
	// a boundary.
	OverlapLines int
	// MaxFileBytes is the file-size cap; larger files are counted and skipped.
	MaxFileBytes int64
	// EmbedConcurrency bounds in-flight embed calls.
	EmbedConcurrency int
	// CandidateLimit is how many chunks the FTS5 prefilter feeds to cosine.
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
	// Progress, when set, is called as the build advances so a CLI can render
	// it. It is called from the build's own goroutine, never concurrently.
	Progress func(Progress)
}

// Phase names where a build is.
type Phase string

const (
	PhasePlanning  Phase = "planning"
	PhaseEmbedding Phase = "embedding"
	PhaseDone      Phase = "done"
)

// Progress is one step report. Totals come from the plan, so a caller can render
// a proportion from the first event onward.
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
	// ConfigID is the existing index generation this build would extend, or ""
	// when it would create one.
	ConfigID      string
	EmbedModel    string
	EmbedProvider string
	// CutOver reports that this build would create a NEW index generation
	// because the embedding model or chunking changed — the create-once
	// discipline, not a mutation of the existing index.
	CutOver bool
	// Files / Chunks describe the whole selected tree.
	Files  int
	Chunks int
	Bytes  int64
	// FilesChanged / EmbedCalls describe the WORK: one embed call per chunk of
	// a changed file. This is the number the operator is shown before starting.
	FilesChanged int
	EmbedCalls   int
	// ChunksReused / FilesDeleted describe what incremental avoids and cleans.
	ChunksReused int
	FilesDeleted int
	// SkippedBinary / SkippedOversize / SkippedGenerated are files selection
	// refused, reported so "N files indexed" is never mistaken for "the whole
	// tree". SkippedGenerated counts dependency lockfiles, which the size cap
	// was documented to exclude but could not (see generatedArtefactNames).
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
	// EmbedCalls counts the calls actually made, including the one dimension
	// probe a new index generation costs.
	EmbedCalls int
	Duration   time.Duration
}

// Status describes a workspace's active index.
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

// Hit is one ranked search result: a citation, its text, its score, and whether
// the file moved underneath it.
type Hit struct {
	Path      string
	StartLine int
	EndLine   int
	Text      string
	Score     float64
	// Stale means the file's content sha no longer matches what was indexed (or
	// the file is gone). A stale hit is still returned — the text may well
	// still be what the user wants — but it is never returned as if it were
	// current. Silently serving a hit whose file changed is a lie.
	Stale bool
}

type service struct {
	store    Store
	embedder Embedder
	tokens   TokenCounter
	cfg      Config
}

// New builds the index service over the store, an embedding seam, and a token
// counter. All three are interfaces: production passes runtimetypes.Store,
// NewLLMRepoEmbedder(...), and ollamatokenizer.NewEstimateTokenizer().
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
// rows" into ErrNoIndex so every caller degrades the same way.
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

// plan walks the tree, chunks it, and diffs it against what is already indexed,
// WITHOUT embedding anything. It returns the plan plus the per-file chunk counts
// keyed by path, which Build reuses so the diff is computed exactly once.
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
		// The embedding model, chunking, or root changed: extending would mix
		// vectors from two models in one table. A new generation is created and
		// cut over to instead — see runtimetypes.WorkspaceIndexConfig.
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
		plan.EmbedCalls += len(chunks)
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

// compatible reports whether an existing index generation may be EXTENDED by a
// build under the current config. Anything that would put differently-produced
// vectors, or vectors about a different tree, into one table means no.
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

	// Cost honesty: the caller learns the embed-call count BEFORE the first call.
	emit(opts.Progress, Progress{
		Phase:       PhasePlanning,
		FilesTotal:  plan.FilesChanged,
		ChunksTotal: plan.EmbedCalls,
		Plan:        plan,
	})

	indexConfig, err := s.ensureConfig(ctx, plan, opts, report)
	if err != nil {
		return nil, err
	}
	report.ConfigID = indexConfig.ID

	// Clear what this build replaces: everything on a forced/new generation,
	// otherwise just the files that changed or vanished.
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
			// Counted, not merely done: an incremental refresh that drops the
			// chunks of edited and deleted files and then reports "0 dropped" is
			// understating what it changed, which is the same class of dishonesty
			// as understating what it spent.
			report.ChunksDeleted = staleChunks
		}
	}

	// Nothing to embed: an incremental no-op still had to walk the tree to know
	// that, and still had to drop deleted files' chunks above.
	if plan.EmbedCalls == 0 {
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
		ChunksTotal: plan.EmbedCalls,
		Plan:        plan,
	})
	return report, nil
}

// ensureConfig returns the index generation this build writes into, creating one
// when the workspace has none or when the model/chunking/root changed.
//
// A new generation needs its vector DIMENSION up front, and only the model can
// say what that is — so one probe embed runs first. That call is not overhead:
// it turns "the configured model cannot embed" (the exact failure the
// chat-model-as-embedding-model bug produced) into an error before N real calls
// are spent, and it is counted in the report so the arithmetic still adds up.
func (s *service) ensureConfig(ctx context.Context, plan *BuildPlan, opts BuildOptions, report *BuildReport) (*runtimetypes.WorkspaceIndexConfig, error) {
	if !plan.CutOver && plan.ConfigID != "" {
		return s.store.GetActiveWorkspaceIndexConfig(ctx, opts.WorkspaceID)
	}
	probe, err := s.embedder.Embed(ctx, "dimension probe")
	if err != nil {
		return nil, fmt.Errorf("workspaceindex: embedding model %q (provider %q) is unusable: %w", s.cfg.EmbedModel, s.cfg.EmbedProvider, err)
	}
	report.EmbedCalls++
	if len(probe) == 0 {
		return nil, fmt.Errorf("workspaceindex: embedding model %q returned a zero-length vector", s.cfg.EmbedModel)
	}
	cfg := &runtimetypes.WorkspaceIndexConfig{
		ID:            uuid.NewString(),
		WorkspaceID:   opts.WorkspaceID,
		Root:          plan.Root,
		EmbedModel:    s.cfg.EmbedModel,
		EmbedProvider: s.cfg.EmbedProvider,
		Dimension:     len(probe),
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

// stalePaths lists the indexed paths whose chunks this build must drop: files
// whose content changed, and files that no longer exist. It also returns how
// many CHUNKS those paths hold, so the build report can state what it dropped
// instead of leaving the number at zero.
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

// embedAndWrite walks the tree a second time, embedding the chunks of changed
// files through a bounded worker pool and writing them in batches.
//
// The second walk is deliberate: the first (in plan) held no file content, so
// peak memory is one file plus one batch regardless of repository size. Re-reading
// and re-hashing costs microseconds against an embed call's milliseconds.
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
		chunksTotal  = plan.EmbedCalls
		filesChanged = plan.FilesChanged
	)

	// flush embeds and writes everything accumulated. It is only ever called on a
	// FILE BOUNDARY, which is what makes an interrupted build safe: a file's
	// chunks are written together or not at all, so a cancelled build never
	// leaves a file half-indexed under a sha that matches disk — which the next
	// incremental build would read as "already done" and skip forever.
	//
	// The one remaining window is a multi-statement write failing part-way, so a
	// failure there deletes every path in the flush before returning.
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

// discardPartialFlush drops the chunks of every path in a failed flush, so no
// file is left partially indexed under a sha that matches disk. It runs on a
// context detached from cancellation: the cleanup of a build that was cancelled
// is exactly the case that must still happen.
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

// embedBatch embeds one batch of chunks through a bounded pool, preserving order
// so the written rows read back in file order. A single failure cancels the rest
// of the batch: half-embedded chunks are worth nothing. It returns how many
// calls were ATTEMPTED alongside the rows, so a failed build still reports the
// cost it incurred.
func (s *service) embedBatch(ctx context.Context, cfg *runtimetypes.WorkspaceIndexConfig, chunks []Chunk) ([]*runtimetypes.WorkspaceChunk, int, error) {
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
			row := &runtimetypes.WorkspaceChunk{
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

func emit(fn func(Progress), p Progress) {
	if fn != nil {
		fn(p)
	}
}
