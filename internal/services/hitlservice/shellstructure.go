package hitlservice

import (
	"context"
	"path"
	"runtime"
	"strings"
	"sync/atomic"

	"mvdan.cc/sh/v3/syntax"
)

// Structural shell policy (phase A): the shell operators in policy.go match
// a tokenized command line, which cannot read a shell line, only refuse one
// (why `git status && go build` — two allowlisted verbs — interrupts an
// operator today). This file gives them a second, structural reading
// (mvdan.cc/sh/v3/syntax, parser only, no interpreter). The tokenizer's
// answer is always the floor: structure may only tighten (name a command the
// tokenizer could not) or, for the node set cleared below, upgrade an ask to
// an allow — never widen an allow or narrow a deny. Unparseable input, an
// uncleared node kind, a non-literal word, or any doubt yields today's verdict.
//
// The node-set audit: CLEARED means an ALLOW may be taken through this node;
// UNCLEARED means the analyzer may still report a dangerous command inside
// it (tightening), but its presence anywhere forbids an ask→allow upgrade.
// The would-have-widened tests in policy_shell_structural_test.go are its proof.
//
//	NODE                          CLEARED?  NOTE
//	File                          single*   exactly one top-level Stmt; `a; b` or a list is uncleared
//	Stmt                          simple*   unnegated, foreground, no Redirs (`cmd &`, `! cmd`, `|&` uncleared)
//	Redirect                      no        sibling of CallExpr, not an Arg; target collected, never dropped
//	Assign                        no        unless name is in clearedAssignmentNames (starts empty)
//	CallExpr                      yes*      literal words, no assigns, name not in unclearedCommandNames
//	BinaryCmd                     yes*      && || | only (PipeAll uncleared), flow-insensitive
//	Subshell/Block/If/While/For/
//	  Case/FuncDecl/Time/Coproc/
//	  Let/Decl/Test/Arithm        no        no control-flow/name resolution modeling; enumerated for deny only
//	Word/Lit                     yes*      only when every part is literal (see wordLiteral)
//	SglQuoted/DblQuoted          yes*      unless it carries a run-time expansion ($'…', "$X")
//	ParamExp/CmdSubst/ArithmExp/
//	  ProcSubst/ExtGlob/BraceExp/
//	  ArrayExpr/WordIter/
//	  CStyleLoop                 no        run-time or bash-only; CmdSubst/ArithmExp/ProcSubst also feed
//	                                        no_command_substitution
//
// A1 (the shell guard): structural analysis runs only when the POSIX shell
// is positively established (see structuralShellEnabled) — mvdan parses
// POSIX/bash, not PowerShell, and a wrong reading could widen instead of fail
// closed.
//
// A2 (the parser differential): an ask→allow upgrade is taken only for a
// line built entirely of simple commands, && || |, and literal words — the
// one corner of the grammar where mvdan and `sh -c` agree. Anything else
// keeps today's answer.

// ShellKind names the shell that will interpret a gated command line. It
// mirrors localtools.ShellKind by value rather than by import (localtools
// imports this package, so the dependency cannot run the other way).
type ShellKind string

const (
	// ShellKindPOSIX is the only kind structural analysis runs on: `sh -c`.
	ShellKindPOSIX ShellKind = "sh"
	// ShellKindPowerShell is what local_shell spawns on Windows; mvdan
	// cannot parse it, so it never reaches the parser (see A1 above).
	ShellKindPowerShell ShellKind = "powershell"
	// ShellKindCmd is cmd.exe — same treatment as powershell.
	ShellKindCmd ShellKind = "cmd"
	// ShellKindUnknown is any kind this package does not recognize; distinct
	// from "" so an unrecognized kind fails closed.
	ShellKindUnknown ShellKind = "unknown"
)

type shellKindContextKey struct{}

// WithShellKind marks ctx with the shell that will actually interpret the
// command lines evaluated under it — the trusted channel that can enable
// structural analysis on a host whose default shell is not POSIX. Nothing
// sets it today; until a caller adopts it, structuralShellEnabled falls back
// to analyzing on non-Windows hosts only.
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

// shellKindArgKey is an optional, untrusted hint in the call args. It may
// only narrow: naming a non-POSIX kind disables analysis, naming "sh" grants
// nothing new.
const shellKindArgKey = "shell_kind"

// structuralShellEnabled is the A1 guard, shaped so no model-controlled
// input can ever turn it on:
//
//	trusted kind (ctx)  | args hint      | host GOOS | analyze?
//	sh/bash/dash        | ignored        | any       | yes — positively established
//	powershell/cmd/other| ignored        | any       | no  — mvdan reads a different program
//	unset               | non-POSIX kind | any       | no  — a hint may only narrow
//	unset               | unset or sh    | !windows  | yes — local_shell can only be sh here
//	unset               | unset or sh    | windows   | no  — fail closed
func structuralShellEnabled(trusted ShellKind, args map[string]any, goos string) bool {
	switch normalizeShellKind(string(trusted)) {
	case ShellKindPOSIX:
		return true
	case "":
		// No trusted declaration: fall through to the host-shape fallback.
	default:
		return false
	}
	if hint := normalizeShellKind(shellKindHintFromArgs(args)); hint != "" && hint != ShellKindPOSIX {
		return false
	}
	// On non-Windows, local_shell's shell detection can only produce sh.
	return goos != "windows"
}

func shellKindHintFromArgs(args map[string]any) string {
	if v, ok := args[shellKindArgKey].(string); ok {
		return v
	}
	return ""
}

// clearedAssignmentNames starts empty on purpose: a command carrying env
// assignments falls to ask unless every name is listed here, and every
// interesting name (PATH, LD_PRELOAD, GIT_SSH_COMMAND...) is a hijack channel.
var clearedAssignmentNames = map[string]bool{}

// unclearedCommandNames never carry an upgrade. Two classes: re-entry (eval,
// source, a nested shell, or a wrapper like env/xargs/sudo/find that runs a
// program named by a later word) and environment mutation (export, set,
// alias, etc., which change what a later command resolves to). This only
// restricts the upgrade path; a single-command line the tokenizer already
// allows stays allowed.
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

// shellReading is the structural reading of one gated call.
type shellReading struct {
	// analyzed is false when A1 refused, or the call is not a shell line at all.
	analyzed bool
	// parsed is false when the parser rejected the source — the floor applies.
	parsed bool
	// commands are every simple command the line runs, anywhere in the tree;
	// enumerating is always safe (tightening only).
	commands []shellCommandView
	// redirects are every redirection, target included; their presence
	// blocks every upgrade.
	redirects    []shellRedirect
	hasCmdSubst  bool
	hasProcSubst bool
	hasArithmExp bool
	// upgradable is A2: the whole line is inside the cleared node set.
	upgradable bool
}

// shellCommandView is one simple command as the analyzer can name it.
type shellCommandView struct {
	// name is the program word resolved through quotes/escapes; empty if not
	// statically knowable.
	name string
	// base is path.Base(name) — what the blacklist and ask-always lists compare.
	base string
	// words is the strict literal token line; nil unless every word is fully literal.
	words []string
	// literal mirrors "words != nil" for readability at the call sites.
	literal bool
	// argCount is how many words the command has, including unknowable ones.
	argCount int
	// assigns are the env assignment names carried as a prefix.
	assigns []string
	// display is a short, human-readable rendering for the decision message.
	display string
}

type shellRedirect struct {
	op      string
	target  string
	heredoc bool
}

// structuralParses counts parser invocations, for the A1 test pinning that a
// powershell-kind call never reaches the parser.
var structuralParses atomic.Int64

// analyzeShellArgs is the single entry point: the structural reading of a
// call, or the zero reading (analyzed=false) when structure must not be consulted.
func analyzeShellArgs(trusted ShellKind, args map[string]any) shellReading {
	var r shellReading
	if len(args) == 0 {
		return r
	}
	if !structuralShellEnabled(trusted, args, runtime.GOOS) {
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
	collectShellNodes(f, &r)
	// A bash-only-accepted line is read for tightening only; the executor is
	// `sh -c`, so a reading sh itself would reject must not be an allow's basis.
	r.upgradable = !bashOnly && fileClearedForUpgrade(f)
	return r
}

// maxShellLineBytes bounds what the parser is asked to read; a megabyte of
// pasted script is not a command line.
const maxShellLineBytes = 64 * 1024

// shellLineFromArgs returns the source a shell will actually interpret, or
// false when nothing will — an argv call's metacharacters are ordinary
// bytes, so structure is consulted only for shell:true, or the one-line
// "command"-only form a real shell would run.
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

// parseShellLine parses POSIX first (the executor is `sh -c`); a rejected
// line gets a second bash parse used for tightening only — bashOnly forces
// upgradable off, so a wider reading can name a dangerous command but never
// admit one.
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

// collectShellNodes walks the whole tree by statement, not call expression,
// since a redirect hangs off the Stmt — walking only calls would miss it.
func collectShellNodes(f *syntax.File, r *shellReading) {
	syntax.Walk(f, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			for _, rd := range n.Redirs {
				r.redirects = append(r.redirects, redirectView(rd))
			}
			if call, ok := n.Cmd.(*syntax.CallExpr); ok {
				if cmd, ok := commandView(call); ok {
					r.commands = append(r.commands, cmd)
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

// commandView renders one simple command. ok is false for an assignment-only
// statement (`FOO=bar`), which runs no program.
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

// displayOf renders a command for a human-facing decision message, marking
// what it cannot resolve rather than claiming a run-time-only value.
func displayOf(call *syntax.CallExpr) string {
	parts := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		if v, ok := wordName(w); ok {
			parts = append(parts, v)
			continue
		}
		parts = append(parts, "…")
	}
	// Control characters are flattened: a decision message must not be able
	// to forge a log line or a terminal escape.
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

// wordUnsafeMeta are characters that make an unquoted literal something else
// at run time: glob/brace/tilde expansion, a backslash escape, and a bare
// '$' the parser did not model as an expansion.
const wordUnsafeMeta = "*?[]{}~\\$"

// wordLiteral is the literal-words rule: a decision may consume a word only
// when built entirely of literal parts (quote-splicing resolves statically,
// closing the `r”m`-style evasion); anything that expands at run time
// poisons the word, and its command falls to ask.
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

// wordName is the lenient resolution for naming a command in the deny/ask
// directions only — it resolves what wordLiteral refuses, but never a
// run-time expansion, and is never the basis of an allow.
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

// unescapeUnquoted applies POSIX unquoted backslash semantics: a backslash
// preserves the next character literally, or continues a line before a newline.
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

// unescapeDoubleQuoted applies POSIX double-quoted backslash semantics: the
// backslash is literal except before $ ` " \ or a newline.
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

// fileClearedForUpgrade is A2 in code: an upgrade is taken only for a line
// built entirely of simple commands, && || |, and literal words. A file with
// more than one top-level statement is never cleared.
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
	// `cmd &` detaches it from the approval that gated it; `! cmd` inverts the
	// status a following && depends on; |& and &| are shell-specific.
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
			// Flow-INSENSITIVE on purpose: `a || b` prices b even though it may
			// never run. The worst path pays.
			return stmtClearedForUpgrade(cmd.X) && stmtClearedForUpgrade(cmd.Y)
		}
		return false
	default:
		// Subshell, Block, If/While/For/Case, FuncDecl, Time, Coproc, Let,
		// Decl, Test, Arithm — every one of them UNCLEARED by the audit.
		return false
	}
}

func callClearedForUpgrade(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false // an assignment-only statement: see the assignment rule
	}
	// THE ASSIGNMENT-PREFIX RULE: a hijack channel, not decoration.
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

// structuralCommandInList reports the first command in the line whose
// basename is in a comma-separated list — the tightening half of the
// blacklist/ask-always operators: it can see the rm in `git status && rm -rf
// ~`, which the token matcher cannot. Commands inside uncleared constructs
// still count.
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

// structuralCommandSubstitution asks whether the AST contains a CmdSubst,
// ProcSubst, or ArithmExp node, so creative quoting produces no false negatives.
func structuralCommandSubstitution(r shellReading) bool {
	return r.parsed && (r.hasCmdSubst || r.hasProcSubst || r.hasArithmExp)
}

// structuralPrefixAllowed is the combining rule and the one place structure
// may admit anything: decompose the line and allow only when every command
// is on the prefix list and the line clears A2 (`git status && go build`
// upgrades; `git status > /tmp/x`, `PATH=/tmp git status`, `(git status)`,
// and `git status; go build` keep today's verdict). It requires several
// commands: a single command gains nothing from structure, since the
// tokenizer already allows a plain one and the audit would not clear an
// unplain one anyway.
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

// tokens is the command's token line as the prefix list compares it: the
// program basename followed by its arguments.
func (c shellCommandView) tokens() []string {
	var words []string
	if c.literal && len(c.words) > 0 {
		words = append([]string(nil), c.words...)
	} else if c.name != "" {
		// An unknowable argument still has a knowable name; later slots get
		// a sentinel that can never equal a prefix word.
		words = []string{c.name}
		for i := 1; i < c.argCount; i++ {
			words = append(words, unknowableWord)
		}
	} else {
		return nil
	}
	words[0] = path.Base(words[0])
	return words
}

// unknowableWord stands in for an argument whose value exists only at run
// time; it contains a space, so no policy entry or prefix word can equal it.
const unknowableWord = "\x00 not statically knowable"

// structuralContradictsPrefix reports that the line runs a command the
// prefix list does not cover — the tightening answer to strings.Fields
// eating a newline (so "git status\ncurl x" reads as one covered line). It
// only ever revokes an allow the tokenizer granted; it never narrows further.
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
