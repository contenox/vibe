package localtools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	toolOutputEnvVar = "CONTENOX_TOOL_OUTPUT_DIR"

	maxSpoolFiles = 256

	maxSpoolAge = 7 * 24 * time.Hour

	maxShellSpoolBytes = 8 * 1024 * 1024
)

func toolOutputRoot() string {
	if v := strings.TrimSpace(os.Getenv(toolOutputEnvVar)); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".contenox", "tool_output")
	}
	return filepath.Join(os.TempDir(), "contenox-tool-output")
}

func spoolBucket(ctx context.Context) string {
	if sid := strings.TrimSpace(sessionIDFromContext(ctx)); sid != "" {
		return "session-" + sanitizeBucket(sid)
	}
	return "day-" + time.Now().UTC().Format("2006-01-02")
}

func sanitizeBucket(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "unknown"
	}
	return out
}

func newSpoolFile(ctx context.Context, tool string) (*os.File, string, error) {
	root := toolOutputRoot()
	// Pruned before creating this run's bucket, so retention (which removes empty bucket dirs) can never delete the fresh directory out from under the open.
	pruneToolOutput(root, maxSpoolFiles, maxSpoolAge)
	bucket := filepath.Join(root, spoolBucket(ctx))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s-%s-%d.txt", sanitizeBucket(tool), time.Now().UTC().Format("150405.000"), os.Getpid())
	path := filepath.Join(bucket, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

func pruneToolOutput(root string, maxFiles int, maxAge time.Duration) {
	type spooled struct {
		path string
		mod  time.Time
	}
	var files []spooled
	dirs := map[string]struct{}{}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root {
				dirs[path] = struct{}{}
			}
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		files = append(files, spooled{path: path, mod: info.ModTime()})
		return nil
	})

	now := time.Now()
	kept := files[:0]
	for _, f := range files {
		if maxAge > 0 && now.Sub(f.mod) > maxAge {
			_ = os.Remove(f.path)
			continue
		}
		kept = append(kept, f)
	}

	if maxFiles > 0 && len(kept) > maxFiles {
		sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
		for _, f := range kept[:len(kept)-maxFiles] {
			_ = os.Remove(f.path)
		}
	}

	// Drop now-empty bucket dirs (deepest first).
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Slice(dirList, func(i, j int) bool { return len(dirList[i]) > len(dirList[j]) })
	for _, d := range dirList {
		_ = os.Remove(d) // removes only if empty
	}
}

type spoolWriter struct {
	tool   string
	ctx    context.Context
	budget int64

	total int64

	head    bytes.Buffer
	headCap int64

	tail    []byte
	tailCap int64

	pre bytes.Buffer

	file     *os.File
	path     string
	spooled  int64
	spoolCap int64
	spoolErr error
	overCap  bool
	spilled  bool
}

func newSpoolWriter(ctx context.Context, tool string, budget int64) *spoolWriter {
	if budget < 2 {
		budget = 2
	}
	head := budget / 5 // 20%
	if head < 1 {
		head = 1
	}
	tail := budget - head // 80%
	if tail < 1 {
		tail = 1
	}
	return &spoolWriter{
		tool:     tool,
		ctx:      ctx,
		budget:   budget,
		headCap:  head,
		tailCap:  tail,
		spoolCap: maxShellSpoolBytes,
	}
}

func (w *spoolWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += int64(n)

	if int64(w.head.Len()) < w.headCap {
		room := w.headCap - int64(w.head.Len())
		if room > int64(len(p)) {
			room = int64(len(p))
		}
		w.head.Write(p[:room])
	}

	w.appendTail(p)

	if !w.spilled {
		if w.total <= w.budget {
			w.pre.Write(p)
		} else {
			w.startSpill()
			w.spillBytes(p)
		}
	} else {
		w.spillBytes(p)
	}

	if w.overCap {
		// Spool cap reached: further output would be discarded anyway; run() treats this as truncation, not a failure.
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (w *spoolWriter) appendTail(p []byte) {
	if int64(len(p)) >= w.tailCap {
		w.tail = append(w.tail[:0], p[int64(len(p))-w.tailCap:]...)
		return
	}
	w.tail = append(w.tail, p...)
	if int64(len(w.tail)) > w.tailCap {
		w.tail = w.tail[int64(len(w.tail))-w.tailCap:]
	}
}

func (w *spoolWriter) startSpill() {
	w.spilled = true
	f, path, err := newSpoolFile(w.ctx, w.tool)
	if err != nil {
		w.spoolErr = err
		return
	}
	w.file = f
	w.path = path
	// Flush the pre-budget bytes first so the spool holds the whole stream.
	w.spillBytes(w.pre.Bytes())
	w.pre.Reset()
}

func (w *spoolWriter) spillBytes(p []byte) {
	if w.file == nil || w.overCap || w.spoolErr != nil {
		return
	}
	room := w.spoolCap - w.spooled
	if room <= 0 {
		w.overCap = true
		return
	}
	if int64(len(p)) > room {
		p = p[:room]
		w.overCap = true
	}
	nn, err := w.file.Write(p)
	w.spooled += int64(nn)
	if err != nil {
		w.spoolErr = err
	}
}

func (w *spoolWriter) truncated() bool { return w.total > w.budget }

func (w *spoolWriter) inlineOutput() string {
	if w.truncated() {
		return w.splitText()
	}
	return w.pre.String()
}

func (w *spoolWriter) fullText() (string, bool) {
	if !w.spilled {
		return w.pre.String(), true
	}
	if w.spoolErr != nil || w.overCap || w.path == "" {
		return "", false
	}
	b, err := os.ReadFile(w.path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (w *spoolWriter) close() string {
	if w.file != nil {
		_ = w.file.Close()
	}
	return w.path
}

func (w *spoolWriter) discard() {
	if w.file != nil {
		_ = w.file.Close()
		_ = os.Remove(w.path)
		w.file = nil
		w.path = ""
	}
}

func (w *spoolWriter) splitText() string {
	headBytes := w.head.Bytes()
	tailBytes := w.tail
	omitted := w.total - int64(len(headBytes)) - int64(len(tailBytes))
	if omitted < 0 {
		omitted = 0
	}
	var loc string
	switch {
	case w.spoolErr != nil:
		loc = "full output could not be spooled: " + w.spoolErr.Error()
	case w.path != "":
		loc = fmt.Sprintf("full output: %s (%s)", w.path, humanSize(w.total))
		if w.overCap {
			loc = fmt.Sprintf("full output (first %s of a larger stream): %s", humanSize(w.spooled), w.path)
		}
	default:
		loc = "full output not available"
	}
	var b strings.Builder
	b.Write(headBytes)
	fmt.Fprintf(&b, "\n... [%s omitted — %s] ...\n", humanSize(omitted), loc)
	b.Write(tailBytes)
	return b.String()
}
