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
- **Seeded envelope**: `goja_eval` = `approve` (explicit rule, comment
  stating the deadline-bounded-memory rationale; dogfooding may argue it
  down). Script tools carry NO seeded rule — they fall to
  `default_action: approve`, which is the correct trust posture for
  operator-authored-but-unreviewed-by-us code; operators add `allow` rules
  per script, exactly like any other envelope edit.
- TS: not in V1. Transpilation needs esbuild-class weight; "bring your own
  build" until dogfooding demands it.

## Build order

1. sandbox core + limits (+ adversarial tests: loop/alloc/recursion bombs,
   interrupt under load, panic paths)
2. script loader/registry + fail-fast validation
3. the `host.tool` bridge through the engine's tool path (recursion guard)
4. engine registration + seeded policy entries
5. hostile escape review (separate pass): sandbox escapes, bridge abuse,
   marshaling edge cases, resource exhaustion
