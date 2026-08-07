package term

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
	"github.com/creack/pty"
	xterm "golang.org/x/term"
)

// tty spins up a real pty pair so raw-mode and reader glue can be exercised
// without inheriting a terminal from the test runner.
func newTTY(t *testing.T) (master *os.File, engine *ANSI, output func() string) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("cannot size pty: %v", err)
	}

	var mu sync.Mutex
	var seen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				seen.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	e, err := New(slave, slave, plainStyles{})
	if err != nil {
		master.Close()
		slave.Close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		e.Close()
		master.Close()
		slave.Close()
	})
	return master, e, func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen.String()
	}
}

func nextEvent(t *testing.T, ch <-chan input.Event) input.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

func waitForOutput(t *testing.T, output func() string, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := output(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in terminal output %q", want, output())
	return ""
}

func TestUnit_EngineStartsRawWithoutAltScreenOrMouse(t *testing.T) {
	_, e, output := newTTY(t)

	if ev := nextEvent(t, e.Events()); ev != (input.ResizeEvent{Width: 80, Height: 24}) {
		t.Fatalf("first event = %#v, want the startup resize", ev)
	}
	if w, h := e.Size(); w != 80 || h != 24 {
		t.Fatalf("Size() = %dx%d, want 80x24", w, h)
	}
	got := waitForOutput(t, output, seqFocusOn)
	if !strings.Contains(got, seqPasteOn) {
		t.Fatalf("bracketed paste not enabled: %q", got)
	}
	for _, banned := range []string{"\x1b[?1049h", "\x1b[?47h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h"} {
		if strings.Contains(got, banned) {
			t.Fatalf("engine enabled a forbidden mode %q", banned)
		}
	}
}

func TestUnit_EngineCommitsAndDecodesInput(t *testing.T) {
	master, e, output := newTTY(t)
	nextEvent(t, e.Events()) // startup resize

	if err := e.Commit(frame.Frame{
		Scrollback: []frame.Line{frame.Plain("history line")},
		Live:       []frame.Line{frame.Plain("> ")},
		Cursor:     frame.Cursor{Row: 0, Col: 2},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got := waitForOutput(t, output, "history line")
	if !strings.Contains(got, seqSyncBegin) || !strings.Contains(got, seqSyncEnd) {
		t.Fatalf("commit was not synchronized: %q", got)
	}

	if _, err := master.WriteString("hi"); err != nil {
		t.Fatalf("write to terminal: %v", err)
	}
	for _, want := range []rune{'h', 'i'} {
		ev := nextEvent(t, e.Events())
		key, ok := ev.(input.KeyEvent)
		if !ok || key.Rune != want {
			t.Fatalf("event = %#v, want the rune %q", ev, want)
		}
	}

	e.Bell()
	waitForOutput(t, output, seqBell)
}

func TestUnit_EngineSuspendRestoresAndResumes(t *testing.T) {
	_, e, output := newTTY(t)
	nextEvent(t, e.Events()) // startup resize

	ran := false
	if err := e.Suspend(func() error {
		ran = true
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.raw {
			t.Error("terminal still raw inside Suspend")
		}
		return nil
	}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if !ran {
		t.Fatal("Suspend did not run fn")
	}
	if ev := nextEvent(t, e.Events()); ev != (input.ResizeEvent{Width: 80, Height: 24}) {
		t.Fatalf("resume event = %#v, want a fresh resize", ev)
	}
	e.mu.Lock()
	raw := e.raw
	e.mu.Unlock()
	if !raw {
		t.Fatal("Suspend did not re-enter raw mode")
	}
	if !e.painter.invalid {
		t.Fatal("Suspend did not invalidate the diff cache")
	}
	got := waitForOutput(t, output, seqPasteOff)
	if !strings.Contains(got, seqCursorShow) || !strings.Contains(got, seqFocusOff) {
		t.Fatalf("Suspend did not hand the terminal back cleanly: %q", got)
	}
}

func TestUnit_EngineSuspendReentersRawAfterPanic(t *testing.T) {
	_, e, _ := newTTY(t)
	nextEvent(t, e.Events()) // startup resize

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic from fn was swallowed")
			}
		}()
		_ = e.Suspend(func() error { panic("editor exploded") })
	}()

	e.mu.Lock()
	raw := e.raw
	e.mu.Unlock()
	if !raw {
		t.Fatal("terminal left cooked after a panicking Suspend")
	}
	nextEvent(t, e.Events()) // resume resize proves the resume path ran
}

func TestUnit_EngineSuspendPropagatesError(t *testing.T) {
	_, e, _ := newTTY(t)
	nextEvent(t, e.Events())

	sentinel := errors.New("wizard cancelled")
	if err := e.Suspend(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Suspend error = %v, want %v", err, sentinel)
	}
}

func TestUnit_EngineCloseIsIdempotentAndClosesEvents(t *testing.T) {
	_, e, output := newTTY(t)
	nextEvent(t, e.Events())

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := e.Restore(); err != nil {
		t.Fatalf("Restore after Close: %v", err)
	}
	got := waitForOutput(t, output, seqFocusOff)
	if !strings.Contains(got, seqCursorShow) || !strings.Contains(got, seqSyncEnd) {
		t.Fatalf("Close did not restore the terminal: %q", got)
	}
	select {
	case _, ok := <-e.Events():
		if ok {
			t.Fatal("event channel still delivering after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event channel did not close")
	}
}

func TestUnit_EngineResizeSignalUpdatesSize(t *testing.T) {
	master, e, _ := newTTY(t)
	nextEvent(t, e.Events())

	// SIGWINCH is delivered to the pty's foreground process group, which the
	// test binary is not part of; the watcher is driven directly instead.
	if err := pty.Setsize(master, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	e.winch <- os.Interrupt

	if ev := nextEvent(t, e.Events()); ev != (input.ResizeEvent{Width: 100, Height: 30}) {
		t.Fatalf("resize event = %#v, want 100x30", ev)
	}
	if w, h := e.Size(); w != 100 || h != 30 {
		t.Fatalf("Size() = %dx%d, want 100x30", w, h)
	}
	if err := e.Commit(frame.Frame{Live: []frame.Line{frame.Plain("x")}}); err != nil {
		t.Fatalf("Commit after resize: %v", err)
	}
	if e.painter.width != 100 || e.painter.height != 30 {
		t.Fatalf("painter size = %dx%d, want 100x30", e.painter.width, e.painter.height)
	}
}

func TestUnit_EngineFlushesAnAmbiguousPrefixWhenInputGoesIdle(t *testing.T) {
	master, e, _ := newTTY(t)
	nextEvent(t, e.Events()) // startup resize

	if _, err := master.WriteString("\x1b"); err != nil {
		t.Fatalf("write to terminal: %v", err)
	}
	ev := nextEvent(t, e.Events())
	if key, ok := ev.(input.KeyEvent); !ok || key.Key != input.KeyEscape {
		t.Fatalf("event = %#v, want the idle flush to resolve a lone ESC", ev)
	}
}

// TestUnit_EngineNewRestoresTheTerminalWhenModeSetupFails pins that New
// never returns an error alongside a raw terminal, since the caller has no
// engine to Close in that case.
func TestUnit_EngineNewRestoresTheTerminalWhenModeSetupFails(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	// A pipe with its reader closed: MakeRaw on the tty succeeds, the
	// mode-enable write to "the terminal" does not.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	r.Close()
	defer w.Close()

	before, err := xterm.GetState(int(slave.Fd()))
	if err != nil {
		t.Skipf("cannot read terminal state: %v", err)
	}
	e, err := New(slave, w, plainStyles{})
	if err == nil {
		e.Close()
		t.Fatal("New succeeded with an unwritable terminal")
	}
	// The failure must be the write after MakeRaw, or this proves nothing
	// about undoing MakeRaw.
	if !strings.Contains(err.Error(), "enable terminal modes") {
		t.Fatalf("New failed at the wrong step: %v", err)
	}
	after, err := xterm.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatalf("read terminal state: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("New failed after MakeRaw and left the terminal in raw mode")
	}
}

// TestUnit_EngineInputEOFClosesEventsAndCloseIsHonest pins that EOF closes
// the event channel without ending the app, and that Close reports an
// unclean restore rather than silently claiming success when the pty is gone.
func TestUnit_EngineInputEOFClosesEventsAndCloseIsHonest(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer slave.Close()
	e, err := New(slave, slave, plainStyles{})
	if err != nil {
		master.Close()
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	nextEvent(t, e.Events()) // startup resize

	master.Close() // the far end of the tty goes away

	deadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-e.Events():
			open = ok
		case <-deadline:
			t.Fatal("event channel did not close on input EOF")
		}
	}

	if err := e.Close(); err == nil {
		t.Fatal("Close reported success restoring a terminal that is gone")
	}
	e.mu.Lock()
	raw, state := e.raw, e.state
	e.mu.Unlock()
	if !raw || state == nil {
		t.Fatal("a failed restore threw away the state a later attempt needs")
	}
	if err := e.Close(); err != nil { // idempotent: the work already ran
		t.Fatalf("second Close: %v", err)
	}
}

// TestUnit_EventSinkResizeNeverBlocks pins that a resize send never blocks,
// since resume() runs on the event channel's only reader.
func TestUnit_EventSinkResizeNeverBlocks(t *testing.T) {
	s := newEventSink(1, 0)
	done := make(chan struct{})
	tab := input.KeyEvent{Key: input.KeyTab}
	s.send(tab, done) // the queue is now full

	fired := make(chan struct{})
	go func() {
		defer close(fired)
		s.sendResize(input.ResizeEvent{Width: 80, Height: 24})
		s.sendResize(input.ResizeEvent{Width: 100, Height: 30}) // coalesces
	}()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("sendResize blocked on a full queue")
	}

	if ev := <-s.ch; ev != input.Event(tab) {
		t.Fatalf("first queued event = %#v, want the key that was already there", ev)
	}
	enter := input.KeyEvent{Key: input.KeyEnter}
	go s.send(enter, done)
	if ev := <-s.ch; ev != input.Event(input.ResizeEvent{Width: 100, Height: 30}) {
		t.Fatalf("event = %#v, want the latest resize ahead of what is queued next", ev)
	}
	if ev := <-s.ch; ev != input.Event(enter) {
		t.Fatalf("event = %#v, want the key queued behind the resize", ev)
	}
}

// TestUnit_EventSinkClosesUnderABlockedProducer pins that close() never
// panics closing the channel under a blocked sender.
func TestUnit_EventSinkClosesUnderABlockedProducer(t *testing.T) {
	s := newEventSink(1, 0)
	done := make(chan struct{})
	s.send(input.KeyEvent{Key: input.KeyTab}, done) // full

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		s.send(input.KeyEvent{Key: input.KeyEnter}, done)
	}()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.close()
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close wedged behind a producer blocked on a full queue")
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked producer was never released")
	}
	for range s.ch { // drains and proves the channel is closed
	}
	s.close() // idempotent
	s.send(input.KeyEvent{}, done)
	s.sendResize(input.ResizeEvent{})
}

// TestUnit_EventSinkDebouncesADrag pins the coalescing the renderer depends on:
// a window drag reports a size every few frames, and acting on each one erases
// and repaints a region the terminal is about to reflow again, leaving one
// truncated copy of the chrome per intermediate width. Only the size the drag
// came to rest at is released, and it is released without anything else having
// to happen.
func TestUnit_EventSinkDebouncesADrag(t *testing.T) {
	const settle = 80 * time.Millisecond
	s := newEventSink(8, settle)
	var mu sync.Mutex
	var adopted []input.ResizeEvent
	s.release = func(ev input.ResizeEvent) {
		mu.Lock()
		adopted = append(adopted, ev)
		mu.Unlock()
	}

	for _, w := range []int{100, 88, 71, 54, 40} {
		s.sendResize(input.ResizeEvent{Width: w, Height: 24})
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(s.ch); n != 0 {
		t.Fatalf("%d resizes reached the app mid-drag, want none until it settles", n)
	}

	var got input.Event
	select {
	case got = <-s.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the settled size never arrived; a drag that stops emits no further events to carry it")
	}
	want := input.ResizeEvent{Width: 40, Height: 24}
	if got != input.Event(want) {
		t.Fatalf("released %#v, want the size the drag came to rest at %#v", got, want)
	}
	select {
	case extra := <-s.ch:
		t.Fatalf("a drag released a second event %#v, want exactly one", extra)
	case <-time.After(3 * settle):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(adopted) != 1 || adopted[0] != want {
		t.Fatalf("engine adopted %#v, want only the released size %#v", adopted, want)
	}
}

// TestUnit_EventSinkImmediateResizeSkipsTheSettleWindow pins that the sizes
// which are not a drag — the engine's first, and the one after Suspend — are
// not made to wait out a window they have no reason to.
func TestUnit_EventSinkImmediateResizeSkipsTheSettleWindow(t *testing.T) {
	s := newEventSink(8, time.Hour)
	s.sendResizeNow(input.ResizeEvent{Width: 80, Height: 24})
	select {
	case ev := <-s.ch:
		if ev != input.Event(input.ResizeEvent{Width: 80, Height: 24}) {
			t.Fatalf("event = %#v, want the size sent immediately", ev)
		}
	default:
		t.Fatal("an immediate resize was held behind the settle window")
	}
}

// TestUnit_EventSinkCloseDropsAPendingResize pins that a resize still inside
// its settle window cannot keep a timer, a goroutine or the channel alive past
// Close.
func TestUnit_EventSinkCloseDropsAPendingResize(t *testing.T) {
	s := newEventSink(8, time.Hour)
	s.sendResize(input.ResizeEvent{Width: 80, Height: 24})

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.close()
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close wedged on a resize waiting out its settle window")
	}
	for ev := range s.ch {
		t.Fatalf("a resize that never settled was delivered after close: %#v", ev)
	}
	s.resizeMu.Lock()
	timer := s.timer
	s.resizeMu.Unlock()
	if timer != nil {
		t.Fatal("close left the settle timer armed")
	}
	s.sendResize(input.ResizeEvent{Width: 10, Height: 10}) // idempotent, rearms nothing
	s.resizeMu.Lock()
	timer = s.timer
	s.resizeMu.Unlock()
	if timer != nil {
		t.Fatal("a resize after close armed a timer on a dead sink")
	}
}

func TestUnit_EngineRejectsNonTerminals(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if _, err := New(f, f, plainStyles{}); err == nil {
		t.Fatal("New on a non-terminal succeeded, want an error")
	}
	if _, err := New(nil, nil, plainStyles{}); err == nil {
		t.Fatal("New with nil handles succeeded, want an error")
	}
}
