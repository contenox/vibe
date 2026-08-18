// Package input defines beam's decoded terminal input events and the pure
// parser that produces them from raw bytes. Nothing here performs I/O: the
// term engine feeds bytes in and forwards the returned events, so decoding
// is fully testable against byte fixtures without a terminal.
package input

// Event is one decoded terminal input occurrence.
type Event interface{ event() }

// Key names the non-rune keys beam distinguishes. Printable input arrives
// as KeyRune with Rune set.
type Key int

const (
	KeyRune Key = iota
	KeyEnter
	KeyTab
	KeyBackspace
	KeyDelete
	KeyEscape
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
)

// KeyEvent is one keystroke. Ctrl+letter arrives with Ctrl true and Rune
// set to the lowercase letter (Ctrl+J is how terminals deliver the newline
// chord; Enter itself is KeyEnter). Shift is set only when the terminal
// reports it unambiguously (modifier-encoded sequences); plain shifted
// runes arrive as their shifted rune with Shift false.
type KeyEvent struct {
	Key   Key
	Rune  rune
	Alt   bool
	Ctrl  bool
	Shift bool
}

func (KeyEvent) event() {}

// PasteEvent is one bracketed paste delivered as a single literal block —
// never reinterpreted as keystrokes, never split.
type PasteEvent struct{ Text string }

func (PasteEvent) event() {}

// ResizeEvent reports the terminal size in cells. The engine emits one at
// startup and after every size change or suspend/resume.
type ResizeEvent struct{ Width, Height int }

func (ResizeEvent) event() {}

// FocusEvent reports terminal focus (DEC mode 1004); completion
// notification suppression depends on it.
type FocusEvent struct{ Focused bool }

func (FocusEvent) event() {}
