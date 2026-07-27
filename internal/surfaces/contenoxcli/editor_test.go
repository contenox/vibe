package contenoxcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnit_ResolveEditor(t *testing.T) {
	cases := []struct {
		name   string
		visual string
		editor string
		want   string
	}{
		{"VISUAL wins", "code --wait", "nano", "code --wait"},
		{"EDITOR fallback", "", "nano", "nano"},
		{"nano default", "", "", "nano"},
		{"VISUAL trims whitespace", "  helix  ", "nano", "helix"},
		{"empty VISUAL ignored", "  ", "nano", "nano"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.editor)
			if got := resolveEditor(); got != tc.want {
				t.Fatalf("resolveEditor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnit_StripCommentLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "drops hash lines",
			in:   "# header\nbody line\n# footer\n",
			want: "body line\n",
		},
		{
			name: "indented hash kept",
			in:   "  # not a comment\nbody\n",
			want: "  # not a comment\nbody\n",
		},
		{
			name: "all comments removed",
			in:   "# a\n# b\n# c\n",
			want: "",
		},
		{
			name: "no comments unchanged",
			in:   "hello\nworld\n",
			want: "hello\nworld\n",
		},
		{
			// The blank line directly touching a comment block is the
			// template's own boundary blank (buildEditorTemplate always
			// inserts exactly one) and must go with it.
			name: "drops single boundary blank before comment run",
			in:   "content\n\n# banner\n",
			want: "content\n",
		},
		{
			// A blank line NOT touching a comment run is user content (a
			// paragraph break), never scaffolding, and must survive.
			name: "keeps blank line not adjacent to a comment run",
			in:   "a\n\nb\n# c\n",
			want: "a\n\nb\n",
		},
		{
			// M5 defect 3: the trim is bounded to exactly the one line the
			// template inserted. A second, user-added blank line right
			// before the boundary blank must survive even though it also
			// sits next to the (now-removed) comment block.
			name: "keeps extra blank beyond the single boundary",
			in:   "a\n\n\n# c\n",
			want: "a\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCommentLines(tc.in); got != tc.want {
				t.Fatalf("stripCommentLines() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnit_BuildEditorTemplate_NoSeed(t *testing.T) {
	got := string(buildEditorTemplate(nil, "qwen2.5:7b"))
	if !strings.HasPrefix(got, "\n# ---") {
		t.Fatalf("template should start with blank line then header; got %q", got)
	}
	if !strings.Contains(got, "# Target Model: qwen2.5:7b") {
		t.Fatalf("template missing model hint; got %q", got)
	}
	if !strings.Contains(got, "Lines starting with '#' are ignored.") {
		t.Fatalf("template missing user instruction; got %q", got)
	}
}

// TestUnit_BuildEditorTemplate_NoSeed_ByteExact pins the exact no-seed shape:
// a blank line, then the fenced banner, and nothing else. This is the shape
// M5 says must NOT change.
func TestUnit_BuildEditorTemplate_NoSeed_ByteExact(t *testing.T) {
	got := string(buildEditorTemplate(nil, ""))
	want := "\n" +
		"# ---------------------------------------------------------\n" +
		"# Write your prompt above. Lines starting with '#' are ignored.\n" +
		"# ---------------------------------------------------------\n"
	if got != want {
		t.Fatalf("no-seed template mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestUnit_BuildEditorTemplate_NoModelHint(t *testing.T) {
	got := string(buildEditorTemplate(nil, ""))
	if strings.Contains(got, "Target Model") {
		t.Fatalf("empty modelHint should omit Target Model line; got %q", got)
	}
}

// TestUnit_BuildEditorTemplate_WithSeed_ByteExact is the M5 regression test
// for defect (1): the seed must land ABOVE the banner (cursor lands on it,
// and the "write your prompt above" instruction is actually true), separated
// from the banner by exactly one blank line.
func TestUnit_BuildEditorTemplate_WithSeed_ByteExact(t *testing.T) {
	seed := []byte("panic: runtime error\nstack trace line\n")
	got := string(buildEditorTemplate(seed, ""))
	want := "panic: runtime error\nstack trace line\n" +
		"\n" +
		"# ---------------------------------------------------------\n" +
		"# Write your prompt above. Lines starting with '#' are ignored.\n" +
		"# ---------------------------------------------------------\n"
	if got != want {
		t.Fatalf("with-seed template mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestUnit_BuildEditorTemplate_WithSeed(t *testing.T) {
	seed := []byte("panic: runtime error\nstack trace line\n")
	got := string(buildEditorTemplate(seed, ""))
	if !strings.Contains(got, "panic: runtime error") {
		t.Fatalf("template missing seed; got %q", got)
	}
	headerStart := strings.Index(got, "# ---------------------------------------------------------\n# Write your prompt above")
	seedStart := strings.Index(got, "panic: runtime error")
	if headerStart < 0 || seedStart > headerStart {
		t.Fatalf("seed should appear above (before) the header block; got %q", got)
	}
	if seedStart != 0 {
		t.Fatalf("seed should be the first thing in the file so the cursor lands on it; got %q", got)
	}
}

func TestUnit_BuildEditorTemplate_SeedWithoutTrailingNewline(t *testing.T) {
	seed := []byte("no trailing newline")
	got := string(buildEditorTemplate(seed, ""))
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("template should end with newline even when seed lacks one; got %q", got)
	}
	if !strings.HasPrefix(got, "no trailing newline\n") {
		t.Fatalf("seed should be normalized to end with its own newline before the boundary blank; got %q", got)
	}
}

// writeNoopEditor points $EDITOR at a script that exits without touching the
// file at all — simulating an operator opening $EDITOR and immediately
// quitting (beam: Ctrl+E then :q).
func writeNoopEditor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noop-editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	return path
}

// writeReplaceEditor points $EDITOR at a script that overwrites whatever
// captureFromEditor wrote with finalContent, simulating an operator who
// actually edited the buffer. finalContent must end in "\n".
func writeReplaceEditor(t *testing.T, finalContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replace-editor.sh")
	script := "#!/bin/sh\ncat > \"$1\" <<'EOF_CAPTURE_TEST'\n" + finalContent + "EOF_CAPTURE_TEST\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	return path
}

// TestUnit_CaptureFromEditor_UnchangedWithSeed_ReturnsSeedVerbatim is the M5
// regression test for defect (2): quitting $EDITOR without touching a seeded
// buffer must hand back the seed exactly, not errEmptyPrompt. It also proves
// round-trip stability across TWO consecutive invocations — chaining the
// first call's output into the second call's seed — so a stray scaffolding
// leftover can't accumulate into the draft across repeated Ctrl+E/:q cycles.
func TestUnit_CaptureFromEditor_UnchangedWithSeed_ReturnsSeedVerbatim(t *testing.T) {
	editor := writeNoopEditor(t)
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	seed := "first draft, not a byte edited"

	got1, err := captureFromEditor([]byte(seed), "some-model")
	if err != nil {
		t.Fatalf("first round: unexpected error: %v", err)
	}
	if got1 != seed {
		t.Fatalf("first round: got %q, want seed %q unchanged", got1, seed)
	}

	// Feed the result straight back in as the next seed, as beam does on a
	// second Ctrl+E over the same draft.
	got2, err := captureFromEditor([]byte(got1), "some-model")
	if err != nil {
		t.Fatalf("second round: unexpected error: %v", err)
	}
	if got2 != seed {
		t.Fatalf("second round: got %q, want seed %q unchanged (accumulating-blank-line regression)", got2, seed)
	}
	if got1 != got2 {
		t.Fatalf("round trip not stable: first %q != second %q", got1, got2)
	}
}

// TestUnit_CaptureFromEditor_UnchangedNoSeed_Aborts is the M5 regression
// test that the no-seed abort path is untouched: quitting $EDITOR without
// writing anything, and without a seed to fall back to, is genuinely
// "nothing to send".
func TestUnit_CaptureFromEditor_UnchangedNoSeed_Aborts(t *testing.T) {
	editor := writeNoopEditor(t)
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	_, err := captureFromEditor(nil, "")
	if !errors.Is(err, errEmptyPrompt) {
		t.Fatalf("got err %v, want errEmptyPrompt", err)
	}
}

// TestUnit_CaptureFromEditor_Edited_StripsCommentsAndBoundaryBlank exercises
// the full captureFromEditor pipeline against a buffer an operator actually
// edited: their own paragraph-break blank line must survive, while the
// template's boundary blank (the one directly touching the banner) and the
// banner itself must be gone.
func TestUnit_CaptureFromEditor_Edited_StripsCommentsAndBoundaryBlank(t *testing.T) {
	final := "My prompt\n" +
		"\n" +
		"line two\n" +
		"\n" +
		templateBannerRule + "\n" +
		"# Write your prompt above. Lines starting with '#' are ignored.\n" +
		templateBannerRule + "\n"
	editor := writeReplaceEditor(t, final)
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	got, err := captureFromEditor([]byte("seed is irrelevant here"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "My prompt\n\nline two\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestUnit_CaptureFromEditor_Edited_LeadingBlankSurvives is the M5
// regression test for defect (3)'s bound: a draft that legitimately starts
// with a blank line after editing must survive, because that blank line is
// not the one the template inserted (it isn't adjacent to the banner).
func TestUnit_CaptureFromEditor_Edited_LeadingBlankSurvives(t *testing.T) {
	final := "\n" +
		"Actual prompt\n" +
		"\n" +
		templateBannerRule + "\n" +
		"# Write your prompt above. Lines starting with '#' are ignored.\n" +
		templateBannerRule + "\n"
	editor := writeReplaceEditor(t, final)
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	got, err := captureFromEditor(nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "\nActual prompt\n"
	if got != want {
		t.Fatalf("got %q, want %q (leading blank should survive)", got, want)
	}
}

// TestUnit_CaptureFromEditor_GenuinelyEmptied_Aborts keeps errEmptyPrompt
// semantics for a buffer the operator actually emptied out, seed or no seed:
// this is not the "unchanged" case, it's a real abort.
func TestUnit_CaptureFromEditor_GenuinelyEmptied_Aborts(t *testing.T) {
	final := "   \n" +
		templateBannerRule + "\n" +
		"# Write your prompt above. Lines starting with '#' are ignored.\n" +
		templateBannerRule + "\n"
	editor := writeReplaceEditor(t, final)
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	_, err := captureFromEditor([]byte("there was a seed here"), "")
	if !errors.Is(err, errEmptyPrompt) {
		t.Fatalf("got err %v, want errEmptyPrompt", err)
	}
}
