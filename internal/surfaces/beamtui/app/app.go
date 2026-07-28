// Package app is beam's composition loop: the app-shell that owns the single
// select over terminal input, the engine-bridge event stream, and one
// deterministic ticker, folding each event into component state and
// committing exactly one frame per iteration. Components stay pure functions
// of (state, width) -> frame lines; this package alone sequences them, reads
// the terminal, or dispatches to the bridge.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/approval"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/composer"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/palette"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/transcript"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/input"
	"github.com/contenox/beam/internal/surfaces/beamtui/keymap"
	"github.com/contenox/beam/internal/surfaces/beamtui/liveness"
	"github.com/contenox/beam/internal/surfaces/beamtui/style"
	"github.com/contenox/beam/internal/surfaces/beamtui/term"
	libacp "github.com/contenox/beam/libacp"
)

const (
	// tickInterval matches liveness's spinnerTick: one tick is one spinner
	// frame, never more, never fewer.
	tickInterval = 130 * time.Millisecond

	// minWidth/minHeight are the smallest terminal beam lays out in. Below
	// either, the live region renders tooSmallText instead of a garbled
	// composite.
	minWidth  = 20
	minHeight = 6

	tooSmallText = "terminal too small"

	// ctrlCWindow is how long the "press again to quit" offer stands.
	ctrlCWindow = 2 * time.Second

	// bellWindow is the completion-notification rate floor: at most one BEL
	// per window, however many notifiable facts land inside it.
	bellWindow = 2 * time.Second

	// shutdownBudget bounds every quit path: a wedged bridge is abandoned
	// rather than waited on, so quitting can never hang the terminal.
	shutdownBudget = 2 * time.Second

	// overlayRows is the row budget one overlay (palette, picker, help) may
	// spend in the live region.
	overlayRows = 8

	// turnActivityID is the liveness id of the whole turn; tool calls track
	// under toolActivityPrefix + their tool-call id.
	turnActivityID     = "turn"
	toolActivityPrefix = "tool:"

	turnLabel         = "working"
	toolLabelFallback = "tool call"
)

// Bridge is the narrow view of *enginebridge.Bridge this loop consumes, so
// the app-shell can be driven headless by testkit.FakeBridge. It carries the
// session-lifecycle calls because switching sessions is an in-session act
// initiated by an operator already inside beam. Nothing here creates a
// bridge, database, or transport — those stay the composition root's.
//
// Every method here must have a same-named, same-signature method on the
// real Bridge (see the compile-time assertion below); testkit's FakeBridge
// satisfies it too.
type Bridge interface {
	// Events is the single ordered event outlet; it closes when the bridge
	// is gone.
	Events() <-chan enginebridge.Event
	// SubmitPrompt sends one turn and returns immediately.
	SubmitPrompt(sessionID libacp.SessionID, text string) error
	// Cancel interrupts the session's in-flight turn.
	Cancel(sessionID libacp.SessionID) error
	// RunShellLine runs one operator `!` line against the session's warm
	// shell, without an LLM turn.
	RunShellLine(sessionID libacp.SessionID, line string) error

	// NewSession mints a session; LoadSession reopens one and replays its
	// transcript; ListSessions is the roster the switcher renders.
	NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error)
	LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error)
	ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error)
	// SetActiveSession points the bridge's session/update filter. See
	// enginebridge.Bridge.SetActiveSession for the call order session.go
	// follows and why the empty-id window exists.
	SetActiveSession(id libacp.SessionID)

	// Close tears the bridge down.
	Close() error
}

var _ Bridge = (*enginebridge.Bridge)(nil)

// Deps is everything the loop needs from the composition root. Nothing here
// is constructed by this package; it is resolved by the composition root and
// handed over whole.
type Deps struct {
	// Term is the terminal engine. Required.
	Term term.Engine
	// Bridge is the runtime seam. Required.
	Bridge Bridge
	// Caps is the process-lifetime capability snapshot. Its Mono profile is
	// what selects every component's ASCII fallback.
	Caps style.Caps
	// SessionID is the ACP session this loop starts on. Required. beam shows
	// exactly one session at a time; /new and the session switcher replace it
	// in place (see session.go).
	SessionID libacp.SessionID
	// Cwd is the workspace directory new sessions are created in and the
	// roster is filtered by. Empty means "no filter, and let the agent
	// resolve the cwd itself".
	Cwd string
	// FreshSession requests the one-time brand welcome header: true when the
	// resolved session replayed no messages.
	FreshSession bool
	// Model, Provider and SessionName are display strings for the welcome
	// header and the status bar. All optional.
	Model, Provider, SessionName string
	// Editor composes a draft in $EDITOR: it receives the current draft as
	// the seed and returns the edited text. An error (including the editor's
	// own empty-abort) leaves the draft untouched. Nil disables the
	// Ctrl+X, Ctrl+E handoff.
	Editor func(seed string) (string, error)
	// FileSource backs the `@` picker. Nil is legal: the picker then shows
	// fileaddr's fixed no-root empty state.
	FileSource *fileaddr.Source
	// Clock is the loop's only clock — liveness snapshots, the Ctrl+C
	// double-press window and bell coalescing all read it. Nil means
	// time.Now.
	Clock func() time.Time
}

// Run drives beam until the user quits, the terminal ends, or ctx is
// cancelled. It always restores the terminal: every quit path runs the
// bounded shutdown, and an unhandled panic restores the terminal before
// re-panicking, so beam can never leave a shell in raw mode.
func Run(ctx context.Context, deps Deps) error {
	a, err := newApp(deps)
	if err != nil {
		return err
	}
	return a.run(ctx)
}

// app is the loop's whole state. It is owned by one goroutine — the select
// loop — which is what lets every component in it be lock-free.
type app struct {
	deps   Deps
	now    func() time.Time
	ascii  bool
	glyphs style.GlyphSet

	width, height int
	focusedWindow bool

	reg   *keymap.Registry
	focus *keymap.FocusManager

	tr   *transcript.Transcript
	comp *composer.Composer
	pal  *palette.Palette
	pick *picker.Picker
	card *approval.Card
	live *liveness.Tracker

	// sessions is the session switcher's own Picker instance (not a mode of
	// pick): unlike the file picker, it owns the keyboard outright instead of
	// being driven by composer typing.
	sessions *picker.Picker

	// The active session's identity, held here because switching replaces
	// it. sessionName is the ACP id's display form; sessionTitle is the
	// server-derived or /rename label, and wins whenever known.
	sessionID    libacp.SessionID
	sessionName  string
	sessionTitle string

	// pickerOpen, sessionsOpen and helpOpen are the overlay states with no
	// component flag of their own (the palette has IsOpen, the card is
	// nil-or-not).
	pickerOpen   bool
	sessionsOpen bool
	helpOpen     bool

	// pickerQuery is the mention query the file list currently holds, so a
	// keystroke that did not change it costs no walk (see refreshPicker).
	pickerQuery string

	// browser is the `@` overlay's position in the workspace tree, alive only
	// while the file list is open, and rebuilt on every open (see
	// openBrowser) so "@" always starts at the workspace root.
	browser *fileaddr.Browser

	// palDismissed is the command token last closed with Esc; hasPalDismissed
	// distinguishes "dismissed a bare /" from "never dismissed". See
	// (*app).dismissPalette.
	palDismissed    string
	hasPalDismissed bool

	// welcomePending is the one-time brand header, printed into scrollback on
	// the first commit and never again.
	welcomePending bool

	// notices are client-side lines (help output, warnings, editor results)
	// waiting to be appended to scrollback by the next commit.
	notices []frame.Line

	// openTools tracks tool-call activities so a turn end can close every one
	// of them; otherwise a tool call whose terminal status never arrives
	// would keep the ticker armed forever.
	openTools map[string]bool

	inFlight bool
	used     int
	size     int
	messages int

	// inbox counts operator-inbox arrivals since launch. It only grows: beam
	// has no dismiss action.
	inbox int

	history []string
	echoSeq int

	// lastPrompt is the text of the most recently submitted TURN prompt —
	// never a `!` shell line, and never a local slash command, both of which
	// the composer's own last/hasLast already restores on a send failure.
	// hasLastPrompt is consumed by exactly one restore (see
	// restoreCancelledPrompt) or overwritten by the next prompt submission,
	// so it never outlives the turn it names.
	lastPrompt    string
	hasLastPrompt bool

	lastCtrlC time.Time
	lastBell  time.Time
	hasCtrlC  bool
	hasBell   bool

	quit bool
}

func newApp(deps Deps) (*app, error) {
	switch {
	case deps.Term == nil:
		return nil, errors.New("beam app: Deps.Term is required")
	case deps.Bridge == nil:
		return nil, errors.New("beam app: Deps.Bridge is required")
	case deps.SessionID == "":
		return nil, errors.New("beam app: Deps.SessionID is required")
	}
	now := deps.Clock
	if now == nil {
		now = time.Now
	}
	a := &app{
		deps:           deps,
		now:            now,
		ascii:          deps.Caps.Profile == style.ProfileMono,
		glyphs:         style.Glyphs(deps.Caps),
		reg:            keymap.NewRegistry(),
		focus:          keymap.NewFocusManager(),
		tr:             transcript.New(),
		comp:           composer.New(),
		pal:            palette.New(),
		pick:           picker.New(),
		sessions:       picker.New(),
		live:           liveness.NewTracker(0),
		openTools:      make(map[string]bool),
		welcomePending: deps.FreshSession,
		focusedWindow:  true,
		sessionID:      deps.SessionID,
		sessionName:    deps.SessionName,
	}
	a.width, a.height = deps.Term.Size()
	a.focus.SetOrder([]keymap.Scope{keymap.ScopeComposer})
	a.pick.SetPageSize(overlayRows)
	a.pick.SetEmptyText(deps.FileSource.EmptyText())
	a.sessions.SetPageSize(overlayRows)
	a.sessions.SetEmptyText(noSessionsText)
	registerBindings(a.reg)
	registerLocalCommands(a.pal)
	return a, nil
}

// run is the select loop: one event in, one frame out, and a ticker that
// exists only while something is happening.
func (a *app) run(ctx context.Context) (err error) {
	// Panic recovery restores the terminal before the panic continues.
	// Term.Close is idempotent, so the shutdown path below may also have run.
	defer func() {
		if r := recover(); r != nil {
			_ = a.deps.Term.Close()
			panic(r)
		}
	}()

	termEvents := a.deps.Term.Events()
	bridgeEvents := a.deps.Bridge.Events()

	var ticker *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	arm := func() {
		switch {
		case a.ticking() && ticker == nil:
			ticker = time.NewTicker(tickInterval)
			tick = ticker.C
		case !a.ticking() && ticker != nil:
			ticker.Stop()
			ticker = nil
			tick = nil
		}
	}

	if err := a.commit(); err != nil {
		a.shutdown()
		return err
	}
	arm()

	for !a.quit {
		select {
		case <-ctx.Done():
			a.shutdown()
			return nil

		case ev, ok := <-termEvents:
			if !ok {
				// The terminal ended (EOF or Close): quitting is the only
				// honest response — there is no input left to serve.
				a.shutdown()
				return nil
			}
			a.onTerminal(ctx, ev)

		case ev, ok := <-bridgeEvents:
			if !ok {
				// The bridge is gone. Keep the surface alive so the operator
				// can read the transcript and quit deliberately; nothing more
				// will arrive.
				bridgeEvents = nil
				a.notice(frame.StyleWarn, "the engine connection closed — press ctrl+c to quit")
				break
			}
			a.onBridge(ev)

		case <-tick:
			// A tick carries no state change; it exists so the frame below
			// re-renders the liveness pulse.
		}

		if err := a.commit(); err != nil {
			a.shutdown()
			return err
		}
		arm()
	}

	a.shutdown()
	return nil
}

// commit builds exactly one frame and hands it to the terminal.
func (a *app) commit() error {
	return a.deps.Term.Commit(a.buildFrame())
}

// ticking reports whether the loop needs its 130ms timer: liveness's own
// answer, plus the Ctrl+C quit offer, which has a two-second life and no
// event of its own to end it.
func (a *app) ticking() bool { return a.live.Ticking() || a.ctrlCArmed() }

// onTerminal folds one decoded input event into state.
func (a *app) onTerminal(ctx context.Context, ev input.Event) {
	switch e := ev.(type) {
	case input.KeyEvent:
		a.onKey(ctx, e)
	case input.PasteEvent:
		// One block, never re-read as keystrokes (the bracketed-paste
		// contract): a pasted "/quit" is text, not a command.
		if a.composerBlocked() {
			return
		}
		a.comp.InsertString(e.Text)
		a.syncOverlays(ctx)
	case input.ResizeEvent:
		a.width, a.height = e.Width, e.Height
	case input.FocusEvent:
		a.focusedWindow = e.Focused
	}
}

// shutdown is every quit path's tail: bounded bridge teardown, then the
// terminal, in that order. Past shutdownBudget the wait is abandoned so a
// hung connection can never hold the terminal in raw mode. The bridge's own
// Close verdict is not consulted here — that governs whether the composition
// root may close the bus and database, and Close is idempotent, so the
// root's own call collects it.
func (a *app) shutdown() {
	// Blank the live region first so the composer/status rows' last paint
	// does not survive under the shell's next prompt.
	_ = a.deps.Term.Commit(frame.Frame{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.deps.Bridge.Close()
	}()
	select {
	case <-done:
	case <-time.After(shutdownBudget):
	}
	_ = a.deps.Term.Close()
}

// notice queues one client-side line for the next commit's scrollback: one
// line, printed into history, never modal, never timed.
func (a *app) notice(id frame.StyleID, text string) {
	a.notices = append(a.notices, frame.Styled(id, text))
}

func (a *app) noticef(id frame.StyleID, format string, args ...any) {
	a.notice(id, fmt.Sprintf(format, args...))
}

// composerBlocked reports whether a modal owns the keyboard outright. The
// approval card, the help overlay and the session switcher do; the palette
// and the file picker do not, because both are driven BY typing into the
// composer.
func (a *app) composerBlocked() bool { return a.card != nil || a.helpOpen || a.sessionsOpen }
