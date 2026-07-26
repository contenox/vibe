package localtools

// Internal-package tests for the tool-hardening primitives (Rec 4/5/7 of
// docs/development/blueprints/tool-hardening.md). These exercise the unexported
// building blocks directly: the streaming line reader's parity with
// strings.Split, the spool retention policy, and the fuzzy suggestion helpers.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamRange must reproduce strings.Split("\n") line semantics exactly for the
// unbounded case, including the trailing-empty-line edge cases.
func TestUnit_StreamRange_ParityWithStringsSplit(t *testing.T) {
	cases := []string{
		"",
		"a",
		"a\n",
		"a\nb",
		"a\nb\n",
		"\n",
		"\n\n",
		"line1\nline2\nline3",
		"line1\nline2\nline3\n",
	}
	for _, in := range cases {
		lines := strings.Split(in, "\n")
		// full range
		got, lastLine, nextLine, err := streamRange(bytes.NewReader([]byte(in)), 1, 1<<30, 0)
		if err != nil {
			t.Fatalf("streamRange(%q): %v", in, err)
		}
		want := strings.Join(lines, "\n")
		if got != want {
			t.Fatalf("streamRange(%q) full = %q; want %q", in, got, want)
		}
		if nextLine != 0 {
			t.Fatalf("streamRange(%q) full nextLine = %d; want 0 (EOF)", in, nextLine)
		}
		if lastLine != len(lines) {
			t.Fatalf("streamRange(%q) lastLine = %d; want %d", in, lastLine, len(lines))
		}
	}
}

// A bounded range must return exactly lines [start,end] and report end+1 as the
// resume line when more remains.
func TestUnit_StreamRange_BoundedRange(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	got, lastLine, nextLine, err := streamRange(bytes.NewReader([]byte(in)), 2, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b\nc" {
		t.Fatalf("range [2,3] = %q; want %q", got, "b\nc")
	}
	if lastLine != 3 || nextLine != 4 {
		t.Fatalf("lastLine=%d nextLine=%d; want 3,4", lastLine, nextLine)
	}
}

// The byte budget must stop at a whole-line boundary and name the next line, so a
// paging loop is exact.
func TestUnit_StreamRange_ByteBudgetPagesByLine(t *testing.T) {
	in := "aaaa\nbbbb\ncccc\ndddd"
	// budget large enough for two 4-char lines + one separator = 9 bytes, but not
	// the third.
	got, lastLine, nextLine, err := streamRange(bytes.NewReader([]byte(in)), 1, 1<<30, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got != "aaaa\nbbbb" {
		t.Fatalf("budgeted head = %q; want %q", got, "aaaa\nbbbb")
	}
	if lastLine != 2 || nextLine != 3 {
		t.Fatalf("lastLine=%d nextLine=%d; want 2,3", lastLine, nextLine)
	}

	// Resume from nextLine yields the rest.
	rest, _, next2, _ := streamRange(bytes.NewReader([]byte(in)), nextLine, 1<<30, 9)
	if rest != "cccc\ndddd" {
		t.Fatalf("resume = %q; want %q", rest, "cccc\ndddd")
	}
	if next2 != 0 {
		t.Fatalf("resume nextLine=%d; want 0 (EOF)", next2)
	}
}

// A single line larger than the budget must be byte-truncated so output stays
// bounded (the one-enormous-line file, e.g. a big binary blob or minified asset).
func TestUnit_StreamRange_SingleHugeLineIsByteBounded(t *testing.T) {
	in := strings.Repeat("x", 1000) // one 1000-byte line, no newline
	got, lastLine, _, err := streamRange(bytes.NewReader([]byte(in)), 1, 1<<30, 64)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != 64 {
		t.Fatalf("huge single line returned %d bytes; want 64 (byte-capped)", len(got))
	}
	if lastLine != 1 {
		t.Fatalf("lastLine=%d; want 1", lastLine)
	}
}

// pruneToolOutput enforces the count cap oldest-first and removes emptied buckets.
func TestUnit_PruneToolOutput_CountCap(t *testing.T) {
	root := t.TempDir()
	bucket := filepath.Join(root, "day-2026-07-21")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-10 * time.Minute)
	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(bucket, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Stagger modtimes so eviction order is deterministic (f-a oldest).
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	pruneToolOutput(root, 2, 0)

	remaining := countFiles(t, root)
	if remaining != 2 {
		t.Fatalf("count cap not honored: %d files remain, want 2", remaining)
	}
	// The two NEWEST (f-d, f-e) must survive; the three oldest are gone.
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest file should have been evicted: %v", err)
	}
	if _, err := os.Stat(paths[4]); err != nil {
		t.Fatalf("newest file must survive: %v", err)
	}
}

// pruneToolOutput removes anything older than maxAge regardless of the count cap.
func TestUnit_PruneToolOutput_AgeCap(t *testing.T) {
	root := t.TempDir()
	bucket := filepath.Join(root, "session-old")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(bucket, "fresh.txt")
	stale := filepath.Join(bucket, "stale.txt")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	pruneToolOutput(root, 100, 24*time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file past maxAge should have been removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh file within maxAge must survive: %v", err)
	}
}

// suggestSiblings must surface substring and small-edit-distance matches, cap the
// count, and never include an exact self-match.
func TestUnit_SuggestSiblings_FuzzyAndCapped(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"README.md", "readme.txt", "reader.go", "main.go", "config.yaml", "unrelated.bin"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := suggestSiblings(dir, "readme", 5)
	if len(got) == 0 {
		t.Fatal("expected sibling suggestions for 'readme'")
	}
	// Case-insensitive substring matches must be present.
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "README.md") || !strings.Contains(joined, "readme.txt") {
		t.Fatalf("expected README.md and readme.txt among suggestions: %v", got)
	}
	if strings.Contains(joined, "unrelated.bin") {
		t.Fatalf("an unrelated name must not be suggested: %v", got)
	}

	// Cap is respected.
	capped := suggestSiblings(dir, "readme", 2)
	if len(capped) > 2 {
		t.Fatalf("suggestion cap not honored: %v", capped)
	}
}

// suggestNearestLines picks the closest window and never returns the pattern
// itself when the pattern is absent — SUGGEST only.
func TestUnit_SuggestNearestLines_PicksClosestWindow(t *testing.T) {
	content := "func Alpha() {}\nfunc Bravo() {}\nfunc Charlie() {}\n"
	near := suggestNearestLines(content, "func Bravl() {}", 0)
	if !strings.Contains(near, "Bravo") {
		t.Fatalf("nearest line should be the Bravo line: %q", near)
	}
	if !strings.HasPrefix(near, "2: ") {
		t.Fatalf("nearest line should be reported with its 1-based number: %q", near)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
