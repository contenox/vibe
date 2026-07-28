package input

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func namedKey(k Key) Event { return KeyEvent{Key: k} }

func runeKey(r rune) Event { return KeyEvent{Key: KeyRune, Rune: r} }

func ctrlKey(r rune) Event { return KeyEvent{Key: KeyRune, Rune: r, Ctrl: true} }

func altKey(r rune) Event { return KeyEvent{Key: KeyRune, Rune: r, Alt: true} }

func assertEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestUnit_ControlBytesMapToKeys(t *testing.T) {
	cases := []struct {
		name string
		in   byte
		want Event
	}{
		{"enter", 0x0d, namedKey(KeyEnter)},
		{"newline chord is ctrl+j", 0x0a, ctrlKey('j')},
		{"tab", 0x09, namedKey(KeyTab)},
		{"backspace", 0x7f, namedKey(KeyBackspace)},
		{"ctrl+h", 0x08, ctrlKey('h')},
		{"ctrl+space", 0x00, ctrlKey(' ')},
		{"ctrl+a", 0x01, ctrlKey('a')},
		{"ctrl+c", 0x03, ctrlKey('c')},
		{"ctrl+u", 0x15, ctrlKey('u')},
		{"ctrl+z", 0x1a, ctrlKey('z')},
		{"ctrl+backslash", 0x1c, ctrlKey('\\')},
		{"ctrl+rightbracket", 0x1d, ctrlKey(']')},
		{"ctrl+caret", 0x1e, ctrlKey('^')},
		{"ctrl+underscore", 0x1f, ctrlKey('_')},
		{"space is a rune", 0x20, runeKey(' ')},
		{"tilde is a rune", 0x7e, runeKey('~')},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			assertEvents(t, p.Feed([]byte{c.in}), []Event{c.want})
			if p.Pending() {
				t.Fatal("Pending() after a complete byte")
			}
		})
	}
}

func TestUnit_SequencesDecode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Event
	}{
		{"printable runes", "abc", []Event{runeKey('a'), runeKey('b'), runeKey('c')}},
		{"multibyte runes", "é日👍", []Event{runeKey('é'), runeKey('日'), runeKey('👍')}},

		{"alt+rune", "\x1bx", []Event{altKey('x')}},
		{"alt+multibyte rune", "\x1bé", []Event{KeyEvent{Key: KeyRune, Rune: 'é', Alt: true}}},
		{"alt+enter", "\x1b\r", []Event{KeyEvent{Key: KeyEnter, Alt: true}}},
		{"alt+backspace", "\x1b\x7f", []Event{KeyEvent{Key: KeyBackspace, Alt: true}}},
		{"alt+ctrl+a", "\x1b\x01", []Event{KeyEvent{Key: KeyRune, Rune: 'a', Ctrl: true, Alt: true}}},
		{"esc esc emits escape", "\x1b\x1bx", []Event{namedKey(KeyEscape), altKey('x')}},

		{"csi up", "\x1b[A", []Event{namedKey(KeyUp)}},
		{"csi down", "\x1b[B", []Event{namedKey(KeyDown)}},
		{"csi right", "\x1b[C", []Event{namedKey(KeyRight)}},
		{"csi left", "\x1b[D", []Event{namedKey(KeyLeft)}},
		{"csi home", "\x1b[H", []Event{namedKey(KeyHome)}},
		{"csi end", "\x1b[F", []Event{namedKey(KeyEnd)}},

		{"csi 1~ home", "\x1b[1~", []Event{namedKey(KeyHome)}},
		{"csi 7~ home", "\x1b[7~", []Event{namedKey(KeyHome)}},
		{"csi 4~ end", "\x1b[4~", []Event{namedKey(KeyEnd)}},
		{"csi 8~ end", "\x1b[8~", []Event{namedKey(KeyEnd)}},
		{"csi 3~ delete", "\x1b[3~", []Event{namedKey(KeyDelete)}},
		{"csi 5~ pgup", "\x1b[5~", []Event{namedKey(KeyPgUp)}},
		{"csi 6~ pgdn", "\x1b[6~", []Event{namedKey(KeyPgDn)}},

		{"focus in", "\x1b[I", []Event{FocusEvent{Focused: true}}},
		{"focus out", "\x1b[O", []Event{FocusEvent{Focused: false}}},

		{"ss3 up", "\x1bOA", []Event{namedKey(KeyUp)}},
		{"ss3 down", "\x1bOB", []Event{namedKey(KeyDown)}},
		{"ss3 right", "\x1bOC", []Event{namedKey(KeyRight)}},
		{"ss3 left", "\x1bOD", []Event{namedKey(KeyLeft)}},
		{"ss3 home", "\x1bOH", []Event{namedKey(KeyHome)}},
		{"ss3 end", "\x1bOF", []Event{namedKey(KeyEnd)}},
		{"unknown ss3 dropped", "\x1bOPa", []Event{runeKey('a')}},

		{"unknown csi letter dropped", "\x1b[Za", []Event{runeKey('a')}},
		{"unknown csi tilde dropped", "\x1b[99~a", []Event{runeKey('a')}},
		{"mouse report dropped", "\x1b[<0;12;34Ma", []Event{runeKey('a')}},
		{"device report dropped", "\x1b[?1;2ca", []Event{runeKey('a')}},
		{"stray paste close dropped", "\x1b[201~a", []Event{runeKey('a')}},
		{"malformed csi resyncs", "\x1b[12\x1b[Aa", []Event{namedKey(KeyUp), runeKey('a')}},

		{"mixed stream", "a\x1b[Ab\r", []Event{
			runeKey('a'), namedKey(KeyUp), runeKey('b'), namedKey(KeyEnter),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			assertEvents(t, p.Feed([]byte(c.in)), c.want)
			if p.Pending() {
				t.Fatal("Pending() after a complete stream")
			}
		})
	}
}

func TestUnit_ModifierBitmaskDecodes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Event
	}{
		{"ctrl+up", "\x1b[1;5A", KeyEvent{Key: KeyUp, Ctrl: true}},
		{"shift+right", "\x1b[1;2C", KeyEvent{Key: KeyRight, Shift: true}},
		{"alt+left", "\x1b[1;3D", KeyEvent{Key: KeyLeft, Alt: true}},
		{"ctrl+alt+down", "\x1b[1;7B", KeyEvent{Key: KeyDown, Ctrl: true, Alt: true}},
		{"shift+alt+ctrl+left", "\x1b[1;8D", KeyEvent{Key: KeyLeft, Shift: true, Alt: true, Ctrl: true}},
		{"ctrl+home", "\x1b[1;5H", KeyEvent{Key: KeyHome, Ctrl: true}},
		{"alt+delete", "\x1b[3;3~", KeyEvent{Key: KeyDelete, Alt: true}},
		{"ctrl+pgup", "\x1b[5;5~", KeyEvent{Key: KeyPgUp, Ctrl: true}},
		{"shift+end", "\x1b[8;2~", KeyEvent{Key: KeyEnd, Shift: true}},
		{"modifier 1 is no modifier", "\x1b[1;1A", KeyEvent{Key: KeyUp}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			assertEvents(t, p.Feed([]byte(c.in)), []Event{c.want})
		})
	}
}

func TestUnit_MultibyteRuneSplitAcrossFeeds(t *testing.T) {
	p := NewParser()
	for i, b := range []byte("\xe6\x97") { // first two bytes of 日
		if evs := p.Feed([]byte{b}); evs != nil {
			t.Fatalf("byte %d produced %#v, want nothing yet", i, evs)
		}
		// A partial rune waits for bytes, not for a clock: it is not pending,
		// because no idle gap could tell the parser what it is.
		if p.Pending() {
			t.Fatalf("Pending() true after byte %d of a partial rune", i)
		}
	}
	assertEvents(t, p.Feed([]byte{0xa5}), []Event{runeKey('日')})
	if p.Pending() {
		t.Fatal("Pending() after the rune completed")
	}
}

func TestUnit_PendingAndFlush(t *testing.T) {
	t.Run("lone esc flushes to escape", func(t *testing.T) {
		p := NewParser()
		if evs := p.Feed([]byte{esc}); evs != nil {
			t.Fatalf("lone ESC emitted %#v, want nothing", evs)
		}
		if !p.Pending() {
			t.Fatal("Pending() false with a lone ESC buffered")
		}
		assertEvents(t, p.Flush(), []Event{namedKey(KeyEscape)})
		if p.Pending() {
			t.Fatal("Pending() after Flush")
		}
		assertEvents(t, p.Flush(), nil)
	})

	t.Run("esc completed by a later feed is alt", func(t *testing.T) {
		p := NewParser()
		p.Feed([]byte{esc})
		assertEvents(t, p.Feed([]byte("x")), []Event{altKey('x')})
		if p.Pending() {
			t.Fatal("Pending() after Alt+x completed")
		}
	})

	// A multi-byte partial is never resolved by time. Flushing it would mean
	// guessing, and the guess that matters — a split "\x1b[200~" — turns a
	// pasted document into keystrokes. It waits for bytes instead.
	t.Run("partial csi survives flush and completes later", func(t *testing.T) {
		p := NewParser()
		if evs := p.Feed([]byte("\x1b[1;")); evs != nil {
			t.Fatalf("partial CSI emitted %#v, want nothing", evs)
		}
		if p.Pending() {
			t.Fatal("Pending() true with a partial CSI buffered: no idle gap can resolve it")
		}
		assertEvents(t, p.Flush(), nil)
		assertEvents(t, p.Feed([]byte("5A")), []Event{KeyEvent{Key: KeyUp, Ctrl: true}})
	})

	t.Run("partial utf8 survives flush and completes later", func(t *testing.T) {
		p := NewParser()
		p.Feed([]byte("\xe6\x97"))
		if p.Pending() {
			t.Fatal("Pending() true with a partial rune buffered")
		}
		assertEvents(t, p.Flush(), nil)
		assertEvents(t, p.Feed([]byte("\xa5")), []Event{runeKey('日')})
	})

	// The buffer is bounded by LENGTH instead of by time: a prefix no escape
	// sequence could still become is dropped whole, never leaked as runes.
	t.Run("an over-long incomplete sequence is dropped whole", func(t *testing.T) {
		p := NewParser()
		if evs := p.Feed([]byte("\x1b[" + strings.Repeat("1", maxPendingBytes+8))); evs != nil {
			t.Fatalf("over-long prefix emitted %#v, want it dropped silently", evs)
		}
		if p.Pending() {
			t.Fatal("Pending() after the malformed prefix was dropped")
		}
		assertEvents(t, p.Feed([]byte("a")), []Event{runeKey('a')})
	})

	t.Run("open paste is not pending and survives flush", func(t *testing.T) {
		p := NewParser()
		p.Feed([]byte("\x1b[200~ab"))
		if p.Pending() {
			t.Fatal("Pending() true during an open paste")
		}
		assertEvents(t, p.Flush(), nil)
		assertEvents(t, p.Feed([]byte("c\x1b[201~")), []Event{PasteEvent{Text: "abc"}})
	})
}

func TestUnit_BracketedPaste(t *testing.T) {
	t.Run("accumulates across feeds", func(t *testing.T) {
		p := NewParser()
		if evs := p.Feed([]byte("\x1b[200~hello ")); evs != nil {
			t.Fatalf("open paste emitted %#v, want nothing", evs)
		}
		if evs := p.Feed([]byte("brave ")); evs != nil {
			t.Fatalf("paste body emitted %#v, want nothing", evs)
		}
		assertEvents(t, p.Feed([]byte("world\x1b[201~")), []Event{PasteEvent{Text: "hello brave world"}})
	})

	t.Run("normalizes carriage returns", func(t *testing.T) {
		p := NewParser()
		got := p.Feed([]byte("\x1b[200~a\r\nb\rc\n\rd\x1b[201~"))
		assertEvents(t, got, []Event{PasteEvent{Text: "a\nb\nc\n\nd"}})
	})

	t.Run("normalizes a crlf split across feeds", func(t *testing.T) {
		p := NewParser()
		p.Feed([]byte("\x1b[200~a\r"))
		assertEvents(t, p.Feed([]byte("\nb\x1b[201~")), []Event{PasteEvent{Text: "a\nb"}})
	})

	t.Run("embedded escape sequences stay literal", func(t *testing.T) {
		body := "\x1b[31mred\x1b[0m \x1b[200~ \x1bOA \x1b"
		p := NewParser()
		assertEvents(t, p.Feed([]byte("\x1b[200~"+body+"\x1b[201~")), []Event{PasteEvent{Text: body}})
		if p.Pending() {
			t.Fatal("Pending() after the paste closed")
		}
	})

	t.Run("close terminator split across feeds", func(t *testing.T) {
		p := NewParser()
		if evs := p.Feed([]byte("\x1b[200~x\x1b[20")); evs != nil {
			t.Fatalf("partial terminator emitted %#v, want nothing", evs)
		}
		if p.Pending() {
			t.Fatal("Pending() true while a partial terminator is held back")
		}
		assertEvents(t, p.Feed([]byte("1~")), []Event{PasteEvent{Text: "x"}})
	})

	t.Run("near miss terminator stays payload", func(t *testing.T) {
		p := NewParser()
		p.Feed([]byte("\x1b[200~x\x1b[20"))
		assertEvents(t, p.Feed([]byte("2~y\x1b[201~")), []Event{PasteEvent{Text: "x\x1b[202~y"}})
	})

	t.Run("keys resume after the paste", func(t *testing.T) {
		p := NewParser()
		got := p.Feed([]byte("\x1b[200~text\x1b[201~\x1b[Aq"))
		assertEvents(t, got, []Event{PasteEvent{Text: "text"}, namedKey(KeyUp), runeKey('q')})
	})

	// Blowing the cap ends the event, not paste mode: the overflow is
	// swallowed up to the terminator. Replaying it through key decoding would
	// hand the app a document as keystrokes — every "\r" an Enter — which is
	// precisely what bracketed paste exists to prevent, so the cap must not
	// become a way to trigger it.
	t.Run("safety cap emits what fits and discards the rest of the paste", func(t *testing.T) {
		p := NewParser()
		overflow := "b\rc\ndrop me"
		got := p.Feed([]byte("\x1b[200~" + strings.Repeat("a", maxPasteBytes) + overflow + "\x1b[201~"))
		if len(got) != 1 {
			t.Fatalf("got %#v, want exactly the capped paste", got)
		}
		paste, ok := got[0].(PasteEvent)
		if !ok {
			t.Fatalf("event is %#v, want PasteEvent", got[0])
		}
		if len(paste.Text) != maxPasteBytes {
			t.Fatalf("paste is %d bytes, want the %d-byte cap", len(paste.Text), maxPasteBytes)
		}
		if strings.ContainsAny(paste.Text, "bcd") {
			t.Fatal("paste text carries payload from past the cap")
		}
		// Key decoding resumes only after the terminator.
		assertEvents(t, p.Feed([]byte("q")), []Event{runeKey('q')})
	})

	t.Run("safety cap never cuts a rune in half", func(t *testing.T) {
		p := NewParser()
		// The cap falls one byte into the multibyte rune that follows.
		body := strings.Repeat("a", maxPasteBytes-1) + "日本"
		got := p.Feed([]byte("\x1b[200~" + body + "\x1b[201~"))
		if len(got) != 1 {
			t.Fatalf("got %#v, want exactly the capped paste", got)
		}
		paste := got[0].(PasteEvent)
		if !utf8.ValidString(paste.Text) {
			t.Fatal("the cap cut inside a UTF-8 rune")
		}
		if len(paste.Text) != maxPasteBytes-1 {
			t.Fatalf("paste is %d bytes, want it backed off to the %d-byte rune boundary", len(paste.Text), maxPasteBytes-1)
		}
	})

	t.Run("the terminator that ends a discarded overflow may be split", func(t *testing.T) {
		p := NewParser()
		got := p.Feed([]byte("\x1b[200~" + strings.Repeat("a", maxPasteBytes) + "tail\x1b[20"))
		if len(got) != 1 {
			t.Fatalf("got %#v, want exactly the capped paste", got)
		}
		if evs := p.Feed([]byte("x\x1b[20")); evs != nil {
			t.Fatalf("discarded overflow emitted %#v, want nothing", evs)
		}
		if p.Pending() {
			t.Fatal("Pending() true while discarding an over-cap paste")
		}
		assertEvents(t, p.Feed([]byte("1~z")), []Event{runeKey('z')})
	})
}

// TestUnit_SplitPasteOpenerSurvivesAnIdleFlush pins that a paste opener
// split by an idle flush still decodes as a paste, not five keystrokes.
func TestUnit_SplitPasteOpenerSurvivesAnIdleFlush(t *testing.T) {
	p := NewParser()
	if evs := p.Feed([]byte{esc}); evs != nil {
		t.Fatalf("lone ESC emitted %#v, want nothing yet", evs)
	}
	if !p.Pending() {
		t.Fatal("Pending() false with a lone ESC buffered")
	}
	assertEvents(t, p.Flush(), []Event{namedKey(KeyEscape)})

	if evs := p.Feed([]byte("[200~line one\r")); evs != nil {
		t.Fatalf("paste body emitted %#v, want it accumulating", evs)
	}
	got := p.Feed([]byte("line two\x1b[201~"))
	assertEvents(t, got, []Event{PasteEvent{Text: "line one\nline two"}})
	for _, ev := range got {
		if key, ok := ev.(KeyEvent); ok && key.Key == KeyEnter {
			t.Fatal("a pasted newline fired Enter")
		}
	}
}

// TestUnit_FlushedEscapeOnlyRevivesForAPasteOpener pins that only a whole
// paste opener revives a flushed ESC; anything else decodes normally.
func TestUnit_FlushedEscapeOnlyRevivesForAPasteOpener(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Event
	}{
		{"unrelated runes", "ab", []Event{runeKey('a'), runeKey('b')}},
		{"a cursor key that is not a paste opener", "[A", []Event{runeKey('['), runeKey('A')}},
		{"a near miss on the opener", "[201~", []Event{runeKey('['), runeKey('2'), runeKey('0'), runeKey('1'), runeKey('~')}},
		{"a bracket typed by hand", "[", []Event{runeKey('[')}},
		{"the opener typed one byte at a time", "[2", []Event{runeKey('['), runeKey('2')}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewParser()
			p.Feed([]byte{esc})
			assertEvents(t, p.Flush(), []Event{namedKey(KeyEscape)})
			assertEvents(t, p.Feed([]byte(c.in)), c.want)
		})
	}
}

// TestUnit_AbsurdCSIParametersAreDroppedWhole pins that an oversized CSI
// parameter field is dropped rather than overflowing onto a value like 200.
func TestUnit_AbsurdCSIParametersAreDroppedWhole(t *testing.T) {
	cases := []string{
		"\x1b[" + strings.Repeat("0", 17) + "200~",
		"\x1b[" + strings.Repeat("1;", maxCSIParams+2) + "A",
	}
	for _, probe := range cases {
		t.Run(probe[:8], func(t *testing.T) {
			p := NewParser()
			if evs := p.Feed([]byte(probe)); evs != nil {
				t.Fatalf("probe emitted %#v, want it dropped whole", evs)
			}
			// Not in paste mode: the next carriage return is still a keystroke.
			assertEvents(t, p.Feed([]byte("\r")), []Event{namedKey(KeyEnter)})
		})
	}
}

func TestUnit_ChunkingMatchesWholeFeed(t *testing.T) {
	streams := []struct {
		name string
		in   string
	}{
		{"mixed keys", "hi\x1b[A\x1b[1;5B\x1bx\r\n\t\x00\x1b[3;3~\x1bOD\x1b[I"},
		{"utf8", "héllo 日本語👍\x1bé"},
		{"paste with embedded escapes", "before\x1b[200~line1\r\nline2\x1b[31mred\x1b[0m\rtail\x1b[201~after\x1b[F"},
		{"trailing lone escape", "abc\x1b"},
		{"escape escape", "\x1b\x1b"},
		{"partial utf8 tail", "ok\xe6\x97"},
		{"partial csi tail", "ok\x1b[1;"},
		{"unterminated paste", "\x1b[200~half\x1b[20"},
	}
	for _, s := range streams {
		t.Run(s.name, func(t *testing.T) {
			whole := NewParser()
			want := append(whole.Feed([]byte(s.in)), whole.Flush()...)

			piecewise := NewParser()
			var got []Event
			for i := 0; i < len(s.in); i++ {
				got = append(got, piecewise.Feed([]byte{s.in[i]})...)
			}
			got = append(got, piecewise.Flush()...)

			assertEvents(t, got, want)
		})
	}
}
