# goja tools — script tools and a sandbox tool over one bounded runtime

Decision record, 2026-07-27. Status: designed, spike-proven, building.

## Why

Two capabilities over one embedded ES runtime (dop251/goja, pure Go):

1. **Script tools** — operator-authored tool definitions as readable files
   (`$CONTENOX_DIR/tools/*.js`), registered as local tools at engine build.
   The lowest-friction extensibility contenox can offer: no Go toolchain, no
   MCP server to run. Extends "everything the AI runs under is a readable
   file" to tooling itself.
2. **`goja_eval`** — a sandbox tool the model drives directly (the
   code-interpreter pattern) for bounded compute: transforms, parsing,
   arithmetic over tool results.

**Naming (maintainer, 2026-07-27): the provider and tools are named `goja`,
never `js`** — "js" drags browser/Node priors into the model's tool choice;
`goja` is a distinctive token that means exactly this sandbox.

## The one boundary rule

Scripts have **no ambient I/O**: no filesystem, no network, no require, no
process. Their only reach into the world is `host.tool(name, args)`, which
routes through the SAME tool execution path the model uses — HITL wrapper
included. A script calling `local_fs.write_file` meets the same envelope a
model call would. One policy boundary, unchanged; script tools inherit the
entire envelope story for free. Scripts may not invoke `goja`-provider
tools (no recursion; depth is exactly one).

## Evidence

- EE mining (ee-mining.md §1): the EE goja CODE stays buried — no interrupt,
  no recovery, an admitted SSRF, thin tests. The lesson ports; ~40 lines of
  shape (fresh-VM-per-exec, hash-keyed program cache) are the only ideas
  kept.
- Spike (2026-07-27, scratchpad): `vm.Interrupt` kills `while(true){}` at
  exactly the deadline (50ms test); `SetMaxCallStackSize` contains recursion
  bombs; host-function bridging is trivial; and the honest number — goja has
  NO memory cap, an allocation bomb grabbed **64MB in 300ms** before the
  interrupt stopped it. Memory is therefore **deadline-bounded, not
  capped**: default deadline 2s ⇒ transient ceiling in the low hundreds of
  MB, then GC. Documented, not hidden. A subprocess+rlimit tier is the
  named escalation if that ceiling ever matters.
- Dependency cost: ≈ +11MB binary (measured in ee-mining.md §1b).

## Design

- `internal/services/gojatool` — the sandbox core: fresh `goja.Runtime` per
  execution, program cache keyed by source hash, interrupt watchdog
  (deadline default 2s, per-script override capped at 30s), stack cap,
  panic recovery at the exec boundary, JSON-only marshaling in and out,
  output cap with an explicit truncation marker. Synchronous only: no event
  loop, no setTimeout, no promises in V1 (documented).
- **Script tool convention**: a `.js` file exports
  `const tool = { name, description, schema }` (schema in the same terse
  OpenAPI shape the built-in toolsets use) and `function run(args)`. Loaded
  at engine build; a bad schema, a missing export, or a name collision
  (against built-ins or another script) is a fail-fast teaching error at
  startup — never a silent skip.
- **Registration**: one provider `goja` exposing `goja_eval` plus every
  loaded script tool. Policy addressing: `tools: "goja", tool: <name>`.
- **Seeded envelope**: `goja_eval` = `allow` (see "The tier decision" below).
  Script tools carry NO seeded rule — they fall to
  `default_action: approve`, which is the correct trust posture for
  operator-authored-but-unreviewed-by-us code; operators add `allow` rules
  per script, exactly like any other envelope edit.
- TS: not in V1. Transpilation needs esbuild-class weight; "bring your own
  build" until dogfooding demands it.

## The program-facing contract (decided 2026-07-27, after live use)

**A tool result is written for a READER, and a reader's answer is not a
contract for a program.** Live e2e found the cost twice, both silent: a script
assumed `git.git_status` returned porcelain (it returns prose) and reported
"4 staged, 2 other, no untracked" for a tree with 1 modified + 1 untracked;
another treated `local_fs.read_file`'s cache stub ("File unchanged since last
read…") as file content. Both returned successfully. Nothing in the stack
could catch either.

**Decision: the value declares its own shape, and prose is never handed over
naked.** `host.tool()` answers with exactly one of three things, decided by the
Go type the tool returned — no table, no registry, nothing to drift:

- **DATA** — any non-string Go value → a plain JS object/array. Fields,
  indexes, iteration, `JSON.stringify` all normal. The one thing it refuses is
  primitive conversion, because `String(obj)` is "[object Object]" and no error.
- **TEXT** — a Go string → `ToolText`, an object with `.text`. Every string
  method a script reaches for (`.split`, `.match`, `.trim`, `.length`) and every
  implicit conversion throws a teaching error naming the tool.
- **NOTHING** — nil → null.

Escape hatch: `host.tool(name, args, {raw: true})` returns the exact value
unwrapped. Parsing prose is sometimes right; what the design buys is that the
decision is *in the source at the call site* rather than the default nobody
chose. Unknown options are refused by name.

Two optional capabilities let a tool say more about a stand-in answer,
asserted structurally (the trick `taskengine.toolDiffProvider` uses) so
neither package imports the other:

- `ProgramText() (string, bool)` — "my rendering stands in for this". The
  read_file cache stub implements it and hands a script the real content: the
  sentence is true of a model whose earlier read is in its context and false of
  every caller that never made that read. The dedup is untouched — those tokens
  still never reach the model.
- `ProgramUnusable() string` — "nothing happened, here is why". read-before-write
  denials implement it; the bridge throws instead of letting a script read an
  apology as a receipt.

**Rejected:** (a) a `host.toolShape(name)` pre-call table — this runtime cannot
honestly answer "what shape does provider.tool return?" before the call (tool
declarations describe ARGUMENTS), and a table here would be a second source of
truth that drifts, which is the exact failure class being removed; (b) a
caller-kind context flag read by each tool — it needs a shared home in the
kernel, and a tool that never learns about the flag fails silently again,
re-creating the invisible default; (c) leaving prose as a bare string with only
documentation — documentation does not fail a build.

**Structured where it is cheap and proven**: `git_status`, `git_log`,
`git_branch_list` now return typed Go values whose `String()` is byte-identical
to the prose they returned before, because the engine renders a
`DataTypeString` result with `fmt.Sprintf("%v", …)`. The model sees exactly what
it saw; a program sees `{branch, head, clean, staged[], unstaged[], untracked[]}`.
Golden-text tests pin both halves. `git_diff`, `git_show`, `git_blame` stay
text — a diff IS a text artifact. Everything else in the tree is unchanged and
still safe, because the TEXT wrapper covers it without the tool knowing.

Consequence, handled: `toolguidance` appends navigation lines to string results
only, so typing a result would have silently dropped its guidance from the
model's view. Typed results that render as text now declare
`AppendGuidance(string) any` and carry the lines in their own rendering.

Per-tool shapes are **not** a stable API. They live beside the tool, they
change when it does, and a script should pin the field it reads.

## Declared reach (decided 2026-07-27)

A script's descriptor may carry `tools: ["git.git_status", "local_fs.read_file"]`.

- **Enforced at runtime**: a `host.tool` call to an undeclared address is refused
  before the trip, with an error naming the address, what the script DID
  declare, and the file to edit. `tools: []` is a declaration that the script
  reaches nothing.
- **Optional, so nothing that worked stops working** — but the loader warns
  ONCE at startup naming every script without one, so unrestricted is never the
  invisible default.
- **Validated at load**: a malformed entry, an unqualified name, or a
  `goja`-provider address is a fail-fast startup error like every other
  descriptor mistake.
- **Exposed as metadata**: `Script.Tools` + `Script.ToolsDeclared` on
  `(*Toolset).Scripts()`, in declaration order. A card renderer MUST read both —
  "declares it reaches nothing" and "declares nothing" both present as an empty
  list and are opposite answers.

This is defence in depth, not the policy boundary: the envelope still evaluates
every call that gets through. What it adds is the one thing the envelope
structurally cannot give — a statement of reach that exists BEFORE the script
runs, which is what an approval card for a script tool can show.

## The tier decision: `goja_eval` = allow (2026-07-27)

Changed from `approve` in `hitl-policy-default.json`, `hitl-policy-acp.json` and
`hitlservice.defaultPolicy()`, each carrying the full rationale.

**Why it is safe**: the sandbox has no ambient I/O BY CONSTRUCTION — no
filesystem, no network, no require/import, no process, no event loop. There is
nothing for an approval to protect: the only reach out is `host.tool`, and every
call it makes is evaluated by the same policy under the rule for the tool it
addresses. What is left is pure compute, bounded by the deadline interrupt, the
call-stack cap and the output cap. The one soft limit is memory (no cap; ~200
MB/s until the deadline fires ⇒ a transient ceiling in the low hundreds of MB,
then GC) — documented above, and not something an approval card can reduce.

**Why it was wrong before**: 8 live approvals in one e2e session, each costing an
operator more than the call did, none of which could have prevented anything.

**What revokes it**: any ambient capability ever reaching the sandbox — a
filesystem shim, a network fetch, a module loader, a VM that outlives the call.
The argument is entirely about there being nothing here to reach.

Script tools keep NO rule (`default_action`), unchanged.

## Build order

1. sandbox core + limits (+ adversarial tests: loop/alloc/recursion bombs,
   interrupt under load, panic paths)
2. script loader/registry + fail-fast validation
3. the `host.tool` bridge through the engine's tool path (recursion guard)
4. engine registration + seeded policy entries
5. hostile escape review (separate pass): sandbox escapes, bridge abuse,
   marshaling edge cases, resource exhaustion
6. **the program-facing contract** (done 2026-07-27): result shapes, declared
   reach, the `goja_eval` tier — all three found by using the thing

## Open, next

- The TEXT wrapper makes a mis-parse impossible to do *by accident*, but a
  script that writes `.text` and parses anyway is still guessing. The answer is
  more structured tools, one at a time, where the record shape is obvious:
  `local_fs.list_dir`, `grep`, `find_files` are the next three (their
  truncation semantics are baked into the rendered string today, which is why
  they were not done in the same pass).
- `local_shell` and MCP results are text by nature and will stay wrapped.
