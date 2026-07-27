package hitlservice

import (
	"context"
	"path"
	"runtime"
	"strings"
	"sync/atomic"

	"mvdan.cc/sh/v3/syntax"
)

// ─────────────────────────────────────────────────────────────────────────────
// STRUCTURAL SHELL POLICY — phase A of docs/development/blueprints/shell-structural.md
//
// The shell operators in policy.go match on a TOKENIZED command line, and a
// tokenizer cannot read a shell line: it can only refuse one. That refusal is
// why `git status && go build` — two allowlisted verbs — interrupts an
// operator today. This file gives the same operators a second, STRUCTURAL
// reading (mvdan.cc/sh/v3/syntax, parser only — no interpreter) that can say
// what a line actually runs.
//
// THE STANDING RULE, which every function here obeys: the tokenizer's answer is
// the FLOOR. Structure is consulted only to
//
//	(a) TIGHTEN — see a command the tokenizer could not name (`git status &&
//	    rm -rf ~` really does run rm; `r''m` really is rm), or
//	(b) UPGRADE an ask to an allow, and only for the node set cleared in the
//	    audit below.
//
// Nothing here can turn an allow into a wider allow, and nothing here can
// narrow a deny: every operator ORs the structural answer onto today's answer
// rather than replacing it. Unparseable input, an uncleared node kind, a
// non-literal word, or any doubt at all yields exactly today's verdict.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE NODE-SET AUDIT (build prerequisite, per the blueprint: "an audit of the
// AST node set, node by node, is a build prerequisite, not a polish item").
//
// CLEARED means: the analyzer understands this node well enough that a policy
// ALLOW may be taken through it. UNCLEARED means: the analyzer may look at the
// node (and will happily report a dangerous command it finds inside one), but
// its presence anywhere in the line forbids the ask→allow upgrade — the line
// keeps the tokenizer's verdict. The audit IS the security argument; the
// would-have-widened tests in policy_shell_structural_test.go are its proof.
//
//	NODE (syntax pkg)   CLEARED?    WHY / WHAT THE FLOOR STILL CATCHES
//	───────────────────────────────────────────────────────────────────────────
//	File                CLEARED*    * exactly ONE top-level Stmt. Two statements
//	                                (`a; b`, or a newline-separated list) are
//	                                UNCLEARED: A2 names the operator set
//	                                (&& || |) and ';' is not in it.
//	Comment             CLEARED     carries no execution.
//	Stmt                CLEARED*    * only when Negated, Background, Coprocess
//	                                and Disown are all false and Redirs is
//	                                empty. `cmd &` detaches, `! cmd` inverts,
//	                                `|&`/`&|` are shell-specific — each is
//	                                UNCLEARED on its own.
//	Redirect            UNCLEARED   THE SPIKE'S LESSON. A redirect is a sibling
//	                                of the CallExpr, not one of its Args: an
//	                                allowlisted `cat` writes anywhere through
//	                                `> /etc/passwd`. Targets ARE collected (see
//	                                shellReading.redirects) so they are never
//	                                silently dropped, but their presence blocks
//	                                every upgrade. Here-docs (Hdoc) likewise.
//	Assign              UNCLEARED   `PATH=/tmp git status` runs someone else's
//	                                git; so do LD_PRELOAD, GIT_SSH_COMMAND,
//	                                BASH_ENV. Cleared only for names in
//	                                clearedAssignmentNames, which starts EMPTY.
//	CallExpr            CLEARED*    * the simple command — the one node a policy
//	                                decision may be taken on — and only with
//	                                literal words, no assignments, at least one
//	                                Arg, and a name that neither re-enters the
//	                                shell nor mutates the environment a later
//	                                command resolves through
//	                                (unclearedCommandNames).
//	BinaryCmd           CLEARED*    * Op ∈ {AndStmt(&&), OrStmt(||), Pipe(|)}
//	                                only, evaluated flow-INSENSITIVELY: `a || b`
//	                                prices b even though it may never run.
//	                                PipeAll (|&) is UNCLEARED.
//	Subshell            UNCLEARED   `(cd /tmp && rm -rf x)` — commands inside are
//	Block               UNCLEARED   still enumerated for the deny direction, but
//	IfClause            UNCLEARED   the line can never be upgraded, because the
//	WhileClause         UNCLEARED   analyzer does not model control flow,
//	ForClause           UNCLEARED   scoping, or iteration (WordIter/CStyleLoop
//	CaseClause          UNCLEARED   included).
//	CaseItem            UNCLEARED
//	FuncDecl            UNCLEARED   `git() { rm -rf /; }; git status` — a
//	                                function SHADOWS an allowlisted name. The
//	                                analyzer does not resolve names, so any
//	                                declaration in the line forbids upgrades.
//	TimeClause          UNCLEARED   wraps another command.
//	CoprocClause        UNCLEARED   background co-process.
//	LetClause           UNCLEARED   arithmetic with side effects.
//	DeclClause          UNCLEARED   declare/export/local as bash parses them.
//	                                NOTE: the POSIX parser reads `export
//	                                PATH=/tmp` as an ordinary CallExpr, so the
//	                                same door is closed a second time, by name,
//	                                in unclearedCommandNames.
//	TestClause          UNCLEARED   [[ ]] (bash-only; POSIX parse fails first).
//	BinaryTest          UNCLEARED
//	UnaryTest           UNCLEARED
//	ParenTest           UNCLEARED
//	ArithmCmd           UNCLEARED   (( )) — bash-only, evaluates.
//	BinaryArithm        UNCLEARED
//	UnaryArithm         UNCLEARED
//	ParenArithm         UNCLEARED
//	FlagsArithm         UNCLEARED
//	TestDecl            UNCLEARED
//	Word                CLEARED*    * only when every part is literal — see
//	                                wordLiteral. This is the literal-words rule.
//	Lit                 CLEARED*    * only without an unexpanded glob or brace
//	                                meta (* ? [ ] { }), a tilde (~ expands to
//	                                $HOME), a backslash escape, or a bare '$'
//	                                the parser did not model as an expansion —
//	                                see wordUnsafeMeta. Quote-splicing resolves
//	                                statically, so an r, two quotes and an m
//	                                read as rm.
//	SglQuoted           CLEARED*    * unless Dollar ($'…' has C escapes). The
//	                                POSIX parser reads $'…' as a literal '$'
//	                                plus a quote, which the bare-'$' rule above
//	                                catches — the door is closed from both sides
//	                                because the two variants disagree about it.
//	DblQuoted           CLEARED*    * only when every inner part is a Lit and
//	                                Dollar is false; "$HOME" is UNCLEARED.
//	ParamExp            UNCLEARED   values exist at run time, not at parse time.
//	Slice/Replace/      UNCLEARED   (ParamExp internals — same reason.)
//	  Expansion
//	CmdSubst            UNCLEARED   AND it is the AST form of the
//	                                no_command_substitution operator: a CmdSubst
//	                                node anywhere in the line matches it, with
//	                                no false negatives from creative quoting.
//	ArithmExp           UNCLEARED   $(( )) / $[ ] — also feeds
//	                                no_command_substitution, matching the
//	                                textual patterns it replaces.
//	ProcSubst           UNCLEARED   <(…) / >(…) — bash-only, so the POSIX
//	                                parser rejects the line outright (floor:
//	                                today's answer); when a variant does parse
//	                                one it feeds no_command_substitution too.
//	ExtGlob             UNCLEARED   ?(…) — pattern that expands.
//	BraceExp            UNCLEARED   {a,b} — expands to several words.
//	ArrayExpr           UNCLEARED   a=(x y) — bash-only assignment form.
//	ArrayElem           UNCLEARED
//	WordIter            UNCLEARED   for-loop iteration list.
//	CStyleLoop          UNCLEARED   for ((;;)) — bash-only.
//	Pos                 n/a         positions, not program structure.
//
// ─────────────────────────────────────────────────────────────────────────────
// A1 — THE POWERSHELL GUARD. local_shell spawns `sh -c` on unix and POWERSHELL
// on Windows (localtools.ShellKindSh / ShellKindPowerShell). mvdan.cc/sh parses
// POSIX/bash and does NOT parse powershell: feeding it a powershell line does
// not fail cleanly, it yields a DIFFERENT program, and a decision taken on that
// reading could WIDEN. Structural analysis therefore runs only when the POSIX
// shell is positively established — see structuralShellEnabled, which fails
// closed on Windows and on any kind it does not recognize.
//
// A2 — THE PARSER DIFFERENTIAL, NAMED AND BOUNDED. In phase A we analyze with
// mvdan and execute with `sh -c`: wherever the two disagree about what a line
// means, the envelope would decide on a reading that is not what runs (the
// classic parse-with-one-tool/execute-with-another bypass class). It is small
// here only because of the cleared set above: an ask→allow UPGRADE is taken
// ONLY for a line built entirely of simple commands, && || |, and literal words
// — the most standardized corner of the grammar, where every POSIX shell (and
// bash, and even fish/csh for this subset) agrees. Anything else keeps today's
// answer. Phase B eliminates this class outright because the parser IS the
// executor; that is a stronger argument for phase B than Windows.
// ─────────────────────────────────────────────────────────────────────────────

// ShellKind names the shell that will interpret a gated command line. It mirrors
// localtools.ShellKind by VALUE rather than by import (localtools imports this
// package, so the dependency cannot run the other way).
type ShellKind string

const (
	// ShellKindPOSIX is the only kind structural analysis runs on: `sh -c`.
	ShellKindPOSIX ShellKind = "sh"
	// ShellKindPowerShell is what local_shell spawns on Windows. mvdan cannot
	// parse it, so it never reaches the parser (A1).
	ShellKindPowerShell ShellKind = "powershell"
	// ShellKindCmd is cmd.exe — same treatment as powershell.
	ShellKindCmd ShellKind = "cmd"
	// ShellKindUnknown is any kind this package does not recognize. It is a
	// distinct value rather than the empty string so an unrecognized kind fails
	// CLOSED instead of falling back to the host default.
	ShellKindUnknown ShellKind = "unknown"
)

type shellKindContextKey struct{}

// WithShellKind marks ctx with the shell that will actually interpret the
// command lines evaluated under it. It is the TRUSTED channel for A1: the host
// code that owns the spawn (localtools.LocalExecTools, shellsession) knows the
// kind, the model does not, and only a value set here can ENABLE structural
// analysis on a host whose default shell is not POSIX.
//
// Nothing sets it today — see the A1 finding in the blueprint work: the shell
// kind never reaches Evaluate, because HITLWrapper.Exec passes only the tool
// call's args map. Until a caller adopts it, the fallback in
// structuralShellEnabled applies: analysis on non-Windows hosts (where
// local_shell's shell detection can only produce sh), never on Windows.
func WithShellKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, shellKindContextKey{}, normalizeShellKind(kind))
}

// ShellKindFromContext returns the trusted shell kind set by WithShellKind, or
// the empty string when none was set.
func ShellKindFromContext(ctx context.Context) ShellKind {
	kind, _ := ctx.Value(shellKindContextKey{}).(ShellKind)
	return kind
}

func normalizeShellKind(s string) ShellKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "sh", "bash", "dash", "ash", "posix":
		// bash and dash are supersets/subsets of the cleared node set; a line
		// that parses under the POSIX parser means the same thing in all three.
		return ShellKindPOSIX
	case "powershell", "pwsh":
		return ShellKindPowerShell
	case "cmd", "cmd.exe":
		return ShellKindCmd
	default:
		return ShellKindUnknown
	}
}

// shellKindArgKey is an OPTIONAL, UNTRUSTED hint a tool layer may put in the
// call args. It is untrusted because the args map is model-authored, so it may
// only NARROW: naming a non-POSIX kind disables structural analysis, naming
// "sh" grants nothing the host did not already grant.
const shellKindArgKey = "shell_kind"

// structuralShellEnabled reports whether a line under this call may be read
// structurally at all. It is the A1 guard, and it is deliberately shaped so
// that no model-controlled input can ever turn it ON:
//
//	trusted kind (ctx)  | args hint      | host GOOS | analyze?
//	sh/bash/dash        | ignored        | any       | YES  — positively established
//	powershell/cmd/other| ignored        | any       | no   — mvdan reads a different program
//	unset               | non-POSIX kind | any       | no   — a hint may only narrow
//	unset               | unset or sh    | !windows  | YES  — local_shell can only be sh here
//	unset               | unset or sh    | windows   | no   — powershell/cmd territory (fail closed)
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
	// On a non-Windows host, localtools.DetectPlatformShellFor can only produce
	// ShellKindSh — the powershell and cmd branches are guarded by
	// goos == "windows". That is what "positively established" means here.
	return goos != "windows"
}

func shellKindHintFromArgs(args map[string]any) string {
	if v, ok := args[shellKindArgKey].(string); ok {
		return v
	}
	return ""
}

// clearedAssignmentNames is the assignment-prefix rule's escape hatch, and it
// STARTS EMPTY on purpose. A command carrying env assignments falls to ask
// unless every assigned name is listed here; nothing has yet earned a place,
// because the interesting names are all hijack channels (PATH, LD_PRELOAD,
// LD_LIBRARY_PATH, GIT_SSH_COMMAND, ENV, BASH_ENV, SHELLOPTS, IFS…) and the
// harmless-looking ones (TZ, LANG) buy an operator nothing worth the door.
var clearedAssignmentNames = map[string]bool{}

// unclearedCommandNames never carry an upgrade, whatever an operator's
// allowlist says. Two classes, one rule:
//
//   - RE-ENTRY: the verb takes its program from data the analyzer cannot read.
//     `eval` and `.`/`source` re-enter the parser on a string; a nested shell
//     re-enters it on a file or a -c argument; env/command/xargs/nohup/timeout/
//     watch/sudo/find run a program named by a later word, so a prefix match on
//     the WRAPPER says nothing about what runs.
//   - ENVIRONMENT MUTATION: the verb changes what a LATER command in the same
//     line resolves to. `export PATH=/tmp && git status` runs someone else's
//     git just as surely as the assignment-prefix form does, and alias/set/
//     unset/readonly/local reach the same door from other sides.
//
// This list only ever restricts the NEW upgrade path — a single-command line
// that today's tokenizer allows stays allowed.
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
	// analyzed is false when the A1 guard refused, or when the call is not a
	// shell line at all (an argv call: nothing interprets its metacharacters).
	analyzed bool
	// parsed is false when the parser rejected the source — the floor applies.
	parsed bool
	// commands are every simple command the line runs, from ANYWHERE in the
	// tree (inside subshells, loops, function bodies and command
	// substitutions included). Enumerating them is always safe: they feed the
	// tightening direction only.
	commands []shellCommandView
	// redirects are every redirection in the line, target included. They are
	// recorded rather than dropped — that is the spike's lesson — and their
	// presence blocks every upgrade.
	redirects    []shellRedirect
	hasCmdSubst  bool
	hasProcSubst bool
	hasArithmExp bool
	// upgradable is A2: the whole line is inside the cleared node set.
	upgradable bool
}

// shellCommandView is one simple command as the analyzer can name it.
type shellCommandView struct {
	// name is the program word resolved through quotes and backslash escapes
	// (so `r''m`, `"rm"` and `r\m` all read as rm). Empty when the word is not
	// statically knowable.
	name string
	// base is path.Base(name) — what the blacklist and ask-always lists compare.
	base string
	// words is the STRICT literal token line (name plus arguments) and is nil
	// unless every word was fully literal; only this feeds an allow.
	words []string
	// literal mirrors "words != nil" for readability at the call sites.
	literal bool
	// argCount is how many words the command has in total, including the name,
	// even when some of them were not statically knowable.
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

// structuralParses counts parser invocations. It exists for the A1 test that
// pins that a powershell-kind call NEVER reaches the parser — a property that
// is otherwise invisible from the outside.
var structuralParses atomic.Int64

// analyzeShellArgs is the single entry point: the structural reading of a call,
// or the zero reading (analyzed=false) when structure must not be consulted.
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
	// A line only the bash parser accepted is read for tightening and never for
	// admission: the executor is `sh -c`, so a reading sh itself would reject
	// must not be the basis of an allow.
	r.upgradable = !bashOnly && fileClearedForUpgrade(f)
	return r
}

// maxShellLineBytes bounds what the parser is asked to read. A policy decision
// is on the hot path of every tool call; a megabyte of pasted script is not a
// command line, and the floor (today's answer) is the right verdict for it.
const maxShellLineBytes = 64 * 1024

// shellLineFromArgs returns the source a SHELL will actually interpret, and
// false when nothing will.
//
// This distinction is load-bearing. local_shell runs `exec.Command(command,
// args...)` — plain argv, no shell — unless the call asks for shell mode, so
// for an argv call the metacharacters in an argument are ordinary bytes:
// reading `git commit -m "fix; rm -rf /"` as two commands would be a FICTION,
// and a deny on that fiction would be a bug. Structure is therefore consulted
// only for
//
//   - shell:true — the platform shell receives command + args as one script,
//     exactly as localtools.PlatformShell.WrapCommand builds it; and
//   - the one-line form (a "command" string with no "args"), which is what
//     shell_session_run submits to a real shell, and which local_shell would
//     hand to exec.Command as a single program name — a lookup that cannot run
//     anything a policy would have to reason about.
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

// parseShellLine parses with the POSIX variant FIRST, not bash, because the
// executor is `sh -c`: reading the line the way the executor will is the
// smallest parser differential available to phase A, and a bash-only construct
// (process substitution, [[ ]], herestrings, arrays) is rejected outright.
//
// A rejected line then gets a SECOND parse under the bash variant, whose result
// is used for the tightening direction only — bashOnly forces upgradable off,
// so a wider reading can name a dangerous command (`cat <(rm -rf /)`) but can
// never admit anything. Without it, a bash-only line would be structurally
// invisible, which is safe but blind.
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

// collectShellNodes walks the WHOLE tree. It walks statements rather than call
// expressions because a redirection hangs off the Stmt, not off the CallExpr —
// the spike reported `cat f.txt` and silently dropped `> /etc/passwd`, and an
// analyzer that only walked calls would have widened the allowlist while
// looking more rigorous than the tokenizer it replaced.
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

// displayOf renders a command for a human-facing decision message. It resolves
// what it can and marks what it cannot, so the message never claims to know a
// value that only exists at run time.
func displayOf(call *syntax.CallExpr) string {
	parts := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		if v, ok := wordName(w); ok {
			parts = append(parts, v)
			continue
		}
		parts = append(parts, "…")
	}
	// The words are model-authored and land in a verdict and a trace line, so
	// control characters are flattened: a decision message must not be able to
	// forge a log line or a terminal escape.
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

// wordUnsafeMeta are the characters that make an unquoted literal something
// other than itself at run time: glob and brace expansion, and tilde expansion.
// A backslash is here too — `r\m` is rm to a shell, and rather than
// re-implement escape rules inside the ALLOW path, an escaped word is simply not
// literal. So is a bare '$': a dollar the parser did NOT turn into a ParamExp
// or an ArithmExp is one this variant does not model as an expansion, and
// another shell may ($'…' is literal to dash and a C-escape to bash). Only a
// word with no run-time meaning at all may carry an allow.
const wordUnsafeMeta = "*?[]{}~\\$"

// wordLiteral is the LITERAL-WORDS RULE: a policy decision may consume a word
// only when it is built entirely of literal parts. Quote-splicing resolves
// statically (`r”m` → rm), which is also what closes the `r”m`-style evasion
// against the blacklist; anything that expands at run time — ParamExp, CmdSubst,
// ArithmExp, ProcSubst, ExtGlob, BraceExp, an unexpanded glob — makes the word
// unknowable, and an unknowable word poisons its command to ask.
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

// wordName is the LENIENT resolution used for NAMING a command in the deny and
// ask directions only. It resolves what wordLiteral refuses to bless — a
// backslash escape, a glob character that is merely part of a name — because a
// name the analyzer can read is a name the blacklist can catch. It still
// refuses every run-time expansion: `$CMD` has no name until it runs. It is
// never the basis of an allow.
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
// preserves the literal value of the character that follows, and a backslash
// before a newline is a line continuation.
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
// backslash is literal EXCEPT before $ ` " \ or a newline.
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

// fileClearedForUpgrade is A2 in code: an ask→allow upgrade may be taken ONLY
// for a line built entirely of simple commands, && || |, and literal words.
// Everything else keeps today's answer. A file with more than one top-level
// statement (`a; b`, or a newline-separated list) is deliberately NOT cleared:
// A2 names the operator set, ';' is not in it, and admitting it later is a
// one-line change that must arrive with its own would-have-widened test.
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
	// THE SPIKE'S LESSON: a redirect is not an argument, and an allowlisted
	// reader with a redirect is a writer.
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

// ─── the operator half: what policy.go asks the reading ──────────────────────

// structuralCommandInList reports the first command in the line whose basename
// is in a comma-separated list. It is the TIGHTENING half of the blacklist and
// ask-always operators: today's matcher reads the first token of the "command"
// argument and therefore cannot see the rm in `git status && rm -rf ~`, nor
// read `r”m` as rm. Commands inside uncleared constructs count — a subshell,
// an if branch or a command substitution still runs them.
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

// structuralCommandSubstitution is no_command_substitution as a STRUCTURAL
// question — "does the AST contain a CmdSubst node?" — instead of a string
// search, so creative quoting produces no false negatives. Process and
// arithmetic expansions are included because the textual patterns it augments
// (`<(`, `>(`, `$((`, `$[`) name them too.
func structuralCommandSubstitution(r shellReading) bool {
	return r.parsed && (r.hasCmdSubst || r.hasProcSubst || r.hasArithmExp)
}

// structuralPrefixAllowed is THE COMBINING RULE and the one place structure may
// ADMIT anything: decompose the line, evaluate each command against the prefix
// list, and allow only when EVERY one of them is allowed. Any command that is
// not on the list simply fails the match, so the call falls through to the
// tiers below exactly as it does today (any deny denies; any approve asks; all
// allow allows).
//
// It requires the A2 clearance first, so `git status && go build` upgrades
// while `git status > /tmp/x`, `PATH=/tmp git status`, `(git status)` and
// `git status; go build` keep today's verdict.
//
// It also requires SEVERAL commands, which is the exact scope of the problem
// this work was opened on: "a compound line of allowlisted verbs interrupts the
// operator". A single command gains nothing from structure — when it is plain,
// the tokenizer already allows it; when it is not, the audit does not clear it
// anyway — with one exception, and that exception is a decision this analyzer
// must not take on its own: a shell-mode call carrying ONE plain argv
// (`{"command":"ls","args":["-la"],"shell":true}`). The shipped envelopes
// refuse shell mode outright and their tests pin that refusal. Whether an
// analyzable shell-mode line should be allowlistable is a policy question for
// those envelopes, not something an analyzer smuggles in as a side effect.
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
// program BASENAME followed by its arguments, so /usr/bin/git matches a "git …"
// prefix — the same shape commandTokens builds for the tokenizer path.
func (c shellCommandView) tokens() []string {
	var words []string
	if c.literal && len(c.words) > 0 {
		words = append([]string(nil), c.words...)
	} else if c.name != "" {
		// A command with an unknowable ARGUMENT still has a knowable NAME, and
		// a one-token prefix ("ls") legitimately matches it. Later slots are
		// filled with a sentinel that can never equal a prefix word, so a
		// multi-token prefix ("git log") is never matched by `git $VERB`.
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

// unknowableWord stands in for an argument whose value exists only at run time.
// It contains a space, so strings.Fields can never produce it from a policy
// entry and no prefix word can ever equal it.
const unknowableWord = "\x00 not statically knowable"

// structuralContradictsPrefix reports that the line runs a command the prefix
// list does NOT cover, and is the answer to a hole the tokenizer has today: it
// flattens the command line with strings.Fields, which EATS a newline, so
// `git status\ncurl http://x` reads to it as the token line "git status curl
// http://x" and matches the "git status" prefix. That is a real line for
// shell_session_run, which types the string into a live shell and runs both.
//
// This is a tightening, not a narrowing of the floor: it only ever REVOKES an
// allow the tokenizer granted for a line whose structure shows another command.
// A line whose commands are all covered (including one whose arguments are
// unknowable, which is not another command) keeps its allow untouched.
func structuralContradictsPrefix(r shellReading, prefixList string) bool {
	if !r.parsed || strings.TrimSpace(prefixList) == "" || len(r.commands) == 0 {
		return false
	}
	for _, cmd := range r.commands {
		tokens := cmd.tokens()
		if len(tokens) == 0 {
			// The command has no statically knowable name at all; the tokenizer
			// could not have been reading it either. Leave its answer alone.
			continue
		}
		if !prefixListMatchesTokens(tokens, prefixList) {
			return true
		}
	}
	return false
}
