package app

import (
	"context"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/composer"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/palette"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/input"
	"github.com/contenox/beam/internal/surfaces/beamtui/keymap"
)

// Binding ids. They are stable identities (a later user-remap file keys off
// them) and they are what the dispatch switch below reads — never a raw
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

// Binding owners, named in a collision error so a human fixing one never has
// to hunt for the other claimant. "app-shell" and "help" are the two reserved
// owners the keymap package itself knows about (ctrl+c and ?).
const (
	ownerShell    = "app-shell"
	ownerHelp     = "help"
	ownerComposer = "composer"
	ownerPalette  = "command-palette"
	ownerPicker   = "file-addressing"
	ownerApproval = "approval-cards"
	ownerSessions = "session-manager"
)

// The client-side slash commands. They are registered as palette locals so
// they appear in the same menu as the agent's own commands, in /help and in
// the `?` overlay, and shadow a same-named remote.
//
// localRename is the one that still goes to the agent: the TITLE is
// server-side truth (session/list reads it back, session_info_update pushes
// it), so beam forwards the line verbatim and only adopts the label locally
// so the status bar does not lag the keystroke by a whole turn. See
// (*app).renameSession.
const (
	localHelp     = "help"
	localQuit     = "quit"
	localNew      = "new"
	localSessions = "sessions"
	localRename   = "rename"
)

// registerBindings declares every chord beam answers, once, at startup. The
// registry's collision check runs here — MustRegister panics in development
// and fails the package's test in CI, naming both owners.
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
	r.MustRegister(keymap.Binding{
		ID: bindEditor, Owner: ownerShell, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"ctrl+e"},
		Help: "compose the draft in $EDITOR",
	})
	r.MustRegister(keymap.Binding{
		ID: bindCancel, Owner: ownerShell, Scope: keymap.ScopeGlobal,
		Keys: []keymap.Chord{"esc"},
		Help: "cancel the running turn",
	})
	// Ctrl+S is safe here specifically because beam owns the terminal: the
	// engine's MakeRaw clears IXON, so 0x13 arrives as a keystroke instead of
	// being eaten as XOFF and freezing the output — the folklore reason this
	// chord is usually avoided does not apply to a raw-mode TUI. It is
	// otherwise unclaimed (see the collision test) and reads as "sessions".
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
	// Backspace is the browser's "up one directory", and it is the one binding
	// beam declares that spends most of its life DECLINING: it means the parent
	// directory only with the file list open and nothing typed after the "@".
	// Every other reading of the key — editing a query, the session switcher,
	// the root's own parent — falls through to the composer, where a backspace
	// is a backspace (see pickerAscend).
	r.MustRegister(keymap.Binding{
		ID: bindPickerAscend, Owner: ownerPicker, Scope: keymap.ScopePicker,
		Keys: []keymap.Chord{"backspace"},
		Help: "up one directory (with nothing typed after the @)",
	})

	// The session switcher shares ScopePicker — it IS a picker, and both
	// overlays want the same enter/esc/arrows. Only j/k are its own, and only
	// while it is the picker on screen: with the FILE list open the operator
	// is still typing a query into the composer, so dispatch DECLINES these
	// two there and the letters land in the buffer where they were aimed.
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

	// Approval card. y/N mirrors the CLI prompt (D6); Esc cancels the turn
	// rather than offering a whole-run abort key.
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
		Help: "cancel the whole turn",
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
// through SubmitPrompt so command parity with an ACP editor is structural.
//
// The session verbs are flat (/new, /sessions, /rename) rather than one
// `/session <verb>`: the palette filters on a command's first token, so three
// names are three visible, completable rows, while one name would complete to
// a prefix and then offer no help at all with the verb that follows it.
// Descriptions are sentence case with a full stop. The five locals were
// written by five different hands and read as five different registers next
// to each other in one menu; the agent's own descriptions arrive however the
// agent wrote them, so beam's half is the only half it can keep consistent.
func registerLocalCommands(p *palette.Palette) {
	p.MustRegisterLocal(localHelp, "Keys and commands, printed here.", "")
	p.MustRegisterLocal(localQuit, "Leave beam.", "")
	p.MustRegisterLocal(localNew, "Start a fresh session.", "")
	p.MustRegisterLocal(localSessions, "Switch to another session (ctrl+s).", "")
	p.MustRegisterLocal(localRename, "Name this session.", "<title>")
}

// helpScopes is every scope the help overlay and /help list. It is the whole
// registry: with one focusable pane there is no "unreachable" half worth
// hiding, and a user asking what the keys are wants the modal keys too.
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

	// The four shared picker chords route by WHICH picker is on screen; the
	// two session-only ones decline when it is the file list, which sends
	// them to the composer as the letters they are.
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
		// The card stays pending until the cancel comes back as a cancelled
		// turn: answering here would put a decision on the wire the operator
		// never gave (approval.Card.MarkCancelled's own rule).
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

// onCtrlC is D3's three-way policy in one place: the composer clears first,
// an in-flight turn is interrupted second, and only a second press inside
// ctrlCWindow quits.
//
// The "press again" offer is LIVE, not history. It used to be a notice —
// immutable scrollback — so a terminal an operator walked away from kept a
// two-second offer on screen forever, and every stray Ctrl+C left another
// permanent copy of an instruction that had already expired. It is now a
// status-bar hint that stands exactly as long as the window does (see
// (*app).status and ctrlCArmed); the ticker stays armed for the window so the
// hint clears itself rather than waiting for the next keystroke.
func (a *app) onCtrlC() {
	if a.comp.ClearOrPass() {
		a.hasCtrlC = false
		return
	}
	if a.inFlight {
		if err := a.deps.Bridge.Cancel(a.sessionID); err != nil {
			a.noticef(frame.StyleError, "cancel failed: %v", err)
		}
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

// ctrlCArmed reports whether the "press again to quit" offer still stands. It
// is read by the status bar (the hint) and by the loop's ticker arming (so the
// hint disappears on its own when the window lapses).
func (a *app) ctrlCArmed() bool {
	return a.hasCtrlC && a.now().Sub(a.lastCtrlC) <= ctrlCWindow
}

// onCancelKey is Esc with no modal open: it interrupts an in-flight turn
// exactly once and never quits (blueprint 4.5).
func (a *app) onCancelKey() {
	if !a.inFlight {
		return
	}
	if err := a.deps.Bridge.Cancel(a.sessionID); err != nil {
		a.noticef(frame.StyleError, "cancel failed: %v", err)
	}
}

// resolveCard answers the pending approval and drops the modal.
func (a *app) resolveCard(allow bool) {
	if a.card == nil {
		return
	}
	a.card.Resolve(allow)
	a.card = nil
}

// openEditor is the Ctrl+E round trip (`chat -e`'s superpower, promoted to
// composer MVP). The two prior-art regressions it exists to prevent are both
// visible here: the draft is carried IN as the seed, and the result is
// carried BACK into the buffer.
//
// It hands the terminal over EMPTY. Suspend gives the screen to another
// program and, on the way back, the engine disowns the live region entirely —
// it cannot know what the child did, so the next commit paints fresh wherever
// the cursor now is and reclaims nothing above it. Whatever the region held
// when the child took over therefore stops being a repaintable region and
// becomes literal, permanent output: the transcript's live tail, and the
// composer under it, frozen mid-draft. It looked like the last block had been
// appended a second time, and a third after the next Ctrl+E.
//
// So the region is blanked before the handover and repainted after it, which
// is the same shrink-to-nothing path shutdown uses and the same reason: the
// only rows beam may leave behind are rows it deliberately printed as history.
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
		// Includes the editor's own empty-abort: the draft simply stands.
		a.noticef(frame.StyleMuted, "editor: %v", editErr)
		return
	}
	a.comp.SetDraft(text)
}

// submit is Enter on the composer: classify, then hand the line to whoever
// owns it. The composer classifies and packages; this decides the consumer
// (blueprint 4.11's ownership ruling).
func (a *app) submit(ctx context.Context) {
	draft := a.comp.Draft()
	if strings.TrimSpace(draft) == "" {
		return
	}
	// The in-flight check happens BEFORE Submit so a refused submission keeps
	// the operator's text instead of clearing it. A `!` line needs no turn and
	// is HITL-exempt, so it is allowed alongside one.
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
			a.runLocal(ctx, e.Name, commandArgs(sub.Text))
			return
		}
	}

	// Everything else is a turn. The bridge never echoes the operator's own
	// line back (only session/load replay produces UserEcho), so the echo is
	// fed to the transcript here, as the same event a replay would deliver.
	a.echo(sub.Text)
	if err := a.deps.Bridge.SubmitPrompt(a.sessionID, sub.Text); err != nil {
		a.noticef(frame.StyleError, "send failed: %v", err)
		a.comp.RestoreLast()
		return
	}
	a.startTurn()
}

// runLocal executes a client-side slash command.
func (a *app) runLocal(ctx context.Context, name, args string) {
	switch name {
	case localQuit:
		a.quit = true
	case localHelp:
		a.notices = append(a.notices, a.helpLines()...)
	case localNew:
		a.newSession(ctx)
	case localSessions:
		a.openSessions(ctx)
	case localRename:
		a.renameSession(ctx, args)
	}
}

// paletteAccept is Enter with the command menu open: complete the selection
// when the buffer does not already name it, otherwise send the line (so a
// completed command with arguments submits rather than losing them).
func (a *app) paletteAccept(ctx context.Context) {
	e, ok := a.pal.Selected()
	if !ok {
		a.pal.Close()
		a.submit(ctx)
		return
	}
	// Value mode first: Enter completes the selected VALUE into the buffer
	// (a second Enter then submits the completed line).
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

// pickerAccept is Enter on the file list: a directory row OPENS, a file row
// SPLICES. fileaddr.IsDir is the branch, and it reads the same trailing slash
// the row itself shows, so what the key does is never a secret the caller
// keeps from the screen.
//
// The splice carries the "@" back. MentionSpan's span INCLUDES the sigil, and
// fileaddr's Item.Label is documented as "exactly the text the composer
// splices after the @" — so handing the bare label to SpliceMention replaced
// "@ret" with "retry.go" and silently un-mentioned the file the operator had
// just picked. What went to the agent was a prompt with a filename in it, not
// an attachment.
//
// A file row's Label is the FULL root-relative path however deep the browsing
// went (fileaddr.Browser's second rule), so a selection means the same file
// whether it was typed at the root or walked to.
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
// rows, new breadcrumb, and no query — a query is scoped to the directory it
// was typed in, so carrying it down would silently re-run somebody's old
// search against a tree they have just left.
//
// A refused name leaves the browser exactly where it was and says so. It is
// reachable in practice, not just in theory: the listing is read fresh on
// every keystroke, so a directory the operator selected can be gone by the
// time they press Enter on it.
func (a *app) descend(ctx context.Context, name string) {
	if err := a.browser.Descend(name); err != nil {
		// The error already names the directory it refused.
		a.noticef(frame.StyleWarn, "file list: %v", err)
		return
	}
	a.resetMention()
	a.loadPicker(ctx, "")
}

// pickerAscend is Backspace with the file list open, and it reports whether it
// claimed the key.
//
// It DECLINES in every case that is not a move up the tree, which is what lets
// one chord do two jobs without either of them surprising anyone. With a query
// typed, backspace edits the query — navigation never eats a keystroke aimed at
// text. At the root, Ascend has no parent to offer and returns false, so the
// key reaches the composer and deletes back over the "@", closing the overlay:
// backspacing out of the browser is the same gesture as backspacing out of any
// other token. And with the SESSION list on screen it is not this overlay's key
// at all — the same decline-to-raw shape the j/k bindings use, inverted, since
// those two belong to the switcher and this one belongs to the file list.
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
// "@" and parks the caret on it, so the overlay stays open with an empty query.
//
// SpliceMention is the only edit that addresses a mention span, and it always
// lands one trailing space after the replacement — which would end the token
// and close the very overlay this is refreshing. One Backspace takes that
// space straight back off. Going through the pair rather than rebuilding the
// draft by hand keeps the sanitize gate and the caret arithmetic in the
// composer, where a multiline draft and a mention with text after it are
// already somebody's problem.
func (a *app) resetMention() {
	start, length, query, ok := a.comp.MentionSpan()
	if !ok || query == "" {
		return
	}
	a.comp.SpliceMention(start, length, mentionSigil)
	a.comp.Backspace()
}

// mentionSigil is the "@" a mention token starts with — the one MentionSpan
// counts into its span and the one pickerAccept has to put back.
const mentionSigil = "@"

// syncOverlays re-derives the two typing-driven overlays from the buffer
// after every edit. Both triggers are the blueprint's: `@` wherever the caret
// is inside a mention token, `/` on a slash-led buffer.
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
		// The raw draft, not just the command token: past the first space the
		// palette switches to VALUE completion (the /model roster and
		// friends), and it needs the argument text to filter by.
		a.pal.SetQuery(draft)
		return
	}
	// Closed on a command-shaped buffer: reopen unless the operator dismissed
	// THIS command with Esc and has not retyped it since (see dismissPalette).
	if a.hasPalDismissed && a.palDismissed == commandToken(draft) {
		return
	}
	a.palDismissed, a.hasPalDismissed = "", false
	a.pal.Open(draft)
}

// dismissPalette is Esc on the command menu: close it AND remember which
// command token was on screen, so the very next keystroke does not put it
// straight back.
//
// The latch keys off the TOKEN rather than on a plain "stay closed" flag,
// which is the difference between two behaviours that both look reasonable
// written down. A flag makes Esc permanent: an operator who dismissed the
// menu over "/mis", then kept typing "sion", never saw it again on that line
// and had no way to ask for it back. Reopening on any keystroke is the other
// extreme — pressing Left to fix a typo would resurrect the menu they had
// just dismissed. Keying on the token means Esc holds for exactly as long as
// the operator is still talking about the same command: navigation, deletion
// inside the argument, and a second look at the line all leave it closed;
// typing another letter of the NAME is a new question and gets a new answer.
func (a *app) dismissPalette() {
	a.palDismissed, a.hasPalDismissed = commandToken(a.comp.Draft()), true
	a.pal.Close()
}

// commandShaped reports whether the buffer is a slash command as far as the
// menu is concerned. It is deliberately the same predicate on the way in as
// on the way out — the palette used to OPEN only on a buffer of exactly "/"
// while closing on anything not slash-led, so a pasted "/qu" never opened a
// menu that a typed "/qu" always did.
func commandShaped(buffer string) bool {
	return strings.HasPrefix(strings.TrimLeft(buffer, " \t"), "/")
}

// refreshPicker points the `@` overlay at query, asking the BROWSER for the
// rows rather than filtering a cached page locally.
//
// The cache was the bug. One open fetched Candidates(ctx, "", 500) and every
// keystroke after that filtered those 500 in memory — but the source caps
// AFTER ranking, so an unfiltered fetch caps alphabetically: in any workspace
// with more than 500 files, everything past the 500th name did not exist as
// far as the picker was concerned, and typing its exact path produced "no
// matching files" for a file plainly on disk.
//
// So the query goes to the source, which ranks the whole walk and caps the
// MATCHES. That is one walk per changed query. It is affordable at repo scale
// — the walk is budgeted (fileaddr.WalkBudget) and beam is one process on one
// machine — and it is only spent when the query actually CHANGED, so cursor
// movement, selection and re-renders inside a mention cost nothing. If a
// pathological tree ever makes the keystroke visible, the debounce belongs
// here, at the one call site, and not in a cache that answers wrongly.
//
// The browser is what makes an EMPTY query useful. It used to be the whole
// walk, alphabetically capped — the one list a user who does not yet know the
// filename has no way to read. Now it is the workspace root's own directory
// listing, and typing narrows the subtree the operator has navigated to rather
// than the repository.
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
// one: a browser that remembered where the last mention was found would open
// the next one in a directory the operator has no memory of choosing.
func (a *app) openBrowser() {
	a.browser = fileaddr.NewBrowser(a.deps.FileSource)
	a.browser.SetASCII(a.ascii)
	a.pick.SetEmptyText(a.deps.FileSource.EmptyText())
}

// loadPicker fills the overlay from the browser's CURRENT directory: rows from
// Query (this directory's listing when query is empty, a recursive search of
// its subtree otherwise) and the breadcrumb header, which is the only thing on
// screen that says where those rows came from once the file rows are full
// root-relative paths.
func (a *app) loadPicker(ctx context.Context, query string) {
	a.pickerQuery = query
	items, err := a.browser.Query(ctx, query, pickerCandidateCap)
	if err != nil {
		a.noticef(frame.StyleWarn, "file list: %v", err)
	}
	// The source already ranked and capped these, so the picker's own query
	// stays empty: an empty query is picker.Filter's documented "keep the
	// caller's order" case, and re-ranking here would be a second opinion
	// about an order that is not this component's to hold — and in browse mode
	// it would break the dirs-first order outright.
	a.pick.SetItems(items)
	a.pick.SetQuery("")
	// A rootless source has no directory to name. Its breadcrumb is a bare
	// "/", which over "no workspace root" would be a crumb for a tree that is
	// not there — the no-root state is a fixed one line, and stays that.
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

// commandArgs is everything AFTER the command name: "/rename the ingest
// rewrite" -> "the ingest rewrite". It is the other half of commandToken and
// splits at the same place, so the two can never disagree about where a name
// ends. A command with no arguments yields "".
func commandArgs(buffer string) string {
	s := strings.TrimLeft(buffer, " \t")
	if !strings.HasPrefix(s, "/") {
		return ""
	}
	i := strings.IndexAny(s[1:], " \t\n")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[1+i:])
}
