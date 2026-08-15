package liblog_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/liblog"
)

// clock is a hand-wound clock, so a test can cross midnight without waiting
// for one.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(stamp string) *clock {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		panic(err)
	}
	return &clock{t: t}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestUnit_Log_FileIsNamedForItsDate(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := filepath.Join(dir, "serve-2026-08-15.log")
	if w.Path() != want {
		t.Fatalf("Path = %q, want %q", w.Path(), want)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "hello\n" {
		t.Fatalf("dated file = %q, %v", b, err)
	}
}

// The reason for dating files at all: "what did this host do on Tuesday?" must
// be a question about filenames.
func TestUnit_Log_MidnightStartsANewDatedFile(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T23:59:30Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("before midnight\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	c.advance(time.Minute) // 00:00:30 the next day
	if _, err := w.Write([]byte("after midnight\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	yesterday, err := os.ReadFile(filepath.Join(dir, "serve-2026-08-15.log"))
	if err != nil {
		t.Fatalf("read yesterday: %v", err)
	}
	today, err := os.ReadFile(filepath.Join(dir, "serve-2026-08-16.log"))
	if err != nil {
		t.Fatalf("read today: %v", err)
	}
	if !strings.Contains(string(yesterday), "before midnight") || strings.Contains(string(yesterday), "after") {
		t.Fatalf("yesterday's file has the wrong lines: %q", yesterday)
	}
	if !strings.Contains(string(today), "after midnight") {
		t.Fatalf("today's file has the wrong lines: %q", today)
	}
}

// A busy day must not produce one enormous file.
func TestUnit_Log_OversizedDaySplitsIntoParts(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 32, Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	for i := range 12 {
		if _, err := w.Write([]byte(fmt.Sprintf("line %02d\n", i))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	for _, want := range []string{"serve-2026-08-15.log", "serve-2026-08-15.2.log"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Fatalf("expected part %q: %v — got %v", want, err, names(t, dir))
		}
	}
	for _, n := range names(t, dir) {
		info, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("stat %q: %v", n, err)
		}
		if info.Size() > 32 {
			t.Fatalf("part %q is %d bytes, over the 32-byte ceiling", n, info.Size())
		}
	}
}

// A new day restarts at part 1 however full the previous day's part was, or
// the part numbers would encode history nobody asked about.
func TestUnit_Log_PartsResetEachDay(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 16, Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	for range 5 {
		if _, err := w.Write([]byte("0123456789\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	c.advance(24 * time.Hour)
	if _, err := w.Write([]byte("new day\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := w.Path(), filepath.Join(dir, "serve-2026-08-16.log"); got != want {
		t.Fatalf("after midnight Path = %q, want the day's first part %q", got, want)
	}
}

// A host restarted repeatedly must not shard its day into a file per launch.
func TestUnit_Log_RestartContinuesTheCurrentPart(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	cfg := liblog.Config{Dir: dir, Name: "serve", MaxBytes: 24, Now: c.Now}

	w, err := liblog.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for range 4 {
		if _, err := w.Write([]byte("aaaaaaaaaa\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	before := w.Path()
	existing, err := os.ReadFile(before)
	if err != nil {
		t.Fatalf("read before restart: %v", err)
	}
	if len(existing) == 0 {
		t.Fatal("expected the part to have content before the restart")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w2, err := liblog.Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()

	// The property: the restart adopts the latest existing part rather than
	// starting a fresh one, and adopts its size rather than truncating it.
	if w2.Path() != before {
		t.Fatalf("restart moved to %q, want to continue %q", w2.Path(), before)
	}
	if w2.Size() != int64(len(existing)) {
		t.Fatalf("restart adopted size %d, want the file's %d", w2.Size(), len(existing))
	}
	if _, err := w2.Write([]byte("more\n")); err != nil {
		t.Fatalf("Write after restart: %v", err)
	}
	// Where "more" lands depends on how full that part already was, which is
	// not what this test is about; what must hold is that nothing was lost.
	after, err := os.ReadFile(before)
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if !strings.HasPrefix(string(after), string(existing)) {
		t.Fatalf("restart truncated the part: %q no longer starts with %q", after, existing)
	}
}

// Retention by count is a hard bound on disk.
func TestUnit_Log_MaxFilesBoundsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 16, MaxFiles: 3, MaxAge: liblog.Unlimited, Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	for i := range 40 {
		if _, err := w.Write([]byte(fmt.Sprintf("entry %03d\n", i))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if got := names(t, dir); len(got) > 3 {
		t.Fatalf("kept %d files, want at most 3: %v", len(got), got)
	}
}

// Retention by age retires a quiet host's logs, which a file count never would.
func TestUnit_Log_MaxAgeRetiresOldDates(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-01T10:00:00Z")
	w, err := liblog.Open(liblog.Config{
		Dir: dir, Name: "serve", MaxFiles: liblog.Unlimited, MaxAge: 3 * 24 * time.Hour, Now: c.Now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	// One line a day for a week: only the last few days may survive.
	for range 7 {
		if _, err := w.Write([]byte("daily\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		c.advance(24 * time.Hour)
	}
	if _, err := w.Write([]byte("final\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := names(t, dir)
	for _, n := range got {
		if strings.Contains(n, "2026-08-01") || strings.Contains(n, "2026-08-02") {
			t.Fatalf("a log older than the age bound survived: %v", got)
		}
	}
	if len(got) == 0 {
		t.Fatal("age retention deleted everything, including the live log")
	}
}

// The file being written is never a deletion candidate, or a tight bound would
// delete the log out from under the process writing it.
func TestUnit_Log_LiveFileSurvivesRetention(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 16, MaxFiles: 1, MaxAge: liblog.Unlimited, Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	for i := range 20 {
		if _, err := w.Write([]byte(fmt.Sprintf("x%03d\n", i))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("the live log was retired: %v", err)
	}
}

// A stray file sharing the directory must never become a deletion candidate.
func TestUnit_Log_ForeignFilesAreNeverRetired(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(stray, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("seed stray: %v", err)
	}
	other := filepath.Join(dir, "other-2026-08-15.log")
	if err := os.WriteFile(other, []byte("another log"), 0o600); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	c := newClock("2026-08-15T10:00:00Z")
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 16, MaxFiles: 1, MaxAge: liblog.Unlimited, Now: c.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	for i := range 20 {
		if _, err := w.Write([]byte(fmt.Sprintf("y%03d\n", i))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	for _, p := range []string{stray, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a file this log does not own was deleted: %q: %v", p, err)
		}
	}
}

// A log line is the unit of meaning: one oversized record is written whole.
func TestUnit_Log_OversizedWriteIsNotSplit(t *testing.T) {
	dir := t.TempDir()
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	big := strings.Repeat("x", 100) + "\n"
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != big {
		t.Fatalf("oversized record was not written whole: %d bytes, want %d", len(b), len(big))
	}
}

// A host logs before it can read its own configuration, so the stored settings
// are applied to a log that is already open.
func TestUnit_Log_ReconfigureAppliesStoredSettings(t *testing.T) {
	dir := t.TempDir()
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if w.MaxBytes() != liblog.DefaultMaxBytes {
		t.Fatalf("MaxBytes = %d, want the default", w.MaxBytes())
	}
	w.Reconfigure(4<<20, 3, 48*time.Hour)
	if w.MaxBytes() != 4<<20 || w.MaxFiles() != 3 || w.MaxAge() != 48*time.Hour {
		t.Fatalf("Reconfigure did not apply: %d/%d/%s", w.MaxBytes(), w.MaxFiles(), w.MaxAge())
	}
	// A zero means "leave it alone", so a partially-configured host keeps the
	// bounds it already had rather than silently reverting to defaults.
	w.Reconfigure(0, 0, 0)
	if w.MaxBytes() != 4<<20 || w.MaxFiles() != 3 || w.MaxAge() != 48*time.Hour {
		t.Fatalf("zero values overwrote live settings: %d/%d/%s", w.MaxBytes(), w.MaxFiles(), w.MaxAge())
	}
}

func TestUnit_Log_UnsetSettingsFallBackToDefaults(t *testing.T) {
	w, err := liblog.Open(liblog.Config{Dir: t.TempDir(), Name: "serve"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	if w.MaxBytes() != liblog.DefaultMaxBytes || w.MaxFiles() != liblog.DefaultMaxFiles || w.MaxAge() != liblog.DefaultMaxAge {
		t.Fatalf("defaults not applied: %d/%d/%s", w.MaxBytes(), w.MaxFiles(), w.MaxAge())
	}
}

func TestUnit_Log_CloseIsIdempotent(t *testing.T) {
	w, err := liblog.Open(liblog.Config{Dir: t.TempDir(), Name: "serve"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close must fail")
	}
}

// slog handlers write from whichever goroutine logged. Run under -race.
func TestUnit_Log_ConcurrentWritesAreSafe(t *testing.T) {
	dir := t.TempDir()
	w, err := liblog.Open(liblog.Config{Dir: dir, Name: "serve", MaxBytes: 256, MaxFiles: 4, MaxAge: liblog.Unlimited})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 32 {
				if _, err := w.Write([]byte(fmt.Sprintf("w%02d-%02d\n", i, j))); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Reconfigure runs while writes are in flight on a live host. Run under -race.
func TestUnit_Log_ReconfigureIsSafeDuringWrites(t *testing.T) {
	w, err := liblog.Open(liblog.Config{Dir: t.TempDir(), Name: "serve", MaxBytes: 128, MaxAge: liblog.Unlimited})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			if _, err := w.Write([]byte("logging along\n")); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 50 {
			w.Reconfigure(int64(64+i), 0, 0)
			_ = w.Path()
		}
	}()
	wg.Wait()
}
