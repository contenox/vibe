# TypeScript ACP client for the VS Code extension — spike and recommendation — 2026-07-28

Answers §8 of [vscode-implementation-plan.md](vscode-implementation-plan.md) ("TypeScript ACP
client: build or vendor?"), which calls this "the main risk in this plan."

## Recommendation

Adopt the official TypeScript SDK, npm package **`@agentclientprotocol/sdk`** (the current,
actively-maintained name; `@zed-industries/agent-client-protocol` is the same project under its
old, now-deprecated name). It fully supports the CLIENT role — session lifecycle, prompt,
`session/update`, serving `session/request_permission` and `fs/*` — and arbitrary `_contenox/*`
extension methods via `onRequest`/`onNotification` with a raw params parser. Bundle it into the
extension host with esbuild (it is ESM-only; the extension host is CommonJS) — a small,
precedented addition, since esbuild already bundles the webview. Keep a hand-written client (b),
spike-validated below at roughly 800–1400 LOC, as a fallback only. Reject (c): it is today's
bespoke-bridge process, one layer removed, and the implementation plan already condemns that
shape.

## What was actually checked

Every claim below was verified against this repo's source or a live network fetch — see
"Unknowns" for the few things that could not be confirmed.

- `libacp/ndjson.go`, `libacp/rpc.go`, `libacp/conn.go`, `libacp/clientconn.go`,
  `libacp/methods.go`, `libacp/session.go`, `libacp/content.go`, `libacp/permission.go`,
  `libacp/client.go`, `libacp/errors.go` — the wire behavior our agent side (`libacp`)
  actually implements.
- `internal/surfaces/acpsvc/initialize.go`, `transport.go`, `fileio.go`,
  `external_terminal.go`, `terminalrun.go` — the agent (`contenox acp`) we'd be talking to.
- `internal/surfaces/beamtui/enginebridge/bridge.go`, `events.go` — the existing in-process
  ACP client (imports zero `beamtui` packages, confirmed by its own import list).
- `packages/vscode/src/bridge/BridgeClient.ts`, `BridgeProcess.ts`, `JsonRpcFramer.ts`,
  `protocol.ts` — today's bespoke bridge client.
- `packages/vscode/package.json`, `tsconfig.json`, `.vscodeignore`,
  `scripts/build-webview.js` — the extension's actual build/package constraints.
- npm registry (`registry.npmjs.org`) and the `agentclientprotocol/typescript-sdk` GitHub repo
  (via `api.github.com` / `raw.githubusercontent.com`, fetched directly since `npmjs.com` itself
  blocks fetching) — the real state of the npm package.
- A throwaway Node spike at
  `/tmp/claude-1000/-home-naro-src-github-com-runtime/be6db57b-6c2f-41ac-a72d-d71b794ea27c/scratchpad/acp_spike.mjs`,
  run against a freshly built `./bin/contenox acp` (`task build`), speaking raw NDJSON with zero
  dependencies.

## Option (a): the official npm package

**Identity, verified.** The package that shows up first in search, `@zed-industries/agent-client-protocol`
(latest `0.4.5`, Apache-2.0), carries an npm deprecation notice on every recent version: *"This
package has been renamed to `@agentclientprotocol/sdk`. Please migrate to continue receiving
updates."* The live package is **`@agentclientprotocol/sdk`**, currently `1.3.0`, Apache-2.0,
published by Zed Industries, repo `github.com/agentclientprotocol/typescript-sdk` (220 stars,
created 2025-10-11, last push 2026-07-25 — three days before this spike, so this is not
abandoned). `1.3.0` itself published 2026-07-21. Do **not** add the old `@zed-industries/*` name
to any dependency list; it will not receive updates.

**Does it support the CLIENT role?** Yes, fully, confirmed by reading `src/acp.ts` from the repo
directly (not just the README). It exports both `agent()`/`AgentApp` and `client()`/`ClientApp`
builders. `ClientApp.onRequest(schema.CLIENT_METHODS.session_request_permission, handler)` and
`.onRequest(schema.CLIENT_METHODS.fs_read_text_file, ...)` /
`fs_write_text_file` / the five `terminal/*` methods are all first-class, typed entries in
`clientRequestSpecs`. `ClientContext` (what `connectWith` hands the caller) exposes
`buildSession(cwd)` → `SessionBuilder` → `.start()`/`.withSession()` → `ActiveSession`, which
wraps `session/new`, routes `session/update` by session id, and exposes `.prompt(...)`,
`.nextUpdate()`, `.readText()`. Session `list`/`delete`/`resume`/`close`/`setMode`/
`setConfigOption` are all typed request methods on the same context.

**Extension methods (`_contenox/*`)?** Yes. Both `AgentApp.onRequest`/`onNotification` and
`ClientApp.onRequest`/`onNotification` have an overload — `onRequest(method: string, paramsParser,
handler)` — explicitly documented as *"Pass a parser as the second argument to register custom
extension methods,"* and the outbound `ClientContext.request<Response, Params>(method: string,
params)` / `.notify(method: string, params)` overloads accept an arbitrary string method name too.
An unregistered inbound method throws `RequestError.methodNotFound(...)` (`-32601`), matching
`libacp.MethodNotFound` (`libacp/errors.go:65-67`). This is a direct match for
`SetExtRequestHandler`/`SetExtNotificationHandler` (`libacp/clientconn.go:141-154`) and is exactly
what `_contenox/terminal/run` needs.

**Wire framing.** `src/stream.ts`'s `ndJsonStream()` reads newline-delimited JSON via
`src/line-buffer.ts`'s `LineBuffer` (byte-scan for `\n`, decode-trim-JSON.parse, skip blank
lines) — the same shape as `libacp/ndjson.go`'s `ndjsonReader.readLine`
(`libacp/ndjson.go:74-101`: `ReadSlice('\n')`, strip the trailing newline, skip empty lines). No
Content-Length framing anywhere in the SDK.

**Cancellation.** `src/jsonrpc.ts:87` defines `CANCEL_REQUEST_METHOD = "$/cancel_request"` — the
identical string `libacp/methods.go:37` reserves (`MethodCancelRequest`). The SDK wires this to an
`AbortSignal` automatically: every handler context (`ClientRequestContext<Params>`) carries a
`signal: AbortSignal` that aborts when the peer sends `$/cancel_request` for that request. This is
a precise match for what a `session/request_permission` handler needs (see "Wire-compatibility
risks" below) — the SDK does the plumbing libacp's `applyCancelRequest`
(`libacp/clientconn.go:313-324`) exists to trigger.

**`_meta` handling.** Checked the generated Zod schema (`src/schema/zod.gen.ts:44`): every
`_meta` field is `z.record(z.string(), z.unknown()).nullish()` wrapped in `defaultOnError(...,
() => undefined)` — an arbitrary, permissive passthrough object that degrades to `undefined`
instead of failing validation on shapes the schema doesn't recognize. This is what carries
`contenox.workspaceConfigOptions` (`internal/surfaces/acpsvc/initialize.go:81-90`) and
`approvalflow.Meta` (`internal/surfaces/beamtui/enginebridge/bridge.go:871-874`) without the SDK
ever needing to know about them.

**Packaging risk (real, not fatal).** The package is **ESM-only**: `package.json` has `"type":
"module"`, `"main": "dist/acp.js"`, no CJS build. `packages/vscode/package.json` has `"type":
"commonjs"` and `"main": "./dist/extension.js"`; `tsconfig.json` uses `"module": "nodenext"`,
`"moduleResolution": "nodenext"`. Under `nodenext`, a CJS file cannot `import` an ESM-only
package as a static import; it needs a dynamic `await import(...)` **or** a bundler that
lowers ESM to CJS at build time. `.vscodeignore` currently excludes `node_modules/**`
wholesale — consistent with `dependencies: {}` in `package.json` today — so simply adding the
package as a dependency and compiling with `tsc` (the current `compile` script) would ship a
VSIX that `require()`s a module that was never packaged. The fix is not exotic: `scripts/build-webview.js`
already runs `esbuild.build({ bundle: true, platform: ..., format: ... })` for the webview;
adding a parallel `esbuild` bundle step for the extension-host entry point
(`format: "cjs"`, `platform: "node"`, `external: ["vscode"]`) makes esbuild itself convert the
ESM dependency into the CJS bundle, sidestepping the dynamic-import question entirely and
producing one file that already contains the dependency — no VSIX/`.vscodeignore` change
needed beyond that new build step. This is one build-pipeline change, made once, not a per-feature
tax.

**Dependency footprint.** One runtime peer dependency: `zod` (`^3.25.0 || ^4.0.0`, MIT, pure JS,
no native/prebuilt binaries). `@agentclientprotocol/sdk@1.3.0` itself is 5.29 MB unpacked
(includes the stable v1 surface plus the experimental v2/HTTP/WS/server code this project would
never import — esbuild tree-shaking should trim most of that from the actual bundle, but this is
unverified without doing the real bundle and checking its size).

## Option (b): hand-write a minimal client

**Spike result: the handshake works, with zero dependencies.** A throwaway ~150-line Node script
(`acp_spike.mjs`, no npm packages) spawned a freshly built `./bin/contenox acp`
(`task build` → `bin/contenox`), and performed:

1. `initialize` (protocolVersion 1, empty fs/terminal client capabilities) → got back
   `protocolVersion: 1`, `agentCapabilities`, `agentInfo: {name: "contenox", ...}`, and a
   `_meta.contenox.workspaceConfigOptions` array (model/HITL-policy/think/token-limit selects) —
   this repo's real, already-running config, since the spike ran against the user's live
   `~/.contenox` dev environment (existing `default-model`/`default-provider` were configured).
2. `session/new` (`cwd`, `mcpServers: []`) → got back a real `sessionId`
   (`acptsspike-<uuid>`) and `configOptions`.
3. Two `session/update` notifications (`available_commands_update`, then `usage_update`)
   arrived **after** the `session/new` response, matching `libacp/conn.go:51-63`'s
   `AfterResponse` mechanism, which defers exactly these two so a client can resolve the session
   before acting on them.
4. `session/delete` on that session id → clean `{}` result, process exited 0.

No parse errors, no framing issues, no unexpected disconnects. The full transcript is at
`/tmp/claude-1000/-home-naro-src-github-com-runtime/be6db57b-6c2f-41ac-a72d-d71b794ea27c/scratchpad/spike_out.log`.
This validates that NDJSON-over-stdio, numeric-id JSON-RPC, and the deferred-notification
ordering are exactly what `libacp/ndjson.go` and `libacp/conn.go` document — hand-rolling the
wire layer is unambiguously *tractable*, whichever option is chosen.

**Size estimate, grounded in what `libacp` actually implements** (non-test LOC, `wc -l`
against `libacp/*.go` excluding `_test.go`, 4121 lines total):

| Piece | Go reference | Est. TS LOC |
|---|---|---|
| NDJSON line framing | `ndjson.go` (127) | 80–120 (mirrors `line-buffer.ts`'s approach) |
| JSON-RPC envelope + id union + dispatch | `rpc.go` (209) | 100–150 |
| Client connection: pending-map, cancellation, extension-method seam | `clientconn.go` (835) | 300–500 — Go's explicit goroutine/mutex bookkeeping (≈40% of this file: `handlerMu`, `reqCancelMu`, `permsMu`, `turnMu`, drain timeouts) has no TS equivalent; Node's single-threaded event loop + `Promise`/`AbortController` replaces it structurally, not line-for-line |
| Wire types actually needed (session, content, permission, prompt, mcp, tools, plan, terminal, fs) | `session.go`+`content.go`+`permission.go`+`prompt.go`+`mcp.go`+`tools.go`+`plan.go`+`terminal.go`+`fs.go` (≈1145 combined) | 400–700 as TS `interface`s (no validation logic unless hand-rolled too) |
| Error codes + typed `Error` | `errors.go`, `clienterrors.go` (182) | 60–100 |
| **Total** | | **≈940–1570**, i.e. the "800–1400" cited in the recommendation, rounded |

This excludes: input validation (libacp leans on Go's `encoding/json` plus manual checks; a TS
client would want *some* runtime validation — e.g. hand-rolled or a small schema lib — adding
back some of what "zero dependencies" was trying to avoid), and a conformance test suite
comparable to `libacp/*_test.go` (4164 lines) — none of which nominally applies to a client-role
subset. Realistically, a client-role-only hand-rolled implementation plus a reasonable test suite
is a multi-week effort, not a multi-day one — and every wire nuance in the "risks" section below
would need re-discovering and re-encoding by hand, whereas the SDK has already encoded them
(cancellation → `AbortSignal`, extension methods, `$/cancel_request`).

## Option (c): keep a Go bridge process

This is architecturally identical to what `packages/vscode/src/bridge/` already does today — a
child process the extension host talks to over stdio — except the child would speak
`contenox acp`'s real ACP instead of the bespoke dialect, and something would still have to
translate ACP back into whatever shape the extension consumes. Concretely, this means either:

- The extension spawns `contenox acp` directly and *is* the ACP client (this is (a) or (b), not
  a distinct option), or
- A **new** Go process sits between the extension and `contenox acp`, doing exactly what
  `enginebridge.Bridge` already does in-process for beamtui (`internal/surfaces/beamtui/enginebridge/bridge.go`) —
  speak ACP as a client, and re-expose a second, narrower protocol to TypeScript.

The second reading is what "(c)" actually means, and it is strictly worse than today's bridge,
not better: it adds a process hop and a second hand-maintained wire protocol (Node ↔ new-Go-process)
on top of the one being eliminated (extension ↔ `contenox vscode-agent`), while still requiring
someone to write and maintain that second protocol's TypeScript half. The entire value of the
plan's "extension is an ACP client" thesis (`vscode-implementation-plan.md` §4) — one session
contract, three surfaces — evaporates if the extension talks to a private Go-owned protocol
again. `enginebridge` is the right shape for an **in-process Go surface** (beamtui); a VS Code
extension host is not a Go process, so the in-process trick doesn't transfer, and re-deriving its
outer layer as an IPC boundary just re-creates the bespoke bridge with extra steps.

## Wire-compatibility risks against our agent side specifically

These apply to option (a) and (b) equally — they're properties of `contenox acp`
(`internal/surfaces/acpsvc`), not of any particular client library.

- **Framing**: `libacp/ndjson.go` is strictly NDJSON — one JSON value per line, no
  Content-Length headers, blank lines skipped (`ndjson.go:60-72`). This is a hard break from
  today's bespoke bridge, which uses LSP-style `Content-Length: N\r\n\r\n` framing
  (`packages/vscode/src/bridge/JsonRpcFramer.ts:1-53`). None of `JsonRpcFramer.ts` is reusable.
- **Method dialect**: today's bridge talks to a **different CLI command entirely** —
  `contenox vscode-agent --stdio` (`packages/vscode/src/bridge/BridgeProcess.ts:292-294`,
  confirmed live in `internal/surfaces/contenoxcli/vscodeagent_cmd.go`) — with its own
  18-ish-method dialect (`chatSend`, `sessionCreate`, `health`, ...;
  `packages/vscode/src/bridge/protocol.ts`), not ACP. Nothing in `BridgeClient.ts`'s method
  surface maps onto ACP method names; this is a full swap of both the transport and the API,
  matching what the implementation plan already says (§4, §6: "The bespoke 18-method dialect ...
  kills"). Whether `contenox vscode-agent` becomes dead code once this lands is not addressed by
  the plan — see Unknowns.
- **Cancel notification name mismatch**: the bespoke bridge sends `"$/cancelRequest"` (camelCase,
  no underscore — `BridgeClient.ts:329,517`). Real ACP's is `"$/cancel_request"`
  (`libacp/methods.go:34-37`, confirmed identical in the official SDK's
  `CANCEL_REQUEST_METHOD` at `src/jsonrpc.ts:87`). A client built by copying today's bridge's
  conventions would silently fail to cancel anything against `contenox acp`.
- **id types**: `libacp`'s two connection types only ever mint sequential numeric ids
  (`atomic.Int64`, e.g. `clientconn.go:583-584`, `conn.go:664-665`) and only match numeric-id
  responses (`clientconn.go:566-569`, `conn.go:647-651`). Practically, this means a client can
  assume `contenox acp` always uses number ids for its own outbound calls
  (`session/request_permission`, `fs/*`, `terminal/*`) — but a spec-correct client should still
  accept string/null ids on the wire, since `RequestID` (`libacp/rpc.go:9-27`) is a genuine
  three-way union and other ACP agents may use strings.
- **Cancellation semantics a client must implement, not just parse**: cancelling a prompt
  (agent-side `Cancel`, `internal/surfaces/acpsvc/transport.go:571-582`) cancels the agent's
  outbound `session/request_permission` call's context, which sends the client
  `$/cancel_request` for that specific request id. The client's own
  `session/request_permission` handler **must** observe that cancellation (via the request's
  `AbortSignal`/context) and resolve promptly — `enginebridge`'s own client half does exactly
  this (`bridge.go:919-924`: `case <-ctx.Done(): ... resolved(libacp.PermissionOutcomeCancelled)`).
  A handler that ignores the signal leaves a permission dialog open in the VS Code UI after the
  user (or the agent) cancels the turn. The official SDK exposes this as `ctx.signal` on
  `ClientRequestContext`; a hand-rolled client (b) must wire this itself or reproduce the bug.
- **Unknown-method handling**: both sides answer an unrecognized non-extension method with
  `-32601` (`libacp/errors.go:65-67`, `conn.go:633-643`/`clientconn.go:552-562`), and silently
  drop unrecognized non-extension *notifications* — a client must not treat either as fatal.
  `IsExtensionMethod` (`libacp/methods.go:58-60`) reserves the entire `_`-prefixed namespace, and
  `$/`-prefixed methods are a third, protocol-owned namespace, never extension-eligible
  (`methods.go:34-37`) — a client dispatching by naive prefix match must special-case `$/`
  before falling through to its extension-method handler.
- **`_meta` is not optional to plumb through**: `contenox acp` puts real, load-bearing data in
  `_meta` outside the stable spec fields — `initialize`'s `_meta.contenox.workspaceConfigOptions`
  (`acpsvc/initialize.go:81-90`, seen live in the spike transcript) and
  `session/request_permission`'s `_meta` → `approvalflow.Meta` (gate reason, named policy, diff —
  `enginebridge/bridge.go:871-874`, cited as the "no HITL reason today" fix in the implementation
  plan's table). A client that drops unrecognized `_meta` keys loses actual panel features, not
  just protocol noise.

## What this would NOT do, and why

- **Not** wrap the extension's use of the SDK behind an abstraction that could swap in a
  hand-rolled client later "just in case." That's speculative generality for a decision this
  spike already has enough evidence to make; if the SDK turns out to be genuinely unworkable
  (see Unknowns), the size estimate above shows a rewrite is bounded and survivable, not a
  reason to pre-pay an abstraction tax today.
- **Not** keep `packages/vscode/src/bridge/*` around in any form, "just for `_contenox/autocomplete`"
  or similar. Every one of its four files (`BridgeClient.ts`, `BridgeProcess.ts`,
  `JsonRpcFramer.ts`, `protocol.ts`) is wrong-framing and wrong-dialect for ACP; keeping any of
  them invites someone to reach for the familiar-looking but incompatible abstraction later.
  `_contenox/autocomplete` is an ACP extension method like `_contenox/terminal/run` already is —
  it does not need its own bespoke transport.
- **Not** invent a second Go process per option (c)'s literal reading — see above.
- **Not** adopt `@zed-industries/agent-client-protocol` under its old name. It is deprecated on
  npm as of the version check above; picking it up now just means migrating immediately.
- **Not** assume the ESM/CJS friction is solved by switching the whole extension's output to
  ESM. VS Code does support ESM extension entry points, but that is a much bigger, unrelated
  change to this codebase's build system (`tsconfig.json`, `package.json`'s `"type"`,
  `vsce`/`.vscodeignore` conventions) than adding one esbuild bundle step, and nothing about this
  question requires it.

## Unknowns, and how the maintainer resolves each

1. **Actual bundle size/behavior of `@agentclientprotocol/sdk` through this project's esbuild,
   with `platform: "node"`/`format: "cjs"`, tree-shaking the unused v2/HTTP/WS/server code.**
   Not verified here — the spike deliberately added no dependency to `packages/vscode`. Resolve
   by: `npm install @agentclientprotocol/sdk zod` in a scratch branch, add an
   `scripts/build-extension.js` mirroring `scripts/build-webview.js`
   (`format: "cjs", platform: "node", external: ["vscode"]`), bundle `src/extension.ts`, and
   check the output size and that `vsce package` produces a working VSIX (`npm run package`).
2. **Whether the official SDK's Zod-validated request/response shapes reject any field
   `contenox acp` actually sends beyond `_meta`** (e.g. a value the generated schema is stricter
   about than `libacp`'s permissive Go structs). Not exercised — the spike bypassed all
   validation by design. Resolve by: running the SDK's real `client()` against `contenox acp`'s
   actual `initialize`/`session/new`/`session/prompt` responses (start from
   `acp_spike.mjs`'s harness, swap the raw NDJSON loop for `ndJsonStream` + `client()`) and
   watching for thrown Zod errors.
3. **Whether `contenox vscode-agent` (`internal/surfaces/contenoxcli/vscodeagent_cmd.go`) becomes
   dead code once the extension speaks ACP to `contenox acp` directly**, and if so, when it gets
   removed. Not addressed by `vscode-implementation-plan.md`. Resolve by: a maintainer decision,
   likely a phase-1-exit follow-up once the panel is confirmed to no longer spawn
   `vscode-agent`.
4. **Whether Node's `Readable.toWeb()`/`Writable.toWeb()` (the standard way to hand a
   `child_process`'s stdio to the SDK's Web-Streams-based `ndJsonStream`) behave correctly across
   every VS Code-supported Node/Electron version this extension ships against**, including any
   remote/WSL/SSH extension-host target `BridgeProcess.ts` already has to account for
   (`vscode.env.remoteName` handling, `BridgeProcess.ts:148-152`). Not tested — the spike used
   plain Node child_process events, not Web Streams. Resolve by: the same scratch-branch spike as
   item 1, run under `vscode-test`/`vscode-test.visual.js` against at least one remote target.
5. **Real download/popularity signal beyond GitHub stars.** The npm search snippet reported "14,360
   weekly downloads" for the *old, deprecated* package name; the new name's own weekly download
   count was not independently confirmed (npm's registry API used here doesn't expose it
   directly, and `npmjs.com`'s package page — which does — returned HTTP 403 to the fetch tool
   used in this spike). Resolve by: checking `npmjs.com/package/@agentclientprotocol/sdk` directly
   in a browser, or `npm view @agentclientprotocol/sdk` locally (which hits the registry API with
   full auth/UA and may succeed where this spike's fetch did not).
