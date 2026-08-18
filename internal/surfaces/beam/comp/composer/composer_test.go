package composer

import (
	"strings"
	"testing"
)

// applyOps runs ops written as "name" or "name:arg"; "ins" types its argument rune by rune, "str" pastes it whole.
func applyOps(t *testing.T, c *Composer, ops []string) {
	t.Helper()
	for _, op := range ops {
		name, arg, _ := strings.Cut(op, ":")
		switch name {
		case "ins":
			for _, r := range arg {
				c.InsertRune(r)
			}
		case "str":
			c.InsertString(arg)
		case "nl":
			c.InsertNewline()
		case "bs":
			c.Backspace()
		case "del":
			c.DeleteForward()
		case "left":
			c.CursorLeft()
		case "right":
			c.CursorRight()
		case "up":
			c.CursorUp()
		case "down":
			c.CursorDown()
		case "wleft":
			c.WordLeft()
		case "wright":
			c.WordRight()
		case "dwb":
			c.DeleteWordBack()
		case "kend":
			c.KillToEnd()
		case "kstart":
			c.KillToStart()
		case "home":
			c.Home()
		case "end":
			c.End()
		default:
			t.Fatalf("unknown op %q", op)
		}
	}
}

// editCase is one scripted edit; wantRow/wantCol pin CursorPos at width 40, where none of these drafts wrap.
type editCase struct {
	name    string
	start   string
	ops     []string
	want    string
	wantLn  int
	wantOff int
	wantRow int
	wantCol int
}

// TestUnit_EditingOps is the multiline editing table; wide runes and emoji
// sit on operation boundaries so every case also pins no-panic behavior.
func TestUnit_EditingOps(t *testing.T) {
	cases := []editCase{
		{
			name: "type ascii", ops: []string{"ins:hello"},
			want: "hello", wantLn: 0, wantOff: 5, wantRow: 0, wantCol: 7,
		},
		{
			name: "type wide runes", ops: []string{"ins:日本語"},
			want: "日本語", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 8,
		},
		{
			name: "caret between wide runes", start: "日本語", ops: []string{"left"},
			want: "日本語", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 6,
		},
		{
			name: "backspace a wide rune", start: "日本語", ops: []string{"bs"},
			want: "日本", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 6,
		},
		{
			name: "backspace an emoji", start: "a🙂b", ops: []string{"left", "bs"},
			want: "ab", wantLn: 0, wantOff: 1, wantRow: 0, wantCol: 3,
		},
		{
			name: "delete forward an emoji", start: "a🙂b", ops: []string{"home", "right", "del"},
			want: "ab", wantLn: 0, wantOff: 1, wantRow: 0, wantCol: 3,
		},
		{
			name: "insert between emoji", start: "🙂🙂", ops: []string{"left", "ins:x"},
			want: "🙂x🙂", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 5,
		},
		{
			name: "newline splits the line", start: "abcd", ops: []string{"home", "right", "right", "nl"},
			want: "ab\ncd", wantLn: 1, wantOff: 0, wantRow: 1, wantCol: 2,
		},
		{
			name: "backspace joins lines", start: "ab\ncd", ops: []string{"home", "bs"},
			want: "abcd", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "backspace at buffer start is a no-op", ops: []string{"ins:ab", "home", "bs"},
			want: "ab", wantLn: 0, wantOff: 0, wantRow: 0, wantCol: 2,
		},
		{
			name: "delete forward joins the next line", start: "日本\ncd", ops: []string{"home", "up", "end", "del"},
			want: "日本cd", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 6,
		},
		{
			name: "delete forward at buffer end is a no-op", start: "ab", ops: []string{"del"},
			want: "ab", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "cursor left wraps to the previous line", start: "ab\ncd", ops: []string{"home", "left"},
			want: "ab\ncd", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "cursor right wraps to the next line", start: "ab\ncd", ops: []string{"home", "up", "end", "right"},
			want: "ab\ncd", wantLn: 1, wantOff: 0, wantRow: 1, wantCol: 2,
		},
		{
			name: "up keeps the cell column across wide runes", start: "日本語\nabcdef", ops: []string{"home", "right", "right", "right", "right", "up"},
			want: "日本語\nabcdef", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 6,
		},
		{
			name: "down keeps the cell column across wide runes", start: "abcdef\n日本語", ops: []string{"home", "up", "home", "right", "right", "right", "right", "down"},
			want: "abcdef\n日本語", wantLn: 1, wantOff: 2, wantRow: 1, wantCol: 6,
		},
		{
			name: "up on the first line without history stays put", start: "ab\ncd", ops: []string{"home", "up", "up"},
			want: "ab\ncd", wantLn: 0, wantOff: 0, wantRow: 0, wantCol: 2,
		},
		{
			name: "down on the last line without recall stays put", start: "ab\ncd", ops: []string{"down"},
			want: "ab\ncd", wantLn: 1, wantOff: 2, wantRow: 1, wantCol: 4,
		},
		{
			name: "word left", start: "one two three", ops: []string{"wleft"},
			want: "one two three", wantLn: 0, wantOff: 8, wantRow: 0, wantCol: 10,
		},
		{
			name: "word left twice", start: "one two three", ops: []string{"wleft", "wleft"},
			want: "one two three", wantLn: 0, wantOff: 4, wantRow: 0, wantCol: 6,
		},
		{
			name: "word left at a line start steps up", start: "one\ntwo", ops: []string{"home", "wleft"},
			want: "one\ntwo", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 5,
		},
		{
			name: "word right", start: "one two three", ops: []string{"home", "wright"},
			want: "one two three", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 5,
		},
		{
			name: "word right over wide runes", start: "日本 語", ops: []string{"home", "wright", "wright"},
			want: "日本 語", wantLn: 0, wantOff: 4, wantRow: 0, wantCol: 9,
		},
		{
			name: "word right at a line end steps down", start: "one\ntwo", ops: []string{"home", "up", "end", "wright"},
			want: "one\ntwo", wantLn: 1, wantOff: 0, wantRow: 1, wantCol: 2,
		},
		{
			name: "delete word back", start: "one two three", ops: []string{"dwb"},
			want: "one two ", wantLn: 0, wantOff: 8, wantRow: 0, wantCol: 10,
		},
		{
			name: "delete word back over wide runes", start: "日本 語", ops: []string{"dwb"},
			want: "日本 ", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 7,
		},
		{
			name: "delete word back at a line start joins", start: "ab\ncd", ops: []string{"home", "dwb"},
			want: "abcd", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "kill to end", start: "one two", ops: []string{"home", "wright", "kend"},
			want: "one", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 5,
		},
		{
			name: "kill to end at a line end eats the break", start: "ab\ncd", ops: []string{"home", "up", "end", "kend"},
			want: "abcd", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "kill to start", start: "one two", ops: []string{"kstart"},
			want: "", wantLn: 0, wantOff: 0, wantRow: 0, wantCol: 2,
		},
		{
			name: "kill to start keeps the tail", start: "日本語abc", ops: []string{"home", "right", "right", "kstart"},
			want: "語abc", wantLn: 0, wantOff: 0, wantRow: 0, wantCol: 2,
		},
		{
			name: "kill to start at a line start is a no-op", start: "ab\ncd", ops: []string{"home", "kstart"},
			want: "ab\ncd", wantLn: 1, wantOff: 0, wantRow: 1, wantCol: 2,
		},
		{
			name: "home and end on a wide line", start: "日本語", ops: []string{"home", "end"},
			want: "日本語", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 8,
		},
		{
			name: "tabs fold to a space", ops: []string{"ins:a\tb"},
			want: "a b", wantLn: 0, wantOff: 3, wantRow: 0, wantCol: 5,
		},
		{
			name: "control runes are ignored", ops: []string{"ins:a\x07b"},
			want: "ab", wantLn: 0, wantOff: 2, wantRow: 0, wantCol: 4,
		},
		{
			name: "a newline rune inserts a break, never submits", ops: []string{"ins:a\nb"},
			want: "a\nb", wantLn: 1, wantOff: 1, wantRow: 1, wantCol: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			if tc.start != "" {
				c.SetDraft(tc.start)
			}
			applyOps(t, c, tc.ops)

			if got := c.Draft(); got != tc.want {
				t.Fatalf("Draft() = %q, want %q", got, tc.want)
			}
			if c.line != tc.wantLn || c.off != tc.wantOff {
				t.Fatalf("caret = (line %d, off %d), want (line %d, off %d)", c.line, c.off, tc.wantLn, tc.wantOff)
			}
			c.Render(40, true, false)
			if row, col := c.CursorPos(); row != tc.wantRow || col != tc.wantCol {
				t.Fatalf("CursorPos() = (%d, %d), want (%d, %d)", row, col, tc.wantRow, tc.wantCol)
			}
		})
	}
}

// TestUnit_PasteIsOneLiteralBlock pins that a multi-line paste lands as one insertion and is never re-read as a trigger.
func TestUnit_PasteIsOneLiteralBlock(t *testing.T) {
	cases := []struct {
		name    string
		start   string
		ops     []string
		paste   string
		want    string
		wantLn  int
		wantOff int
	}{
		{
			name:  "trailing newline is preserved",
			paste: "line one\nline two\n",
			want:  "line one\nline two\n", wantLn: 2, wantOff: 0,
		},
		{
			name:  "triggers inside a paste stay literal",
			paste: "hello\n/help\n!rm -rf /\n@file",
			want:  "hello\n/help\n!rm -rf /\n@file", wantLn: 3, wantOff: 5,
		},
		{
			name: "paste splits the line it lands in", start: "ab", ops: []string{"left"},
			paste: "X\nY",
			want:  "aX\nYb", wantLn: 1, wantOff: 1,
		},
		{
			name:  "CRLF and lone CR normalize to line breaks",
			paste: "a\r\nb\rc",
			want:  "a\nb\nc", wantLn: 2, wantOff: 1,
		},
		{
			name:  "tabs fold and other control runes drop",
			paste: "a\tb\x00c",
			want:  "a bc", wantLn: 0, wantOff: 4,
		},
		{
			name:  "a single-line paste keeps the caret in the line",
			start: "start end", ops: []string{"wleft"},
			paste: "middle ",
			want:  "start middle end", wantLn: 0, wantOff: 13,
		},
		{
			name:  "an empty paste is a no-op",
			start: "keep", paste: "",
			want: "keep", wantLn: 0, wantOff: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			if tc.start != "" {
				c.SetDraft(tc.start)
			}
			applyOps(t, c, tc.ops)
			c.InsertString(tc.paste)

			if got := c.Draft(); got != tc.want {
				t.Fatalf("Draft() = %q, want %q", got, tc.want)
			}
			if c.line != tc.wantLn || c.off != tc.wantOff {
				t.Fatalf("caret = (line %d, off %d), want (line %d, off %d)", c.line, c.off, tc.wantLn, tc.wantOff)
			}
		})
	}
}

// TestUnit_PastedTriggersSubmitAsOneChatTurn pins that a pasted block classifies as a whole, not by an embedded trigger line.
func TestUnit_PastedTriggersSubmitAsOneChatTurn(t *testing.T) {
	const paste = "look at this:\n/help\n!rm -rf /\n"

	c := New()
	c.InsertString(paste)
	sub, ok := c.Submit()
	if !ok {
		t.Fatal("Submit() refused a pasted block")
	}
	if sub.Kind != KindChat {
		t.Fatalf("Kind = %v, want chat", sub.Kind)
	}
	if sub.Text != paste {
		t.Fatalf("Text = %q, want the paste verbatim %q", sub.Text, paste)
	}
}

// TestUnit_Classify pins the trigger table: shell before slash, bare prefix falls through as chat.
func TestUnit_Classify(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantKind    Kind
		wantPayload string
	}{
		{"shell with a space", "! ls", KindShell, "ls"},
		{"shell without a space", "!ls -la", KindShell, "ls -la"},
		{"shell after leading whitespace", "  ! ls -la ", KindShell, "ls -la "},
		{"a bare prefix is chat", "!", KindChat, "!"},
		{"a prefix and whitespace is chat", "!  ", KindChat, "!  "},
		{"a prefix mid-line is chat", "wow! ls", KindChat, "wow! ls"},
		{"command after leading whitespace", "  /help", KindCommand, "  /help"},
		{"an unknown command is still a command", "/nonexistent", KindCommand, "/nonexistent"},
		{"a command with args", "/model qwen3", KindCommand, "/model qwen3"},
		{"a slash mid-line is chat", "hi /help", KindChat, "hi /help"},
		{"a pasted path is chat", "look at /etc/passwd", KindChat, "look at /etc/passwd"},
		{"plain prose is chat", "what does this do?", KindChat, "what does this do?"},
		{"multibyte prose is chat", "日本語で説明して", KindChat, "日本語で説明して"},
		{"a mention is chat", "@main.go what is this", KindChat, "@main.go what is this"},
		{"a multiline buffer classifies by its first token", "! ls\nsecond", KindShell, "ls\nsecond"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.text)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Text != tc.text {
				t.Fatalf("Text = %q, want the buffer verbatim %q", got.Text, tc.text)
			}
			if got.Payload != tc.wantPayload {
				t.Fatalf("Payload = %q, want %q", got.Payload, tc.wantPayload)
			}
		})
	}
}

// TestUnit_SubmitClearsAndRestoreLastPutsItBack pins the failed-validation path end to end.
func TestUnit_SubmitClearsAndRestoreLastPutsItBack(t *testing.T) {
	const text = "explain 日本語 please\nsecond line"

	c := New()
	c.SetDraft(text)

	sub, ok := c.Submit()
	if !ok {
		t.Fatal("Submit() refused a non-empty buffer")
	}
	if sub.Text != text {
		t.Fatalf("Text = %q, want %q", sub.Text, text)
	}
	if !c.Empty() {
		t.Fatalf("Submit() left the buffer as %q, want it cleared", c.Draft())
	}

	if !c.RestoreLast() {
		t.Fatal("RestoreLast() = false, want the submitted text back")
	}
	if got := c.Draft(); got != text {
		t.Fatalf("restored draft = %q, want %q", got, text)
	}
	if c.line != 1 || c.off != len([]rune("second line")) {
		t.Fatalf("restored caret = (%d, %d), want the end of the buffer", c.line, c.off)
	}

	// A second restore must not duplicate or resurrect anything.
	if c.RestoreLast() {
		t.Fatal("second RestoreLast() = true, want a no-op")
	}
	if got := c.Draft(); got != text {
		t.Fatalf("draft after the second RestoreLast = %q, want %q unchanged", got, text)
	}
}

// TestUnit_RestoreLastYieldsToTypedText pins that in-flight keystrokes win over the restore.
func TestUnit_RestoreLastYieldsToTypedText(t *testing.T) {
	c := New()
	c.SetDraft("first")
	if _, ok := c.Submit(); !ok {
		t.Fatal("Submit() refused")
	}
	c.InsertString("typed while in flight")

	if c.RestoreLast() {
		t.Fatal("RestoreLast() = true over a non-empty buffer")
	}
	if got := c.Draft(); got != "typed while in flight" {
		t.Fatalf("draft = %q, want the typed text kept", got)
	}
}

// TestUnit_SubmitWhitespaceOnlyIsNoOp pins that the buffer survives and nothing is retained for RestoreLast.
func TestUnit_SubmitWhitespaceOnlyIsNoOp(t *testing.T) {
	for _, draft := range []string{"", "   ", "\n", "  \n \n  "} {
		c := New()
		c.SetDraft(draft)

		if sub, ok := c.Submit(); ok {
			t.Fatalf("Submit(%q) = %+v, true; want a no-op", draft, sub)
		}
		if got := c.Draft(); got != draft {
			t.Fatalf("Submit(%q) changed the buffer to %q", draft, got)
		}
		if c.RestoreLast() {
			t.Fatalf("Submit(%q) retained something for RestoreLast", draft)
		}
	}
}

// TestUnit_ClearOrPass pins Ctrl+C: consume and clear with text, pass the chord on when empty.
func TestUnit_ClearOrPass(t *testing.T) {
	c := New()
	if c.ClearOrPass() {
		t.Fatal("ClearOrPass() consumed the chord on an empty buffer")
	}

	c.SetDraft("some text\nover two lines")
	if !c.ClearOrPass() {
		t.Fatal("ClearOrPass() did not consume the chord on a non-empty buffer")
	}
	if !c.Empty() {
		t.Fatalf("buffer = %q after clearing, want empty", c.Draft())
	}
	if c.line != 0 || c.off != 0 {
		t.Fatalf("caret = (%d, %d) after clearing, want (0, 0)", c.line, c.off)
	}

	// Whitespace is text: there is still something to clear.
	c.SetDraft("   ")
	if !c.ClearOrPass() {
		t.Fatal("ClearOrPass() did not consume the chord on a whitespace buffer")
	}
	if c.ClearOrPass() {
		t.Fatal("ClearOrPass() consumed the chord again on the now-empty buffer")
	}
}

// TestUnit_DraftRoundTrip pins the $EDITOR symmetry: Draft/SetDraft round-trip exactly, multibyte included.
func TestUnit_DraftRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"single line", "one line"},
		{"multiline", "first\nsecond\nthird"},
		{"trailing newline", "first\nsecond\n"},
		{"leading and interior blank lines", "\n\nbody\n\ntail"},
		{"wide runes", "日本語のテキスト\n二行目"},
		{"emoji", "ship it 🙂🚀\nnext 🙂 line"},
		{"mixed", "prompt 日本語 🙂 /not-a-command\n!not-shell"},
		{"only whitespace", "   \n  "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.SetDraft(tc.text)

			if got := c.Draft(); got != tc.text {
				t.Fatalf("Draft() = %q, want %q", got, tc.text)
			}
			lines := strings.Split(tc.text, "\n")
			wantLn := len(lines) - 1
			wantOff := len([]rune(lines[wantLn]))
			if c.line != wantLn || c.off != wantOff {
				t.Fatalf("caret = (%d, %d), want the end of the buffer (%d, %d)", c.line, c.off, wantLn, wantOff)
			}

			// Round-tripping through the editor twice must be stable.
			c.SetDraft(c.Draft())
			if got := c.Draft(); got != tc.text {
				t.Fatalf("second round trip = %q, want %q", got, tc.text)
			}
		})
	}
}

// TestUnit_SetDraftSanitizes pins that the buffer only ever holds printable runes and line breaks.
func TestUnit_SetDraftSanitizes(t *testing.T) {
	c := New()
	c.SetDraft("a\tb\x07c\r\nsecond\rthird")
	if got, want := c.Draft(), "a bc\nsecond\nthird"; got != want {
		t.Fatalf("Draft() = %q, want %q", got, want)
	}
}
