package hitlservice

import (
	"context"
	"path"
	"runtime"
	"strings"
	"sync/atomic"

	"mvdan.cc/sh/v3/syntax"
)

// ShellKind names the shell that will interpret a gated command line; it
// mirrors localtools.ShellKind by value rather than import to avoid a
// dependency cycle.
type ShellKind string

const (
	// ShellKindPOSIX is the only kind structural analysis runs on: `sh -c`.
	ShellKindPOSIX ShellKind = "sh"
	// ShellKindPowerShell is what local_shell spawns on Windows; mvdan
	// cannot parse it, so it never reaches the parser.
	ShellKindPowerShell ShellKind = "powershell"
	// ShellKindCmd is cmd.exe — same treatment as powershell.
	ShellKindCmd ShellKind = "cmd"
	// ShellKindUnknown is any kind this package does not recognize; distinct
	// from "" so an unrecognized kind fails closed.
	ShellKindUnknown ShellKind = "unknown"
)

type shellKindContextKey struct{}

// WithShellKind marks ctx with the shell that will interpret command lines
// evaluated under it, enabling structural analysis when the shell is POSIX.
func WithShellKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, shellKindContextKey{}, normalizeShellKind(kind))
}

// ShellKindFromContext returns the trusted shell kind set by WithShellKind,
// or "" when none was set.
func ShellKindFromContext(ctx context.Context) ShellKind {
	kind, _ := ctx.Value(shellKindContextKey{}).(ShellKind)
	return kind
}

func normalizeShellKind(s string) ShellKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "sh", "bash", "dash", "ash", "posix":
		// bash and dash parse the same cleared node set as POSIX.
		return ShellKindPOSIX
	case "powershell", "pwsh":
		return ShellKindPowerShell
	case "cmd", "cmd.exe":
		return ShellKindCmd
	default:
		return ShellKindUnknown
	}
}

func structuralShellEnabled(trusted ShellKind, goos string) bool {
	switch normalizeShellKind(string(trusted)) {
	case ShellKindPOSIX:
		return true
	case "":
	default:
		return false
	}
	// On non-Windows, local_shell's shell detection can only produce sh.
	return goos != "windows"
}

var clearedAssignmentNames = map[string]bool{}

var unclearedCommandNames = map[string]bool{
	// re-entry
	"eval": true, "exec": true, "source": true, ".": true,
	"sh": true, "bash": true, "dash": true, "ash": true, "ksh": true, "zsh": true, "fish": true,
	"env": true, "command": true, "builtin": true, "xargs": true,
	"nohup": true, "setsid": true, "timeout": true, "watch": true, "time": true,
	"sudo": true, "doas": true, "su": true, "find": true,
	// environment mutation
	"export": true, "set": true, "unset": true, "readonly": true, "local": true,
	"alias": true, "unalias": true, "declare": true, "typeset": true, "trap": true,
	"shift": true, "getopts": true, "ulimit": true, "umask": true,
}

type shellReading struct {
	analyzed     bool
	parsed       bool
	commands     []shellCommandView
	redirects    []shellRedirect
	hasCmdSubst  bool
	hasProcSubst bool
	hasArithmExp bool
	upgradable   bool
}

type shellCommandView struct {
	name     string
	base     string
	words    []string
	literal  bool
	argCount int
	assigns  []string
	display  string
}

type shellRedirect struct {
	op      string
	target  string
	heredoc bool
}

var structuralParses atomic.Int64

func analyzeShellArgs(trusted ShellKind, args map[string]any) shellReading {
	var r shellReading
	if len(args) == 0 {
		return r
	}
	if !structuralShellEnabled(trusted, runtime.GOOS) {
		return r
	}
	src, ok := shellLineFromArgs(args)
	if !ok {
		return r
	}
	r.analyzed = true
	f, bashOnly, ok := parseShellLine(src)
	if !ok {
		return r
	}
	r.parsed = true
	collectShellNodes(f, &r, 0)
	// bashOnly forces upgradable off: sh -c is the executor, so a reading sh itself would reject cannot ground an allow.
	r.upgradable = !bashOnly && fileClearedForUpgrade(f)
	return r
}

const maxShellLineBytes = 64 * 1024

func shellLineFromArgs(args map[string]any) (string, bool) {
	cmd, _ := args["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", false
	}
	extra := argTokens(args["args"])
	if shellModeRequested(args) {
		line := cmd
		if len(extra) > 0 {
			line += " " + strings.Join(extra, " ")
		}
		return boundedLine(line)
	}
	if len(extra) > 0 {
		return "", false
	}
	return boundedLine(cmd)
}

func boundedLine(line string) (string, bool) {
	if len(line) > maxShellLineBytes {
		return "", false
	}
	return line, true
}

func parseShellLine(src string) (f *syntax.File, bashOnly bool, ok bool) {
	structuralParses.Add(1)
	f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(src), "")
	if err == nil && f != nil {
		return f, false, true
	}
	structuralParses.Add(1)
	f, err = syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil || f == nil {
		return nil, false, false
	}
	return f, true, true
}

func collectShellNodes(f *syntax.File, r *shellReading, depth int) {
	assigns := literalAssignments(f)
	syntax.Walk(f, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			for _, rd := range n.Redirs {
				r.redirects = append(r.redirects, redirectView(rd))
			}
			switch cmd := n.Cmd.(type) {
			case *syntax.CallExpr:
				if view, ok := commandView(cmd); ok {
					r.commands = append(r.commands, view)
				}
				peelWrapper(lenientWords(cmd), r, depth)
				if words, ok := resolveAssignedWords(cmd, assigns); ok {
					revealWords(words, r, depth)
				}
			case *syntax.BinaryCmd:
				if cmd.Op == syntax.Pipe {
					revealPipedPayload(cmd, r, depth)
				}
			}
		case *syntax.CmdSubst:
			r.hasCmdSubst = true
		case *syntax.ProcSubst:
			r.hasProcSubst = true
		case *syntax.ArithmExp:
			r.hasArithmExp = true
		}
		return true
	})
}

const (
	maxRevealDepth      = 4
	maxRevealedCommands = 256
)

var nestedShellNames = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ash": true,
	"ksh": true, "zsh": true, "fish": true,
}

func lenientWords(call *syntax.CallExpr) []string {
	out := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		v, ok := wordName(w)
		if !ok {
			v = ""
		}
		out = append(out, v)
	}
	return out
}

func revealWords(words []string, r *shellReading, depth int) {
	if depth > maxRevealDepth || len(r.commands) >= maxRevealedCommands {
		return
	}
	if len(words) == 0 || words[0] == "" {
		return
	}
	r.commands = append(r.commands, viewFromWords(words))
	peelWrapper(words, r, depth)
}

func peelWrapper(words []string, r *shellReading, depth int) {
	if depth >= maxRevealDepth || len(words) == 0 || words[0] == "" {
		return
	}
	base := path.Base(words[0])
	switch {
	case nestedShellNames[base]:
		if payload, ok := dashCPayload(words); ok {
			revealSource(payload, r, depth+1)
		}
	case base == "xargs":
		if rest, ok := xargsCommandWords(words); ok {
			revealWords(rest, r, depth+1)
		}
	case base == "eval":
		if src, ok := joinKnown(words[1:]); ok {
			revealSource(src, r, depth+1)
		}
	}
}

func revealSource(src string, r *shellReading, depth int) {
	if depth > maxRevealDepth || len(r.commands) >= maxRevealedCommands {
		return
	}
	src = strings.TrimSpace(src)
	if src == "" || len(src) > maxShellLineBytes {
		return
	}
	f, _, ok := parseShellLine(src)
	if !ok {
		return
	}
	collectShellNodes(f, r, depth)
}

func viewFromWords(words []string) shellCommandView {
	out := shellCommandView{
		name:     words[0],
		base:     path.Base(words[0]),
		argCount: len(words),
		display:  displayWords(words),
	}
	for _, w := range words {
		if w == "" {
			return out
		}
	}
	out.words = append([]string(nil), words...)
	out.literal = true
	return out
}

func dashCPayload(words []string) (string, bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "" || w == "-" || w == "--" || !strings.HasPrefix(w, "-") {
			return "", false
		}
		if strings.ContainsRune(w[1:], 'c') {
			if i+1 < len(words) && words[i+1] != "" {
				return words[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

var xargsValueOptions = map[string]bool{
	"-a": true, "-d": true, "-E": true, "-I": true, "-L": true,
	"-n": true, "-P": true, "-s": true,
	"--arg-file": true, "--delimiter": true, "--eof": true, "--replace": true,
	"--max-lines": true, "--max-args": true, "--max-procs": true,
	"--max-chars": true, "--process-slot-var": true,
}

func xargsCommandWords(words []string) ([]string, bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "" {
			return nil, false
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return words[i:], true
		}
		if w == "--" {
			if i+1 < len(words) {
				return words[i+1:], true
			}
			return nil, false
		}
		if xargsValueOptions[w] {
			i++
		}
	}
	return nil, false
}

func revealPipedPayload(pipe *syntax.BinaryCmd, r *shellReading, depth int) {
	if depth >= maxRevealDepth {
		return
	}
	producer, ok := stmtCallWords(pipe.X)
	if !ok || !consumerEvaluatesStdin(pipe.Y) {
		return
	}
	for _, src := range printedPayloads(producer) {
		revealSource(src, r, depth+1)
	}
}

func stmtCallWords(st *syntax.Stmt) ([]string, bool) {
	if st == nil {
		return nil, false
	}
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, false
	}
	return lenientWords(call), true
}

func consumerEvaluatesStdin(st *syntax.Stmt) bool {
	words, ok := stmtCallWords(st)
	if !ok || words[0] == "" {
		return false
	}
	base := path.Base(words[0])
	return nestedShellNames[base] || base == "eval" || base == "source" || base == "."
}

func printedPayloads(words []string) []string {
	base := path.Base(words[0])
	if base != "echo" && base != "printf" {
		return nil
	}
	rest := words[1:]
	if base == "echo" {
		for len(rest) > 0 && isEchoFlag(rest[0]) {
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return nil
	}
	raw, ok := joinKnown(rest)
	if !ok {
		return nil
	}
	out := []string{raw}
	if decoded := decodeShellEscapes(raw); decoded != raw {
		out = append(out, decoded)
	}
	return out
}

func isEchoFlag(w string) bool {
	switch w {
	case "-n", "-e", "-E", "-ne", "-en", "-nE", "-En":
		return true
	}
	return false
}

func joinKnown(words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	for _, w := range words {
		if w == "" {
			return "", false
		}
	}
	return strings.Join(words, " "), true
}

func decodeShellEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case '\\':
			sb.WriteByte('\\')
		case 'a':
			sb.WriteByte(0x07)
		case 'b':
			sb.WriteByte(0x08)
		case 'e':
			sb.WriteByte(0x1b)
		case 'f':
			sb.WriteByte(0x0c)
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'v':
			sb.WriteByte(0x0b)
		case 'x':
			if n, width, ok := parseEscapedNumber(s[i+1:], 16, 2); ok {
				sb.WriteByte(n)
				i += width
				continue
			}
			sb.WriteString(`\x`)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			start := i
			if c == '0' {
				start = i + 1 // printf's \0NNN form
			}
			if n, width, ok := parseEscapedNumber(s[start:], 8, 3); ok {
				sb.WriteByte(n)
				i = start + width - 1
				continue
			}
			sb.WriteByte(c)
		default:
			sb.WriteByte('\\')
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func parseEscapedNumber(s string, base, maxDigits int) (value byte, width int, ok bool) {
	n := 0
	for width < len(s) && width < maxDigits {
		d := digitValue(s[width])
		if d < 0 || d >= base {
			break
		}
		next := n*base + d
		if next > 0xff {
			break
		}
		n = next
		width++
	}
	if width == 0 {
		return 0, 0, false
	}
	return byte(n), width, true
}

func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}

func literalAssignments(f *syntax.File) map[string]string {
	out := map[string]string{}
	syntax.Walk(f, func(node syntax.Node) bool {
		as, ok := node.(*syntax.Assign)
		if !ok || as.Append || as.Naked || as.Name == nil || as.Index != nil || as.Value == nil {
			return true
		}
		if v, ok := wordName(as.Value); ok && v != "" {
			out[as.Name.Value] = v
		}
		return true
	})
	return out
}

func resolveAssignedWords(call *syntax.CallExpr, assigns map[string]string) ([]string, bool) {
	if len(assigns) == 0 || len(call.Args) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(call.Args))
	resolved := false
	for _, w := range call.Args {
		if v, ok := wordName(w); ok {
			out = append(out, v)
			continue
		}
		if name, ok := soleParamName(w); ok {
			if v, ok := assigns[name]; ok {
				out = append(out, v)
				resolved = true
				continue
			}
		}
		out = append(out, "")
	}
	if !resolved || out[0] == "" {
		return nil, false
	}
	return out, true
}

func soleParamName(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) != 1 {
		return "", false
	}
	pe, ok := w.Parts[0].(*syntax.ParamExp)
	if !ok || pe.Param == nil {
		return "", false
	}
	if pe.Excl || pe.Length || pe.Width || pe.IsSet ||
		pe.Index != nil || pe.Slice != nil || pe.Repl != nil || pe.Exp != nil ||
		len(pe.Modifiers) > 0 {
		return "", false
	}
	return pe.Param.Value, true
}

func redirectView(rd *syntax.Redirect) shellRedirect {
	out := shellRedirect{op: rd.Op.String(), heredoc: rd.Hdoc != nil}
	if rd.Word != nil {
		if target, ok := wordName(rd.Word); ok {
			out.target = target
		} else {
			out.target = "(not statically knowable)"
		}
	}
	return out
}

func commandView(call *syntax.CallExpr) (shellCommandView, bool) {
	var out shellCommandView
	for _, as := range call.Assigns {
		name := ""
		if as.Name != nil {
			name = as.Name.Value
		}
		out.assigns = append(out.assigns, name)
	}
	if len(call.Args) == 0 {
		return out, false
	}
	out.argCount = len(call.Args)
	if name, ok := wordName(call.Args[0]); ok && name != "" {
		out.name = name
		out.base = path.Base(name)
	}
	literal := true
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		lit, ok := wordLiteral(w)
		if !ok {
			literal = false
			break
		}
		words = append(words, lit)
	}
	if literal {
		out.words = words
		out.literal = true
	}
	out.display = displayOf(call)
	return out, true
}

const maxDisplayRunes = 60

func displayOf(call *syntax.CallExpr) string {
	parts := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		if v, ok := wordName(w); ok {
			parts = append(parts, v)
			continue
		}
		parts = append(parts, "…")
	}
	return flattenDisplay(parts)
}

func displayWords(words []string) string {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		if w == "" {
			w = "…"
		}
		parts = append(parts, w)
	}
	return flattenDisplay(parts)
}

func flattenDisplay(parts []string) string {
	// Control characters are flattened so a decision message cannot forge a log line or terminal escape.
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(parts, " "))
	if r := []rune(s); len(r) > maxDisplayRunes {
		s = string(r[:maxDisplayRunes-1]) + "…"
	}
	return s
}

const wordUnsafeMeta = "*?[]{}~\\$"

func wordLiteral(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) == 0 {
		return "", false
	}
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, wordUnsafeMeta) {
				return "", false
			}
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok || strings.Contains(lit.Value, "\\") {
					return "", false
				}
				sb.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return sb.String(), true
}

func wordName(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) == 0 {
		return "", false
	}
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(unescapeUnquoted(p.Value))
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				sb.WriteString(unescapeDoubleQuoted(lit.Value))
			}
		default:
			return "", false
		}
	}
	return sb.String(), true
}

func unescapeUnquoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			sb.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			break // trailing backslash: line continuation
		}
		i++
		if s[i] == '\n' {
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func unescapeDoubleQuoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$', '`', '"', '\\':
			i++
			sb.WriteByte(s[i])
		case '\n':
			i++
		default:
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
}

func fileClearedForUpgrade(f *syntax.File) bool {
	if f == nil || len(f.Stmts) != 1 {
		return false
	}
	return stmtClearedForUpgrade(f.Stmts[0])
}

func stmtClearedForUpgrade(st *syntax.Stmt) bool {
	if st == nil || st.Cmd == nil {
		return false
	}
	// `cmd &` detaches from the gating approval; `! cmd` inverts && status; |&/&| are shell-specific.
	if st.Negated || st.Background || st.Coprocess || st.Disown {
		return false
	}
	// A redirect is not an argument: an allowlisted reader with one is a writer.
	if len(st.Redirs) > 0 {
		return false
	}
	switch cmd := st.Cmd.(type) {
	case *syntax.CallExpr:
		return callClearedForUpgrade(cmd)
	case *syntax.BinaryCmd:
		switch cmd.Op {
		case syntax.AndStmt, syntax.OrStmt, syntax.Pipe:
			// Flow-insensitive on purpose: `a || b` prices b even though it may never run.
			return stmtClearedForUpgrade(cmd.X) && stmtClearedForUpgrade(cmd.Y)
		}
		return false
	default:
		return false
	}
}

func callClearedForUpgrade(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	// Assignment prefixes are a hijack channel, not decoration.
	for _, as := range call.Assigns {
		if as.Name == nil || !clearedAssignmentNames[as.Name.Value] {
			return false
		}
	}
	for _, w := range call.Args {
		if _, ok := wordLiteral(w); !ok {
			return false
		}
	}
	name, ok := wordLiteral(call.Args[0])
	if !ok || name == "" {
		return false
	}
	return !unclearedCommandNames[path.Base(name)]
}

func structuralCommandInList(r shellReading, list string) (shellCommandView, bool) {
	if !r.parsed || strings.TrimSpace(list) == "" {
		return shellCommandView{}, false
	}
	for _, cmd := range r.commands {
		if cmd.base == "" {
			continue
		}
		for _, name := range strings.Split(list, ",") {
			if name = strings.TrimSpace(name); name != "" && cmd.base == name {
				return cmd, true
			}
		}
	}
	return shellCommandView{}, false
}

func structuralCommandSubstitution(r shellReading) bool {
	return r.parsed && (r.hasCmdSubst || r.hasProcSubst || r.hasArithmExp)
}

func structuralPrefixAllowed(r shellReading, prefixList string) bool {
	if strings.TrimSpace(prefixList) == "" {
		return false
	}
	if !r.analyzed || !r.parsed || !r.upgradable || len(r.commands) < 2 {
		return false
	}
	for _, cmd := range r.commands {
		if !cmd.literal || len(cmd.words) == 0 || len(cmd.assigns) > 0 {
			return false
		}
		if unclearedCommandNames[cmd.base] {
			return false
		}
		if !prefixListMatchesTokens(cmd.tokens(), prefixList) {
			return false
		}
	}
	return true
}

func (c shellCommandView) tokens() []string {
	var words []string
	if c.literal && len(c.words) > 0 {
		words = append([]string(nil), c.words...)
	} else if c.name != "" {
		// An unknowable argument still has a knowable name; later slots get a sentinel no prefix word can equal.
		words = []string{c.name}
		for i := 1; i < c.argCount; i++ {
			words = append(words, unknowableWord)
		}
	} else {
		return nil
	}
	// Mirrors allowlistProgramWord: a pathed word keeps its path, so structure never grants what the tokenizer refused.
	words[0] = allowlistProgramWord(words[0])
	return words
}

const unknowableWord = "\x00 not statically knowable"

func structuralContradictsPrefix(r shellReading, prefixList string) bool {
	if !r.parsed || strings.TrimSpace(prefixList) == "" || len(r.commands) == 0 {
		return false
	}
	for _, cmd := range r.commands {
		tokens := cmd.tokens()
		if len(tokens) == 0 {
			// No statically knowable name: the tokenizer couldn't read it either.
			continue
		}
		if !prefixListMatchesTokens(tokens, prefixList) {
			return true
		}
	}
	return false
}
