# yaegi — typed tool orchestration for the agent

Blueprint, opened 2026-07-27. Status: **design in progress — spike-proven
core, open questions marked for co-authorship. Do not build until the OPEN
QUESTIONS section is closed.**

## The idea

Use traefik/yaegi (a pure-Go Go interpreter) to give the model a **typed Go
surface over the tools its envelope allows**, so it orchestrates real
functions instead of string-named JSON calls.

The claim worth testing is not "another sandbox". It is that **the exposed
symbol table can be most of the envelope**: which functions exist at all is
decided by policy, the model gets a discoverable typed API, and the JSON
marshaling boundary between a script and the tools it drives disappears.

## Why this is on the table at all

Live dogfooding of the goja script tools (2026-07-27) found the sharpest
edge in that feature is not the sandbox — it is that scripts inherit tool
outputs written for a *reader*, not a program. Two real cases in one
session: a script assumed `git_status` returned porcelain (it returns prose)
and reported "4 staged, 2 other, no untracked" for a tree with 1 modified +
1 untracked — **confidently wrong, with nothing anywhere to catch it**; and
another treated `read_file`'s cache stub ("File unchanged since last read…")
as file content.

A typed surface eliminates that failure class by construction. That is the
argument this blueprint exists to evaluate properly.

## Spike evidence (scratchpad/yaegi-spike, 2026-07-27)

| Probe | Result |
| --- | --- |
| Expose our own typed funcs via `interp.Exports`; import and call from interpreted Go | **WORKS** — real return values, no marshaling |
| Cancel `for {}` via `EvalWithContext` | **WORKS** — stopped at 201ms against a 200ms deadline |
| `import "os"` when stdlib was never `Use`d | **UNPROVEN** — the probe was malformed (a syntax error masked the real answer). MUST be verified before any build. |
| `go func(){ for {} }()` | **ACCEPTED, AND IT LEAKS** — an interpreted goroutine outlives the evaluation and spins forever. goja cannot do this (no goroutines). |
| Binary cost | ~7.7MB spike binary WITHOUT stdlib symbols; yaegi's size explodes with `stdlib.Symbols`. Real delta against our binary is unmeasured. |

## Proposed shape (a starting point, not a decision)

- **Pre-flight AST gate, before anything runs**: parse with `go/parser` and
  reject `GoStmt` (the goroutine hole), `unsafe`, any import outside the
  allowed symbol map, and any construct that can outlive the call. This is
  the gate that makes the rest defensible; it must exist first.
- **Plain-data export frontier**: exposed functions take and return only
  strings, numbers, slices, and method-less structs — no interfaces, no
  callbacks, no host objects carrying method sets. Most reflection
  reach-through (OQ-1) dies here by construction, and it keeps the
  generated API listing small (OQ-6).
- **Policy-filtered symbol table built per call** from the same evaluation
  the HITL wrapper runs, so admission and the envelope cannot drift.
- **Per-call approval still routes through the wrapper.** The symbol table
  is coarse admission, never the whole gate (see OQ-2).
- Context deadline, output cap, and the recursion/allocation discipline the
  other sandboxes already carry (see `goja-tools.md` for the measured
  numbers and the honest memory story).
- **If built, it REPLACES goja's script tools** rather than sitting beside
  them — two script-authoring languages is sprawl, and Go is the better fit
  for a Go-shop harness. `goja_eval` (inline compute, no world access) is a
  separate question and may survive either way.

## OPEN QUESTIONS — for the co-author

**OQ-1 — Containment, unproven.** Can interpreted code reach anything not
in the symbol map? Verify: unlisted stdlib imports, `unsafe`, reflection
from an exposed type back to unexported state, method sets on exposed
structs reaching further than intended. A build cannot start until this is
answered with running code, not documentation.

  The plain-data export frontier above closes the LAST of those four by
  construction — no method sets cross the boundary, so there is nothing to
  reach through. It closes none of the other three: unlisted imports and
  `unsafe` arrive through the import resolver, not the export frontier, and
  they must still be tested. Add a fifth to verify while here: whether an
  exported slice or map is a COPY or aliases host memory — a returned
  `[]byte` sharing an array the host still writes to is a live channel back
  regardless of how plain its type looks.
Prior art sets the burden of proof: upstream yaegi does not claim to
contain untrusted code (Traefik's plugin model trusts authors), and the
spike's goroutine leak already shows interpreter semantics escaping the
symbol table. So the working assumption is the reverse of goja's: the
interpreter is a typed *executor*, not a boundary — the boundary is the
AST gate + the symbol map + what the exports make reachable. Whatever
passes, the package doc will have to say out loud that containment is
weaker than goja's (per the security-claim standing constraint below).

**OQ-2 — How much of the envelope does the symbol table actually express?**
The envelope is per-CALL with conditions (path globs, command prefixes,
argument shapes). A symbol table encodes only "this tool exists". So the
typed surface expresses the coarse layer and per-call evaluation still
happens at call time — approve-tier tools raise cards from inside a script.
The elegant framing ("the symbol table IS the envelope") is therefore
partly true, and the honest fraction must be stated in the package doc or it
becomes a false security claim. **Is the coarse layer worth the machinery on
its own?**

**OQ-3 — Does typed orchestration actually beat `host.tool`?** This is
empirical and answerable with what already exists: does dogfooding produce
multi-step orchestrations often enough to justify a second interpreter, and
do their bugs cluster in tool-output parsing (which types fix) or in logic
(which types do not)? The goja structured-output fix currently in flight
changes this answer — measure after it lands.
Name the counterfactual before scoring: the *discoverability* half of the
typed surface — signatures in front of the model — can be had in goja by
generating typed stubs of the host API into the tool description, no
second interpreter. yaegi's non-replicable delta is *machine-checked*
calls (a wrong field name fails loudly instead of flowing on as
undefined). Score that delta, not the whole surface.

**OQ-4 — Cost.** Real binary delta against our binary with a
policy-filtered (non-stdlib) symbol table; interpretation speed for
realistic orchestrations; memory behavior under a hostile program.

**OQ-5 — Authoring ergonomics.** Do models write correct Go against a
generated symbol table as reliably as they write JS? A Go shop's agent
plausibly does better in Go — but that is a hypothesis, and it is cheap to
test with a handful of real prompts against a stub API.
One prior runs *against* Go here: models write Go with the stdlib as a
reflex — `fmt`, `strings`, `errors` appear unbidden — so a Go dialect
without stdlib fights training priors harder than browserless JS fights JS
priors. Either the symbol table carries a curated pure-function subset
(fmt.Sprintf, strings, strconv, encoding/json — no I/O; size cost lands in
OQ-4) or the teach-refusal for `import "fmt"` must be excellent. And a
product note: the harness courts Go AND TS shops; since this REPLACES goja
scripts (stated above), tool authoring becomes Go-only — plausibly fine,
but it should be decided on purpose, not inherited from the mechanism.

**OQ-6 — Discoverability.** How does the model learn the surface? A
generated API listing in the tool description costs tokens every turn; a
`list_api()` call costs a round trip. Which, and does it change with symbol
count? The cache work changes this trade: a *stable* generated listing is
prefix-cache-friendly (canonical tool order and day-granular determinism
are already enforced), so its token cost is mostly paid once per session.
Measure with caching on, or the answer will come out wrong.

**OQ-7 — Script suspension vs. durable envelopes.** Approvals park ≤30s,
then checkpoint-and-release the run — but an interpreter frame is not
serializable. If an approve-tier card raised from *inside* a script hits
the park window, the script cannot checkpoint; only the chain task can. On
resume the task re-runs, which re-runs the whole script — and every
allow-tier call before the gate executes a second time. Whatever the
resolution — synchronous full-timeout wait for script-raised asks,
whole-script replay with at-least-once semantics stated in the tool
description, or refusing approve-tier calls inside scripts — it must be
decided and written down, and the SAME question applies to the goja
scripts shipping today. This is an envelope-identity question wearing an
interpreter costume; it outranks the rest of this file by the repo's own
priority rule. `direktiv-mining.md` M1 records a production-proven third
option: checkpoint boundaries as a language concept — steps with JSON
memory between them, resume re-enters at the recorded step, no frame ever
serialized.

## Co-author position (Claude, 2026-07-27 — recommendation, not a ruling)

**Do not build yet — sequence it.**

1. Land the in-flight goja structured-output fix. It addresses both live
   incidents that motivated this blueprint; after it, the case for yaegi
   rests only on machine-checked calls, Go fluency, and discoverability.
2. Answer OQ-5/OQ-3 with the stub experiment (a day: N real orchestration
   prompts against a typed yaegi stub vs goja+structured outputs, first-try
   correctness), and OQ-1's probes the same week — they are independent.
3. Build only if BOTH come back decisive: a real authoring-accuracy win AND
   containment probes that pass behind the AST gate + plain-data frontier.
   If either is marginal, the verdict is LEARN like eino: bank the report,
   write the revisit trigger, keep one script language.

The uncomfortable trade, said out loud: yaegi swaps goja's stronger
containment story for types. For a product whose identity is the envelope,
weaker containment needs a decisive win elsewhere to pay for itself. And
OQ-7 must be answered for the scripts already shipping, regardless of what
happens to this blueprint.

## Rejected use cases (recorded so they are not re-proposed)

- **Interpreted chain handlers** — reopens the closed handler table that
  chainlint proves at load time (same objection recorded in `ee-mining.md`
  against goja hooks; unchanged by yaegi being typed).
- **User-authored providers/connectors** — MCP is that story.
- **Running project code** — the compat treadmill; the sandboxed shell
  already runs the project's own toolchain under the envelope.

## Standing constraints this must satisfy

From the repo's rules, not negotiable by the design: in-process pure Go, no
subprocess sidecar · mechanisms not product shapes · surfaces stay thin,
logic in services · anything advertised must actually work · a security
claim is stated at exactly its true strength or not at all.
