package input

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

const (
	esc = 0x1b

	// maxPasteBytes caps one bracketed paste; past it the paste is
	// terminated with what accumulated and the overflow is swallowed up to
	// the terminator rather than replayed (see scanPaste).
	maxPasteBytes = 10 << 20

	// maxPendingBytes bounds an unresolved prefix by length rather than
	// time, since Flush cannot guess an incomplete sequence without risking
	// a paste body decoded as keystrokes.
	maxPendingBytes = 64

	// maxCSIParams and maxCSIDigits bound what a CSI sequence may claim to
	// be. An oversized field could overflow the accumulator and alias onto
	// a meaningful value (200, the paste opener); such a sequence is
	// dropped whole.
	maxCSIParams = 16
	maxCSIDigits = 6
)

// pasteEnd is the only byte sequence that closes a bracketed paste; escape
// sequences inside the paste are literal payload.
var pasteEnd = []byte("\x1b[201~")

// pasteStart is the bracketed-paste opener. It is matched byte by byte only
// on the one path that has to re-assemble it — see Parser.flushedEsc.
var pasteStart = []byte("\x1b[200~")

// pasteNewlines normalizes the line endings raw-mode terminals deliver in a
// paste (\r\n and lone \r) to \n.
var pasteNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// Parser decodes a raw terminal byte stream into Events. It is a pure state
// machine (no I/O, no clock, no goroutines), so feeding the same bytes
// always yields the same events regardless of chunking. Serves one input
// stream and is not safe for concurrent use.
type Parser struct {
	buf     []byte
	paste   []byte
	inPaste bool

	// discarding swallows the remainder of a paste that blew the size cap,
	// up to and including its terminator. Without it the overflow would be
	// re-parsed as keystrokes, and every \r in a pasted document would fire
	// Enter — the one thing bracketed paste exists to prevent.
	discarding bool

	// flushedEsc records that the previous Flush resolved a lone ESC. The
	// next Feed may put that ESC back in front of its bytes when they are
	// still on their way to a paste opener; see Feed.
	flushedEsc bool
}

// NewParser returns a Parser with an empty buffer.
func NewParser() *Parser { return &Parser{} }

// Feed decodes b and returns every event it completes, in stream order.
// Bytes forming an incomplete prefix (a partial escape sequence or UTF-8
// rune) are buffered and decoded when the rest arrives. An open bracketed
// paste accumulates across calls and yields exactly one PasteEvent at its
// terminator.
//
// One repair happens here: if the previous Flush resolved a lone ESC as
// KeyEscape and these bytes spell the rest of a split paste opener, the ESC
// is put back so the opener is still recognized whole, at the cost of one
// spurious KeyEscape the app already saw.
func (p *Parser) Feed(b []byte) []Event {
	if len(b) == 0 {
		return nil
	}
	flushed := p.flushedEsc
	p.flushedEsc = false
	if flushed && len(p.buf) == 0 && !p.inPaste && !p.discarding && completesPasteStart(b) {
		p.buf = append(p.buf, esc)
	}
	p.buf = append(p.buf, b...)
	out := p.decode()
	if !p.inPaste && !p.discarding && len(p.buf) > maxPendingBytes {
		p.buf = nil // malformed beyond any sequence beam knows: dropped whole
	}
	return out
}

// Pending reports whether an idle gap would resolve something: a lone ESC,
// the one ambiguity in the protocol that only time can settle. Every other
// incomplete prefix waits for bytes, not a clock, and is therefore not
// pending; an open paste is never pending either.
func (p *Parser) Pending() bool {
	return !p.inPaste && !p.discarding && len(p.buf) == 1 && p.buf[0] == esc
}

// Flush resolves the lone-ESC ambiguity as KeyEscape; the engine calls it
// after a short idle whenever Pending reports true. It resolves nothing
// else: a multi-byte partial sequence stays buffered, bounded by length
// instead (maxPendingBytes). Flush cannot end an open paste either — one
// interrupted by EOF or Close is dropped rather than delivered
// half-complete, since a truncated document is worse than none.
func (p *Parser) Flush() []Event {
	if p.inPaste || p.discarding || len(p.buf) == 0 {
		return nil
	}
	if len(p.buf) == 1 && p.buf[0] == esc {
		p.buf = nil
		p.flushedEsc = true
		return []Event{KeyEvent{Key: KeyEscape}}
	}
	return nil
}

// decode drains p.buf, leaving any prefix it cannot yet resolve buffered.
func (p *Parser) decode() []Event {
	var out []Event
	for len(p.buf) > 0 {
		var (
			evs  []Event
			n    int
			done bool
		)
		switch {
		case p.discarding:
			evs, n, done = p.scanDiscard()
		case p.inPaste:
			evs, n, done = p.scanPaste()
		default:
			evs, n, done = p.scanEvent()
		}
		out = append(out, evs...)
		p.buf = p.buf[n:]
		if !done {
			break
		}
	}
	if len(p.buf) == 0 {
		p.buf = nil
	}
	return out
}

// scanEvent decodes one event from the front of p.buf, returning the bytes
// consumed and whether the buffer held a complete unit.
func (p *Parser) scanEvent() ([]Event, int, bool) {
	b := p.buf[0]
	if b == esc {
		return p.scanEscape()
	}
	if b < utf8.RuneSelf {
		return []Event{asciiKey(b, false)}, 1, true
	}
	if !utf8.FullRune(p.buf) {
		return nil, 0, false
	}
	r, size := utf8.DecodeRune(p.buf)
	if r == utf8.RuneError && size <= 1 {
		return nil, 1, true
	}
	return []Event{KeyEvent{Key: KeyRune, Rune: r}}, size, true
}

// scanEscape decodes an ESC-introduced form: CSI, SS3, ESC ESC, or an
// Alt-modified key.
func (p *Parser) scanEscape() ([]Event, int, bool) {
	if len(p.buf) < 2 {
		return nil, 0, false
	}
	switch b := p.buf[1]; {
	case b == '[':
		return p.scanCSI()
	case b == 'O':
		return p.scanSS3()
	case b == esc:
		// The first ESC resolves as Escape; the second stays buffered and
		// may still introduce a sequence of its own.
		return []Event{KeyEvent{Key: KeyEscape}}, 1, true
	case b < utf8.RuneSelf:
		return []Event{asciiKey(b, true)}, 2, true
	}
	if !utf8.FullRune(p.buf[1:]) {
		return nil, 0, false
	}
	r, size := utf8.DecodeRune(p.buf[1:])
	if r == utf8.RuneError && size <= 1 {
		return nil, 2, true
	}
	return []Event{KeyEvent{Key: KeyRune, Rune: r, Alt: true}}, 1 + size, true
}

// scanCSI decodes ESC [ ... <final>. A sequence whose final byte is missing
// stays buffered; one interrupted by a byte that cannot belong to it is
// consumed up to that byte, which is then reparsed as a fresh unit.
func (p *Parser) scanCSI() ([]Event, int, bool) {
	i := 2
	for i < len(p.buf) && p.buf[i] >= 0x30 && p.buf[i] <= 0x3f {
		i++
	}
	params := string(p.buf[2:i])
	for i < len(p.buf) && p.buf[i] >= 0x20 && p.buf[i] <= 0x2f {
		i++
	}
	if i == len(p.buf) {
		return nil, 0, false
	}
	final := p.buf[i]
	if final < 0x40 || final > 0x7e {
		return nil, i, true
	}
	if !saneParams(params) {
		return nil, i + 1, true // consumed and dropped whole
	}
	return p.csiEvent(params, final), i + 1, true
}

// saneParams rejects a parameter string no terminal would emit: more fields
// than any key encoding uses, or a field with more digits than csiParams can
// accumulate without overflowing onto a meaningful value like 200.
func saneParams(s string) bool {
	if s == "" {
		return true
	}
	fields, digits := 1, 0
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			fields++
			digits = 0
			if fields > maxCSIParams {
				return false
			}
			continue
		}
		digits++
		if digits > maxCSIDigits {
			return false
		}
	}
	return true
}

// scanSS3 decodes ESC O <letter>, the cursor-key form terminals emit in
// application keypad mode.
func (p *Parser) scanSS3() ([]Event, int, bool) {
	if len(p.buf) < 3 {
		return nil, 0, false
	}
	k, ok := cursorKey(p.buf[2])
	if !ok {
		return nil, 3, true
	}
	return []Event{KeyEvent{Key: k}}, 3, true
}

// csiEvent dispatches a complete CSI sequence. Forms beam does not use —
// including mouse and private-marker sequences — return no event: they are
// dropped, never leaked as runes.
func (p *Parser) csiEvent(params string, final byte) []Event {
	if strings.ContainsAny(params, ":<=>?") {
		return nil
	}
	nums := csiParams(params)
	switch final {
	case 'I':
		return []Event{FocusEvent{Focused: true}}
	case 'O':
		return []Event{FocusEvent{Focused: false}}
	case '~':
		if len(nums) == 0 {
			return nil
		}
		if nums[0] == 200 {
			p.inPaste = true
			return nil
		}
		k, ok := tildeKey(nums[0])
		if !ok {
			return nil
		}
		ev := KeyEvent{Key: k}
		applyMod(&ev, modParam(nums))
		return []Event{ev}
	}
	k, ok := cursorKey(final)
	if !ok {
		return nil
	}
	ev := KeyEvent{Key: k}
	applyMod(&ev, modParam(nums))
	return []Event{ev}
}

// scanPaste accumulates paste payload until the exact ESC[201~ terminator,
// holding back only the tail that could still grow into it. Blowing the cap
// does not end paste mode, only the event: what fits is emitted (cut back
// to a rune boundary), and the parser discards everything up to the
// terminator rather than feeding the overflow back through key decoding.
func (p *Parser) scanPaste() ([]Event, int, bool) {
	take, consumed, done := len(p.buf), len(p.buf), false
	if i := bytes.Index(p.buf, pasteEnd); i >= 0 {
		take, consumed, done = i, i+len(pasteEnd), true
	} else if k := partialSuffix(p.buf, pasteEnd); k > 0 {
		take, consumed = len(p.buf)-k, len(p.buf)-k
	}
	if room := maxPasteBytes - len(p.paste); take >= room {
		p.paste = append(p.paste, p.buf[:room]...)
		p.paste = p.paste[:trimPartialRune(p.paste)]
		p.discarding = true
		return []Event{p.closePaste()}, room, true
	}
	p.paste = append(p.paste, p.buf[:take]...)
	if !done {
		return nil, consumed, false
	}
	return []Event{p.closePaste()}, consumed, true
}

// scanDiscard swallows the tail of an over-cap paste, holding back only the
// bytes that could still grow into the terminator so a terminator split
// across Feeds is still recognised. Key decoding resumes after it.
func (p *Parser) scanDiscard() ([]Event, int, bool) {
	if i := bytes.Index(p.buf, pasteEnd); i >= 0 {
		p.discarding = false
		return nil, i + len(pasteEnd), true
	}
	if k := partialSuffix(p.buf, pasteEnd); k > 0 {
		return nil, len(p.buf) - k, false
	}
	return nil, len(p.buf), false
}

// trimPartialRune returns the length of b with a trailing incomplete UTF-8
// rune removed, so a hard cut never emits half a character. Bytes that are
// not UTF-8 at all are left alone: there is no boundary to find, and the
// paste is delivered as the terminal sent it.
func trimPartialRune(b []byte) int {
	n := len(b)
	for i := 1; i <= utf8.UTFMax && i <= n; i++ {
		if !utf8.RuneStart(b[n-i]) {
			continue
		}
		if r, size := utf8.DecodeRune(b[n-i:]); size == i && (r != utf8.RuneError || size > 1) {
			return n // the last rune is complete
		}
		return n - i // an incomplete (or invalid) trailing rune goes with the overflow
	}
	return n
}

// completesPasteStart reports whether b, appended to an ESC that Flush
// already resolved, spells the bracketed-paste opener whole. Requiring the
// whole opener in one chunk avoids re-attaching on a bare "[", which would
// otherwise swallow a user pressing Escape then typing "[".
func completesPasteStart(b []byte) bool {
	return bytes.HasPrefix(b, pasteStart[1:])
}

// closePaste emits the accumulated payload as one event with its line
// endings normalized, and leaves paste mode. The rewrite is skipped when
// there is nothing to rewrite: a paste is the one payload that can be
// megabytes.
func (p *Parser) closePaste() Event {
	text := string(p.paste)
	if bytes.IndexByte(p.paste, '\r') >= 0 {
		text = pasteNewlines.Replace(text)
	}
	p.paste = nil
	p.inPaste = false
	return PasteEvent{Text: text}
}

// asciiKey maps a single-byte code to its KeyEvent, applying the control
// chords terminals deliver in raw mode. Enter is 0x0D; 0x0A is the separate
// Ctrl+J newline chord, never folded into it.
func asciiKey(b byte, alt bool) KeyEvent {
	ev := KeyEvent{Alt: alt}
	switch {
	case b == 0x0d:
		ev.Key = KeyEnter
	case b == 0x09:
		ev.Key = KeyTab
	case b == 0x7f:
		ev.Key = KeyBackspace
	case b == 0x00:
		ev.Key, ev.Rune, ev.Ctrl = KeyRune, ' ', true
	case b == 0x0a:
		ev.Key, ev.Rune, ev.Ctrl = KeyRune, 'j', true
	case b >= 0x01 && b <= 0x1a:
		ev.Key, ev.Rune, ev.Ctrl = KeyRune, rune('a'+b-1), true
	case b >= 0x1c && b <= 0x1f:
		ev.Key, ev.Rune, ev.Ctrl = KeyRune, rune("\\]^_"[b-0x1c]), true
	default:
		ev.Key, ev.Rune = KeyRune, rune(b)
	}
	return ev
}

// cursorKey maps the final byte shared by the CSI and SS3 cursor forms.
func cursorKey(b byte) (Key, bool) {
	switch b {
	case 'A':
		return KeyUp, true
	case 'B':
		return KeyDown, true
	case 'C':
		return KeyRight, true
	case 'D':
		return KeyLeft, true
	case 'H':
		return KeyHome, true
	case 'F':
		return KeyEnd, true
	}
	return KeyRune, false
}

// tildeKey maps the numeric CSI <n>~ forms. Both historical encodings of
// Home (1, 7) and End (4, 8) are accepted.
func tildeKey(n int) (Key, bool) {
	switch n {
	case 1, 7:
		return KeyHome, true
	case 4, 8:
		return KeyEnd, true
	case 3:
		return KeyDelete, true
	case 5:
		return KeyPgUp, true
	case 6:
		return KeyPgDn, true
	}
	return KeyRune, false
}

// csiParams splits a CSI parameter string into its numeric fields; empty
// fields decode as 0, matching the terminal convention of omitting defaults.
func csiParams(s string) []int {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, ";")
	out := make([]int, len(fields))
	for i, f := range fields {
		n := 0
		for j := 0; j < len(f); j++ {
			n = n*10 + int(f[j]-'0')
		}
		out[i] = n
	}
	return out
}

// modParam returns the modifier field of a CSI sequence, which is always the
// second parameter, or 0 when absent.
func modParam(nums []int) int {
	if len(nums) < 2 {
		return 0
	}
	return nums[1]
}

// applyMod sets the flags encoded in a CSI modifier parameter, where m-1 is
// a bitmask: 1 Shift, 2 Alt, 4 Ctrl.
func applyMod(ev *KeyEvent, m int) {
	if m < 2 {
		return
	}
	bits := m - 1
	ev.Shift = bits&1 != 0
	ev.Alt = bits&2 != 0
	ev.Ctrl = bits&4 != 0
}

// partialSuffix returns the length of the longest suffix of b that is a
// proper prefix of sep — the bytes that must stay buffered because the next
// Feed could complete sep.
func partialSuffix(b, sep []byte) int {
	max := len(sep) - 1
	if len(b) < max {
		max = len(b)
	}
	for k := max; k > 0; k-- {
		if bytes.Equal(b[len(b)-k:], sep[:k]) {
			return k
		}
	}
	return 0
}
