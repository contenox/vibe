package app

import (
	"context"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/comp/composer"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/fileaddr"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/palette"
	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/input"
	"github.com/contenox/contenox/internal/surfaces/beam/keymap"
)

// Binding ids are stable identities the dispatch switch reads — never a raw
// chord.
const (
	bindQuit    = "app.interrupt"
	bindEditor  = "app.editor"
	bindCancel  = "app.cancel"
	bindHelp    = "help.show"
	bindSubmit  = "composer.submit"
	bindNewline = "composer.newline"

	bindPaletteAccept   = "palette.accept"
	bindPaletteComplete = "palette.complete"
	bindPaletteNext     = "palette.next"
	bindPalettePrev     = "palette.prev"
	bindPaletteClose    = "palette.close"

	bindPickerAccept = "picker.accept"
	bindPickerNext   = "picker.next"
	bindPickerPrev   = "picker.prev"
	bindPickerClose  = "picker.close"
	bindPickerAscend = "picker.ascend"

	bindSessions     = "sessions.show"
	bindSessionsNext = "sessions.next"
	bindSessionsPrev = "sessions.prev"

	bindApprovalAllow  = "approval.allow"
	bindApprovalDeny   = "approval.deny"
	bindApprovalCancel = "approval.cancel"

	bindHelpClose = "help.close"
)

// Binding owners are named in a collision error so a human fixing one never
// has to hunt for the other claimant. "app-shell" and "help" are reserved by
// the keymap package itself (ctrl+c and ?).
const (
	ownerShell    = "app-shell"
	ownerHelp     = "help"
	ownerComposer = "composer"
	ownerPalette  = "command-palette"
	ownerPicker   = "file-addressing"
	ownerApproval = "approval-cards"
	ownerSessions = "session-manager"
)

// The client-side slash commands, registered as palette locals so they
// appear in the same menu as the agent's own commands and shadow a
// same-named remote.
//
// Only what is genuinely client-side belongs here. Keybindings are: they are
// this terminal's, not the session's, which is why the list is /keys and not
// a second /help — the ACP core owns /help, and a command name must mean one
// thing across every client. Anything the core already answers (/rename among
// them) is deliberately absent, so the core's version is the one that runs.
const (
	localKeys     = "keys"
	localQuit     = "quit"
	localNew      = "new"
	localSessions = "sessions"
)

// registerBindings declares every chord beam answers, once, at startup.
// MustRegister panics on a collision, naming both owners.
func registerBindings(r *keymap.Registry) {
	// Global. Reserved chords (ctrl+c, ?) must carry their reserved owner.
	r.MustRegister(keymap.Binding{
		ID: bindQuit, Owner: ownerShell, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"ctrl+c"},
		Help: "clear the composer, then interrupt the turn, then quit",
	})
	r.MustRegister(keymap.Binding{
		ID: bindHelp, Owner: ownerHelp, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"?"},
		Help: "show the keys (on an empty composer)",
	})
	// The readline/bash double-press convention, not a single Ctrl+E: VS
	// Code's integrated terminal (and some other editor terminals) steals a
	// bare Ctrl+E for its own Quick Open before beam ever sees it, but passes
	// Ctrl+X and Ctrl+E through untouched when they arrive as this two-key
	// chord, exactly as every terminal already does for bash's own
	// edit-and-execute-command binding.
	r.MustRegister(keymap.Binding{
		ID: bindEditor, Owner: ownerShell, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"ctrl+x ctrl+e"},
		Help: "compose the draft in $EDITOR",
	})
	r.MustRegister(keymap.Binding{
		ID: bindCancel, Owner: ownerShell, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"esc"},
		Help: "cancel the running turn",
	})
	// Ctrl+S is safe here because the engine's MakeRaw clears IXON, so 0x13
	// arrives as a keystroke instead of being eaten as XOFF.
	r.MustRegister(keymap.Binding{
		ID: bindSessions, Owner: ownerSessions, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"ctrl+s"},
		Help: "switch sessions",
	})

	// Composer.
	r.MustRegister(keymap.Binding{
		ID: bindSubmit, Owner: ownerComposer, Scope: keymap.ScopeComposer,
		Keys: []keymap.Chord{"enter"},
		Help: "send the line (/ command, ! shell, or a prompt)",
	})
	r.MustRegister(keymap.Binding{
		ID: bindNewline, Owner: ownerComposer, Scope: keymap.ScopeComposer,
		Keys: []keymap.Chord{"ctrl+j", "alt+enter"},
		Help: "insert a newline",
	})

	// Command palette.
	r.MustRegister(keymap.Binding{
		ID: bindPaletteAccept, Owner: ownerPalette, Scope: keymap.ScopePalette,
		Keys: []keymap.Chord{"enter"},
		Help: "complete the selected command, or send it",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPaletteComplete, Owner: ownerPalette, Scope: keymap.ScopePalette,
		Keys: []keymap.Chord{"tab"},
		Help: "complete the selected command",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPaletteNext, Owner: ownerPalette, Scope: keymap.ScopePalette,
		Keys: []keymap.Chord{"down"},
		Help: "next command",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPalettePrev, Owner: ownerPalette, Scope: keymap.ScopePalette,
		Keys: []keymap.Chord{"up"},
		Help: "previous command",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPaletteClose, Owner: ownerPalette, Scope: keymap.ScopePalette,
		Keys: []keymap.Chord{"esc"},
		Help: "close the command menu",
	})

	// File picker (@ mentions).
	r.MustRegister(keymap.Binding{
		ID: bindPickerAccept, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"enter"},
		Help: "attach the selected file",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPickerNext, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"down"},
		Help: "next file",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPickerPrev, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"up"},
		Help: "previous file",
	})
	r.MustRegister(keymap.Binding{
		ID: bindPickerClose, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"esc"},
		Help: "close the file list",
	})
	// Backspace means "up one directory" only with the file list open and
	// nothing typed after the "@"; every other reading declines and falls
	// through to the composer (see pickerAscend).
	r.MustRegister(keymap.Binding{
		ID: bindPickerAscend, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"backspace"},
		Help: "up one directory (with nothing typed after the @)",
	})

	// The session switcher shares ScopePicker with the file list. Only j/k
	// are its own; with the file list open dispatch declines them so the
	// letters land in the composer buffer instead.
	r.MustRegister(keymap.Binding{
		ID: bindSessionsNext, Owner: ownerSessions, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"j"},
		Help: "next session (in the session list)",
	})
	r.MustRegister(keymap.Binding{
		ID: bindSessionsPrev, Owner: ownerSessions, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"k"},
		Help: "previous session (in the session list)",
	})

	// Approval card. y/N mirrors the CLI prompt; Esc cancels the turn rather
	// than offering a whole-run abort key — and says so only while there is
	// one, since an ask can outlive the turn that raised it (see MarkDetached).
	r.MustRegister(keymap.Binding{
		ID: bindApprovalAllow, Owner: ownerApproval, Scope: keymap.ScopeApproval,
		Keys: []keymap.Chord{"y"},
		Help: "allow this call",
	})
	r.MustRegister(keymap.Binding{
		ID: bindApprovalDeny, Owner: ownerApproval, Scope: keymap.ScopeApproval,
		Keys: []keymap.Chord{"n"},
		Help: "deny this call",
	})
	r.MustRegister(keymap.Binding{
		ID: bindApprovalCancel, Owner: ownerApproval, Scope: keymap.ScopeApproval,
		Keys: []keymap.Chord{"esc"},
		Help: "cancel the whole turn, if one is still running",
	})

	// Help overlay.
	r.MustRegister(keymap.Binding{
		ID: bindHelpClose, Owner: ownerHelp, Scope: keymap.ScopeHelp,
		Keys: []keymap.Chord{"esc"},
		Help: "close this list",
	})
}

// registerLocalCommands declares the slash commands beam answers itself.
// Everything else — /mission included — is the agent's, dispatched verbatim
// through SubmitPrompt. Descriptions are sentence case with a full stop,
// which is the one register beam's own half of the menu can keep consistent.
func registerLocalCommands(p *palette.Palette) {
	p.MustRegisterLocal(localKeys, "Keybindings and commands, printed here.", "")
	p.MustRegisterLocal(localQuit, "Leave beam.", "")
	p.MustRegisterLocal(localNew, "Start a fresh session.", "")
	p.MustRegisterLocal(localSessions, "Switch to another session (ctrl+s).", "")
}

// helpScopes is every scope the `?` overlay and /keys list — the whole
// registry, modal keys included.
var helpScopes = []keymap.Scope{
	keymap.ScopeGlobal,
	keymap.ScopeComposer,
	keymap.ScopePalette,
	keymap.ScopePicker,
	keymap.ScopeApproval,
	keymap.ScopeHelp,
}

// onKey routes one keystroke. The registry decides first, in the scopes the
// current modal state makes live; anything it does not claim falls through to
// the composer as raw editing input.
func (a *app) onKey(ctx context.Context, k input.KeyEvent) {
	a.refreshModal()
	if act, ok := a.reg.Resolve(a.focus.ActiveScopes(), k, a.now()); ok {
		if a.dispatch(ctx, act) {
			return
		}
	}
	a.editKey(ctx, k)
}

// refreshModal syncs the focus manager's modal stack with the overlay state
// the components actually hold, so Resolve always sees the scopes the frame
// is about to draw. Precedence is the blocking-est first: an approval card
// owns the keyboard, then the help overlay, then the two typing-driven
// overlays.
func (a *app) refreshModal() {
	want := keymap.Scope("")
	switch {
	case a.card != nil:
		want = keymap.ScopeApproval
	case a.helpOpen:
		want = keymap.ScopeHelp
	case a.sessionsOpen, a.pickerOpen:
		want = keymap.ScopePicker
	case a.pal.IsOpen():
		want = keymap.ScopePalette
	}
	for {
		if _, ok := a.focus.PopModal(); !ok {
			break
		}
	}
	if want != "" {
		a.focus.PushModal(want)
	}
}

// dispatch performs one resolved action. It reports false when the action
// declines the keystroke, which hands it on to raw editing — the single case
// is "?" typed into a non-empty composer, where the user means the character.
func (a *app) dispatch(ctx context.Context, act keymap.Action) bool {
	switch act.BindingID {
	case bindQuit:
		a.onCtrlC()
	case bindCancel:
		a.onCancelKey()
	case bindEditor:
		a.openEditor()
	case bindHelp:
		if !a.comp.Empty() {
			return false
		}
		a.helpOpen = true

	case bindSubmit:
		a.submit(ctx)
	case bindNewline:
		a.comp.InsertNewline()
		a.syncOverlays(ctx)

	case bindPaletteAccept:
		a.paletteAccept(ctx)
	case bindPaletteComplete:
		if text, ok := a.pal.CompleteText(); ok {
			a.comp.SetDraft(text)
			a.syncOverlays(ctx)
		}
	case bindPaletteNext:
		a.pal.Move(1)
	case bindPalettePrev:
		a.pal.Move(-1)
	case bindPaletteClose:
		a.dismissPalette()

	case bindSessions:
		a.openSessions(ctx)

	// The four shared picker chords route by which picker is on screen; the
	// two session-only ones decline when it is the file list.
	case bindPickerAccept:
		if a.sessionsOpen {
			a.sessionsAccept(ctx)
			return true
		}
		a.pickerAccept(ctx)
	case bindPickerNext:
		if a.sessionsOpen {
			a.sessions.Move(1)
			return true
		}
		a.pick.Move(1)
	case bindPickerPrev:
		if a.sessionsOpen {
			a.sessions.Move(-1)
			return true
		}
		a.pick.Move(-1)
	case bindPickerClose:
		if a.sessionsOpen {
			a.sessionsOpen = false
			return true
		}
		a.closePicker()
	case bindPickerAscend:
		return a.pickerAscend(ctx)
	case bindSessionsNext:
		if !a.sessionsOpen {
			return false
		}
		a.sessions.Move(1)
	case bindSessionsPrev:
		if !a.sessionsOpen {
			return false
		}
		a.sessions.Move(-1)

	case bindApprovalAllow:
		a.resolveCard(true)
	case bindApprovalDeny:
		a.resolveCard(false)
	case bindApprovalCancel:
		if a.card != nil && a.card.Detached() {
			// Nothing here to cancel: the turn that raised this ask has
			// ended and the ask outlived it. Cancelling the session would
			// interrupt whatever ran next, and would not touch the ask.
			a.notice(frame.StyleWarn, "no turn is running — this approval is still open: y allows it, n denies it, and the run resumes from where it stopped")
			break
		}
		// The card stays pending until the cancel comes back as a cancelled
		// turn (see approval.Card.MarkCancelled).
		if err := a.deps.Bridge.Cancel(a.sessionID); err != nil {
			a.noticef(frame.StyleError, "cancel failed: %v", err)
		}

	case bindHelpClose:
		a.helpOpen = false
	}
	return true
}

// editKey is beam's ONLY raw-key path: the editing vocabulary the registry
// deliberately does not model as semantic actions, because every one of them
// means exactly one thing to a text buffer and nothing to anyone else.
func (a *app) editKey(ctx context.Context, k input.KeyEvent) {
	if a.sessionsOpen {
		// The switcher blocks the composer, so an unclaimed key has nowhere
		// else to go: route it to the roster filter, the same thing typing
		// does with the file list open.
		a.sessionsKey(k)
		return
	}
	if a.composerBlocked() {
		return
	}
	switch {
	case k.Ctrl && !k.Alt:
		switch k.Rune {
		case 'a':
			a.comp.Home()
		case 'w':
			a.comp.DeleteWordBack()
		case 'k':
			a.comp.KillToEnd()
		case 'u':
			a.comp.KillToStart()
		default:
			return
		}
	case k.Alt && !k.Ctrl:
		switch k.Rune {
		case 'b':
			a.comp.WordLeft()
		case 'f':
			a.comp.WordRight()
		default:
			return
		}
	default:
		switch k.Key {
		case input.KeyRune:
			if k.Rune == 0 {
				return
			}
			a.comp.InsertRune(k.Rune)
		case input.KeyEnter:
			// Enter reaching here means no scope claimed it (a modal with no
			// enter binding): treat it as a newline rather than losing it.
			a.comp.InsertNewline()
		case input.KeyBackspace:
			a.comp.Backspace()
		case input.KeyDelete:
			a.comp.DeleteForward()
		case input.KeyLeft:
			a.comp.CursorLeft()
		case input.KeyRight:
			a.comp.CursorRight()
		case input.KeyUp:
			a.comp.CursorUp()
		case input.KeyDown:
			a.comp.CursorDown()
		case input.KeyHome:
			a.comp.Home()
		case input.KeyEnd:
			a.comp.End()
		default:
			return
		}
	}
	a.syncOverlays(ctx)
}

// onCtrlC is the three-way Ctrl+C policy: the composer clears first, an
// in-flight turn is interrupted second, and only a second press inside
// ctrlCWindow quits. The "press again" offer is a status-bar hint (see
// ctrlCArmed) that clears itself when the window lapses, not a scrollback
// notice that would otherwise outlive its own expiry.
func (a *app) onCtrlC() {
	if a.comp.ClearOrPass() {
		a.hasCtrlC = false
		return
	}
	if a.inFlight {
		if err := a.deps.Bridge.Cancel(a.sessionID); err != nil {
			a.noticef(frame.StyleError, "cancel failed: %v", err)
		}
		a.restoreCancelledPrompt()
		a.hasCtrlC = false
		return
	}
	now := a.now()
	if a.hasCtrlC && now.Sub(a.lastCtrlC) <= ctrlCWindow {
		a.quit = true
		return
	}
	a.hasCtrlC = true
	a.lastCtrlC = now
}

// ctrlCArmed reports whether the "press again to quit" offer still stands.
func (a *app) ctrlCArmed() bool {
	return a.hasCtrlC && a.now().Sub(a.lastCtrlC) <= ctrlCWindow
}

// onCancelKey is Esc with no modal open: it interrupts an in-flight turn
// exactly once and never quits.
func (a *app) onCancelKey() {
	if !a.inFlight {
		return
	}
	if err := a.deps.Bridge.Cancel(a.sessionID); err != nil {
		a.noticef(frame.StyleError, "cancel failed: %v", err)
	}
	a.restoreCancelledPrompt()
}

// restoreCancelledPrompt puts the just-cancelled turn's prompt back into the
// composer so the operator can edit and resubmit it — the whole point of
// cancelling rather than waiting the turn out. It never touches scrollback:
// the cancelled line already stands there, appended and unrewritten, exactly
// as the operator sent it. It restores only into an EMPTY composer, so an
// Esc cancel never clobbers a draft already in progress (Ctrl+C's own
// composer-clear arm runs first and would already have consumed a non-empty
// buffer before this is ever reached). One restore, or the next submitted
// prompt, consumes it either way.
func (a *app) restoreCancelledPrompt() {
	if !a.hasLastPrompt || !a.comp.Empty() {
		return
	}
	a.comp.SetDraft(a.lastPrompt)
	a.lastPrompt, a.hasLastPrompt = "", false
}

// resolveCard answers the pending approval and retires the modal. The
// verdict goes into scrollback on the way out (see retireCard): an approval
// answered and forgotten leaves a transcript in which a gated call simply
// happened.
func (a *app) resolveCard(allow bool) {
	if a.card == nil {
		return
	}
	a.card.Resolve(allow)
	a.retireCard()
}

// openEditor is the Ctrl+X, Ctrl+E round trip: the draft is carried in as the seed,
// and the result is carried back into the buffer. The live region is blanked
// before the handover and repainted after, the same shrink-to-nothing path
// shutdown uses — otherwise Suspend's screen handover leaves the live
// region's last paint as permanent, un-repaintable output once control
// returns and the engine repaints only from the cursor down.
func (a *app) openEditor() {
	if a.deps.Editor == nil {
		a.notice(frame.StyleWarn, "no editor configured")
		return
	}
	seed := a.comp.Draft()
	var text string
	var editErr error
	if err := a.deps.Term.Commit(frame.Frame{}); err != nil {
		a.noticef(frame.StyleError, "editor: %v", err)
		return
	}
	if err := a.deps.Term.Suspend(func() error {
		text, editErr = a.deps.Editor(seed)
		return nil
	}); err != nil {
		a.noticef(frame.StyleError, "editor: %v", err)
		return
	}
	if editErr != nil {
		// Includes the editor's own empty-abort; the draft stands unchanged.
		a.noticef(frame.StyleMuted, "editor: %v", editErr)
		return
	}
	a.comp.SetDraft(text)
}

// submit is Enter on the composer: classify, then hand the line to whoever
// owns it. The composer classifies and packages; this decides the consumer.
func (a *app) submit(ctx context.Context) {
	draft := a.comp.Draft()
	if strings.TrimSpace(draft) == "" {
		return
	}
	// Checked before Submit so a refused submission keeps the operator's
	// text. A `!` line needs no turn, so it is allowed alongside one.
	if a.inFlight && composer.Classify(draft).Kind != composer.KindShell {
		a.notice(frame.StyleWarn, "a turn is already running — ctrl+c interrupts it")
		return
	}

	sub, ok := a.comp.Submit()
	if !ok {
		return
	}
	a.closePalette()
	a.closePicker()
	a.remember(sub.Text)

	switch sub.Kind {
	case composer.KindShell:
		if err := a.deps.Bridge.RunShellLine(a.sessionID, sub.Payload); err != nil {
			a.noticef(frame.StyleError, "shell: %v", err)
			a.comp.RestoreLast()
		}
		return

	case composer.KindCommand:
		if e, ok := a.pal.Lookup(commandToken(sub.Text)); ok && e.Local {
			a.echo(sub.Text)
			a.runLocal(ctx, e.Name)
			return
		}
	}

	// Everything else is a turn. The bridge never echoes the operator's own
	// line back, so the echo is fed to the transcript here.
	a.echo(sub.Text)
	if err := a.deps.Bridge.SubmitPrompt(a.sessionID, sub.Text); err != nil {
		a.noticef(frame.StyleError, "send failed: %v", err)
		a.comp.RestoreLast()
		return
	}
	a.lastPrompt, a.hasLastPrompt = sub.Text, true
	a.startTurn()
}

// runLocal executes a client-side slash command. It takes no arguments: not
// one of beam's locals has any, since a command that needs an argument is
// almost always the session's business and therefore the core's.
func (a *app) runLocal(ctx context.Context, name string) {
	switch name {
	case localQuit:
		a.quit = true
	case localKeys:
		a.notices = append(a.notices, a.helpLines()...)
	case localNew:
		a.newSession(ctx)
	case localSessions:
		a.openSessions(ctx)
	}
}

// paletteAccept is Enter with the command menu open: complete the selection
// when the buffer does not already name it, otherwise send the line.
func (a *app) paletteAccept(ctx context.Context) {
	e, ok := a.pal.Selected()
	if !ok {
		a.pal.Close()
		a.submit(ctx)
		return
	}
	// Value mode first: Enter completes the selected value (a second Enter
	// then submits the completed line).
	if v, ok := a.pal.CompleteValueText(); ok && a.comp.Draft() != v {
		a.comp.SetDraft(v)
		a.syncOverlays(ctx)
		return
	}
	if commandToken(a.comp.Draft()) == e.Name {
		a.pal.Close()
		a.submit(ctx)
		return
	}
	a.comp.SetDraft("/" + e.Name + " ")
	a.syncOverlays(ctx)
}

// pickerAccept is Enter on the file list: a directory row opens (fileaddr.IsDir),
// a file row splices. The splice text carries the "@" sigil back since
// MentionSpan's span includes it. Item.Label is the full root-relative path
// however deep the browsing went, so a selection means the same file whether
// typed at the root or walked to.
func (a *app) pickerAccept(ctx context.Context) {
	if it, ok := a.pick.Selected(); ok {
		if fileaddr.IsDir(it) {
			a.descend(ctx, fileaddr.DirName(it))
			return
		}
		if start, length, _, spanOK := a.comp.MentionSpan(); spanOK {
			a.comp.SpliceMention(start, length, mentionSigil+it.Label)
		}
	}
	a.closePicker()
	a.syncOverlays(ctx)
}

// descend walks into a directory row and re-points the overlay at it: new
// rows, new breadcrumb, no query (a query is scoped to the directory it was
// typed in). A refused name leaves the browser where it was — the listing is
// read fresh on every keystroke, so a selected directory can be gone by the
// time Enter is pressed.
func (a *app) descend(ctx context.Context, name string) {
	if err := a.browser.Descend(name); err != nil {
		// The error already names the directory it refused.
		a.noticef(frame.StyleWarn, "file list: %v", err)
		return
	}
	a.resetMention()
	a.loadPicker(ctx, "")
}

// pickerAscend is Backspace with the file list open, and reports whether it
// claimed the key. It declines whenever the key is not a move up the tree —
// a query typed, the session list on screen, or the browser already at the
// root — so the composer sees an ordinary backspace instead.
func (a *app) pickerAscend(ctx context.Context) bool {
	if a.sessionsOpen || !a.pickerOpen || a.pickerQuery != "" {
		return false
	}
	if !a.browser.Ascend() {
		return false
	}
	a.loadPicker(ctx, "")
	return true
}

// resetMention truncates the mention token the caret sits in back to a bare
// "@", so the overlay stays open with an empty query. SpliceMention always
// lands a trailing space, which would close the token; one Backspace takes
// it back off, keeping the caret arithmetic in the composer.
func (a *app) resetMention() {
	start, length, query, ok := a.comp.MentionSpan()
	if !ok || query == "" {
		return
	}
	a.comp.SpliceMention(start, length, mentionSigil)
	a.comp.Backspace()
}

// mentionSigil is the "@" a mention token starts with.
const mentionSigil = "@"

// syncOverlays re-derives the two typing-driven overlays from the buffer
// after every edit: `@` wherever the caret is inside a mention token, `/` on
// a slash-led buffer.
func (a *app) syncOverlays(ctx context.Context) {
	if a.composerBlocked() {
		return
	}

	if _, _, query, ok := a.comp.MentionSpan(); ok {
		a.refreshPicker(ctx, query)
		a.pal.Close()
		return
	}
	a.closePicker()

	draft := a.comp.Draft()
	if !commandShaped(draft) {
		a.pal.Close()
		a.palDismissed, a.hasPalDismissed = "", false
		return
	}
	if a.pal.IsOpen() {
		// The raw draft: past the first space the palette switches to value
		// completion and needs the argument text to filter by.
		a.pal.SetQuery(draft)
		return
	}
	// Reopen unless the operator dismissed this command with Esc and has not
	// retyped it since (see dismissPalette).
	if a.hasPalDismissed && a.palDismissed == commandToken(draft) {
		return
	}
	a.palDismissed, a.hasPalDismissed = "", false
	a.pal.Open(draft)
}

// dismissPalette is Esc on the command menu: close it and remember which
// command token was on screen, so the next keystroke does not reopen it. The
// latch keys off the token rather than a flag, so it holds only as long as
// the operator is still typing the same command name; a new name gets a new
// answer.
func (a *app) dismissPalette() {
	a.palDismissed, a.hasPalDismissed = commandToken(a.comp.Draft()), true
	a.pal.Close()
}

// commandShaped reports whether the buffer is a slash command as far as the
// menu is concerned, using the same predicate to open and to close so a
// pasted command and a typed one behave alike.
func commandShaped(buffer string) bool {
	return strings.HasPrefix(strings.TrimLeft(buffer, " \t"), "/")
}

// refreshPicker points the `@` overlay at query, asking the browser for rows
// rather than filtering a cached page locally: the source ranks the whole
// walk and caps the matches, so a query is re-walked only when it actually
// changes (fileaddr.WalkBudget bounds the cost). An empty query is the
// workspace root's own directory listing, not an alphabetically capped walk.
func (a *app) refreshPicker(ctx context.Context, query string) {
	if a.pickerOpen && a.pickerQuery == query {
		return
	}
	if !a.pickerOpen {
		a.openBrowser()
	}
	a.pickerOpen = true
	a.loadPicker(ctx, query)
}

// openBrowser starts a browse at the workspace root. Every `@` gets a fresh
// one, so a browser never opens where a previous mention happened to end.
func (a *app) openBrowser() {
	a.browser = fileaddr.NewBrowser(a.deps.FileSource)
	a.browser.SetASCII(a.ascii)
	a.pick.SetEmptyText(a.deps.FileSource.EmptyText())
}

// loadPicker fills the overlay from the browser's current directory: rows
// from Query and the breadcrumb header.
func (a *app) loadPicker(ctx context.Context, query string) {
	a.pickerQuery = query
	items, err := a.browser.Query(ctx, query, pickerCandidateCap)
	if err != nil {
		a.noticef(frame.StyleWarn, "file list: %v", err)
	}
	// The source already ranked and capped these; an empty picker query
	// keeps its order (picker.Filter's documented pass-through case).
	a.pick.SetItems(items)
	a.pick.SetQuery("")
	// A rootless source has no directory to name — its breadcrumb stays "".
	header := ""
	if a.deps.FileSource.HasRoot() {
		header = a.browser.Breadcrumb(a.width, a.ascii)
	}
	a.pick.SetHeader(header)
}

func (a *app) closePicker() {
	a.pickerOpen = false
	a.pickerQuery = ""
	a.browser = nil
	a.pick.SetHeader("")
}

// closePalette closes the menu and forgets any Esc dismissal — used wherever
// the buffer the dismissal referred to is gone (a submit, a session switch).
func (a *app) closePalette() {
	a.pal.Close()
	a.palDismissed, a.hasPalDismissed = "", false
}

// pickerCandidateCap is how many MATCHES one query may return — the cap now
// applies after the source's ranking, so it bounds the list an operator
// scrolls rather than the set they can search.
const pickerCandidateCap = 500

// remember appends one submitted line to the recall list the composer walks
// with Up/Down.
func (a *app) remember(text string) {
	a.history = append(a.history, text)
	a.comp.SetHistory(a.history)
}

// echo feeds the operator's own line to the transcript as the user turn it
// is, using the same event shape a session replay delivers.
func (a *app) echo(text string) {
	a.echoSeq++
	a.tr.Apply(userEcho(a.sessionID, a.echoSeq, text))
	a.messages++
}

// commandToken is the bare command name in a buffer: "/qu" -> "qu",
// "  /mission audit deps" -> "mission", anything not slash-led -> "".
func commandToken(buffer string) string {
	s := strings.TrimLeft(buffer, " \t")
	if !strings.HasPrefix(s, "/") {
		return ""
	}
	s = s[1:]
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
