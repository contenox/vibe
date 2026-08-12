package localtools

import (
	"strings"
	"testing"
)

// TestUnit_UnifiedDiff_AnnotatesDuplicateInsertion pins that an added line byte-identical to the adjacent context line is called out, not left to look like a no-op insertion.
func TestUnit_UnifiedDiff_AnnotatesDuplicateInsertion(t *testing.T) {
	old := "func f() {\n\ta.noticef(\"start\")\n\tdoWork()\n}\n"
	next := "func f() {\n\ta.noticef(\"start\")\n\ta.noticef(\"start\")\n\tdoWork()\n}\n"

	rendered := unifiedDiff("f.go", old, next)

	if !strings.Contains(rendered, "+\ta.noticef(\"start\")  (inserts duplicate of adjacent line)") {
		t.Fatalf("added duplicate line was not annotated:\n%s", rendered)
	}
	if strings.Count(rendered, "(inserts duplicate of adjacent line)") != 1 {
		t.Fatalf("expected exactly one duplicate annotation, got:\n%s", rendered)
	}
}

// TestUnit_UnifiedDiff_AnnotatesWhitespaceOnlyChange pins that a whitespace-only difference from a neighbour is called out, with the whitespace made visible on the line itself, not just in the note.
func TestUnit_UnifiedDiff_AnnotatesWhitespaceOnlyChange(t *testing.T) {
	old := "func f() {\n    return 1\n}\n"
	// Re-indented with a tab instead of four spaces: same bytes otherwise.
	next := "func f() {\n\treturn 1\n}\n"

	rendered := unifiedDiff("f.go", old, next)

	if !strings.Contains(rendered, "(whitespace-only change)") {
		t.Fatalf("whitespace-only change was not annotated:\n%s", rendered)
	}
	if !strings.Contains(rendered, "+→return 1  (whitespace-only change)") {
		t.Fatalf("tab was not made visible on the added line:\n%s", rendered)
	}
}

// TestUnit_UnifiedDiff_AnnotatesTrailingWhitespaceChange pins that trailing whitespace (invisible in a terminal) renders as a visible glyph plus the same note.
func TestUnit_UnifiedDiff_AnnotatesTrailingWhitespaceChange(t *testing.T) {
	old := "line one\nline two\n"
	next := "line one\nline two   \n"

	rendered := unifiedDiff("f.go", old, next)

	if !strings.Contains(rendered, "(whitespace-only change)") {
		t.Fatalf("trailing-whitespace change was not annotated:\n%s", rendered)
	}
	if !strings.Contains(rendered, "+line two···  (whitespace-only change)") {
		t.Fatalf("trailing spaces were not made visible on the added line:\n%s", rendered)
	}
}

// TestUnit_UnifiedDiff_OrdinaryChangeIsNeverAnnotated pins that a genuine, unambiguous change carries no annotation at all.
func TestUnit_UnifiedDiff_OrdinaryChangeIsNeverAnnotated(t *testing.T) {
	old := "func f() {\n\treturn 1\n}\n"
	next := "func f() {\n\treturn 2\n}\n"

	rendered := unifiedDiff("f.go", old, next)

	if strings.Contains(rendered, "(whitespace-only change)") || strings.Contains(rendered, "(inserts duplicate of adjacent line)") {
		t.Fatalf("an ordinary content change must not carry an annotation:\n%s", rendered)
	}
}

// TestUnit_VisibleWhitespace pins the glyph substitution rules directly:
// tabs always become →, and only *trailing* spaces become ·; interior and
// leading spaces are left alone since they are not the ambiguous case.
func TestUnit_VisibleWhitespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"return 1", "return 1"},
		{"\treturn 1", "→return 1"},
		{"a  b", "a  b"},
		{"trailing   ", "trailing···"},
		{"\tboth  ", "→both··"},
	}
	for _, c := range cases {
		if got := visibleWhitespace(c.in); got != c.want {
			t.Errorf("visibleWhitespace(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
