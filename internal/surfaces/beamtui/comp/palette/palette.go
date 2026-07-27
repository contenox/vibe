// Package palette owns slash-command discovery for beam's composer: the
// merged command set (whatever the last `available_commands_update`
// advertised, plus the locals other components register), the
// case-insensitive prefix filter behind the `/` trigger, and the overlay
// rows the app draws in the live region.
//
// The set is never hardcoded. `/mission` is capability-gated and an
// externally delegated session replaces the whole menu, so the remote half
// is exactly what SetRemote was last handed and nothing else — the parity
// criterion in blueprint 4.13 item 2 ("the palette renders the same
// available_commands set an ACP editor receives").
//
// This package is deliberately a lookup table and a renderer, never a gate.
// It exposes no API by which a submission can be rejected: an unrecognized
// `/token` is prompt text, which is what acpsvc's parseCommand decides
// server-side (item 6). Dispatch — a remote name onto the normal send path,
// a local name into its in-process handler — is the app shell's call, made
// from Selected/Lookup; the handler itself lives with the component that
// registered the name, not here.
//
// Every render is a pure function of (state, width, maxRows, ascii) →
// []frame.Line: no terminal reads, no capability probing, no styles beyond
// the semantic frame IDs. A Palette is owned by one goroutine (the UI loop)
// and carries no locking of its own.
package palette

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/beam/libacp"
)

// ASCIIMarker is the selection marker a Mono terminal sees, exported so
// testkit's glyph-parity test can hold every surface's ASCII beam-bar against
// the style package's GlyphSet in one place. Components may not import style,
// so the agreement can only be checked from outside.
const ASCIIMarker = "|"

// EmptyInputHint is the composer's empty-buffer affordance (blueprint 4.13
// item 11). The copy lives here so the palette owns the one sentence that
// advertises it; the composer decides when to draw it.
const EmptyInputHint = "type / for commands"

const (
	// markerUnicode is the beam-bar in its active-picker role: the same gold
	// stroke the brand device and the composer sigil use, marking which row
	// Enter would act on. ASCII degrades to a pipe.
	markerUnicode = "▌"
	markerASCII   = ASCIIMarker

	// emptyText is what an over-filtered palette says. It states the
	// consequence rather than the failure: nothing is blocked, the line is
	// still a perfectly good prompt (item 6).
	emptyUnicode = "no matching command — Enter sends as chat"
	emptyASCII   = "no matching command - Enter sends as chat"

	// emptyValueText is the same sentence for an over-filtered VALUE list. A
	// value the server never advertised is still a legal argument — the
	// operator may know something the runtime has not discovered yet — so the
	// line says what Enter will do, not that the value is wrong.
	emptyValueUnicode = "no matching value — Enter sends it anyway"
	emptyValueASCII   = "no matching value - Enter sends it anyway"

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."

	// upUnicode/upASCII mark the footer's "there is more above" count. The
	// caret is the same one comp/composer uses for a scrolled draft, so one
	// glyph means "scrolled past" everywhere in the live region.
	upUnicode = "↑"
	upASCII   = "^"

	// nameGap separates a command name from its description; indent is the
	// unselected row's stand-in for the marker, so names stay in one column
	// whichever row is selected.
	nameGap = "  "
	indent  = "  "
)

// ErrDuplicateLocal reports two local registrations claiming one name.
// Blueprint 4.13 item 8 makes this fail-fast rather than last-write-wins:
// two components silently fighting over `/sessions` is a bug that must
// surface at wiring time, not at the keystroke that picks the loser.
var ErrDuplicateLocal = errors.New("palette: local command already registered")

// ErrInvalidName reports a registration whose name is empty or contains a
// space (the buffer splits the command at the first space, so a spaced name
// could never be typed).
var ErrInvalidName = errors.New("palette: invalid command name")

// Entry is one command in the merged set. Name is bare — no leading `/`,
// which the renderer and CompleteText add — matching the ACP wire, where
// AvailableCommand.Name is "help", not "/help".
//
// Local marks the dispatch half: true means an in-process handler answers
// it and nothing is sent, false means the line goes to the agent verbatim
// and acpsvc's parseCommand does the work.
type Entry struct {
	Name        string
	Description string
	Hint        string
	Local       bool
}

// Palette is the command menu behind the composer's `/` trigger.
//
// The zero value is not usable; call New. A fresh Palette knows no
// commands at all: locals arrive by registration and remotes by SetRemote,
// so a session that advertised nothing shows nothing.
type Palette struct {
	remote []libacp.AvailableCommand
	locals map[string]Entry

	// domains are the argument VALUES a command accepts, by bare command name
	// (see SetValueDomains). A name with no entry has no value mode at all.
	domains map[string][]string

	open  bool
	query string
	// arg is the first argument as typed so far, and hasArg records that the
	// space after the command name has been typed. Together they are the whole
	// of the value-mode trigger: they are set from whatever Open/SetQuery was
	// handed, so a caller passing only the command token (the shape the app has
	// always passed) simply never enters value mode.
	arg    string
	hasArg bool
	sel    int
}

// New returns an empty palette. Nothing is pre-registered — not even
// `/quit` or `/help`: the app owns which locals exist, and this package
// owning a default set would make them unremovable by the surface that
// hosts them.
func New() *Palette {
	return &Palette{locals: make(map[string]Entry)}
}

// SetRemote replaces the agent-advertised half of the set with whatever the
// latest CommandsUpdated reported. It REPLACES rather than merges: a
// re-bound or delegated session legitimately drops commands, and a merge
// would keep offering a name the agent no longer answers.
//
// Two things happen on the way in. Every string is SANITIZED: an
// available_commands_update is a peer's payload, its descriptions land in an
// overlay drawn over the composer, and an escape sequence there would write
// to the terminal from underneath a menu the operator is navigating by
// muscle memory.
//
// And the SELECTION follows its command by name. These updates arrive
// unprompted — a session rebinds, a mode changes — and can land between the
// operator reading a row and pressing Enter. Holding the index still would
// silently move the selection onto whatever command sorted into that slot,
// which is the one way a palette can dispatch something nobody chose. If the
// selected name is gone the index clamps, because there is nothing to follow.
func (p *Palette) SetRemote(cmds []libacp.AvailableCommand) {
	selected := p.selectedName()

	out := make([]libacp.AvailableCommand, 0, len(cmds))
	for _, c := range cmds {
		c.Name = sanitize.Line(c.Name)
		c.Description = sanitize.Line(c.Description)
		if c.Input != nil {
			in := *c.Input
			in.Hint = sanitize.Line(in.Hint)
			c.Input = &in
		}
		out = append(out, c)
	}
	p.remote = out

	ents := p.Filtered()
	if len(ents) == 0 {
		p.sel = 0
		return
	}
	for i, e := range ents {
		if e.Name == selected && selected != "" {
			p.sel = i
			return
		}
	}
	p.sel = clampIndex(p.sel, len(ents))
}

// selectedName is the highlighted entry's name regardless of whether the
// overlay is open, so SetRemote can restore a selection the operator will see
// the moment they reopen it.
func (p *Palette) selectedName() string {
	ents := p.Filtered()
	if len(ents) == 0 {
		return ""
	}
	return ents[clampIndex(p.sel, len(ents))].Name
}

// RegisterLocal adds a TUI-local command. Locals shadow same-named remotes
// (the in-process handler wins), and a second local on one name is
// ErrDuplicateLocal — never a silent overwrite.
//
// A leading `/` on name is accepted and stripped, so a caller may register
// the string it shows the user.
func (p *Palette) RegisterLocal(name, description, hint string) error {
	// Sanitize before validating, so validation sees the name that will
	// actually be stored and looked up — a name that changed shape after the
	// check would be registered under one string and matched against another.
	// Descriptions and hints are rendered, so they go through the same gate:
	// a local is in-process today, but "in-process" is not "trusted to hold a
	// well-formed string".
	name = sanitize.Line(name)

	// Validate before normalize: normalize CUTS at the first space (a query
	// is one token), and silently registering "two words" as "two" would
	// hide the mistake instead of reporting it.
	n := strings.TrimPrefix(strings.TrimSpace(name), "/")
	if n == "" || strings.ContainsAny(n, " \t") {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if _, exists := p.locals[n]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateLocal, n)
	}
	p.locals[n] = Entry{
		Name:        n,
		Description: sanitize.Line(description),
		Hint:        sanitize.Line(hint),
		Local:       true,
	}
	return nil
}

// MustRegisterLocal is RegisterLocal for wiring code, panicking on a
// collision. Registration happens once at startup from a fixed set of
// components, so a duplicate is a programming error the process should not
// survive (item 8's fail-fast rule).
func (p *Palette) MustRegisterLocal(name, description, hint string) {
	if err := p.RegisterLocal(name, description, hint); err != nil {
		panic(err)
	}
}

// Open shows the palette filtered by query. The composer calls this when
// the trigger rule fires — a leading `/` on the trimmed buffer, never
// stricter than the server's parseCommand (item 1).
//
// query may be the raw buffer: a leading `/` is tolerated, the text up to the
// first space is the command filter, and the text after it is the argument the
// value mode reads (SetValueDomains). Passing only the command token is
// equally valid and simply never reaches value mode.
func (p *Palette) Open(query string) {
	p.open = true
	p.SetQuery(query)
}

// SetQuery updates the filter and returns the selection to the top, since
// the row under it changed meaning. It does not open or close the palette:
// openness follows the trigger rule, which is the composer's to evaluate.
//
// Whatever it is handed past the command name is the ARGUMENT half: hand it
// the raw buffer and a command with a known value domain switches the overlay
// to that domain (see SetValueDomains); hand it the bare command token — the
// shape the composer has always passed — and nothing changes.
func (p *Palette) SetQuery(q string) {
	p.query = normalize(q)
	p.arg, p.hasArg = argument(q)
	p.sel = 0
}

// Close hides the palette without touching the buffer (item 4: Esc closes,
// the typed text stays).
func (p *Palette) Close() {
	p.open = false
	p.query = ""
	p.arg = ""
	p.hasArg = false
	p.sel = 0
}

// SetValueDomains replaces the argument-value domains: the values each command
// accepts for its first argument, keyed by bare command name ("model", not
// "/model"). It is the value half of item 2's "never hardcoded" — the app feeds
// it from the session config options the server advertises (see
// enginebridge.ValueDomains), so the values offered for `/model ` are the
// models the runtime actually has, and a session that advertised none offers
// none.
//
// It REPLACES, for the same reason SetRemote does: a rebound or delegated
// session legitimately has a different set of models, and a merge would keep
// offering one it no longer runs.
//
// A name with no values — absent, empty, or all-blank — has NO value mode: its
// arguments are ordinary text the palette neither completes nor comments on.
// This package still exposes no way to reject anything; an argument outside the
// domain is a perfectly good argument that simply has no row (item 6).
//
// Values are sanitized on the way in for the same reason command descriptions
// are: they come from a peer's payload and land in an overlay drawn over the
// composer. The selection follows its VALUE by name across the replacement, so
// an update landing between reading a row and pressing Tab cannot silently move
// it onto a different model.
func (p *Palette) SetValueDomains(domains map[string][]string) {
	selected, hadSelection := p.SelectedValue()

	out := make(map[string][]string, len(domains))
	for name, values := range domains {
		name = normalize(sanitize.Line(name))
		if name == "" {
			continue
		}
		clean := make([]string, 0, len(values))
		seen := make(map[string]struct{}, len(values))
		for _, v := range values {
			v = strings.TrimSpace(sanitize.Line(v))
			if v == "" {
				continue
			}
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			clean = append(clean, v)
		}
		if len(clean) == 0 {
			continue
		}
		out[name] = clean
	}
	p.domains = out

	if !hadSelection {
		return
	}
	values := p.FilteredValues()
	if len(values) == 0 {
		p.sel = 0
		return
	}
	for i, v := range values {
		if v == selected {
			p.sel = i
			return
		}
	}
	p.sel = clampIndex(p.sel, len(values))
}

// valueDomain reports the domain in play: the values for the typed command
// name, once the space after it has been typed. Its second result is the
// value-mode predicate — false means the overlay is the ordinary command menu.
func (p *Palette) valueDomain() ([]string, bool) {
	if !p.hasArg || len(p.domains) == 0 {
		return nil, false
	}
	values, ok := p.domains[p.query]
	if !ok || len(values) == 0 {
		return nil, false
	}
	return values, true
}

// FilteredValues is the current command's value domain under the partial
// argument typed so far: case-insensitive PREFIX matches first, then substring
// matches, each keeping the server's order (which puts the current model first).
// Prefix-before-substring is what makes typing the start of a name behave like
// completion while still finding "sonnet" inside "claude-sonnet-4".
//
// It returns nil whenever the palette is not in value mode, which is the same
// as "this argument is free text".
func (p *Palette) FilteredValues() []string {
	domain, ok := p.valueDomain()
	if !ok {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(p.arg))
	if q == "" {
		out := make([]string, len(domain))
		copy(out, domain)
		return out
	}
	var prefix, contains []string
	for _, v := range domain {
		lower := strings.ToLower(v)
		switch {
		case strings.HasPrefix(lower, q):
			prefix = append(prefix, v)
		case strings.Contains(lower, q):
			contains = append(contains, v)
		}
	}
	return append(prefix, contains...)
}

// SelectedValue is the highlighted domain value. It reports false when the
// palette is closed, when the command has no domain, or when the partial
// matched nothing — the states in which there is no value to complete and the
// typed text must be left exactly as it is.
func (p *Palette) SelectedValue() (string, bool) {
	if !p.open {
		return "", false
	}
	values := p.FilteredValues()
	if len(values) == 0 {
		return "", false
	}
	return values[clampIndex(p.sel, len(values))], true
}

// CompleteValueText is Tab's payload in value mode: the whole buffer rebuilt as
// the command plus the selected value.
//
// It carries NO trailing space, unlike CompleteText. A value is a complete
// argument, so the cursor belongs right behind it where Enter sends the line —
// and, because the buffer is still "name value", a second Tab re-selects the
// same value instead of falling back to the command completion and wiping the
// argument that was just chosen.
func (p *Palette) CompleteValueText() (string, bool) {
	value, ok := p.SelectedValue()
	if !ok {
		return "", false
	}
	return "/" + p.query + " " + value, true
}

// argument splits the argument half off a buffer: the text after the first
// space, and whether that space was typed at all.
//
// It reports false once a SECOND space appears, because the value domain
// belongs to the FIRST argument: "/mission reviewer audit the parser" is past
// the agent name, and the objective that follows is prose the palette has
// nothing to say about. That is also the rule that will let /mission's
// agent-name domain ride this seam unchanged.
func argument(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t")
	s = strings.TrimPrefix(s, "/")
	i := strings.IndexAny(s, " \t\n")
	if i < 0 {
		return "", false
	}
	rest := s[i+1:]
	if strings.ContainsAny(rest, " \t\n") {
		return "", false
	}
	return rest, true
}

// IsOpen reports whether the overlay is showing, which is also whether the
// app should route Up/Down/Tab to the palette instead of the composer.
func (p *Palette) IsOpen() bool { return p.open }

// Move walks the selection by delta rows, clamped to whatever the overlay is
// currently listing — commands, or a command's values. It does not wrap: a held
// Down key should come to rest at the last row, not cycle past it.
func (p *Palette) Move(delta int) {
	n := len(p.Filtered())
	if values := p.FilteredValues(); values != nil {
		n = len(values)
	}
	if n == 0 {
		p.sel = 0
		return
	}
	p.sel = clampIndex(p.sel+delta, n)
}

// Selected returns the highlighted entry. It reports false when the palette
// is closed or the filter matched nothing — the states in which Enter must
// fall through to an ordinary submission.
func (p *Palette) Selected() (Entry, bool) {
	if !p.open {
		return Entry{}, false
	}
	ents := p.Filtered()
	if len(ents) == 0 {
		return Entry{}, false
	}
	return ents[clampIndex(p.sel, len(ents))], true
}

// CompleteText is Tab's payload: the selected command's name plus one
// trailing space, ready to replace the buffer. Tab completes, it never
// submits — the space is what leaves the cursor sitting where an argument
// goes (item 4).
//
// In value mode it is CompleteValueText instead, so one key completes the
// command and then its argument without the caller having to know which half
// the operator is in. It then reports false rather than falling back when the
// partial matches nothing: completing "/model " over a "/model zzz" the
// operator is still typing would delete what they wrote, and Tab may not lose
// text.
func (p *Palette) CompleteText() (string, bool) {
	if _, ok := p.valueDomain(); ok {
		return p.CompleteValueText()
	}
	e, ok := p.Selected()
	if !ok {
		return "", false
	}
	return "/" + e.Name + " ", true
}

// Lookup resolves a bare or slash-prefixed name against the merged set,
// locals shadowing remotes. It is the dispatch split of item 7 in one call:
// found-and-Local goes to the in-process handler, found-and-remote is sent
// verbatim, and not-found is prompt text.
func (p *Palette) Lookup(name string) (Entry, bool) {
	n := normalize(name)
	if n == "" {
		return Entry{}, false
	}
	if e, ok := p.locals[n]; ok {
		return e, true
	}
	for _, c := range p.remote {
		if normalize(c.Name) == n {
			return remoteEntry(c), true
		}
	}
	return Entry{}, false
}

// ArgHint is the static argument-hint line (item 5): once a name and a
// space are typed and the name resolves, the AvailableCommandInput.Hint
// comes back VERBATIM. beam does not paraphrase a hint the agent wrote —
// the hint is the agent's own documentation of its argument.
//
// It reads the buffer directly rather than the selection, because the hint
// must survive the palette closing once the operator moves on to typing
// arguments. A command with no hint, or an unknown name, reports false.
//
// When the command has a value domain the line also states its SIZE — "(12
// values)" — because the rows above it are a filtered window and the operator
// otherwise cannot tell whether the runtime knows twelve models or two. It is
// the one number that says how much the list is hiding. A command with a domain
// but no hint gets the count alone; a command with a hint but no domain gets
// the hint alone, byte-identical to before.
func (p *Palette) ArgHint(buffer string) (string, bool) {
	s := strings.TrimLeft(buffer, " \t")
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	head, _, spaced := strings.Cut(s, " ")
	if !spaced {
		return "", false
	}
	e, ok := p.Lookup(head)
	if !ok {
		return "", false
	}
	size := ""
	if n := len(p.domains[normalize(head)]); n > 0 {
		size = fmt.Sprintf("(%d values)", n)
	}
	switch {
	case e.Hint != "" && size != "":
		return e.Hint + " " + size, true
	case size != "":
		return size, true
	case e.Hint != "":
		return e.Hint, true
	}
	return "", false
}

// Filtered is the merged set under the current query: locals shadowing
// same-named remotes, case-insensitive prefix match, alphabetical. A bare
// `/` (empty query) is the full set (item 3).
func (p *Palette) Filtered() []Entry {
	merged := make(map[string]Entry, len(p.remote)+len(p.locals))
	for _, c := range p.remote {
		n := normalize(c.Name)
		if n == "" {
			continue
		}
		merged[n] = remoteEntry(c)
	}
	// Locals last: shadowing is a deliberate override, not a collision.
	for n, e := range p.locals {
		merged[n] = e
	}

	q := strings.ToLower(p.query)
	out := make([]Entry, 0, len(merged))
	for n, e := range merged {
		if q == "" || strings.HasPrefix(strings.ToLower(n), q) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Render draws the overlay: one row per matching command, bounded to
// maxRows with a "+N more" footer, or a single explanatory row when nothing
// matched. A closed palette, a non-positive width, or a non-positive
// maxRows renders nothing, so the caller can hand through its live-region
// budget unguarded.
//
// maxRows is the TOTAL line budget, footer included — the same contract
// comp/picker has, because both are overlays the app-shell hands a slice of
// the live region and neither can be allowed to spend one more row than it
// was given. This used to promise maxRows COMMAND rows and then add the
// footer on top, so a caller reserving four lines could get five and push the
// composer off the bottom of the terminal. Two overlays with two different
// answers to "what does maxRows mean" is a bug waiting for whichever one the
// caller guessed wrong about.
//
// The footer reports BOTH directions — "↑N above" when the window has
// scrolled past the top, "+N more" when commands remain below it, and both
// when the window is in the middle of a long set. It used to count only what
// was below and then hand its line back to a command once the window reached
// the end, which is precisely the moment the operator most needs to be told
// the list continues upward: at the bottom of a scrolled menu the rows above
// are the ONLY hidden ones, and nothing on screen said they existed. So the
// footer now stands whenever anything at all is hidden, and it is indented to
// the same column as the command names so it reads as a note about the list
// rather than as another row of it.
//
// No row exceeds width — descriptions are caller data of unbounded length and
// are truncated rune-safely with an ellipsis.
func (p *Palette) Render(width, maxRows int, ascii bool) []frame.Line {
	if !p.open || width <= 0 || maxRows <= 0 {
		return nil
	}

	// In value mode the rows are the command's values, not the command set:
	// once `/model ` is typed the useful list is the models, and the command
	// itself is already spelled out in the buffer above.
	ents := p.Filtered()
	rowOf := func(i int, selected bool) frame.Line { return row(ents[i], selected, ascii) }
	n := len(ents)
	empty := emptyText(ascii)
	if _, inValueMode := p.valueDomain(); inValueMode {
		matched := p.FilteredValues()
		rowOf = func(i int, selected bool) frame.Line { return valueRow(matched[i], selected, ascii) }
		n = len(matched)
		empty = emptyValueText(ascii)
	}
	if n == 0 {
		return []frame.Line{clamp(frame.Styled(frame.StyleMuted, empty), width, ascii)}
	}

	rows := maxRows
	footer := false
	if n > maxRows {
		footer = true
		rows = maxRows - 1
		if rows < 1 {
			// A footer with no rows above it says nothing useful; spend the
			// single available line on the selected command instead.
			rows = 1
			footer = false
		}
	}
	if rows > n {
		rows = n
	}

	sel := clampIndex(p.sel, n)
	// Scroll the window so the selection is always drawn; without this a
	// long menu would highlight a row nobody can see.
	start := 0
	if sel >= rows {
		start = sel - rows + 1
	}
	if max := n - rows; start > max {
		start = max
	}
	if start < 0 {
		start = 0
	}

	lines := make([]frame.Line, 0, rows+1)
	for i := start; i < start+rows; i++ {
		lines = append(lines, clamp(rowOf(i, i == sel), width, ascii))
	}
	if footer {
		text := footerText(start, n-(start+rows), ascii)
		lines = append(lines, clamp(frame.Styled(frame.StyleMuted, text), width, ascii))
	}
	return lines
}

// footerText is the hidden-rows note: what the window has scrolled past, what
// it has not reached yet, or both. It carries the same indent an unselected
// row does, so the note lines up under the names it is counting.
func footerText(above, below int, ascii bool) string {
	up := upMarker(ascii)
	var parts []string
	if above > 0 {
		parts = append(parts, fmt.Sprintf("%s%d above", up, above))
	}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", below))
	}
	return indent + strings.Join(parts, "  ")
}

func upMarker(ascii bool) string {
	if ascii {
		return upASCII
	}
	return upUnicode
}

// row is one command line: the active-picker marker or its two-space
// stand-in, the slash-prefixed name, and the description.
func row(e Entry, selected bool, ascii bool) frame.Line {
	var l frame.Line
	if selected {
		l = frame.Line{
			frame.S(frame.StyleBrand, marker(ascii)),
			frame.S(frame.StyleNone, " "),
			frame.S(frame.StyleActive, "/"+e.Name),
		}
	} else {
		l = frame.Line{frame.S(frame.StyleNone, indent+"/"+e.Name)}
	}
	if e.Description != "" {
		l = append(l, frame.S(frame.StyleMuted, nameGap+e.Description))
	}
	return l
}

// valueRow is one argument-value line. It carries no slash and no description:
// the value is what will be typed into the buffer verbatim, and showing it as
// anything else would misrepresent what Tab is about to write.
func valueRow(value string, selected bool, ascii bool) frame.Line {
	if selected {
		return frame.Line{
			frame.S(frame.StyleBrand, marker(ascii)),
			frame.S(frame.StyleNone, " "),
			frame.S(frame.StyleActive, value),
		}
	}
	return frame.Line{frame.S(frame.StyleNone, indent+value)}
}

func remoteEntry(c libacp.AvailableCommand) Entry {
	e := Entry{Name: normalize(c.Name), Description: c.Description}
	if c.Input != nil {
		e.Hint = c.Input.Hint
	}
	return e
}

// normalize reduces a name or query to the bare comparison form: no
// surrounding space, no leading `/`, nothing past the first space. Both
// halves of the set — wire names like "help" and registered names like
// "/help" — land on the same key.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}

func clampIndex(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func marker(ascii bool) string {
	if ascii {
		return markerASCII
	}
	return markerUnicode
}

func emptyText(ascii bool) string {
	if ascii {
		return emptyASCII
	}
	return emptyUnicode
}

func emptyValueText(ascii bool) string {
	if ascii {
		return emptyValueASCII
	}
	return emptyValueUnicode
}

func ellipsis(ascii bool) string {
	if ascii {
		return ellipsisASCII
	}
	return ellipsisUnicode
}

// clamp cuts l to at most width cells, rune-safely and span-wise, marking
// the cut with an ellipsis when one fits.
func clamp(l frame.Line, width int, ascii bool) frame.Line {
	if width <= 0 {
		return frame.Line{}
	}
	if textwidth.Width(l.Text()) <= width {
		return l
	}

	tail := ellipsis(ascii)
	if textwidth.Width(tail) > width {
		tail = ""
	}
	budget := width - textwidth.Width(tail)

	out := make(frame.Line, 0, len(l)+1)
	used := 0
	for _, s := range l {
		w := textwidth.Width(s.Text)
		if used+w <= budget {
			out = append(out, s)
			used += w
			continue
		}
		if rem := budget - used; rem > 0 {
			out = append(out, frame.S(s.Style, textwidth.Truncate(s.Text, rem, "")))
		}
		break
	}
	if tail != "" {
		out = append(out, frame.S(frame.StyleMuted, tail))
	}
	return out
}
