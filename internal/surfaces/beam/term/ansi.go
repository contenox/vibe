package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/input"
	xterm "golang.org/x/term"
)

const (
	eventBuffer      = 256                    // decoded events held for a busy app loop
	pendingFlushWait = 15 * time.Millisecond  // idle gap that resolves an ambiguous prefix
	idlePollWait     = 250 * time.Millisecond // backstop wakeup; the wake pipe does the real work
	pausePollWait    = 20 * time.Millisecond  // wakeup rate while a child owns the tty
	readerStopWait   = time.Second            // how long Close waits for the reader to let go of the tty
	parkWait         = 100 * time.Millisecond // how long Suspend waits for the reader to park
	// resizeSettleWait is the quiet period a reported size must survive before
	// the engine acts on it. A window drag reports every geometry it passes
	// through, and acting on each one costs an erase and a full repaint of a
	// region the terminal is about to reflow again — which is what deposits one
	// truncated copy of the composer hint and status bar per intermediate width.
	// Long enough to swallow a drag, short enough that letting go feels instant.
	resizeSettleWait = 120 * time.Millisecond
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
// app-shell loop goroutine. There is one internal exception: the resize
// watcher erases the live region the moment a gesture begins (see
// onResizeGesture), so every painter access is serialized under paintMu —
// whole frames at a time, which keeps escape sequences uninterleaved, the
// property the single-writer rule exists for. Restore stays lock-free, so a
// deferred panic handler on any goroutine can hand the terminal back.
type ANSI struct {
	in, out *os.File
	painter *painter
	parser  eventParser

	// paintMu serializes painter writes between the app-shell loop (Commit,
	// Suspend, Close) and the resize watcher's gesture-start erase.
	paintMu sync.Mutex

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
		events:      newEventSink(eventBuffer, resizeSettleWait),
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
	// Set before any goroutine exists, so the sink's hooks are visible to
	// every one of them without further synchronization.
	e.events.release = e.applySize
	e.events.gestureStart = e.onResizeGesture
	e.winch = notifyResize()
	e.events.sendResizeNow(input.ResizeEvent{Width: width, Height: height})
	go e.readLoop()
	go e.watchResize()
	return e, nil
}

// Events yields decoded input events; the channel closes on EOF or Close.
func (e *ANSI) Events() <-chan input.Event { return e.events.ch }

// Commit renders one frame into the terminal (see painter.commit); the
// engine's additions are picking up a size change the resize goroutine
// observed, and withholding the live region while a resize gesture is still
// settling. Mid-gesture the terminal's real geometry is not the one the frame
// was laid out for — painting the region would put rows on screen that the
// drag's next width rewraps, which is exactly the desync onResizeGesture just
// erased its way out of — so only scrollback (raw, width-independent) goes
// out, and the settle's ResizeEvent triggers the full repaint.
func (e *ANSI) Commit(f frame.Frame) error {
	e.paintMu.Lock()
	defer e.paintMu.Unlock()
	if e.events.resizePending() {
		f = frame.Frame{Scrollback: f.Scrollback, Cursor: frame.Cursor{Hidden: true}}
	}
	e.painter.resize(e.Size())
	return e.painter.commit(f)
}

// onResizeGesture runs the moment a debounced resize gesture begins — before
// the drag's later widths reflow the live region — and erases the region while
// the painter's row counts still describe the screen. This is the only painter
// write outside the app-shell loop (see paintMu). During Suspend the child
// owns the screen and there is no region to take down, so the hook declines.
func (e *ANSI) onResizeGesture() {
	if e.paused.Load() {
		return
	}
	select {
	case <-e.done:
		return
	default:
	}
	e.paintMu.Lock()
	defer e.paintMu.Unlock()
	e.painter.disown()
}

// Size reports the terminal size the engine has adopted — the last one whose
// ResizeEvent was released to the app, not the last one the tty reported. The
// two differ only while a drag is in flight, and that gap is the point: the app
// lays out for the size it was told, and Commit paints at the size the app laid
// out for, so no frame is ever built for one geometry and painted at another.
func (e *ANSI) Size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.width, e.height
}

// applySize adopts a resize at the instant the sink releases it to the app,
// which is what keeps Size, the app's layout and painter.resize describing one
// geometry. See eventSink.settle for why release lags the tty.
func (e *ANSI) applySize(ev input.ResizeEvent) {
	e.mu.Lock()
	e.width, e.height = ev.Width, ev.Height
	e.mu.Unlock()
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
	e.paintMu.Lock()
	_ = e.painter.clear()
	e.paintMu.Unlock()
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
		// clean line, never beside leftover chrome. The watcher is already
		// down, so paintMu is only honesty here, not a contested lock.
		e.paintMu.Lock()
		_ = e.painter.clear()
		e.paintMu.Unlock()
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
	e.paintMu.Lock()
	e.painter.reset()
	e.paintMu.Unlock()
	width, height := e.probeSize()
	// Not debounced: a child process just repainted the screen, so the app must
	// be told to redraw now rather than after a settle window it has no reason
	// to wait out.
	e.events.sendResizeNow(input.ResizeEvent{Width: width, Height: height})
	return err
}

// probeSize asks the tty for its current geometry without adopting it: the
// engine's size advances only through applySize, when a resize is released.
// A tty that will not report falls back to the adopted size, which reads as
// "nothing changed" and emits no event.
func (e *ANSI) probeSize() (int, int) {
	width, height, err := xterm.GetSize(int(e.out.Fd()))
	if err != nil {
		return e.Size()
	}
	return width, height
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

// watchResize turns size changes into ResizeEvents. It is signal-driven where
// the platform has a signal, and polled where it does not (Windows, see
// resizePollInterval) — a poll that finds the same size emits nothing, so the
// two paths deliver the same events and only their latency differs.
func (e *ANSI) watchResize() {
	defer close(e.watcherDone)

	var tick <-chan time.Time
	if every := resizePollInterval(); every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	// nil on a signal-driven platform: a receive on it blocks forever, which
	// is exactly "never poll".
	lastW, lastH := e.Size()

	for {
		select {
		case <-e.done:
			return
		case <-tick:
			width, height := e.probeSize()
			if width == lastW && height == lastH {
				continue // the common case while nobody is dragging
			}
			lastW, lastH = width, height
			e.events.sendResize(input.ResizeEvent{Width: width, Height: height})
		case <-e.winch:
			select { // a signal queued as the engine closed is not a resize
			case <-e.done:
				return
			default:
			}
			width, height := e.probeSize()
			lastW, lastH = width, height
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
// ever true and Commit re-reads it from the engine anyway). The slot is also
// where a drag is debounced into the single size it came to rest at — see
// settle, and painter.eraseRegion for what each size acted on costs.
type eventSink struct {
	mu       sync.Mutex
	ch       chan input.Event
	closing  chan struct{}
	closed   bool
	inFlight sync.WaitGroup

	// settle is how long a reported size must go unchanged before its event is
	// released. The coalescing slot alone only merges sizes that arrive faster
	// than the app drains the queue, which a drag never does — it reports a new
	// size every few frames and the app keeps up with every one. Debouncing is
	// what turns a whole drag into a single erase and repaint. Zero releases
	// immediately, which is what the tests and sendResizeNow rely on.
	settle time.Duration
	// release, if set, adopts a size at the instant it is handed to the app.
	// Assigned once before the sink is shared with any goroutine.
	release func(input.ResizeEvent)
	// gestureStart, if set, runs when a debounced resize gesture begins — the
	// first report while none is pending, never on sendResizeNow's immediate
	// path — so the engine can take the live region down while its row counts
	// still describe the screen (see ANSI.onResizeGesture). It fires once per
	// gesture: later reports of the same drag find the slot occupied.
	// Assigned once before the sink is shared with any goroutine.
	gestureStart func()

	resizeMu  sync.Mutex
	resize    input.ResizeEvent
	hasResize bool
	releaseAt time.Time   // earliest instant the pending resize may be released
	timer     *time.Timer // wakes flushResize at releaseAt; nil until first armed
}

func newEventSink(size int, settle time.Duration) *eventSink {
	return &eventSink{ch: make(chan input.Event, size), closing: make(chan struct{}), settle: settle}
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

// sendResize records a reported size without ever blocking, so it is safe from
// the app loop itself, and restarts the settle window: the size only reaches
// the app once it has stood still for settle. Each call supersedes the last,
// since only the newest size is ever true.
func (s *eventSink) sendResize(ev input.ResizeEvent) {
	s.pendResize(ev, s.settle)
}

// sendResizeNow bypasses the settle window for sizes that are not a drag — the
// engine's first size and the one after Suspend — where waiting would only
// delay the app's first or next frame.
func (s *eventSink) sendResizeNow(ev input.ResizeEvent) {
	s.pendResize(ev, 0)
}

func (s *eventSink) pendResize(ev input.ResizeEvent, wait time.Duration) {
	s.resizeMu.Lock()
	fresh := !s.hasResize
	s.resize, s.hasResize = ev, true
	s.releaseAt = time.Now().Add(wait)
	if wait > 0 {
		s.armLocked(wait)
	}
	gesture := s.gestureStart
	s.resizeMu.Unlock()
	// Outside the lock: the hook writes to the terminal, and nothing here
	// races it — the pending slot is already occupied, so a settle elapsing
	// concurrently only releases the size the hook is erasing ahead of.
	if fresh && wait > 0 && gesture != nil {
		gesture()
	}
	s.flushResize()
}

// resizePending reports whether a resize gesture is still in flight: a size
// has been reported that the app has not yet been handed. While true, the
// terminal's real geometry and the app's layout width cannot be assumed to
// agree, which is what ANSI.Commit's live-region withholding keys off.
func (s *eventSink) resizePending() bool {
	s.resizeMu.Lock()
	defer s.resizeMu.Unlock()
	return s.hasResize
}

// armLocked schedules the wakeup that releases a pending resize when nothing
// else does. It is the only thing that makes a drag's last size arrive at all:
// once the drag stops there are no further events to piggyback on. A closed
// sink is never armed, so no timer outlives Close.
func (s *eventSink) armLocked(d time.Duration) {
	select {
	case <-s.closing:
		return
	default:
	}
	if s.timer == nil {
		s.timer = time.AfterFunc(d, s.flushResize)
		return
	}
	s.timer.Reset(d)
}

// flushResize releases the pending resize if its settle window has elapsed,
// placing it ahead of whatever the caller is about to queue. A resize that has
// not settled is left pending with the wakeup rearmed; one that has settled but
// still does not fit in the queue stays pending and immediately releasable,
// unless a newer one has replaced it in the meantime.
func (s *eventSink) flushResize() {
	s.resizeMu.Lock()
	if !s.hasResize {
		s.resizeMu.Unlock()
		return
	}
	if wait := time.Until(s.releaseAt); wait > 0 {
		s.armLocked(wait)
		s.resizeMu.Unlock()
		return
	}
	ev := s.resize
	s.hasResize = false
	release := s.release
	s.resizeMu.Unlock()

	// Adopting the size before the event is queued is what lets the app read
	// Size() from its handler and get the geometry the event describes.
	if release != nil {
		release(ev)
	}
	if s.trySend(ev) {
		return
	}
	s.resizeMu.Lock()
	if !s.hasResize {
		s.resize, s.hasResize = ev, true
		s.releaseAt = time.Time{} // already settled: the next attempt delivers
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
	// After closing, armLocked refuses to rearm, so stopping once is final; a
	// callback already running only finds the sink closed and gives up.
	s.resizeMu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.hasResize = false
	s.resizeMu.Unlock()
	s.inFlight.Wait()
	close(s.ch)
}
