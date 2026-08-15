// Package liblog provides a date-organised, size-bounded log directory: the
// writer a long-lived host points its structured logs at when nobody is
// watching a terminal.
//
// It exists because an append-only log file is a slow leak. `contenox serve`
// is meant to run for days, and a process whose only log destination grows
// without bound eventually fills the disk it shares with the database — a
// failure that arrives long after the change that caused it, on the machine
// least likely to be watched.
//
// Files are named `<name>-<YYYY-MM-DD>.log`, and a day that outgrows its size
// bound continues in `<name>-<YYYY-MM-DD>.2.log`, `.3.log`, and so on. Asking
// "what did this host do on Tuesday?" is then a question about filenames
// rather than about how many times the file happened to roll.
//
// # Why there is no background goroutine
//
// Every decision this package makes happens on the write path, under the lock
// the writer needs anyway:
//
//   - the day check and the size check are an integer compare and a cached
//     date-string compare, far cheaper than the file write they precede;
//   - the expensive part — scanning the directory to retire old files — runs
//     only when a new file is actually created, which is at most once per day
//     plus once per size part.
//
// So retention is event-driven rather than swept on a timer: no goroutine to
// supervise, no second mutex, and nothing to leak if the host is killed. A
// counter ("organise every N appends") would be cheaper still, but it cannot
// be right at a day boundary — an idle host that logs once after midnight
// would file that line under yesterday until the counter happened to trip.
//
// A Writer is safe for concurrent use, because slog handlers write from
// whichever goroutine logged.
package liblog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes is a single day-part's size ceiling when a caller expresses
// no preference: large enough that a normal day of INFO-level operation fits
// in one part, small enough to open in an editor.
const DefaultMaxBytes int64 = 10 << 20 // 10 MiB

// DefaultMaxFiles is how many log files survive by default, counted across
// every date and part. At the default size that is a bounded ~140 MiB.
const DefaultMaxFiles = 14

// DefaultMaxAge retires a log by date regardless of how few files exist, so a
// quiet host does not keep last quarter's logs simply because it never wrote
// enough to hit the file count.
const DefaultMaxAge = 14 * 24 * time.Hour

// dateLayout is the date stamp in a log's name. It sorts lexicographically in
// chronological order, which is what lets retention sort by filename.
const dateLayout = "2006-01-02"

// Config describes a log directory. The zero value is not usable; every field
// except Now has a documented fallback applied by [Open].
type Config struct {
	// Dir is the directory log files live in. Created if absent.
	Dir string
	// Name is the base name: "serve" yields serve-2026-08-15.log.
	Name string
	// MaxBytes bounds one part. Non-positive means [DefaultMaxBytes].
	MaxBytes int64
	// MaxFiles bounds how many files are retained across all dates.
	// Non-positive means [DefaultMaxFiles]; use [Unlimited] for no bound.
	MaxFiles int
	// MaxAge retires files older than this by their date stamp. Zero means
	// [DefaultMaxAge]; negative means no age bound.
	MaxAge time.Duration
	// Now is the clock, injectable so tests can cross a midnight boundary
	// without waiting for one. Nil means [time.Now].
	Now func() time.Time
}

// Unlimited disables a retention bound.
const Unlimited = -1

// Writer is an [io.WriteCloser] over a date-organised log directory.
type Writer struct {
	mu   sync.Mutex
	cfg  Config
	day  string // date stamp of the open file
	part int    // 1 for the day's first file
	f    *os.File
	size int64
}

// Open prepares the log directory and opens today's current part for
// appending. A restart continues the latest existing part rather than starting
// a new one, so a host that is restarted repeatedly does not shard its day
// into a file per launch.
func Open(cfg Config) (*Writer, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("liblog: a log needs a name")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("liblog: create log directory: %w", err)
	}

	w := &Writer{cfg: cfg}
	day := cfg.Now().Format(dateLayout)
	if err := w.openDay(day, w.latestPart(day)); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends p, starting a new file first when the date has changed or when
// p would carry the current part past its ceiling.
//
// A single write larger than the ceiling is still written whole: a log line is
// the unit of meaning here, and splitting one across two files to honour a
// size bound would corrupt the record to protect a number.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}

	// Date first: a new day starts at part 1 however full yesterday's part is.
	if today := w.cfg.Now().Format(dateLayout); today != w.day {
		if err := w.roll(today, 1); err != nil {
			return 0, err
		}
	} else if w.size > 0 && w.size+int64(len(p)) > w.cfg.MaxBytes {
		if err := w.roll(w.day, w.part+1); err != nil {
			return 0, err
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the current file. It is safe to call more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Reconfigure updates the retention settings of a live log.
//
// It exists because a host must be able to log before it can read its own
// configuration: the log opens on defaults during boot, and the stored
// settings are applied once the database is readable. Only bounds change —
// the directory and name are fixed at [Open], so no in-flight write can be
// redirected out from under a reader.
func (w *Writer) Reconfigure(maxBytes int64, maxFiles int, maxAge time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if maxBytes > 0 {
		w.cfg.MaxBytes = maxBytes
	}
	if maxFiles != 0 {
		w.cfg.MaxFiles = maxFiles
	}
	if maxAge != 0 {
		w.cfg.MaxAge = maxAge
	}
}

// Path reports the file currently being written, so a status screen can name
// the exact file rather than a pattern.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.filename(w.day, w.part)
}

// Dir, MaxBytes, MaxFiles and MaxAge report the settings in force, so a status
// screen can state them rather than restating defaults and risking a lie.
func (w *Writer) Dir() string { return w.cfg.Dir }

func (w *Writer) MaxBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg.MaxBytes
}

func (w *Writer) MaxFiles() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg.MaxFiles
}

func (w *Writer) MaxAge() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg.MaxAge
}

// Size reports the current part's size.
func (w *Writer) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// roll closes the current file, opens (day, part), and retires whatever the
// retention bounds no longer cover. Callers hold w.mu.
func (w *Writer) roll(day string, part int) error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return fmt.Errorf("liblog: close before roll: %w", err)
		}
		w.f = nil
	}
	if err := w.openDay(day, part); err != nil {
		return err
	}
	// Only here, never per write: this is the one path that adds a file, so it
	// is the only one that can push the directory past its bounds.
	w.retire()
	return nil
}

// openDay opens (day, part) for appending and adopts its existing size, so a
// restart continues a part rather than overwriting it. Callers hold w.mu.
func (w *Writer) openDay(day string, part int) error {
	path := w.filename(day, part)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("liblog: open %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("liblog: stat %q: %w", path, err)
	}
	w.f, w.day, w.part, w.size = f, day, part, info.Size()
	return nil
}

// filename renders a log's path. The day's first part carries no part suffix,
// so the common case reads as a plain dated log.
func (w *Writer) filename(day string, part int) string {
	if part <= 1 {
		return filepath.Join(w.cfg.Dir, fmt.Sprintf("%s-%s.log", w.cfg.Name, day))
	}
	return filepath.Join(w.cfg.Dir, fmt.Sprintf("%s-%s.%d.log", w.cfg.Name, day, part))
}

// latestPart reports the highest part already present for day, or 1 when the
// day has no log yet.
func (w *Writer) latestPart(day string) int {
	highest := 1
	for _, f := range w.existing() {
		if f.day == day && f.part > highest {
			highest = f.part
		}
	}
	return highest
}

// logFile is one discovered log, parsed from its name rather than its mtime:
// the name is what the operator sorts by, and a copied or restored file keeps
// its meaning while its mtime does not.
type logFile struct {
	path string
	day  string
	part int
}

// existing lists this log's files, oldest first.
func (w *Writer) existing() []logFile {
	matches, err := filepath.Glob(filepath.Join(w.cfg.Dir, w.cfg.Name+"-*.log"))
	if err != nil {
		return nil
	}
	out := make([]logFile, 0, len(matches))
	for _, m := range matches {
		if f, ok := w.parse(m); ok {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day < out[j].day
		}
		return out[i].part < out[j].part
	})
	return out
}

// parse reads the date and part back out of a filename, rejecting anything
// that is not this log's own naming — a stray file in the directory must never
// become a deletion candidate.
func (w *Writer) parse(path string) (logFile, bool) {
	base := filepath.Base(path)
	rest, ok := strings.CutPrefix(base, w.cfg.Name+"-")
	if !ok {
		return logFile{}, false
	}
	rest, ok = strings.CutSuffix(rest, ".log")
	if !ok {
		return logFile{}, false
	}
	day, partStr, hasPart := strings.Cut(rest, ".")
	if _, err := time.Parse(dateLayout, day); err != nil {
		return logFile{}, false
	}
	part := 1
	if hasPart {
		n, err := strconv.Atoi(partStr)
		if err != nil || n < 2 {
			return logFile{}, false
		}
		part = n
	}
	return logFile{path: path, day: day, part: part}, true
}

// retire deletes whatever the bounds no longer cover, oldest first. The file
// currently open is never a candidate, so a bound smaller than one file cannot
// delete the log being written. Callers hold w.mu.
func (w *Writer) retire() {
	files := w.existing()
	live := w.filename(w.day, w.part)

	keep := make([]logFile, 0, len(files))
	for _, f := range files {
		if f.path == live {
			keep = append(keep, f)
			continue
		}
		if w.cfg.MaxAge > 0 {
			if day, err := time.Parse(dateLayout, f.day); err == nil {
				if w.cfg.Now().Sub(day) > w.cfg.MaxAge {
					_ = os.Remove(f.path)
					continue
				}
			}
		}
		keep = append(keep, f)
	}

	if w.cfg.MaxFiles <= 0 {
		return
	}
	for i := 0; len(keep)-i > w.cfg.MaxFiles; i++ {
		if keep[i].path == live {
			continue
		}
		_ = os.Remove(keep[i].path)
	}
}
