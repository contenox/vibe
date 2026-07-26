package contenoxcli

import (
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

func TestUnit_BuildEditorTemplate_NoModelHint(t *testing.T) {
	got := string(buildEditorTemplate(nil, ""))
	if strings.Contains(got, "Target Model") {
		t.Fatalf("empty modelHint should omit Target Model line; got %q", got)
	}
}

func TestUnit_BuildEditorTemplate_WithSeed(t *testing.T) {
	seed := []byte("panic: runtime error\nstack trace line\n")
	got := string(buildEditorTemplate(seed, ""))
	if !strings.Contains(got, "panic: runtime error") {
		t.Fatalf("template missing seed; got %q", got)
	}
	headerEnd := strings.Index(got, "# ---------------------------------------------------------\n# Write your prompt above")
	seedStart := strings.Index(got, "panic: runtime error")
	if headerEnd < 0 || seedStart < headerEnd {
		t.Fatalf("seed should appear after the header block; got %q", got)
	}
}

func TestUnit_BuildEditorTemplate_SeedWithoutTrailingNewline(t *testing.T) {
	seed := []byte("no trailing newline")
	got := string(buildEditorTemplate(seed, ""))
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("template should end with newline even when seed lacks one; got %q", got)
	}
}
