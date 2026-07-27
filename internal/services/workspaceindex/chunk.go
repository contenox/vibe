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

// Chunk is one indexed span of one file, carrying everything a hit needs to be a
// CITATION rather than a floating blob: the workspace-relative path, the 1-based
// inclusive line range, and the digest of the file it came from.
//
// SHA is the digest of the WHOLE FILE, not of Text. One value then answers both
// "did this file change since I indexed it" (incremental re-index) and "is this
// hit still true" (staleness marking at query time) — see
// runtimetypes.WorkspaceChunk.ContentSHA.
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

// walkStats records what selection threw away, so a build can report honestly
// what it did NOT look at instead of silently pretending the tree is smaller.
type walkStats struct {
	Selected         int
	SkippedBinary    int
	SkippedOversize  int
	SkippedGenerated int
	Bytes            int64
}

const (
	// maxFileBytesDefault caps how large a file may be and still be indexed.
	// 512 KiB is far above any hand-written source or prose file and far below
	// the generated artefacts (minified bundles, lockfiles, checked-in corpora,
	// SVG blobs) that would otherwise dominate an index with text no human
	// wrote and no question is about. Files over the cap are counted and
	// reported, never silently dropped.
	maxFileBytesDefault = 512 * 1024

	// chunkTokensDefault is the per-chunk token budget. Small enough to sit
	// inside every embedding model's input limit (the smallest common one is
	// 512 tokens), large enough that a chunk holds a whole function or a whole
	// section of prose rather than a fragment.
	chunkTokensDefault = 400

	// overlapLinesDefault is how many lines each chunk repeats from its
	// predecessor, so a passage that straddles a boundary is still wholly
	// present in one of the two chunks.
	overlapLinesDefault = 5

	// sniffBinaryBytes bounds how much of a file is read to classify it as
	// binary. Mirrors localtools/fs_util.go's constant of the same name.
	sniffBinaryBytes = 512

	// binaryInvalidUTF8Fraction mirrors localtools/fs_util.go: the share of a
	// sniffed sample that must fail to decode as UTF-8 before the sample is
	// called binary on that basis alone.
	binaryInvalidUTF8Fraction = 0.3
)

// ChunkTokensForContext derives a chunk budget from an embedding model's input
// limit, leaving a quarter of the window as headroom for the estimator's own
// error (the token counter is a ~4-chars-per-token heuristic, not the model's
// real tokenizer — see ollamatokenizer.EstimateTokenizer). Returns the default
// budget when the limit is unknown.
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

// TokenCounter is the token-budget seam: the one method the chunker needs from a
// tokenizer. *ollamatokenizer.EstimateTokenizer satisfies it directly, which is
// the intended production wiring — the same estimator acpsvc already budgets
// with, rather than a second heuristic invented here.
type TokenCounter interface {
	CountTokens(ctx context.Context, modelName string, prompt string) (int, error)
}

// isBinarySample mirrors localtools/fs_util.go's heuristic: a sample is binary
// if it contains a NUL byte — never valid in well-formed UTF-8 text — or if more
// than binaryInvalidUTF8Fraction of it fails to decode as UTF-8. Same known
// limits: legacy 8-bit encodings can be misclassified as binary, and a binary
// format with an all-ASCII header can be misclassified as text. Cheap beats
// precise for a filter whose only job is keeping garbage out of an index.
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

// generatedArtefactNames are DEPENDENCY LOCKFILES: machine-written manifests of
// resolved versions and hashes.
//
// maxFileBytesDefault already documents the intent — its comment names
// "lockfiles" among the generated artefacts "that would otherwise dominate an
// index with text no human wrote and no question is about" — but a SIZE cap
// cannot carry it out, because a lockfile under the cap is indexed like source.
// Measured on this repository: package-lock.json produced 132 chunks and go.sum
// 34, together 2.3% of the index and 166 embed calls spent on text nobody will
// ever ask a question about.
//
// This is deliberately a SHORT list of unambiguous machine-generated names, not
// a heuristic. Anything broader (test fixtures, golden files, generated code)
// is a judgement about what a workspace is FOR, and that belongs to the shared
// noise filter this package's noise.go is a copy of — not to a list here.
// Refusals are counted into walkStats and reported, exactly like the binary and
// oversize refusals, so "N files indexed" is never mistaken for "the whole tree".
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

// walkWorkspace streams every indexable file under root to visit, in a stable
// (lexical) order.
//
// Selection is the SAME noise filter the agent's find_files and beam's
// @-completion use — gitignore plus the skip-dir basenames (see noise.go) — so
// the human, the agent and the index agree on which files exist. On top of that
// it drops binaries (sniffed, not guessed from the extension), files over
// maxFileBytes, and dependency lockfiles (generatedArtefactNames) — all three
// counted into walkStats so a build reports what it refused rather than
// pretending the tree is smaller.
//
// Containment is NOT the filter's job: every candidate is resolved through
// vfs.Contain, so a symlink inside the tree pointing outside it is refused
// before its bytes are ever read.
func walkWorkspace(ctx context.Context, root string, maxFileBytes int64, visit func(sourceFile) error) (walkStats, error) {
	var stats walkStats
	ignore := gitignoreFor(root)

	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is not a reason to abandon the build; the
			// files we CAN see are still worth indexing.
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
		// A dependency lockfile is refused by NAME before its bytes are read:
		// the size cap that was meant to exclude it cannot, and embedding it
		// costs real calls for text no question is about.
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

		// Containment before content: a link or path that escapes the root is
		// refused here, not filtered by the noise rules above.
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
// approximate token budget.
//
// Line-oriented rather than character-oriented because a chunk's whole value is
// being citable: {path, startLine, endLine} names a region a human can open, and
// a chunk that begins mid-line names nothing. Overlap exists so a passage
// straddling a boundary survives whole in at least one chunk.
//
// A single line that exceeds the budget by itself becomes its own chunk rather
// than being split or dropped: a minified line is rare in a file that passed the
// binary and size filters, and silently discarding content is the one outcome
// worse than an oversized chunk.
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

	// Count each line once. Summing per-line estimates is slightly conservative
	// versus counting the joined text (the estimator has a per-call floor of 1
	// token), which errs toward chunks below the budget — the safe direction.
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
		// Step forward by at least one line, repeating `overlap` lines of
		// context. The max() guarantees progress even when overlap >= the chunk
		// size, which would otherwise loop forever.
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return chunks, nil
}

// splitLines splits content into lines WITHOUT a trailing empty element, so a
// file ending in a newline does not report a phantom final line. Line numbers
// returned by the chunker are 1-based indexes into this slice, which is what an
// editor shows.
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
