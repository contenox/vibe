package localtools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const listSizeNoticeThreshold = 1 << 20

const sniffBinaryBytes = 512

const binaryInvalidUTF8Fraction = 0.3

func isExecutable(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

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

func sniffPrefix(content []byte) []byte {
	if len(content) > sniffBinaryBytes {
		return content[:sniffBinaryBytes]
	}
	return content
}

func sniffBinarySample(sample []byte) bool {
	return isBinarySample(sample)
}

func fileSizeAndExecFlag(info os.FileInfo) string {
	desc := humanSize(info.Size())
	if isExecutable(info) {
		desc += ", executable"
	}
	return desc
}

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
	}
	if len(flags) > 0 {
		desc += ", " + strings.Join(flags, " ")
	}
	return desc
}

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

func countTextLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// AtomicFileIO is an optional interface a FileIO implementation may satisfy to
// provide its own crash-safe write; when it does, the tool defers to it.
type AtomicFileIO interface {
	WriteFileAtomic(ctx context.Context, path string, data []byte) error
}

func (h *LocalFSTools) writeFileDurable(ctx context.Context, absPath string, data []byte) error {
	if aw, ok := h.fileIO.(AtomicFileIO); ok {
		return aw.WriteFileAtomic(ctx, absPath, data)
	}
	return h.fileIO.WriteFile(ctx, absPath, data)
}

type listCollector struct {
	out       []string
	budget    int64
	bytes     int64
	offset    int
	seen      int
	scanned   int
	maxScan   int
	truncated bool
}

func (c *listCollector) add(entry string) bool {
	c.seen++
	if c.seen <= c.offset {
		return true
	}
	size := int64(len(entry) + 1)
	if c.budget > 0 && c.bytes+size > c.budget {
		c.truncated = true
		return false
	}
	c.out = append(c.out, entry)
	c.bytes += size
	return true
}

func (c *listCollector) visit() bool {
	c.scanned++
	if c.maxScan > 0 && c.scanned > c.maxScan {
		c.truncated = true
		return false
	}
	return true
}

func (c *listCollector) nextOffset() int {
	return c.offset + len(c.out)
}
