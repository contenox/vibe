package testkit

import (
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beam/frame"
)

func TestUnit_EncodeLinesStyleTags(t *testing.T) {
	got := EncodeLines([]frame.Line{
		frame.L(frame.S(frame.StyleUser, "you> "), frame.S(frame.StyleNone, "hi")),
		frame.Plain("plain"),
	})
	want := "[user]you> [/]hi\nplain\n"
	if got != want {
		t.Fatalf("EncodeLines = %q, want %q", got, want)
	}
}

func TestUnit_EncodeFrameSectionsAndCursor(t *testing.T) {
	f := frame.Frame{
		Scrollback: []frame.Line{frame.Plain("old")},
		Live:       []frame.Line{frame.Styled(frame.StyleMuted, "status")},
		Cursor:     frame.Cursor{Row: 0, Col: 3},
	}
	got := EncodeFrame(f)
	want := "── scrollback ──\nold\n── live (cursor 0,3) ──\n[muted]status[/]\n"
	if got != want {
		t.Fatalf("EncodeFrame = %q, want %q", got, want)
	}
}
