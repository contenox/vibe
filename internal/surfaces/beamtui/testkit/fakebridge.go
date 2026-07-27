// This file is FakeBridge: a scripted double for *enginebridge.Bridge, so a
// component's tests drive engine-bridge's event contract (fixtures.go) and
// assert on the calls a surface made without a real acpsvc.Transport, a real
// ACP loopback, or a database anywhere in the picture.
package testkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	libacp "github.com/contenox/beam/libacp"
)

// EngineBridge is the subset of *enginebridge.Bridge's API a beam surface
// (app-shell and everything under it) actually drives: the event outlet,
// prompt submission and cancellation, shell passthrough, and the active-
// session/session-lifecycle calls. It exists so a surface can depend on an
// interface instead of the concrete Bridge, and so this package can satisfy
// it with FakeBridge in tests.
//
// The shape is deliberately a mirror of bridge.go, not an invention: every
// method here has a same-named, same-signature method on *enginebridge.Bridge
// (see the compile-time assertion below), and there is nothing extra —
// Transport() stays off this interface because it exists for exactly one
// caller (the in-process mission fleet), which holds a concrete *Bridge, not
// this interface.
type EngineBridge interface {
	// Events returns the single ordered outlet. See enginebridge.Bridge.Events.
	Events() <-chan enginebridge.Event

	// Initialize performs the ACP handshake.
	Initialize(ctx context.Context) (libacp.InitializeResponse, error)

	// Session lifecycle: create, replay, re-attach, list, close, delete.
	NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error)
	LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error)
	ResumeSession(ctx context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error)
	ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error)
	CloseSession(ctx context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error)
	DeleteSession(ctx context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error)

	// SetActiveSession points the session/update filter; ActiveSession reads
	// it back. See enginebridge.Bridge.SetActiveSession.
	SetActiveSession(id libacp.SessionID)
	ActiveSession() libacp.SessionID

	// SubmitPrompt sends text as a turn and returns immediately; Cancel
	// interrupts the session's in-flight turn; RunShellLine runs one
	// operator line against the session's warm shell without a turn.
	SubmitPrompt(sessionID libacp.SessionID, text string) error
	Cancel(sessionID libacp.SessionID) error
	RunShellLine(sessionID libacp.SessionID, line string) error

	// Close tears the bridge down.
	Close() error
}

// fakeBridgeChanBuffer bounds FakeBridge's internal event channel. It is
// sized generously past anything a single fixture (or a small hand-rolled
// test script) would ever queue, so Play can send inline without a pump
// goroutine: a test that scripts more events than this in one Play call will
// block on the send, which is the trade FakeBridge makes for "no goroutine
// leaks, ever" over "arbitrarily large scripts."
const fakeBridgeChanBuffer = 4096

// FakeBridge is a scripted, call-recording double for *enginebridge.Bridge.
// It is safe for concurrent use. The zero value is not usable — build one
// with NewFakeBridge.
type FakeBridge struct {
	mu     sync.Mutex
	queued []enginebridge.Event
	events chan enginebridge.Event
	calls  []string
	active libacp.SessionID
	closed bool
}

var (
	_ EngineBridge = (*FakeBridge)(nil)
	_ EngineBridge = (*enginebridge.Bridge)(nil)
)

// NewFakeBridge returns a FakeBridge with nothing scripted and an empty call
// log.
func NewFakeBridge() *FakeBridge {
	return &FakeBridge{events: make(chan enginebridge.Event, fakeBridgeChanBuffer)}
}

// Script queues events to be delivered by the next Play call(s), in the
// order given. Script itself does not touch the channel — nothing is
// observable on Events() until Play runs.
func (f *FakeBridge) Script(events ...enginebridge.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, events...)
}

// Play delivers every event queued by Script so far onto Events(), in order,
// and clears the queue. It sends inline (no goroutine): the channel's
// buffer (fakeBridgeChanBuffer) absorbs the send, so a test may call Play
// and then range over Events() afterward — the two are not required to run
// concurrently, which is what "synchronously drainable" means here.
//
// Play on a closed FakeBridge delivers nothing instead of panicking, and a
// Close racing a Play cannot make it panic either: the lock is held for the
// whole delivery, so Close either wins outright (Play drops the remaining
// events) or waits (Play finishes first). A double is not worth a test
// failure that reads as a data race in the code under test.
//
// The lock being held across the sends is why the channel buffer is sized
// past any plausible script: a Play that blocked on a full channel would
// block Close with it. Script more than fakeBridgeChanBuffer events in one
// Play and that is the deadlock you get.
func (f *FakeBridge) Play() {
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := f.queued
	f.queued = nil
	for _, e := range pending {
		if f.closed {
			return
		}
		f.events <- e
	}
}

// Close closes the event channel. It is idempotent.
func (f *FakeBridge) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.events)
	return nil
}

// Calls returns every recorded method call, formatted with its arguments, in
// call order. The returned slice is a copy; mutating it does not affect the
// log.
func (f *FakeBridge) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// record appends one formatted call to the log. Callers hold no lock when
// calling it; record takes its own.
func (f *FakeBridge) record(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
}

func (f *FakeBridge) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Events returns the channel Play delivers scripted events on.
func (f *FakeBridge) Events() <-chan enginebridge.Event { return f.events }

// Initialize records the call and reports success — FakeBridge has no
// handshake to perform.
func (f *FakeBridge) Initialize(_ context.Context) (libacp.InitializeResponse, error) {
	f.record("Initialize()")
	return libacp.InitializeResponse{}, nil
}

// NewSession records the call and returns a zero-value response. Component
// tests that need a specific session id in play should Script events
// carrying that id directly rather than rely on this return value.
func (f *FakeBridge) NewSession(_ context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	f.record("NewSession(cwd=%q)", req.Cwd)
	if f.isClosed() {
		return libacp.NewSessionResponse{}, enginebridge.ErrClosed
	}
	return libacp.NewSessionResponse{}, nil
}

// LoadSession records the call and returns a zero-value response.
func (f *FakeBridge) LoadSession(_ context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	f.record("LoadSession(%s)", req.SessionID)
	if f.isClosed() {
		return libacp.LoadSessionResponse{}, enginebridge.ErrClosed
	}
	return libacp.LoadSessionResponse{}, nil
}

// ResumeSession records the call and returns a zero-value response.
func (f *FakeBridge) ResumeSession(_ context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
	f.record("ResumeSession(%s)", req.SessionID)
	if f.isClosed() {
		return libacp.ResumeSessionResponse{}, enginebridge.ErrClosed
	}
	return libacp.ResumeSessionResponse{}, nil
}

// ListSessions records the call and returns an empty roster.
func (f *FakeBridge) ListSessions(_ context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	f.record("ListSessions(cwd=%q)", req.Cwd)
	if f.isClosed() {
		return libacp.ListSessionsResponse{}, enginebridge.ErrClosed
	}
	return libacp.ListSessionsResponse{}, nil
}

// CloseSession records the call and no-ops.
func (f *FakeBridge) CloseSession(_ context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	f.record("CloseSession(%s)", req.SessionID)
	if f.isClosed() {
		return libacp.CloseSessionResponse{}, enginebridge.ErrClosed
	}
	return libacp.CloseSessionResponse{}, nil
}

// DeleteSession records the call and no-ops.
func (f *FakeBridge) DeleteSession(_ context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	f.record("DeleteSession(%s)", req.SessionID)
	if f.isClosed() {
		return libacp.DeleteSessionResponse{}, enginebridge.ErrClosed
	}
	return libacp.DeleteSessionResponse{}, nil
}

// SetActiveSession records the call and stores id for ActiveSession.
func (f *FakeBridge) SetActiveSession(id libacp.SessionID) {
	f.record("SetActiveSession(%s)", id)
	f.mu.Lock()
	f.active = id
	f.mu.Unlock()
}

// ActiveSession returns the id last passed to SetActiveSession, or "" before
// any call.
func (f *FakeBridge) ActiveSession() libacp.SessionID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// SubmitPrompt records the call. It never itself emits a TurnEnded/
// TurnFailed — script those explicitly via Script if a test needs the turn
// to conclude.
func (f *FakeBridge) SubmitPrompt(sessionID libacp.SessionID, text string) error {
	f.record("SubmitPrompt(%s, %q)", sessionID, text)
	if f.isClosed() {
		return enginebridge.ErrClosed
	}
	if text == "" {
		return enginebridge.ErrEmptyPrompt
	}
	return nil
}

// Cancel records the call.
func (f *FakeBridge) Cancel(sessionID libacp.SessionID) error {
	f.record("Cancel(%s)", sessionID)
	if f.isClosed() {
		return enginebridge.ErrClosed
	}
	return nil
}

// RunShellLine records the call. It never itself emits ShellRunStarted/
// ShellRunResult/TerminalChunk — script those explicitly, exactly as a real
// Bridge would deliver them asynchronously on Events().
func (f *FakeBridge) RunShellLine(sessionID libacp.SessionID, line string) error {
	f.record("RunShellLine(%s, %q)", sessionID, line)
	if f.isClosed() {
		return enginebridge.ErrClosed
	}
	return nil
}
