package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// ErrWorkspaceIndexConfigLive refuses deletion of the workspace's currently active index config.
var ErrWorkspaceIndexConfigLive = errors.New("workspace index config is live; create a new config and cut over instead")

// ErrVectorDimensionMismatch is returned when a vector's length disagrees with its index config's dimension.
var ErrVectorDimensionMismatch = errors.New("vector dimension does not match the index config")

// ErrWorkspaceChunkConfigMixed refuses a batch whose chunks do not all belong to one index config.
var ErrWorkspaceChunkConfigMixed = errors.New("workspace chunk batch spans multiple index configs")

// WorkspaceIndexConfig is one immutable generation of a workspace's retrieval index (table workspace_index_configs); Dimension 0 declares a lexical-only generation.
type WorkspaceIndexConfig struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspaceId"`
	Root          string    `json:"root"`
	EmbedModel    string    `json:"embedModel"`
	EmbedProvider string    `json:"embedProvider"`
	Dimension     int       `json:"dimension"`
	ChunkTokens   int       `json:"chunkTokens"`
	ChunkOverlap  int       `json:"chunkOverlapLines"`
	CreatedAt     time.Time `json:"createdAt"`
}

// WorkspaceChunk is one indexed span of one file (table workspace_chunks); Vector is a little-endian float32 blob whose length must match the owning config's Dimension.
type WorkspaceChunk struct {
	ID          string    `json:"id"`
	ConfigID    string    `json:"configId"`
	WorkspaceID string    `json:"workspaceId"`
	Path        string    `json:"path"`
	StartLine   int       `json:"startLine"`
	EndLine     int       `json:"endLine"`
	ContentSHA  string    `json:"contentSha"`
	Text        string    `json:"text"`
	Vector      []float32 `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
	// Score is the lexical prefilter's bm25 rank, populated by SearchWorkspaceChunks and not persisted.
	Score float64 `json:"score,omitempty"`
}

// WorkspaceIndexedFile is the (path, sha) pair the incremental re-index diffs against the working tree.
type WorkspaceIndexedFile struct {
	Path       string `json:"path"`
	ContentSHA string `json:"contentSha"`
	Chunks     int    `json:"chunks"`
}

const workspaceIndexConfigColumns = `id, workspace_id, root, embed_model, embed_provider, dimension, chunk_tokens, chunk_overlap_lines, created_at`

const workspaceChunkColumns = `id, config_id, workspace_id, path, start_line, end_line, content_sha, text, vector, created_at`

const workspaceChunkInsertBatchMax = 90

// EncodeVector serializes an embedding as a little-endian float32 blob.
func EncodeVector(vec []float32) []byte {
	out := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

// DecodeVector reverses EncodeVector, refusing any blob that is not exactly dimension floats wide; dimension <= 0 skips the check.
func DecodeVector(blob []byte, dimension int) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("%w: blob of %d bytes is not a whole number of float32s", ErrVectorDimensionMismatch, len(blob))
	}
	n := len(blob) / 4
	if dimension > 0 && n != dimension {
		return nil, fmt.Errorf("%w: stored vector has %d dimensions, config declares %d", ErrVectorDimensionMismatch, n, dimension)
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out, nil
}

func (s *store) CreateWorkspaceIndexConfig(ctx context.Context, cfg *WorkspaceIndexConfig) error {
	if cfg == nil {
		return errors.New("workspace_index_configs: nil config")
	}
	if cfg.Dimension < 0 {
		return fmt.Errorf("workspace_index_configs: create %s: dimension must not be negative, got %d", cfg.ID, cfg.Dimension)
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now().UTC()
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO workspace_index_configs
		(`+workspaceIndexConfigColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		cfg.ID, cfg.WorkspaceID, cfg.Root, cfg.EmbedModel, cfg.EmbedProvider,
		cfg.Dimension, cfg.ChunkTokens, cfg.ChunkOverlap, cfg.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("workspace_index_configs: create %s: %w", cfg.ID, err)
	}
	return nil
}

func (s *store) GetWorkspaceIndexConfig(ctx context.Context, id string) (*WorkspaceIndexConfig, error) {
	var cfg WorkspaceIndexConfig
	err := s.Exec.QueryRowContext(ctx, `
		SELECT `+workspaceIndexConfigColumns+`
		FROM workspace_index_configs WHERE id = $1`, id).Scan(
		&cfg.ID, &cfg.WorkspaceID, &cfg.Root, &cfg.EmbedModel, &cfg.EmbedProvider,
		&cfg.Dimension, &cfg.ChunkTokens, &cfg.ChunkOverlap, &cfg.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, fmt.Errorf("workspace_index_configs: get %s: %w", id, err)
	}
	return &cfg, nil
}

// GetActiveWorkspaceIndexConfig returns the workspace's newest config, the one searches run against.
func (s *store) GetActiveWorkspaceIndexConfig(ctx context.Context, workspaceID string) (*WorkspaceIndexConfig, error) {
	var cfg WorkspaceIndexConfig
	err := s.Exec.QueryRowContext(ctx, `
		SELECT `+workspaceIndexConfigColumns+`
		FROM workspace_index_configs
		WHERE workspace_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, workspaceID).Scan(
		&cfg.ID, &cfg.WorkspaceID, &cfg.Root, &cfg.EmbedModel, &cfg.EmbedProvider,
		&cfg.Dimension, &cfg.ChunkTokens, &cfg.ChunkOverlap, &cfg.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, fmt.Errorf("workspace_index_configs: active for %s: %w", workspaceID, err)
	}
	return &cfg, nil
}

func (s *store) ListWorkspaceIndexConfigs(ctx context.Context, workspaceID string, createdAtCursor *time.Time, limit int) ([]*WorkspaceIndexConfig, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+workspaceIndexConfigColumns+`
		FROM workspace_index_configs
		WHERE workspace_id = $1 AND created_at < $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, workspaceID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace_index_configs: list: %w", err)
	}
	defer rows.Close()

	out := []*WorkspaceIndexConfig{}
	for rows.Next() {
		var cfg WorkspaceIndexConfig
		if err := rows.Scan(
			&cfg.ID, &cfg.WorkspaceID, &cfg.Root, &cfg.EmbedModel, &cfg.EmbedProvider,
			&cfg.Dimension, &cfg.ChunkTokens, &cfg.ChunkOverlap, &cfg.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("workspace_index_configs: scan row: %w", err)
		}
		out = append(out, &cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace_index_configs: rows error: %w", err)
	}
	return out, nil
}

// DeleteWorkspaceIndexConfig reaps a superseded index generation and its
// chunks, refusing the workspace's live config with ErrWorkspaceIndexConfigLive.
func (s *store) DeleteWorkspaceIndexConfig(ctx context.Context, id string) error {
	cfg, err := s.GetWorkspaceIndexConfig(ctx, id)
	if err != nil {
		return err
	}
	active, err := s.GetActiveWorkspaceIndexConfig(ctx, cfg.WorkspaceID)
	if err != nil {
		return err
	}
	if active.ID == id {
		return fmt.Errorf("%w: %s is the active index for workspace %s", ErrWorkspaceIndexConfigLive, id, cfg.WorkspaceID)
	}
	if err := s.DeleteWorkspaceChunksForConfig(ctx, id); err != nil {
		return err
	}
	result, err := s.Exec.ExecContext(ctx, `DELETE FROM workspace_index_configs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("workspace_index_configs: delete %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// AppendWorkspaceChunks inserts a batch of chunks and mirrors their text into the FTS5 table in the same call, checking every vector against the owning config's dimension.
func (s *store) AppendWorkspaceChunks(ctx context.Context, chunks ...*WorkspaceChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	if len(chunks) > workspaceChunkInsertBatchMax {
		return fmt.Errorf("%w: %d chunks exceeds the %d-per-call batch bound", ErrAppendLimitExceeded, len(chunks), workspaceChunkInsertBatchMax)
	}
	configID := chunks[0].ConfigID
	for _, c := range chunks {
		if c == nil {
			return errors.New("workspace_chunks: nil chunk in batch")
		}
		if c.ConfigID != configID {
			return fmt.Errorf("%w: %s and %s", ErrWorkspaceChunkConfigMixed, configID, c.ConfigID)
		}
	}
	cfg, err := s.GetWorkspaceIndexConfig(ctx, configID)
	if err != nil {
		return fmt.Errorf("workspace_chunks: append: %w", err)
	}

	now := time.Now().UTC()
	const cols = 10
	valueStrings := make([]string, 0, len(chunks))
	valueArgs := make([]any, 0, len(chunks)*cols)
	ftsStrings := make([]string, 0, len(chunks))
	ftsArgs := make([]any, 0, len(chunks)*3)

	for i, c := range chunks {
		if len(c.Vector) != cfg.Dimension {
			return fmt.Errorf("%w: chunk %s (%s:%d-%d) has %d dimensions, config %s declares %d",
				ErrVectorDimensionMismatch, c.ID, c.Path, c.StartLine, c.EndLine, len(c.Vector), cfg.ID, cfg.Dimension)
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = now
		}
		valueStrings = append(valueStrings, placeholderGroup(i*cols+1, cols))
		valueArgs = append(valueArgs,
			c.ID, c.ConfigID, c.WorkspaceID, c.Path, c.StartLine, c.EndLine,
			c.ContentSHA, c.Text, EncodeVector(c.Vector), c.CreatedAt,
		)
		ftsStrings = append(ftsStrings, placeholderGroup(i*3+1, 3))
		ftsArgs = append(ftsArgs, c.Text, c.ID, c.ConfigID)
	}

	if _, err := s.Exec.ExecContext(ctx, `
		INSERT INTO workspace_chunks
		(`+workspaceChunkColumns+`)
		VALUES `+strings.Join(valueStrings, ", "), valueArgs...); err != nil {
		return fmt.Errorf("workspace_chunks: append %d rows: %w", len(chunks), err)
	}
	if _, err := s.Exec.ExecContext(ctx, `
		INSERT INTO workspace_chunks_fts (text, chunk_id, config_id)
		VALUES `+strings.Join(ftsStrings, ", "), ftsArgs...); err != nil {
		return fmt.Errorf("workspace_chunks_fts: append %d rows: %w", len(chunks), err)
	}
	return nil
}

// ListWorkspaceIndexedFiles returns one row per indexed file with its recorded content sha, ordered by path.
func (s *store) ListWorkspaceIndexedFiles(ctx context.Context, configID string) ([]WorkspaceIndexedFile, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT path, MIN(content_sha), COUNT(*)
		FROM workspace_chunks
		WHERE config_id = $1
		GROUP BY path
		ORDER BY path`, configID)
	if err != nil {
		return nil, fmt.Errorf("workspace_chunks: list indexed files: %w", err)
	}
	defer rows.Close()

	out := []WorkspaceIndexedFile{}
	for rows.Next() {
		var f WorkspaceIndexedFile
		if err := rows.Scan(&f.Path, &f.ContentSHA, &f.Chunks); err != nil {
			return nil, fmt.Errorf("workspace_chunks: scan indexed file: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace_chunks: rows error: %w", err)
	}
	return out, nil
}

// DeleteWorkspaceChunksForPaths drops every chunk of the named files, FTS mirror included.
func (s *store) DeleteWorkspaceChunksForPaths(ctx context.Context, configID string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, configID)
	holes := make([]string, len(paths))
	for i, p := range paths {
		holes[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, p)
	}
	in := "(" + strings.Join(holes, ", ") + ")"

	// FTS first: it is addressed by chunk_id, which only the base table knows.
	if _, err := s.Exec.ExecContext(ctx, `
		DELETE FROM workspace_chunks_fts
		WHERE chunk_id IN (
			SELECT id FROM workspace_chunks WHERE config_id = $1 AND path IN `+in+`
		)`, args...); err != nil {
		return fmt.Errorf("workspace_chunks_fts: delete paths: %w", err)
	}
	if _, err := s.Exec.ExecContext(ctx, `
		DELETE FROM workspace_chunks WHERE config_id = $1 AND path IN `+in, args...); err != nil {
		return fmt.Errorf("workspace_chunks: delete paths: %w", err)
	}
	return nil
}

// DeleteWorkspaceChunksForConfig empties one index generation — the --force
// rebuild path, and the reap half of DeleteWorkspaceIndexConfig.
func (s *store) DeleteWorkspaceChunksForConfig(ctx context.Context, configID string) error {
	if _, err := s.Exec.ExecContext(ctx, `
		DELETE FROM workspace_chunks_fts WHERE config_id = $1`, configID); err != nil {
		return fmt.Errorf("workspace_chunks_fts: delete config: %w", err)
	}
	if _, err := s.Exec.ExecContext(ctx, `
		DELETE FROM workspace_chunks WHERE config_id = $1`, configID); err != nil {
		return fmt.Errorf("workspace_chunks: delete config: %w", err)
	}
	return nil
}

// SearchWorkspaceChunks is the lexical prefilter: FTS5 MATCH capped at limit, ordered ascending by bm25 since SQLite's bm25 is more-negative-is-better.
func (s *store) SearchWorkspaceChunks(ctx context.Context, configID string, match string, limit int) ([]*WorkspaceChunk, error) {
	if strings.TrimSpace(match) == "" {
		return []*WorkspaceChunk{}, nil
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	cfg, err := s.GetWorkspaceIndexConfig(ctx, configID)
	if err != nil {
		return nil, fmt.Errorf("workspace_chunks: search: %w", err)
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+prefixColumns("c.", workspaceChunkColumns)+`, bm25(workspace_chunks_fts)
		FROM workspace_chunks_fts
		JOIN workspace_chunks c ON c.id = workspace_chunks_fts.chunk_id
		WHERE workspace_chunks_fts MATCH $1 AND workspace_chunks_fts.config_id = $2
		ORDER BY bm25(workspace_chunks_fts)
		LIMIT $3`, match, configID, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace_chunks: fts search: %w", err)
	}
	defer rows.Close()
	return scanWorkspaceChunks(rows, cfg.Dimension, true)
}

// ScanWorkspaceChunks reads chunks in insertion order, capped at limit.
func (s *store) ScanWorkspaceChunks(ctx context.Context, configID string, limit int) ([]*WorkspaceChunk, error) {
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	cfg, err := s.GetWorkspaceIndexConfig(ctx, configID)
	if err != nil {
		return nil, fmt.Errorf("workspace_chunks: scan: %w", err)
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+workspaceChunkColumns+`
		FROM workspace_chunks
		WHERE config_id = $1
		ORDER BY path, start_line
		LIMIT $2`, configID, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace_chunks: scan: %w", err)
	}
	defer rows.Close()
	return scanWorkspaceChunks(rows, cfg.Dimension, false)
}

func (s *store) CountWorkspaceChunks(ctx context.Context, configID string) (int64, error) {
	var n int64
	err := s.Exec.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspace_chunks WHERE config_id = $1`, configID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("workspace_chunks: count: %w", err)
	}
	return n, nil
}

func scanWorkspaceChunks(rows *sql.Rows, dimension int, withScore bool) ([]*WorkspaceChunk, error) {
	out := []*WorkspaceChunk{}
	for rows.Next() {
		var c WorkspaceChunk
		var blob []byte
		dest := []any{
			&c.ID, &c.ConfigID, &c.WorkspaceID, &c.Path, &c.StartLine, &c.EndLine,
			&c.ContentSHA, &c.Text, &blob, &c.CreatedAt,
		}
		if withScore {
			dest = append(dest, &c.Score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("workspace_chunks: scan row: %w", err)
		}
		vec, err := DecodeVector(blob, dimension)
		if err != nil {
			return nil, fmt.Errorf("workspace_chunks: chunk %s (%s:%d-%d): %w", c.ID, c.Path, c.StartLine, c.EndLine, err)
		}
		c.Vector = vec
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace_chunks: rows error: %w", err)
	}
	return out, nil
}

func placeholderGroup(start, count int) string {
	holes := make([]string, count)
	for i := range holes {
		holes[i] = fmt.Sprintf("$%d", start+i)
	}
	return "(" + strings.Join(holes, ", ") + ")"
}

func prefixColumns(prefix, columns string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, ", ")
}
