package localtools

// spool.go implements Rec 4 of docs/development/blueprints/tool-hardening.md
// ("never truncate silently") for ephemeral SHELL output: the full stdout/stderr
// of a command whose inline result was size-capped is written to a durable spool
// file, and the truncated inline result names that file concretely
// ("full output: <path> (N KiB)"). File READS do not spool — the file on disk is
// its own durable copy, so read_file names the exact resume line instead (see
// fs.go); spooling a read would be a redundant, possibly huge copy.
//
// Spool location: $CONTENOX_TOOL_OUTPUT_DIR when set, else ~/.contenox/tool_output
// (the opencode `.opencode/tool_output/` pattern, rehomed under the runtime's
// ~/.contenox data root). Output is bucketed per session (or per calendar day
// when no session id is in scope). Retention is bounded (pruneToolOutput):
// anything older than maxSpoolAge is removed, and the newest maxSpoolFiles are
// kept, oldest-first eviction beyond that. Both caps are documented constants.

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
	// toolOutputEnvVar overrides the spool root. Useful for tests (hermetic temp
	// dir) and for deployments whose data dir is not ~/.contenox.
	toolOutputEnvVar = "CONTENOX_TOOL_OUTPUT_DIR"

	// maxSpoolFiles bounds how many spool files are retained across all buckets.
	// Past this, the oldest are evicted before a new one is written.
	maxSpoolFiles = 256

	// maxSpoolAge bounds how long a spool file survives regardless of the count
	// cap. A week is long enough to inspect a derailment after the fact, short
	// enough that the directory does not grow without bound.
	maxSpoolAge = 7 * 24 * time.Hour

	// maxShellSpoolBytes caps how much of a single command's output is written to
	// its spool file, so one runaway command cannot fill the disk. Output beyond
	// this is dropped and the notice says so. Kept well above the typical inline
	// budget so the spool always holds far more than the truncated inline result.
	maxShellSpoolBytes = 8 * 1024 * 1024
)

// toolOutputRoot returns the spool root directory (not yet created).
func toolOutputRoot() string {
	if v := strings.TrimSpace(os.Getenv(toolOutputEnvVar)); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".contenox", "tool_output")
	}
	// Last resort: OS temp dir, so spooling never becomes a hard dependency on a
	// resolvable home directory.
	return filepath.Join(os.TempDir(), "contenox-tool-output")
}

// spoolBucket resolves the per-run subdirectory name: the session id when one is
// bound (the true run boundary a dispatched unit holds), else the calendar day.
func spoolBucket(ctx context.Context) string {
	if sid := strings.TrimSpace(sessionIDFromContext(ctx)); sid != "" {
		return "session-" + sanitizeBucket(sid)
	}
	return "day-" + time.Now().UTC().Format("2006-01-02")
}

// sanitizeBucket keeps a bucket name filesystem-safe.
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

// newSpoolFile creates (and returns an open handle to) a fresh spool file for
// `tool` under the resolved root+bucket, pruning stale files first. The returned
// error is fatal-flavored: a spool it cannot open means the environment is
// broken, not the model's call. Caller closes the file.
func newSpoolFile(ctx context.Context, tool string) (*os.File, string, error) {
	root := toolOutputRoot()
	// Prune BEFORE creating this run's bucket, so retention (which removes empty
	// bucket dirs) can never delete the fresh directory out from under the open.
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

// pruneToolOutput enforces the retention policy over every spool file under root:
// remove anything older than maxAge, then evict oldest-first until at most
// maxFiles remain, then drop any bucket directories left empty. Best-effort: any
// individual failure is skipped, since retention must never break a live call.
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

// spoolWriter is the io.Writer handed to a command's stdout/stderr. It:
//   - counts every byte (total),
//   - retains a bounded head (first headCap bytes) and tail (last tailCap bytes)
//     for the 20%/80% inline split,
//   - spills the FULL stream to a spool file once total exceeds the inline budget
//     (lazily, so a small command never touches disk), up to maxShellSpoolBytes.
//
// It never returns a short write until the spool cap is hit, so the command runs
// to completion and the spool captures the whole stream — the opposite of the old
// capWriter, which stopped the command at the inline budget and lost the tail.
type spoolWriter struct {
	tool   string
	ctx    context.Context
	budget int64 // inline budget; total>budget means "truncated"

	total int64

	head    bytes.Buffer
	headCap int64

	tail    []byte // ring: last tailCap bytes
	tailCap int64

	pre bytes.Buffer // buffers bytes until the budget is first exceeded

	file     *os.File
	path     string
	spooled  int64
	spoolCap int64
	spoolErr error // fatal: spool unwritable / disk full
	overCap  bool  // spool cap reached; stream stopped
	spilled  bool
}

// newSpoolWriter builds a spoolWriter for the given inline budget. The head gets
// 20% of the budget, the tail 80% — gemini-cli's finding that command errors
// cluster at the tail.
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

	// Head: first headCap bytes.
	if int64(w.head.Len()) < w.headCap {
		room := w.headCap - int64(w.head.Len())
		if room > int64(len(p)) {
			room = int64(len(p))
		}
		w.head.Write(p[:room])
	}

	// Tail ring: keep the last tailCap bytes.
	w.appendTail(p)

	// Spool: buffer until the budget is first exceeded, then spill everything.
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
		// Stop the command: the spool cap is reached, so further output would be
		// discarded anyway. run() treats this as a truncation, not a failure.
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

// inlineOutput returns what goes into the model-facing result field: the full
// stream when it fit the budget (held whole in `pre`), or the 20/80 split with a
// spool pointer when it did not.
func (w *spoolWriter) inlineOutput() string {
	if w.truncated() {
		return w.splitText()
	}
	return w.pre.String()
}

// close flushes and closes the spool file, returning its path (or "" if nothing
// was spilled).
func (w *spoolWriter) close() string {
	if w.file != nil {
		_ = w.file.Close()
	}
	return w.path
}

// discard closes and removes the spool file — used when the captured output is
// poisoned (a backend-signalled mid-stream truncation) and must not be surfaced.
func (w *spoolWriter) discard() {
	if w.file != nil {
		_ = w.file.Close()
		_ = os.Remove(w.path)
		w.file = nil
		w.path = ""
	}
}

// splitText renders the 20%-head/80%-tail inline view with a middle marker that
// names the spool file, per Rec 4. Called only when truncated().
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
