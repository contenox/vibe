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

// structuralShellEnabled is the A1 guard. The shell kind is declared by the
// caller that spawns the shell (HITLWrapper, at construction) and travels on
// the context; call args never reach it, so no model-controlled input decides
// whether the analyzer runs:
//
//	trusted kind (ctx)  | host GOOS | analyze?
//	sh/bash/dash        | any       | yes — positively established
//	powershell/cmd/other| any       | no  — mvdan reads a different program
//	unset               | !windows  | yes — local_shell can only be sh here
//	unset               | windows   | no  — fail closed
func structuralShellEnabled(trusted ShellKind, goos string) bool {
	switch normalizeShellKind(string(trusted)) {
	case ShellKindPOSIX:
		return true
	case "":
		// No trusted declaration: fall through to the host-shape fallback.
	default:
		return false
	}
	// On non-Windows, local_shell's shell detection can only produce sh.
	return goos != "windows"
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
// since a redirect hangs off the Stmt — walking only calls would miss it. It
// also runs the normalization pass (see the reveal rule below), which is why
// it carries a depth: a revealed payload is collected by re-entering here.
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

// THE REVEAL RULE (normalization to a fixed point). A tokenized reading sees
// the words a line was written with; a shell runs the program those words
// name after it has peeled wrappers, decoded escapes, and reconstructed
// argument lists. Everything below closes that gap in exactly ONE direction:
// a normalization may only ever ADD a shellCommandView, never remove or
// rewrite one. That is what keeps it inside the monotonicity contract at the
// top of this file, because every consumer of r.commands is monotone in the
// set:
//
//   - structuralCommandInList  — more commands, more deny/ask catches
//   - structuralPrefixAllowed  — EVERY command must be listed, so more
//     commands can only refuse an upgrade, never grant one
//   - structuralContradictsPrefix — more commands, more revoked allows
//   - structuralProgramWords   — more names to vouch for, never fewer
//
// A reveal may therefore be wrong (a guessed reading of an ambiguous line)
// without being unsafe: the worst outcome is an ask that could have been an
// allow. Where a reading is ambiguous, EVERY candidate is revealed rather
// than one being chosen.
//
// The reveal never reaches the upgrade path in practice either: every trigger
// word below (sh/bash/dash/ash/ksh/zsh/fish, xargs, eval, source) is in
// unclearedCommandNames, so a line carrying one is never upgradable — pinned
// by TestUnit_ShellAnalyzer_RevealNeverClearsAnUpgrade.
const (
	// maxRevealDepth bounds wrapper peeling; `sh -c "xargs sh -c ..."` nests,
	// and one evaluation must not become unbounded parsing.
	maxRevealDepth = 4
	// maxRevealedCommands bounds how far the enumeration may grow. Reveals
	// only: a plain line's collection is unchanged.
	maxRevealedCommands = 256
)

// nestedShellNames take their program from a -c payload the outer parse can
// only see as one opaque word.
var nestedShellNames = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ash": true,
	"ksh": true, "zsh": true, "fish": true,
}

// lenientWords resolves a call's words the way wordName does — the
// deny-direction resolution — with "" standing for a word only run time knows.
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

// revealWords records one reconstructed command and keeps peeling it, so
// `xargs sh -c 'rm -rf /'` names xargs, sh, AND rm.
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

// peelWrapper reveals what a wrapper actually runs. Each arm is additive: the
// wrapper's own command view was already recorded by the caller.
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

// revealSource parses a payload and collects it into the same reading. A
// payload that only the bash parser accepts is still collected: bashOnly is
// dropped here because r.upgradable is computed from the TOP-LEVEL file alone
// (see analyzeShellArgs), so a wider reading here can only tighten.
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

// viewFromWords renders a reconstructed command. literal is claimed only when
// every word was statically knowable; otherwise tokens() falls back to the
// name plus unknowable sentinels, which no prefix entry can equal.
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

// dashCPayload returns a nested shell's -c program text. It stops at the
// first non-option word (a script filename, not a payload) and at any word it
// could not read, rather than guessing which slot held the -c.
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

// xargsValueOptions take their value as a SEPARATE word, so the word after
// them is not the command xargs reconstructs. An attached form (-n1, -I{})
// carries its own value and needs no entry.
var xargsValueOptions = map[string]bool{
	"-a": true, "-d": true, "-E": true, "-I": true, "-L": true,
	"-n": true, "-P": true, "-s": true,
	"--arg-file": true, "--delimiter": true, "--eof": true, "--replace": true,
	"--max-lines": true, "--max-args": true, "--max-procs": true,
	"--max-chars": true, "--process-slot-var": true,
}

// xargsCommandWords returns the command line xargs reconstructs from its own
// arguments — the program that actually runs, which the outer reading names
// only as "xargs".
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

// revealPipedPayload peels a literal payload piped into an evaluator:
// `printf '\162\155 -rf /' | sh` runs rm, a name no reading of the written
// words contains.
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

// consumerEvaluatesStdin reports whether the right side of a pipe runs its
// standard input as a program.
func consumerEvaluatesStdin(st *syntax.Stmt) bool {
	words, ok := stmtCallWords(st)
	if !ok || words[0] == "" {
		return false
	}
	base := path.Base(words[0])
	return nestedShellNames[base] || base == "eval" || base == "source" || base == "."
}

// printedPayloads is what a literal echo/printf writes to stdout, as the
// candidate program texts a piped evaluator would then run. BOTH the raw join
// and its escape-decoded form are returned: which one a given shell's echo
// produces is exactly what this must not have to guess, and revealing both
// only ever adds commands.
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

// joinKnown joins words back into one line, refusing when any of them exists
// only at run time — a hole in the middle would make the join a different
// program than the one that runs.
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

// decodeShellEscapes applies printf/echo -e backslash decoding, which is how
// a command name is spelled when it is meant not to be read as one
// (`printf '\162\155'` is "rm"). Unknown escapes keep their backslash.
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

// parseEscapedNumber reads up to maxDigits digits of the given base,
// returning the byte they encode and how many bytes were consumed. It stops
// before overflowing a byte rather than rejecting the escape, matching what a
// shell does with a too-large sequence: the extra digit is output text.
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

// literalAssignments collects NAME=value pairs whose value is statically
// knowable, from anywhere in the tree. Flow-INSENSITIVE exactly as the A2
// clearance rule is: reading the wrong branch's assignment can only reveal a
// command that is not run, which asks where it could have allowed — never the
// other way round.
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

// resolveAssignedWords substitutes collected assignments into a command's
// words, so `CMD=rm; $CMD -rf /` names rm. ok is false when nothing resolved,
// so a command with no variable in it is never recorded twice.
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

// soleParamName reports the variable a word consists of entirely — $X or
// ${X}. Any modifier (length, slice, replacement, default) makes the value
// something other than the assignment, so it resolves to nothing.
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
	return flattenDisplay(parts)
}

// displayWords is displayOf for a reconstructed command (see viewFromWords),
// where "" is the already-resolved form of an unknowable word.
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
	// Same normalization as the tokenizer twin (allowlistProgramWord): a
	// pathed program word keeps its path, so it cannot inherit a bare-name
	// allow. Structure may only ever revoke, so keeping this in step means a
	// pathed word the tokenizer refused is refused here too.
	words[0] = allowlistProgramWord(words[0])
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
