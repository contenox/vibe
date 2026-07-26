package localtools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// listSizeNoticeThreshold is the size above which a listing appends a compact
// human-readable size next to a file name. Kept high enough that ordinary
// source and text files never carry the annotation — it exists purely to flag
// files large enough that a model should think twice before read_file'ing
// them.
const listSizeNoticeThreshold = 1 << 20 // 1 MiB

// sniffBinaryBytes bounds how much of a file this package reads to classify it
// as binary vs. text. 512 bytes catches the common binary formats (ELF/PE/
// Mach-O magic, PNG/JPEG/GIF headers, gzip/zip) cheaply, even against a
// multi-gigabyte file.
const sniffBinaryBytes = 512

// binaryInvalidUTF8Fraction is the share of a sniffed sample that must fail to
// decode as UTF-8 before the sample is classified binary on that basis alone
// (independent of the NUL-byte check in isBinarySample).
const binaryInvalidUTF8Fraction = 0.3

// isExecutable reports whether info's regular-file mode has any executable bit
// set (owner, group, or other).
//
// Limitation: on Windows os.FileMode carries no meaningful executable bit, so
// this always reports false there. That is a gap in the annotation, not a
// security control — nothing in this package relies on it to deny access.
func isExecutable(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

// humanSize renders a byte count as a compact binary-unit string, e.g.
// humanSize(50746820) == "48 MiB". Values under 1 KiB print as whole bytes.
// The exact byte count is reported alongside this wherever it is used, so
// nothing is lost to the rounding.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	if val >= 10 {
		return fmt.Sprintf("%.0f %ciB", val, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %ciB", val, "KMGTPE"[exp])
}

// isBinarySample applies a cheap, best-effort text/binary heuristic to a
// content prefix: the sample is binary if it contains a NUL byte — never valid
// in well-formed UTF-8 text — or if more than binaryInvalidUTF8Fraction of it
// fails to decode as UTF-8.
//
// Known limits: legacy 8-bit encodings (Latin-1, Shift-JIS) are not valid
// UTF-8 and can be misclassified as binary; conversely a binary format whose
// first sniffBinaryBytes happen to decode as valid UTF-8 (an archive with an
// all-ASCII header) can be misclassified as text. This trades precision for
// being cheap enough to run before every read — it is not a MIME sniffer.
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

// sniffPrefix returns the first sniffBinaryBytes of already-loaded content.
func sniffPrefix(content []byte) []byte {
	if len(content) > sniffBinaryBytes {
		return content[:sniffBinaryBytes]
	}
	return content
}

// sniffBinaryFile classifies a file on disk by reading at most
// sniffBinaryBytes from its start — independent of any read-size policy, so it
// stays cheap against a file too large for read_file to ever load.
//
// Every content-consuming tool calls this *before* loading the file, not
// after: sniffing a 1 MiB buffer you have already paid to read saves the
// context but not the I/O, and tools that never sniffed at all (grep, sed,
// count_stats) would happily emit binary garbage into the transcript.
func sniffBinaryFile(absPath string) (bool, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, sniffBinaryBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return isBinarySample(buf[:n]), nil
}

// fileSizeAndExecFlag renders "<size>[, executable]" for compact use inside
// teaching error messages, e.g. "48 MiB, executable" or "312 B".
func fileSizeAndExecFlag(info os.FileInfo) string {
	desc := humanSize(info.Size())
	if isExecutable(info) {
		desc += ", executable"
	}
	return desc
}

// describePathForError renders a short description of what a path actually is
// — kind, size, and any executable/binary flags — for error messages where the
// model expected something else (e.g. list_dir called on a file).
func describePathForError(absPath string, info os.FileInfo) string {
	kind := "regular file"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case info.IsDir():
		kind = "directory"
	case !info.Mode().IsRegular():
		kind = "special file"
	}
	desc := fmt.Sprintf("%s, %s", kind, humanSize(info.Size()))

	var flags []string
	if isExecutable(info) {
		flags = append(flags, "executable")
	}
	if info.Mode().IsRegular() {
		if binary, err := sniffBinaryFile(absPath); err == nil && binary {
			flags = append(flags, "binary")
		}
	}
	if len(flags) > 0 {
		desc += ", " + strings.Join(flags, " ")
	}
	return desc
}

// fileEntrySuffix renders the ls -F-style annotation appended to a
// non-directory listing entry: "*" when any executable bit is set, plus a
// compact human size in parentheses when the file exceeds
// listSizeNoticeThreshold. Small, non-executable files get no suffix, so
// ordinary listings stay compact.
func fileEntrySuffix(info os.FileInfo) string {
	var suffix string
	if isExecutable(info) {
		suffix += "*"
	}
	if info.Size() > listSizeNoticeThreshold {
		suffix += " (" + humanSize(info.Size()) + ")"
	}
	return suffix
}

// countLines returns the number of lines in s without allocating a slice of
// every line, which strings.Split would.
func countTextLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// ---------------------------------------------------------------------------
// Atomic writes
// ---------------------------------------------------------------------------

// AtomicFileIO is an optional interface a FileIO implementation may satisfy to
// provide its own crash-safe write. When it does, the tool defers to it.
type AtomicFileIO interface {
	WriteFileAtomic(ctx context.Context, path string, data []byte) error
}

// writeFileDurable replaces absPath's contents such that a failure partway
// through leaves the original file intact.
//
// A truncate-in-place write that fails mid-way — disk full is the case this
// package already handles explicitly, so it is known to happen — leaves a
// half-written file on disk *and* invalidates the session's read marker for
// it. The model is then holding stale content, cannot write without re-reading,
// and the file it re-reads is corrupt. Writing to a sibling temp file and
// renaming makes the replacement atomic on POSIX and on Windows for same-volume
// renames.
func (h *LocalFSTools) writeFileDurable(ctx context.Context, absPath string, data []byte) error {
	if aw, ok := h.fileIO.(AtomicFileIO); ok {
		return aw.WriteFileAtomic(ctx, absPath, data)
	}
	if !h.fileIOIsOS {
		// A custom (test or virtual) FileIO owns its own durability
		// semantics; going behind it with os-level calls would bypass the
		// abstraction entirely.
		return h.fileIO.WriteFile(ctx, absPath, data)
	}

	dir := filepath.Dir(absPath)
	// Same directory, therefore same filesystem, therefore the rename is
	// atomic rather than a copy.
	tmp, err := os.CreateTemp(dir, ".contenox-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	// Preserve the destination's mode when it already exists; a fresh temp
	// file is 0600, which would silently strip the executable bit from a
	// script being edited.
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(absPath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, absPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Listing output collector
// ---------------------------------------------------------------------------

// listCollector accumulates listing entries under a byte budget, so a walk
// stops as soon as its output can no longer fit rather than materialising the
// entire tree and discovering afterwards that it must be thrown away.
type listCollector struct {
	out       []string
	budget    int64 // 0 means unlimited
	bytes     int64
	offset    int // entries to skip before collecting
	seen      int // entries considered, including skipped ones
	scanned   int // filesystem entries visited, including filtered ones
	maxScan   int
	truncated bool
}

// add records an entry. It returns false when the collector is full and the
// walk should stop.
func (c *listCollector) add(entry string) bool {
	c.seen++
	if c.seen <= c.offset {
		return true
	}
	size := int64(len(entry) + 1) // +1 for the joining newline
	if c.budget > 0 && c.bytes+size > c.budget {
		c.truncated = true
		return false
	}
	c.out = append(c.out, entry)
	c.bytes += size
	return true
}

// visit records that a filesystem entry was examined, whether or not it was
// collected. It returns false once the scan ceiling is hit, which bounds the
// cost of a large offset on a pathological tree.
func (c *listCollector) visit() bool {
	c.scanned++
	if c.maxScan > 0 && c.scanned > c.maxScan {
		c.truncated = true
		return false
	}
	return true
}

// nextOffset is the offset a follow-up call should pass to resume.
func (c *listCollector) nextOffset() int {
	return c.offset + len(c.out)
}
