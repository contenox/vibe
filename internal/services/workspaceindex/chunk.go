package workspaceindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/contenox/beam/internal/services/vfs"
)

// Chunk is one indexed span of one file: a citation (workspace-relative
// path, 1-based inclusive line range) plus the digest of the file it came
// from. SHA is the digest of the whole file, not of Text — it answers both
// "did this file change" (incremental re-index) and "is this hit still
// true" (staleness marking at query time).
type Chunk struct {
	Path      string
	StartLine int
	EndLine   int
	SHA       string
	Text      string
}

// sourceFile is one file that survived selection, with its content already read.
type sourceFile struct {
	RelPath string
	AbsPath string
	Size    int64
	Content string
	SHA     string
}

// walkStats records what selection threw away, so a build can report what
// it did not look at instead of pretending the tree is smaller.
type walkStats struct {
	Selected         int
	SkippedBinary    int
	SkippedOversize  int
	SkippedGenerated int
	Bytes            int64
}

const (
	// maxFileBytesDefault caps file size for indexing: far above any
	// hand-written file, far below the generated artefacts (minified
	// bundles, checked-in corpora) that would otherwise dominate the index.
	// Files over the cap are counted, never silently dropped.
	maxFileBytesDefault = 512 * 1024

	// chunkTokensDefault is the per-chunk token budget: under the smallest
	// common embedding-model input limit (512 tokens), large enough for a
	// whole function or section of prose.
	chunkTokensDefault = 400

	// overlapLinesDefault is how many lines each chunk repeats from its
	// predecessor, so a passage straddling a boundary survives whole.
	overlapLinesDefault = 5

	// sniffBinaryBytes bounds how much of a file is read to classify it as
	// binary. Mirrors localtools/fs_util.go.
	sniffBinaryBytes = 512

	// binaryInvalidUTF8Fraction mirrors localtools/fs_util.go: the share of
	// a sniffed sample that must fail UTF-8 decode to call it binary.
	binaryInvalidUTF8Fraction = 0.3
)

// ChunkTokensForContext derives a chunk budget from an embedding model's
// input limit, leaving a quarter as headroom for the estimator's own error
// (a ~4-chars-per-token heuristic, not the model's real tokenizer). Returns
// the default budget when the limit is unknown.
func ChunkTokensForContext(contextLength int) int {
	if contextLength <= 0 {
		return chunkTokensDefault
	}
	budget := contextLength * 3 / 4
	if budget < 64 {
		return 64
	}
	return budget
}

// TokenCounter is the token-budget seam the chunker needs.
// *ollamatokenizer.EstimateTokenizer satisfies it, the same estimator acpsvc
// already budgets with.
type TokenCounter interface {
	CountTokens(ctx context.Context, modelName string, prompt string) (int, error)
}

// isBinarySample mirrors localtools/fs_util.go's heuristic: binary if it
// contains a NUL byte, or if more than binaryInvalidUTF8Fraction of it fails
// to decode as UTF-8.
func isBinarySample(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	invalid := 0
	for i := 0; i < len(sample); {
		r, size := utf8.DecodeRune(sample[i:])
		if r == utf8.RuneError && size == 1 {
			invalid++
			i++
			continue
		}
		i += size
	}
	return float64(invalid)/float64(len(sample)) > binaryInvalidUTF8Fraction
}

// generatedArtefactNames are dependency lockfiles: machine-written manifests
// the size cap alone cannot exclude. Deliberately a short, explicit list
// rather than a heuristic — broader generated-file judgement belongs to the
// shared noise filter (noise.go). Refusals are counted into walkStats.
var generatedArtefactNames = map[string]bool{
	"go.sum":              true,
	"package-lock.json":   true,
	"yarn.lock":           true,
	"pnpm-lock.yaml":      true,
	"npm-shrinkwrap.json": true,
	"Cargo.lock":          true,
	"poetry.lock":         true,
	"Pipfile.lock":        true,
	"composer.lock":       true,
	"Gemfile.lock":        true,
	"packages.lock.json":  true,
	"flake.lock":          true,
}

// isGeneratedArtefact reports whether a file basename is a dependency lockfile.
func isGeneratedArtefact(base string) bool { return generatedArtefactNames[base] }

func fileSHA(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// walkWorkspace streams every indexable file under root to visit, in a
// stable (lexical) order. Selection uses the same noise filter as the
// agent's find_files and beam's @-completion (see noise.go), then drops
// binaries, oversize files, and dependency lockfiles — all counted into
// walkStats. Every candidate is resolved through vfs.Contain, so an
// escaping symlink is refused before its bytes are read.
func walkWorkspace(ctx context.Context, root string, maxFileBytes int64, visit func(sourceFile) error) (walkStats, error) {
	var stats walkStats
	ignore := gitignoreFor(root)

	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory does not abort the build.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if abs == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		base := d.Name()

		if d.IsDir() {
			if skipDir(base) || ignore.Match(rel, true) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks and devices are not files to index; only regular files are.
		if !d.Type().IsRegular() {
			return nil
		}
		if ignore.Match(rel, false) {
			return nil
		}
		// Refused by name before its bytes are read; the size cap alone cannot exclude it.
		if isGeneratedArtefact(base) {
			stats.SkippedGenerated++
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.Size() > maxFileBytes {
			stats.SkippedOversize++
			return nil
		}

		// Containment before content: an escaping link is refused here.
		safeAbs, containErr := vfs.Contain(root, abs)
		if containErr != nil {
			return nil
		}
		content, readErr := os.ReadFile(safeAbs)
		if readErr != nil {
			return nil
		}
		if isBinarySample(content[:min(len(content), sniffBinaryBytes)]) {
			stats.SkippedBinary++
			return nil
		}

		stats.Selected++
		stats.Bytes += info.Size()
		return visit(sourceFile{
			RelPath: rel,
			AbsPath: safeAbs,
			Size:    info.Size(),
			Content: string(content),
			SHA:     fileSHA(content),
		})
	})
	if err != nil {
		return stats, err
	}
	return stats, nil
}

// chunkFile splits a file into overlapping, line-oriented chunks sized by an
// approximate token budget. Line-oriented so a chunk names a citable region
// {path, startLine, endLine} rather than a mid-line fragment. A single line
// exceeding the budget becomes its own chunk rather than being split or
// dropped.
func chunkFile(ctx context.Context, tokens TokenCounter, model string, f sourceFile, budget, overlap int) ([]Chunk, error) {
	if budget <= 0 {
		budget = chunkTokensDefault
	}
	if overlap < 0 {
		overlap = 0
	}
	lines := splitLines(f.Content)
	if len(lines) == 0 {
		return nil, nil
	}

	// Summing per-line estimates is slightly conservative, erring toward chunks below budget.
	costs := make([]int, len(lines))
	for i, line := range lines {
		n, err := tokens.CountTokens(ctx, model, line)
		if err != nil {
			return nil, fmt.Errorf("count tokens for %s:%d: %w", f.RelPath, i+1, err)
		}
		costs[i] = n
	}

	var chunks []Chunk
	start := 0
	for start < len(lines) {
		end, used := start, 0
		for end < len(lines) {
			next := used + costs[end]
			if end > start && next > budget {
				break
			}
			used = next
			end++
		}
		if end == start { // a single over-budget line: keep it whole
			end = start + 1
		}
		text := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, Chunk{
				Path:      f.RelPath,
				StartLine: start + 1,
				EndLine:   end,
				SHA:       f.SHA,
				Text:      text,
			})
		}
		if end >= len(lines) {
			break
		}
		// Step forward by at least one line; guards against overlap >= chunk size looping forever.
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return chunks, nil
}

// splitLines splits content into lines without a trailing empty element, so
// a file ending in a newline does not report a phantom final line.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}
