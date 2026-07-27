package frame

import "testing"

func TestUnit_AllStylesUniqueAndClosed(t *testing.T) {
	seen := map[StyleID]bool{}
	for _, id := range All() {
		if seen[id] {
			t.Fatalf("duplicate StyleID %q in All()", id)
		}
		seen[id] = true
	}
	if !seen[StyleNone] {
		t.Fatal("All() must include StyleNone")
	}
}

func TestUnit_LineTextIsCopyPayload(t *testing.T) {
	l := L(S(StyleUser, "you> "), S(StyleNone, "hello "), S(StyleCode, "world"))
	if got, want := l.Text(), "you> hello world"; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
	if got := (Line{}).Text(); got != "" {
		t.Fatalf("empty line Text() = %q, want empty", got)
	}
}
