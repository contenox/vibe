// Package app is beam's composition loop: the app-shell (blueprint 4.4) that
// owns the single select over terminal input, the engine-bridge event stream
// and one deterministic ticker, folds each event into component state, and
// commits exactly ONE frame per iteration.
//
// It is the only place in beam that holds every component at once, and it
// holds them the way the blueprint's testability doctrine requires: the
// components stay pure functions of (state, width) -> frame lines, this
// package sequences them. Nothing here reads a terminal (term.Engine does),
// calls a service (enginebridge does), or renders a styled cell (the style
// package does).
//
// # The loop
//
// One iteration is: receive one event, apply it, build one frame, commit it.
// The 130ms ticker is armed ONLY while liveness.Ticking() reports open work,
// so an idle beam schedules zero timers and burns no CPU — the flat-CPU-at-
// rest half of the liveness acceptance criteria (blueprint 4.7).
//
// # Inline rendering
//
// Scrollback is append-only and taken from the transcript at commit time
// (Transcript.TakeAppends), so settled lines are printed once into the
// terminal's real history and are resize-immune by construction. Only the
// bounded Live region — overlay, composer, status bar — repaints, which is
// why a resize touches two rows and a text box rather than the whole screen.
//
// # Keys
//
// Every binding is declared once at startup with an Owner and a Help string
// (keymap.Registry), which is what makes the collision test free enforcement
// and the help overlay a pure projection of the registrations. Keystrokes the
// registry does not claim fall through to the composer as raw editing input;
// that fall-through is the ONLY raw-key path in beam and it lands in exactly
// one function (editKey).
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
	// tickInterval is the app loop's motion granularity. It matches the
	// liveness package's own spinnerTick, so one tick is exactly one spinner
	// frame: no tick renders an identical frame twice, and no spinner frame
	// is skipped.
	tickInterval = 130 * time.Millisecond

	// minWidth/minHeight are the smallest terminal beam lays out in. Below
	// either, the live region is one honest sentence instead of a garbled
	// composite (blueprint 4.4 requirement 2 — the predecessor's relayout
	// lesson).
	minWidth  = 20
	minHeight = 6

	// tooSmallText is that sentence.
	tooSmallText = "terminal too small"

	// ctrlCWindow is how long the "press again to quit" offer stands (D3).
	ctrlCWindow = 2 * time.Second

	// bellWindow is the completion-notification rate floor: at most one BEL
	// per window, however many notifiable facts land inside it.
	bellWindow = 2 * time.Second

	// shutdownBudget bounds every quit path (blueprint 4.4 requirement 8):
	// quitting can never hang the terminal, so a wedged bridge is abandoned
	// rather than waited on.
	shutdownBudget = 2 * time.Second

	// overlayRows is the row budget one overlay (palette, picker, help) may
	// spend in the live region.
	overlayRows = 8

	// turnActivityID is the liveness id of the whole turn; tool calls track
	// under toolActivityPrefix + their tool-call id.
	turnActivityID     = "turn"
	toolActivityPrefix = "tool:"

	// turnLabel and toolLabelFallback are what the status line says while
	// work is open and the activity carries no better title.
	turnLabel         = "working"
	toolLabelFallback = "tool call"
)

// Bridge is exactly the runtime surface this loop consumes — a deliberately
// narrow view of *enginebridge.Bridge, so the app-shell can be driven headless
// by testkit.FakeBridge.
//
// It carries the session-lifecycle calls (blueprint 4.8) because SWITCHING is
// an in-session act: the composition root resolves the FIRST session, but
// every one after it is chosen by an operator who is already inside beam,
// and a surface that cannot start or switch a session is one an operator has
// to quit and relaunch to leave. Nothing here creates a bridge, a database or
// a transport — those stay the root's (D51).
//
// The compile-time assertion below is the contract: every method has a
// same-named, same-signature method on the real Bridge. testkit's FakeBridge
// and EngineBridge interface satisfy it too (asserted in this package's
// tests, which is where a test-only dependency belongs).
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
// is constructed by this package: the terminal, the bridge, the session and
// the capability snapshot are all resolved by beam_cmd (the named composition
// root, blueprint D51) and handed over whole.
type Deps struct {
	// Term is the terminal engine. Required.
	Term term.Engine
	// Bridge is the runtime seam. Required.
	Bridge Bridge
	// Caps is the process-lifetime capability snapshot. Its Mono profile is
	// what selects every component's ASCII fallback.
	Caps style.Caps
	// SessionID is the ACP session this loop STARTS on. Required. beam shows
	// exactly one session at a time (D13), but not always this one: /new and
	// the session switcher replace it in place (see session.go).
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
	// own empty-abort) leaves the draft untouched. Nil disables Ctrl+E.
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

	// sessions is the session switcher's own picker. It is a SECOND instance
	// rather than a mode of pick, because the two overlays differ in the one
	// way that matters to the keyboard: the file picker is driven BY typing
	// into the composer, and this one owns the keyboard outright.
	sessions *picker.Picker

	// The active session's identity, held here rather than read from Deps
	// because switching replaces it. sessionName is the ACP id's display
	// form; sessionTitle is the humane label the server derives (or the
	// operator set with /rename) and wins whenever it is known.
	sessionID    libacp.SessionID
	sessionName  string
	sessionTitle string

	// pickerOpen, sessionsOpen and helpOpen are the overlay states with no
	// component flag of their own (the palette has IsOpen, the card is
	// nil-or-not).
	pickerOpen   bool
	sessionsOpen bool
	helpOpen     bool

	// pickerQuery is the mention query the file list currently HOLDS, so a
	// keystroke that did not change it costs no walk (see refreshPicker).
	pickerQuery string

	// browser is the `@` overlay's position in the workspace tree, alive only
	// while the file list is open. It is built FRESH on every open (see
	// openBrowser), so "@" always starts at the workspace root rather than
	// wherever the last mention was hunted down.
	browser *fileaddr.Browser

	// palDismissed is the command token the operator last closed the palette
	// over with Esc, and hasPalDismissed distinguishes "dismissed a bare /"
	// from "never dismissed". See (*app).dismissPalette.
	palDismissed    string
	hasPalDismissed bool

	// welcomePending is the one-time brand header, printed into scrollback on
	// the first commit and never again.
	welcomePending bool

	// notices are client-side lines (help output, warnings, editor results)
	// waiting to be appended to scrollback by the next commit.
	notices []frame.Line

	// openTools tracks tool-call activities so a turn end can close every one
	// of them; a tool call whose terminal status never arrives would
	// otherwise keep the ticker armed forever.
	openTools map[string]bool

	inFlight bool
	used     int
	size     int
	messages int

	// inbox counts operator-inbox arrivals since launch — see
	// statusbar.State.Inbox for why that is the honest number and not a
	// backlog. It only ever grows: beam has no dismiss action.
	inbox int

	history []string
	echoSeq int

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

// run is the select loop. Its shape is the whole app-shell: one event in, one
// frame out, and a ticker that exists only while something is happening.
func (a *app) run(ctx context.Context) (err error) {
	// Panic recovery restores the terminal before the panic continues
	// (blueprint 4.4, "Also owns: panic recovery restoring the terminal").
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

// ticking reports whether the loop needs its 130ms timer.
//
// It is liveness's own answer plus the one piece of motion that is beam's
// rather than the engine's: the Ctrl+C quit offer, which is a hint with a
// two-second life and no event to end it. Without this the offer would be
// painted and then sit there until the operator happened to press something
// else, which is the exact staleness moving it out of scrollback was meant to
// cure. Everything else here still schedules zero timers at rest.
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
// terminal, in that order. Past the budget the wait is abandoned — a hung
// connection must never hold the terminal in raw mode (blueprint 4.4
// requirement 8), which is the one thing this budget protects.
//
// The bridge's own Close verdict is deliberately not consulted here: it is a
// statement about whether the composition root may go on to close the bus and
// the database, which is the composition root's business, not the shell's.
// Close is idempotent, so the root's own Close call collects that verdict.
func (a *app) shutdown() {
	// Blank the live region first: without this the composer/status rows'
	// last paint (the gold gutter included) survives under the shell's next
	// prompt. An empty frame takes the painter's shrink path, erasing every
	// owned row and parking on a clean one.
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

// notice queues one client-side line for the next commit's scrollback. It is
// the shared transient-notice primitive (D48) in its simplest honest form:
// one line, printed into history, never modal, never timed.
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
