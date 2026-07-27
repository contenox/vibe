# direktiv-mining — a sobek flow engine, read for the script tracks

Mining report, 2026-07-27. Status: assessment only; nothing ported.

Source: github.com/direktiv/direktiv (Apache-2.0 — direct code reading is
fine, no whiteroom needed, unlike Crush/FSL), shallow clone in a disposable
job tmp. Direktiv is a k8s serverless workflow engine — the *product shape*
is everything we are not (server, namespaces, gorm/Postgres, services) and
none of it ports. But its flow-authoring pipeline is TypeScript → sobek
(grafana's maintained goja fork) with an AST validation layer, which is
three of our open tracks in production form. Paths below are
direktiv-relative.

## The verdict, up front

**One mechanism resolves an open question of ours outright (M1); two are
direct lifts for the goja track (M2, M3); one is input to a pending ruling
(M4). The product shape stays buried.**

## M1 — Checkpoint boundaries as a language concept
**→ answers yaegi-tools OQ-7 / shell-structural OQ-5 (suspension vs.
durable envelopes)**

Direktiv never serializes an interpreter frame — the *language surface*
makes serialization trivial instead. Flows are plain functions; all
cross-state memory passes through `transition(nextFn, payload)` where the
payload is JSON-marshaled at the boundary
(`internal/engine/runtime/runtime.go:295-350`). `ExecScript`
(`runtime.go:437`) can start at ANY state function with a recorded JSON
input: resume = re-run the script text (top-level is definitions only) +
invoke the recorded function with the recorded memory. Crash, suspend,
migrate — the checkpoint is always (function name, JSON payload), never a
frame. Host functions also re-check `ctx.Done()` on entry
(`runtime.go:298`, `:389`), so cancellation weaves through every host
boundary without vm-level preemption.

**The lesson for us**: our OQ-7 problem ("an approve-tier ask parks
mid-script; the frame can't checkpoint; replay double-runs earlier calls")
dissolves if script tools that want gated calls are *structured as steps
with JSON memory between them* — the ask parks at a step boundary, resume
re-enters at the recorded step, steps before it never re-run. Exactly-once
per step, no frame serialization, and it composes with the existing
checkpoint machinery (which already stores name+JSON shapes). The ruling
OQ-7 asks for has a third option beyond "block synchronously" and
"whole-script replay": **make the boundary a first-class part of the script
contract.** Cost: a script API shape (`step`/`transition`-like), not new
engine machinery.

## M2 — Parse, don't execute: AST extraction of script metadata
**→ gojatool load safety + envelope admission preview**

Direktiv extracts a script's entire static footprint WITHOUT running it:
`sobek/parser` + `sobek/ast` walk (`internal/compiler/ast.go`, exhaustive
node walker) pulls the declarative `flow` config object, all function
names, referenced secret names, action configs, and even the
state-transition graph — from imperative JS, at load time, with
severity-graded `ValidationError{line, col, severity}`.

Two lifts for us, zero new dependencies (dop251/goja ships the same
`parser` + `ast` packages as sobek):

1. `gojatool/script.go` currently *executes* a script to read its
   `const tool = {…}` descriptor. AST extraction reads it without running
   top-level code — hostile scripts get zero execution before validation.
2. **Static `host.tool` footprint**: walk calls to `host.tool(<literal>)`
   and the envelope knows, at load time, which tools a script can reach —
   coarse admission before anything runs, and approval cards that say
   "this script calls git_commit, write_file" instead of naming a script
   file. (Literal-args-only, same fail-closed discipline as
   shell-structural's literal-words rule; a computed tool name poisons the
   script to ask.)

## M3 — TS authoring via ts.transpileModule inside the VM, source maps end-to-end
**→ the gojatool TS slice (TODO §4)**

`internal/compiler/transpiler.go`: an embedded `ts-5.9.2.js` runs
`ts.transpileModule` (syntax-only, `reportDiagnostics: false`) inside sobek
with `sourceMap: true`. This is production proof that transpile-only
typescript.js in a goja-family VM works — revising this repo's earlier
"wrong horse" note, which was about the type *checker*, not the
transpiler. esbuild remains the better recommendation for us (much faster,
and bundling is the win that turns pure-JS npm into tool material;
direktiv re-instantiates the transpiler per compile and hides the cost
behind a cache — don't copy that). But if a zero-new-Go-dep variant is
ever wanted, this is the existence proof.

The detail to lift regardless of transpiler choice: **source maps threaded
through BOTH stages** — `go-sourcemap` parsed onto the AST file for
validation positions (`ast.go:82-92`) AND `parser.WithSourceMapLoader` on
the *runtime* VM (`runtime.go:47-51`) so runtime stack traces report the
TS the author wrote, not the transpiled JS. TS tool authoring without this
produces teaching errors that point at the wrong file.

## M4 — The sync-twin convention for async hosts
**→ input to the goja async ruling (TODO §5)**

Direktiv exposes `fetch` (returns a real `sobek.Promise`, resolved from a
goroutine — `commands_http.go:224-229`) AND `fetchSync` (blocking twin).
The mineable shape is the *twin convention*: a sandbox can ship blocking
host calls only (`fetchSync`-style) and defer the event loop entirely —
which is precisely the cheapest answer available to our async ruling.
Caveat, stated so nobody copies it blindly: resolving a promise from a
foreign goroutine against a VM that is not goroutine-safe is a discipline
question direktiv answers loosely; if we ever admit promises, we pump a
job queue on the VM's own goroutine, not this.

## Also noted

- **sobek vs goja**: direktiv (and k6, whose engine sobek is) run on the
  maintained grafana fork. Not a decision — an evaluation item for the
  goja track: compare ES-feature coverage, perf, and maintenance cadence
  against dop251/goja before the TS slice lands.
- `SetMaxCallStackSize(256)` and host-error-as-JS-exception
  (`panic(vm.ToValue(...))`) match gojatool's existing discipline —
  confirmation, not news.

## Leave buried

The server/product shape (k8s, gorm, namespaces, secrets manager, gateway
plugins); `ParseFuncNameFromText` (stringifies a JS function to recover
its name — the AST already knows it); per-compile transpiler
re-instantiation; cross-goroutine promise resolution (see M4).
