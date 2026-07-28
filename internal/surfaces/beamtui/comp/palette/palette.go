// Package palette owns slash-command discovery for beam's composer: the
// merged remote+local command set, the case-insensitive `/` prefix filter,
// and the overlay rows drawn in the live region. It is a lookup table and
// renderer only, never a gate — an unrecognized token is plain prompt text,
// and dispatch is the app shell's job. A Palette belongs to one goroutine.
package palette

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/contenox/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/contenox/libacp"
)

// ASCIIMarker is the selection marker on a Mono terminal, exported so
// testkit's glyph-parity test can check it against style's GlyphSet.
const ASCIIMarker = "|"

// EmptyInputHint is the composer's empty-buffer affordance: the palette
// owns this one sentence; the composer decides when to draw it.
const EmptyInputHint = "type / for commands"

const (
	// markerUnicode marks the active row; ASCII degrades to a pipe.
	markerUnicode = "▌"
	markerASCII   = ASCIIMarker

	// States the consequence, not a failure: the line still sends as chat.
	emptyUnicode = "no matching command — Enter sends as chat"
	emptyASCII   = "no matching command - Enter sends as chat"

	// Same, for an over-filtered value list: an unadvertised value is still
	// a legal argument.
	emptyValueUnicode = "no matching value — Enter sends it anyway"
	emptyValueASCII   = "no matching value - Enter sends it anyway"

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."

	// Marks "scrolled past"; the same glyph comp/composer uses.
	upUnicode = "↑"
	upASCII   = "^"

	// nameGap separates a name from its description; indent stands in for
	// the marker on unselected rows.
	nameGap = "  "
	indent  = "  "
)

// ErrDuplicateLocal reports two local registrations claiming one name;
// fails fast rather than last-write-wins.
var ErrDuplicateLocal = errors.New("palette: local command already registered")

// ErrInvalidName reports a registration whose name is empty or contains a
// space — a spaced name could never be typed.
var ErrInvalidName = errors.New("palette: invalid command name")

// Entry is one command in the merged set; Name is bare (no leading `/`).
// Local true means an in-process handler answers it and nothing is sent.
type Entry struct {
	Name        string
	Description string
	Hint        string
	Local       bool
}

// Palette is the command menu behind the composer's `/` trigger. The zero
// value is not usable; call New.
type Palette struct {
	remote []libacp.AvailableCommand
	locals map[string]Entry

	// domains holds each command's accepted argument values (SetValueDomains).
	domains map[string][]string

	open  bool
	query string
	// arg is the first argument typed so far; hasArg is whether the space
	// after the command name was typed — together, the value-mode trigger.
	arg    string
	hasArg bool
	sel    int
}

// New returns an empty palette. Nothing is pre-registered — the app owns
// which locals exist.
func New() *Palette {
	return &Palette{locals: make(map[string]Entry)}
}

// SetRemote replaces the agent-advertised half of the set with the latest
// CommandsUpdated report — never merges, so a rebound session's dropped
// commands actually disappear. Strings are sanitized before reaching the
// overlay; the selection follows its command by name, clamping if it's gone.
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

// selectedName is the highlighted entry's name, open or not, so SetRemote
// can restore the selection when the overlay reopens.
func (p *Palette) selectedName() string {
	ents := p.Filtered()
	if len(ents) == 0 {
		return ""
	}
	return ents[clampIndex(p.sel, len(ents))].Name
}

// RegisterLocal adds a TUI-local command, shadowing same-named remotes. A
// second local on one name returns ErrDuplicateLocal; a leading `/` on name
// is accepted and stripped.
func (p *Palette) RegisterLocal(name, description, hint string) error {
	// Sanitize before validating: a name must not change shape after the check.
	name = sanitize.Line(name)

	// Validate before normalizing: "two words" should error, not silently
	// become "two".
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

// MustRegisterLocal is RegisterLocal for startup wiring; it panics on a
// collision, since a duplicate there is a programming error.
func (p *Palette) MustRegisterLocal(name, description, hint string) {
	if err := p.RegisterLocal(name, description, hint); err != nil {
		panic(err)
	}
}

// Open shows the palette filtered by query when a leading `/` fires the
// trigger rule on the trimmed buffer. query may be the raw buffer: text up
// to the first space is the command filter, the rest is the argument value
// mode reads (SetValueDomains); a bare command token never reaches it.
func (p *Palette) Open(query string) {
	p.open = true
	p.SetQuery(query)
}

// SetQuery updates the filter and resets the selection to the top; it does
// not open or close the palette. Text past the command name can switch a
// command with a known value domain into that domain (SetValueDomains).
func (p *Palette) SetQuery(q string) {
	p.query = normalize(q)
	p.arg, p.hasArg = argument(q)
	p.sel = 0
}

// Close hides the palette without touching the buffer: Esc closes, the
// typed text stays.
func (p *Palette) Close() {
	p.open = false
	p.query = ""
	p.arg = ""
	p.hasArg = false
	p.sel = 0
}

// SetValueDomains replaces the argument-value domains — the values each
// command accepts for its first argument — keyed by bare command name,
// replacing rather than merging like SetRemote. A name with no values has no
// value mode; values are sanitized and the selection follows by name.
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

// valueDomain reports the values for the typed command name once the space
// after it is typed; false means the ordinary command menu is showing.
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
// argument typed so far: prefix matches first, then substring matches, each
// in the server's order. Returns nil when not in value mode.
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

// SelectedValue is the highlighted domain value; false when the palette is
// closed, the command has no domain, or the partial matched nothing.
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

// CompleteValueText is Tab's payload in value mode: the buffer rebuilt as
// the command plus the selected value, with no trailing space, so a second
// Tab re-selects the same value instead of wiping it.
func (p *Palette) CompleteValueText() (string, bool) {
	value, ok := p.SelectedValue()
	if !ok {
		return "", false
	}
	return "/" + p.query + " " + value, true
}

// argument splits the argument half off a buffer: text after the first
// space, and whether that space was typed. Reports false past a second
// space — the value domain belongs to the first argument only.
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

// IsOpen reports whether the overlay is showing, i.e. whether the app
// should route Up/Down/Tab to the palette instead of the composer.
func (p *Palette) IsOpen() bool { return p.open }

// Move walks the selection by delta rows, clamped to whatever the overlay is
// currently listing (commands or values). It does not wrap.
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

// Selected returns the highlighted entry; false when the palette is closed
// or the filter matched nothing.
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

// CompleteText is Tab's payload: the selected command's name plus a
// trailing space — completes, never submits. Delegates to CompleteValueText
// in value mode, and reports false on an unmatched partial rather than fall
// back, since Tab must never delete text the operator already typed.
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

// Lookup resolves a bare or slash-prefixed name, locals shadowing remotes:
// found-and-Local is in-process, found-and-remote is sent verbatim,
// not-found is prompt text.
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

// ArgHint is the static argument-hint line: once a name and a space are
// typed and the name resolves, AvailableCommandInput.Hint comes back
// verbatim, read from the buffer directly so it survives the palette
// closing. A command with a value domain also gets its size appended.
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

// Filtered is the merged set under the current query: locals shadow
// same-named remotes, case-insensitive prefix match, alphabetical order.
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
// maxRows with a "+N more" footer, or one row when nothing matched. Renders
// nothing when closed, or width/maxRows is non-positive.
//
// maxRows is the total budget, footer included (comp/picker's contract).
// The footer reports both directions and shows whenever rows are hidden.
func (p *Palette) Render(width, maxRows int, ascii bool) []frame.Line {
	if !p.open || width <= 0 || maxRows <= 0 {
		return nil
	}

	// In value mode the rows are the command's values, not the command set:
	// the command name is already spelled out in the buffer above.
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
			// A footer with nothing above it says nothing useful; spend the
			// one available line on the selected command instead.
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

// footerText is the hidden-rows note: what the window scrolled past, what it
// has not reached yet, or both — indented to line up under the row names.
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

// valueRow is one argument-value line: no slash, no description — the value
// is exactly what Tab will write into the buffer.
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
// surrounding space, no leading `/`, nothing past the first space.
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
