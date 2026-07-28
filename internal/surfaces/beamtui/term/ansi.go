package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/input"
	xterm "golang.org/x/term"
)

const (
	eventBuffer      = 256                    // decoded events held for a busy app loop
	pendingFlushWait = 15 * time.Millisecond  // idle gap that resolves an ambiguous prefix
	idlePollWait     = 250 * time.Millisecond // backstop wakeup; the wake pipe does the real work
	pausePollWait    = 20 * time.Millisecond  // wakeup rate while a child owns the tty
	readerStopWait   = time.Second            // how long Close waits for the reader to let go of the tty
	parkWait         = 100 * time.Millisecond // how long Suspend waits for the reader to park
)

// eventParser is the decoding half of the engine, owned by the input
// package. Keeping it as an interface is what lets the renderer's tests run
// with no terminal and no decoding at all.
type eventParser interface {
	// Feed decodes a chunk of raw bytes, carrying incomplete sequences over
	// to the next call.
	Feed(b []byte) []input.Event
	// Pending reports an ambiguous buffered prefix (a lone ESC) that only an
	// idle gap can resolve.
	Pending() bool
	// Flush resolves that prefix, emitting what it turned out to be.
	Flush() []input.Event
}

// ANSI is beam's production Engine: an inline renderer over a raw-mode tty.
// It never enables the alternate screen or a mouse mode, since the
// terminal's own selection is beam's copy/paste story; scrollback prints
// raw and lets the terminal soft-wrap it (painter.renderRaw).
//
// Every method except Events and Restore must be called from the single
// app-shell loop goroutine: one writer means interleaved escape sequences
// cannot corrupt a frame. Restore is the exception, so a deferred panic
// handler on any goroutine can hand the terminal back.
type ANSI struct {
	in, out *os.File
	painter *painter
	parser  eventParser

	events    *eventSink
	done      chan struct{}
	closeOnce sync.Once
	paused    atomic.Bool

	// The reader sits in a poll over the tty and wakeR; writing one byte to
	// wakeW returns it immediately, so pausing and closing never wait out a
	// timeout and never race the caller's use of the tty.
	wakeR, wakeW *os.File
	parked       chan struct{}
	readerDone   chan struct{}
	watcherDone  chan struct{}

	mu     sync.Mutex // guards the fields below against the resize goroutine
	width  int
	height int
	raw    bool
	state  *xterm.State

	winch chan os.Signal
}

var _ Engine = (*ANSI)(nil)

// New puts in into raw mode, enables bracketed paste and focus reporting on
// out, and starts the input reader and SIGWINCH watcher. The initial
// ResizeEvent is queued before New returns, so the app loop's first select
// always knows the size. The caller must Close (or at least Restore) the
// engine, including from a panic handler.
func New(in, out *os.File, styles StyleResolver) (*ANSI, error) {
	if in == nil || out == nil {
		return nil, errors.New("beam term: nil terminal handle")
	}
	width, height, err := xterm.GetSize(int(out.Fd()))
	if err != nil {
		width, height, err = xterm.GetSize(int(in.Fd()))
	}
	if err != nil {
		return nil, fmt.Errorf("beam term: terminal size: %w", err)
	}
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("beam term: wake pipe: %w", err)
	}
	e := &ANSI{
		in:          in,
		out:         out,
		parser:      newParser(),
		events:      newEventSink(eventBuffer),
		done:        make(chan struct{}),
		wakeR:       wakeR,
		wakeW:       wakeW,
		parked:      make(chan struct{}, 1),
		readerDone:  make(chan struct{}),
		watcherDone: make(chan struct{}),
		width:       width,
		height:      height,
		painter: &painter{
			out:    out,
			styles: styles,
			width:  width,
			height: height,
		},
	}
	e.mu.Lock()
	err = e.enterRawLocked()
	e.mu.Unlock()
	if err != nil {
		wakeR.Close()
		wakeW.Close()
		return nil, err
	}
	e.winch = notifyResize()
	e.events.sendResize(input.ResizeEvent{Width: width, Height: height})
	go e.readLoop()
	go e.watchResize()
	return e, nil
}

// Events yields decoded input events; the channel closes on EOF or Close.
func (e *ANSI) Events() <-chan input.Event { return e.events.ch }

// Commit renders one frame into the terminal (see painter.commit); the
// engine's only addition is picking up a size change the resize goroutine
// observed.
func (e *ANSI) Commit(f frame.Frame) error {
	e.painter.resize(e.Size())
	return e.painter.commit(f)
}

// Size reports the last observed terminal size in cells.
func (e *ANSI) Size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.width, e.height
}

// Bell emits BEL. Errors are ignored: a terminal that cannot take a bell
// cannot take a report about it either.
func (e *ANSI) Bell() { _, _ = io.WriteString(e.out, seqBell) }

// Suspend hands the terminal to fn — cooked mode, cursor visible, beam's
// modes off, input reader parked so fn gets every keystroke — then takes it
// back, emits a fresh ResizeEvent, and forces the next Commit to repaint.
// Re-entry also happens when fn panics; the panic then continues to
// unwind past a terminal that is already usable.
func (e *ANSI) Suspend(fn func() error) (err error) {
	if fn == nil {
		return nil
	}
	e.park()
	// Erase the live region before handover: resume cannot know what the
	// child drew, so anything left on screen would become permanent,
	// duplicated scrollback.
	_ = e.painter.clear()
	if rerr := e.Restore(); rerr != nil {
		e.paused.Store(false)
		return fmt.Errorf("beam term: suspend: %w", rerr)
	}
	defer func() {
		e.paused.Store(false)
		if rerr := e.resume(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	return fn()
}

// Close restores the terminal and shuts the engine down. Idempotent and
// safe from a deferred panic handler.
func (e *ANSI) Close() error {
	var err error
	e.closeOnce.Do(func() {
		close(e.done)
		stopResize(e.winch)
		e.wake()
		// No engine goroutine may touch the tty after Close returns.
		select {
		case <-e.readerDone:
		case <-time.After(readerStopWait):
		}
		select {
		case <-e.watcherDone:
		case <-time.After(readerStopWait):
		}
		// Erase the live region so the shell's next prompt lands on a
		// clean line, never beside leftover chrome.
		_ = e.painter.clear()
		err = e.Restore()
		e.events.close()
		e.wakeR.Close()
		e.wakeW.Close()
	})
	return err
}

// park pauses the input reader and waits for it to confirm, so a child
// process started by Suspend gets every keystroke typed at it.
func (e *ANSI) park() {
	// Drain a stale token an earlier park left behind, or this park would
	// return before the reader actually let go of the tty.
	select {
	case <-e.parked:
	default:
	}
	e.paused.Store(true)
	e.wake()
	select {
	case <-e.parked:
	case <-e.readerDone:
	case <-time.After(parkWait):
	}
}

// wake interrupts the reader's poll.
func (e *ANSI) wake() {
	if e.wakeW != nil {
		_, _ = e.wakeW.Write([]byte{0})
	}
}

// Restore returns the terminal to the state beam found it in — cooked mode,
// cursor visible, synchronized output ended, bracketed paste and focus
// reporting off — without shutting the engine down. Idempotent, callable
// from any goroutine, and deliberately the smallest possible path so a
// panic handler can make it its first act.
func (e *ANSI) Restore() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.restoreLocked()
}

func (e *ANSI) restoreLocked() error {
	if !e.raw {
		return nil
	}
	_, werr := io.WriteString(e.out, seqSyncEnd+seqCursorShow+seqPasteOff+seqFocusOff)
	if rerr := xterm.Restore(int(e.in.Fd()), e.state); rerr != nil {
		// The terminal is still raw; keep raw/state set so a later Restore
		// or Close can try again.
		return fmt.Errorf("beam term: restore: %w", rerr)
	}
	e.raw = false
	e.state = nil
	if werr != nil {
		return fmt.Errorf("beam term: restore: %w", werr)
	}
	return nil
}

func (e *ANSI) enterRawLocked() error {
	if e.raw {
		return nil
	}
	state, err := xterm.MakeRaw(int(e.in.Fd()))
	if err != nil {
		return fmt.Errorf("beam term: raw mode: %w", err)
	}
	if _, werr := io.WriteString(e.out, seqPasteOn+seqFocusOn); werr != nil {
		// Raw mode took, the mode-enable write did not. New's caller has no
		// engine to Close on error, so give the termios back before returning.
		if rerr := xterm.Restore(int(e.in.Fd()), state); rerr != nil {
			return fmt.Errorf("beam term: enable terminal modes: %w (restoring raw mode also failed: %v)", werr, rerr)
		}
		return fmt.Errorf("beam term: enable terminal modes: %w", werr)
	}
	e.state = state
	e.raw = true
	return nil
}

// resume runs on the app-shell loop, the event channel's only reader, so
// nothing here may block on it; sendResize is the non-blocking path.
func (e *ANSI) resume() error {
	e.mu.Lock()
	err := e.enterRawLocked()
	e.mu.Unlock()
	e.painter.reset()
	width, height := e.refreshSize()
	e.events.sendResize(input.ResizeEvent{Width: width, Height: height})
	return err
}

func (e *ANSI) refreshSize() (int, int) {
	width, height, err := xterm.GetSize(int(e.out.Fd()))
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.width, e.height = width, height
	}
	return e.width, e.height
}

// readLoop polls the tty rather than blocking in Read, so a paused engine
// consumes nothing while a child owns the terminal, an ambiguous prefix
// flushes on an idle gap, and Close never waits on a keystroke that may
// never come.
func (e *ANSI) readLoop() {
	defer close(e.readerDone)
	defer e.events.close()
	buf := make([]byte, 4096)
	wake := make([]byte, 64)
	parked := false
	for {
		select {
		case <-e.done:
			return
		default:
		}
		if e.paused.Load() {
			if !parked {
				parked = true
				select {
				case e.parked <- struct{}{}:
				default:
				}
			}
			select {
			case <-e.done:
				return
			case <-time.After(pausePollWait):
			}
			continue
		}
		parked = false
		wait := idlePollWait
		if e.parser.Pending() {
			wait = pendingFlushWait
		}
		ready, woken, err := waitReadable(e.in.Fd(), e.wakeR.Fd(), wait)
		if err != nil {
			return
		}
		if woken {
			_, _ = e.wakeR.Read(wake)
			continue
		}
		if !ready {
			if e.parser.Pending() {
				e.emit(e.parser.Flush())
			}
			// Bounds how long a resize can sit in the coalescing slot.
			e.events.flushResize()
			continue
		}
		if e.paused.Load() {
			continue
		}
		n, rerr := e.in.Read(buf)
		if n > 0 {
			e.emit(e.parser.Feed(buf[:n]))
		}
		if rerr != nil {
			return
		}
	}
}

func (e *ANSI) watchResize() {
	defer close(e.watcherDone)
	for {
		select {
		case <-e.done:
			return
		case <-e.winch:
			select { // a signal queued as the engine closed is not a resize
			case <-e.done:
				return
			default:
			}
			width, height := e.refreshSize()
			e.events.sendResize(input.ResizeEvent{Width: width, Height: height})
		}
	}
}

func (e *ANSI) emit(events []input.Event) {
	for _, ev := range events {
		e.events.send(ev, e.done)
	}
}

// eventSink is the engine's outbound queue. It makes two things structurally
// impossible: a send outliving the channel (producers register under a
// mutex and release it before blocking, so close() can signal them to
// abandon and wait them out before closing the channel with none in
// flight), and a send blocking the consumer (resume runs on the event
// channel's only reader, so a blocking send from it would deadlock the
// process with the terminal raw and ISIG off; resizes instead go through a
// one-deep coalescing slot that never blocks, since only the latest size is
// ever true and Commit re-reads it from the engine anyway).
type eventSink struct {
	mu       sync.Mutex
	ch       chan input.Event
	closing  chan struct{}
	closed   bool
	inFlight sync.WaitGroup

	resizeMu  sync.Mutex
	resize    input.ResizeEvent
	hasResize bool
}

func newEventSink(size int) *eventSink {
	return &eventSink{ch: make(chan input.Event, size), closing: make(chan struct{})}
}

// enter registers a producer, reporting whether the sink is still open. Every
// enter that returns true must be matched by a leave.
func (s *eventSink) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.inFlight.Add(1)
	return true
}

func (s *eventSink) leave() { s.inFlight.Done() }

// send queues an event, blocking until the app loop makes room, the engine
// shuts down, or the sink closes. Only the engine's own goroutines may call
// it — never the app loop.
func (s *eventSink) send(ev input.Event, done <-chan struct{}) {
	s.flushResize()
	if !s.enter() {
		return
	}
	defer s.leave()
	select {
	case s.ch <- ev:
	case <-s.closing:
	case <-done:
	}
}

// sendResize delivers a resize without ever blocking, so it is safe from the
// app loop itself. See the type comment for why the coalescing slot is enough.
func (s *eventSink) sendResize(ev input.ResizeEvent) {
	s.resizeMu.Lock()
	s.resize, s.hasResize = ev, true
	s.resizeMu.Unlock()
	s.flushResize()
}

// flushResize tries to place the pending resize ahead of whatever the caller
// is about to queue. A resize that still does not fit stays pending unless a
// newer one has replaced it in the meantime.
func (s *eventSink) flushResize() {
	s.resizeMu.Lock()
	ev, ok := s.resize, s.hasResize
	s.hasResize = false
	s.resizeMu.Unlock()
	if !ok || s.trySend(ev) {
		return
	}
	s.resizeMu.Lock()
	if !s.hasResize {
		s.resize, s.hasResize = ev, true
	}
	s.resizeMu.Unlock()
}

// trySend delivers without blocking, reporting whether the event is now the
// queue's problem. A closed sink counts as delivered: there is no one left to
// tell.
func (s *eventSink) trySend(ev input.Event) bool {
	if !s.enter() {
		return true
	}
	defer s.leave()
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

func (s *eventSink) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.closing)
	s.mu.Unlock()
	s.inFlight.Wait()
	close(s.ch)
}
