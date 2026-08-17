package runtimetypes_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func hasFTS5Search() bool {
	return runtimetypes.TestBackendDefault() == runtimetypes.TestBackendSQLite
}

func requireFTS5Search(t *testing.T) {
	t.Helper()
	if !hasFTS5Search() {
		t.Skip("SearchWorkspaceChunks speaks FTS5 MATCH/bm25; schema_postgres.sql mirrors the three columns but no Postgres query path exists yet")
	}
}

func setupWorkspaceIndexStore(t *testing.T) (context.Context, runtimetypes.Store, libdb.Exec) {
	t.Helper()
	return runtimetypes.SetupStoreExec(t)
}

func newIndexConfig(id, workspaceID string) *runtimetypes.WorkspaceIndexConfig {
	return &runtimetypes.WorkspaceIndexConfig{
		ID:            id,
		WorkspaceID:   workspaceID,
		Root:          "/tmp/ws",
		EmbedModel:    "nomic-embed-text",
		EmbedProvider: "ollama",
		Dimension:     4,
		ChunkTokens:   256,
		ChunkOverlap:  2,
		CreatedAt:     time.Now().UTC(),
	}
}

func newChunk(id, configID, path string, start, end int, text string, vec []float32) *runtimetypes.WorkspaceChunk {
	return &runtimetypes.WorkspaceChunk{
		ID:          id,
		ConfigID:    configID,
		WorkspaceID: "ws-1",
		Path:        path,
		StartLine:   start,
		EndLine:     end,
		ContentSHA:  "sha-" + path,
		Text:        text,
		Vector:      vec,
	}
}

func TestUnit_WorkspaceIndexConfig_CreateGetActive(t *testing.T) {
	ctx, store, _ := setupWorkspaceIndexStore(t)

	cfg := newIndexConfig("cfg-1", "ws-1")
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, cfg))

	got, err := store.GetWorkspaceIndexConfig(ctx, "cfg-1")
	require.NoError(t, err)
	require.Equal(t, cfg.WorkspaceID, got.WorkspaceID)
	require.Equal(t, cfg.Root, got.Root)
	require.Equal(t, cfg.EmbedModel, got.EmbedModel)
	require.Equal(t, cfg.EmbedProvider, got.EmbedProvider)
	require.Equal(t, 4, got.Dimension)
	require.Equal(t, 256, got.ChunkTokens)
	require.Equal(t, 2, got.ChunkOverlap)

	active, err := store.GetActiveWorkspaceIndexConfig(ctx, "ws-1")
	require.NoError(t, err)
	require.Equal(t, "cfg-1", active.ID)

	_, err = store.GetWorkspaceIndexConfig(ctx, "nope")
	require.ErrorIs(t, err, libdb.ErrNotFound)
	_, err = store.GetActiveWorkspaceIndexConfig(ctx, "no-such-workspace")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// TestUnit_WorkspaceIndexConfig_CreateOnceAndCutover asserts a config is create-once, the live config cannot be deleted, and a newer config becomes active by cutover, not mutation.
func TestUnit_WorkspaceIndexConfig_CreateOnceAndCutover(t *testing.T) {
	ctx, store, _ := setupWorkspaceIndexStore(t)

	first := newIndexConfig("cfg-1", "ws-1")
	first.CreatedAt = time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, first))

	dup := newIndexConfig("cfg-1", "ws-1")
	dup.EmbedModel = "some-other-model"
	require.Error(t, store.CreateWorkspaceIndexConfig(ctx, dup), "re-creating an index config id must be refused")

	stored, err := store.GetWorkspaceIndexConfig(ctx, "cfg-1")
	require.NoError(t, err)
	require.Equal(t, "nomic-embed-text", stored.EmbedModel, "the refused create must not have mutated the row")

	require.ErrorIs(t, store.DeleteWorkspaceIndexConfig(ctx, "cfg-1"), runtimetypes.ErrWorkspaceIndexConfigLive)

	second := newIndexConfig("cfg-2", "ws-1")
	second.EmbedModel = "mxbai-embed-large"
	second.Dimension = 8
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, second))

	active, err := store.GetActiveWorkspaceIndexConfig(ctx, "ws-1")
	require.NoError(t, err)
	require.Equal(t, "cfg-2", active.ID, "the newest config is the active one — that is the cutover")

	require.NoError(t, store.AppendWorkspaceChunks(ctx, newChunk("c1", "cfg-1", "a.md", 1, 3, "old generation", []float32{1, 0, 0, 0})))
	require.NoError(t, store.DeleteWorkspaceIndexConfig(ctx, "cfg-1"))
	_, err = store.GetWorkspaceIndexConfig(ctx, "cfg-1")
	require.ErrorIs(t, err, libdb.ErrNotFound)
	n, err := store.CountWorkspaceChunks(ctx, "cfg-1")
	require.NoError(t, err)
	require.Zero(t, n, "reaping a config must take its chunks with it")

	other := newIndexConfig("cfg-other", "ws-2")
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, other))
	activeOther, err := store.GetActiveWorkspaceIndexConfig(ctx, "ws-2")
	require.NoError(t, err)
	require.Equal(t, "cfg-other", activeOther.ID)

	list, err := store.ListWorkspaceIndexConfigs(ctx, "ws-1", nil, 0)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = store.ListWorkspaceIndexConfigs(ctx, "ws-1", nil, runtimetypes.MAXLIMIT+1)
	require.ErrorIs(t, err, runtimetypes.ErrLimitParamExceeded)
}

func TestUnit_WorkspaceChunks_RoundTripAndDeletes(t *testing.T) {
	ctx, store, _ := setupWorkspaceIndexStore(t)
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, newIndexConfig("cfg-1", "ws-1")))

	chunks := []*runtimetypes.WorkspaceChunk{
		newChunk("c1", "cfg-1", "docs/a.md", 1, 20, "retry backoff is explained here", []float32{1, 0, 0, 0}),
		newChunk("c2", "cfg-1", "docs/a.md", 18, 40, "and continues in the second chunk", []float32{0, 1, 0, 0}),
		newChunk("c3", "cfg-1", "docs/b.md", 1, 12, "unrelated prose about widgets", []float32{0, 0, 1, 0}),
	}
	require.NoError(t, store.AppendWorkspaceChunks(ctx, chunks...))

	n, err := store.CountWorkspaceChunks(ctx, "cfg-1")
	require.NoError(t, err)
	require.EqualValues(t, 3, n)

	all, err := store.ScanWorkspaceChunks(ctx, "cfg-1", 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "docs/a.md", all[0].Path)
	require.Equal(t, 1, all[0].StartLine)
	require.Equal(t, 20, all[0].EndLine)
	require.Equal(t, "retry backoff is explained here", all[0].Text)
	require.Equal(t, []float32{1, 0, 0, 0}, all[0].Vector, "the float32 blob must round-trip exactly")
	require.Equal(t, "sha-docs/a.md", all[0].ContentSHA)

	files, err := store.ListWorkspaceIndexedFiles(ctx, "cfg-1")
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Equal(t, "docs/a.md", files[0].Path)
	require.Equal(t, "sha-docs/a.md", files[0].ContentSHA)
	require.Equal(t, 2, files[0].Chunks)

	// Dropping one file's chunks leaves the other file intact, FTS mirror included.
	require.NoError(t, store.DeleteWorkspaceChunksForPaths(ctx, "cfg-1", "docs/a.md"))
	n, err = store.CountWorkspaceChunks(ctx, "cfg-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	if hasFTS5Search() {
		hits, err := store.SearchWorkspaceChunks(ctx, "cfg-1", `"retry"`, 10)
		require.NoError(t, err)
		require.Empty(t, hits, "deleting chunks must delete their FTS rows too, or search resurrects the dead")
	}

	require.NoError(t, store.DeleteWorkspaceChunksForConfig(ctx, "cfg-1"))
	n, err = store.CountWorkspaceChunks(ctx, "cfg-1")
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestUnit_WorkspaceIndex_FTS5IsAvailable asserts FTS5 works in the driver the product ships (modernc.org/sqlite).
func TestUnit_WorkspaceIndex_FTS5IsAvailable(t *testing.T) {
	requireFTS5Search(t)
	ctx, store, _ := setupWorkspaceIndexStore(t)
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, newIndexConfig("cfg-1", "ws-1")))
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, newIndexConfig("cfg-2", "ws-2")))

	require.NoError(t, store.AppendWorkspaceChunks(ctx,
		newChunk("c1", "cfg-1", "docs/retry.md", 1, 10, "exponential retry backoff for flaky backends", []float32{1, 0, 0, 0}),
		newChunk("c2", "cfg-1", "docs/other.md", 1, 10, "the widget catalogue and its taxonomy", []float32{0, 1, 0, 0}),
	))
	require.NoError(t, store.AppendWorkspaceChunks(ctx,
		newChunk("c3", "cfg-2", "docs/retry.md", 1, 10, "retry backoff in another workspace entirely", []float32{0, 0, 1, 0}),
	))

	hits, err := store.SearchWorkspaceChunks(ctx, "cfg-1", `"retry" OR "backoff"`, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the prefilter must match the retry chunk and nothing else in this config")
	require.Equal(t, "c1", hits[0].ID)
	require.Equal(t, []float32{1, 0, 0, 0}, hits[0].Vector, "a prefilter hit must arrive with its vector, ready to rank")
	require.Less(t, hits[0].Score, 0.0, "SQLite bm25 returns a negative relevance; ascending order is best-first")

	// bm25 ordering: the chunk mentioning both terms outranks the one mentioning one.
	require.NoError(t, store.AppendWorkspaceChunks(ctx,
		newChunk("c4", "cfg-1", "docs/half.md", 1, 10, "retry semantics only", []float32{0, 0, 0, 1}),
	))
	hits, err = store.SearchWorkspaceChunks(ctx, "cfg-1", `"retry" OR "backoff"`, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, "c1", hits[0].ID, "bm25 must rank the chunk matching both terms first")

	// Scoping: a search never crosses index generations.
	hits, err = store.SearchWorkspaceChunks(ctx, "cfg-2", `"retry"`, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "c3", hits[0].ID)

	// An empty match expression is a no-op, not a full scan.
	hits, err = store.SearchWorkspaceChunks(ctx, "cfg-1", "   ", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}

// TestUnit_WorkspaceIndex_LexicalOnlyGenerationIsSearchable asserts a dimension-0 generation has no vectors but its FTS5 mirror is searchable.
func TestUnit_WorkspaceIndex_LexicalOnlyGenerationIsSearchable(t *testing.T) {
	requireFTS5Search(t)
	ctx, store, _ := setupWorkspaceIndexStore(t)

	cfg := newIndexConfig("cfg-lex", "ws-lex")
	cfg.EmbedModel = ""
	cfg.EmbedProvider = ""
	cfg.Dimension = 0
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, cfg))

	require.NoError(t, store.AppendWorkspaceChunks(ctx,
		newChunk("l1", "cfg-lex", "docs/retry.md", 1, 10, "exponential retry backoff for flaky backends", nil),
		newChunk("l2", "cfg-lex", "docs/other.md", 1, 10, "the widget catalogue and its taxonomy", nil),
	))

	hits, err := store.SearchWorkspaceChunks(ctx, "cfg-lex", `"retry" OR "backoff"`, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the lexical leg must work with no vectors at all")
	require.Equal(t, "l1", hits[0].ID)
	require.Empty(t, hits[0].Vector, "a lexical-only chunk carries no vector")
	require.Less(t, hits[0].Score, 0.0)

	all, err := store.ScanWorkspaceChunks(ctx, "cfg-lex", 10)
	require.NoError(t, err)
	require.Len(t, all, 2, "an empty vector blob must round-trip, not read back as NULL")

	// The dimension check also holds in reverse: a vector cannot be smuggled into a lexical-only generation.
	err = store.AppendWorkspaceChunks(ctx, newChunk("l3", "cfg-lex", "docs/x.md", 1, 5, "with a vector", []float32{1, 0, 0, 0}))
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch)

	// Negative is a bug, not a mode.
	bad := newIndexConfig("cfg-bad", "ws-lex")
	bad.Dimension = -1
	require.Error(t, store.CreateWorkspaceIndexConfig(ctx, bad))
}

// TestUnit_WorkspaceChunks_DimensionMismatchIsRefused asserts the config's dimension is enforced both on write and on load.
func TestUnit_WorkspaceChunks_DimensionMismatchIsRefused(t *testing.T) {
	ctx, store, exec := setupWorkspaceIndexStore(t)
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, newIndexConfig("cfg-1", "ws-1")))

	err := store.AppendWorkspaceChunks(ctx, newChunk("bad", "cfg-1", "a.md", 1, 5, "wrong width", []float32{1, 2, 3}))
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch)
	n, err := store.CountWorkspaceChunks(ctx, "cfg-1")
	require.NoError(t, err)
	require.Zero(t, n, "a rejected batch must write nothing at all")

	// A batch spanning two configs is refused: it would smuggle vectors past the single-config dimension check.
	require.NoError(t, store.CreateWorkspaceIndexConfig(ctx, newIndexConfig("cfg-2", "ws-2")))
	err = store.AppendWorkspaceChunks(ctx,
		newChunk("m1", "cfg-1", "a.md", 1, 5, "ok", []float32{1, 0, 0, 0}),
		newChunk("m2", "cfg-2", "a.md", 1, 5, "ok", []float32{1, 0, 0, 0}),
	)
	require.ErrorIs(t, err, runtimetypes.ErrWorkspaceChunkConfigMixed)

	// Read side: plant a 3-float vector directly under a dimension-4 config.
	_, err = exec.ExecContext(ctx, `
		INSERT INTO workspace_chunks (id, config_id, workspace_id, path, start_line, end_line, content_sha, text, vector, created_at)
		VALUES ('planted', 'cfg-1', 'ws-1', 'a.md', 1, 5, 'sha', 'planted text', $1, $2)`,
		runtimetypes.EncodeVector([]float32{1, 2, 3}), time.Now().UTC())
	require.NoError(t, err)

	_, err = store.ScanWorkspaceChunks(ctx, "cfg-1", 10)
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch, "a mismatched vector must be a load error, never a silent wrong answer")
}

func TestUnit_WorkspaceIndex_VectorCodecRoundTrip(t *testing.T) {
	vec := []float32{0, 1, -1, 0.5, 1e-7, -3.25}
	blob := runtimetypes.EncodeVector(vec)
	require.Len(t, blob, len(vec)*4)

	got, err := runtimetypes.DecodeVector(blob, len(vec))
	require.NoError(t, err)
	require.Equal(t, vec, got)

	_, err = runtimetypes.DecodeVector(blob, len(vec)+1)
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch)

	_, err = runtimetypes.DecodeVector([]byte{1, 2, 3}, 0)
	require.ErrorIs(t, err, runtimetypes.ErrVectorDimensionMismatch, "a blob that is not a whole number of float32s is corrupt")

	// dimension <= 0 means "do not check", for callers with no config in hand.
	got, err = runtimetypes.DecodeVector(blob, 0)
	require.NoError(t, err)
	require.Equal(t, vec, got)
}
