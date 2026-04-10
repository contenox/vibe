# Chat modes & context injection (Cursor-like)

This document outlines a **clean path** toward Cursor-like **modes** (Ask vs agent-style, read-only vs tool-heavy, etc.) and **meaningful injection** of each mode’s **outputs** into the next turn’s chat context. It fixes **boundaries** so UI, HTTP API, persistence, and the chain engine stay separable.

## Goals

1. **Modes** are explicit: the user (or product defaults) picks a **policy** for the next turn or session — not only “which chain JSON” hidden in a long dropdown.
2. **Outputs of modes** can feed the **next** user or assistant turns in a **structured** way (summaries, retrieved snippets, plan state, review artifacts) — not only invisible side effects inside one chain run.
3. **Single chat thread** stays coherent: injected material is **visible to the model** and ideally **inspectable in the UI** (collapsible strips, badges), not buried in opaque history blobs.

## Non-goals (for early phases)

- Pixel-perfect parity with Cursor (Review queue UX, diff viewer) — those can layer on once **context bundles** exist.
- Replacing the task-chain engine — modes **map onto** chains and template variables; they do not fork execution.

---

## Terminology

| Term | Meaning |
|------|---------|
| **Mode** | A named product policy: autonomy level, default chain(s), allowed hooks, and **how** prior artifacts are merged into the prompt for the next request. |
| **Mode run** | One `POST /chats/{id}/chat` (or future batch) executed under a given mode. |
| **Artifact** | Structured output of a mode run: e.g. `{ kind: "grep", paths: [...], excerpt }`, `{ kind: "plan_snapshot", planId, steps[] }`, not only final assistant text. |
| **Context bundle** | The set of artifacts (plus optional file pointers) the **client and server agree** to attach for the **next** message. |
| **Injection** | Turning a context bundle into **chat history entries** and/or **template variables** before `taskService.Execute`. |

---

## Current baseline (runtime)

- **Beam** sends `POST /api/chats/{id}/chat` with body `{ message }` and query `chainId`, `model`, `provider` (`internal/internalchatapi`, `packages/beam/src/lib/api.ts`).
- **Execution** is always: load history → append user message → **one chain execution** → persist result (`chatservice.Manager`, `messagestore`).
- **Planning inside chat** can already exist via the **`plan_manager`** local hook inside a chain — tools talk to `planservice`; no separate chat API is required for that *mechanism*.

What is **missing** for Cursor-like behaviour is **first-class mode + artifact + injection** in the API and storage, not a second chat microservice.

---

## Boundaries (who owns what)

```
┌─────────────────────────────────────────────────────────────────┐
│  Beam UI                                                         │
│  • Mode selector, “context strip” (N files, plan badge, etc.)   │
│  • Displays persisted artifacts; never invents server truth       │
└────────────────────────────┬────────────────────────────────────┘
                             │ JSON: message + modeId + contextBundle?
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│  Chat HTTP API (internalchatapi)                                 │
│  • AuthZ, validation, request ID                                  │
│  • Resolves mode → default chainId / template vars / caps         │
│  • **Injection point**: merge context bundle → messages / vars   │
└────────────────────────────┬────────────────────────────────────┘
                             │
         ┌───────────────────┴───────────────────┐
         ▼                                       ▼
┌─────────────────────┐               ┌─────────────────────┐
│  Session / messages  │               │  taskService.Execute │
│  (chatservice + DB)  │               │  (task chains)       │
│  • Canonical history   │               │  • Unchanged model   │
│  • Optional artifact   │               │  • Hooks, tools     │
│    rows if needed      │               │  • plan_manager     │
└─────────────────────┘               └─────────────────────┘
```

### Rules of thumb

1. **Chain engine** stays **dumb about product modes**: it receives `ChatHistory` + `TaskChainDefinition` + `template vars`. It should not hard-code “Ask” vs “Agent”.
2. **Mode resolution** belongs at the **API boundary** (or a thin `modeservice` called only from the API): map `modeId` → `chainId`, hook allowlists, and **injection recipe**.
3. **Injection** should happen **once**, in one place: **after** loading stored history and **before** `Execute`, so every chain sees the same augmented history. Avoid duplicating injection in Beam and the server.
4. **Artifacts** that must survive refresh and multi-device use belong in the **DB** (or blob store keyed by chat); **ephemeral** injection can stay request-only for prototypes.

---

## Injection strategies (choose explicitly per phase)

### A. History injection (recommended default)

Append **synthetic messages** (usually `system` or `user` role) that contain serialized artifacts:

- Pros: Works with every chain; model sees a clear block; easy to debug.
- Cons: Grows token usage; need truncation/summarization policy for large bundles.

**Boundary:** Implemented in **internalchatapi** (or shared helper used only there), not inside individual task handlers.

### B. Template variable injection

Put compact strings into `taskengine.WithTemplateVars` (e.g. `mode`, `injected_context`, `plan_summary`):

- Pros: Small history; chains that already use `{{var:*}}` can branch.
- Cons: Every chain must **opt in** to use those vars; poor discoverability in generic chains.

**Boundary:** Combine with **A** for generic chains; use **B** only for curated chain families.

### C. First-class “context attachment” rows (later)

Store artifacts outside message text; render in UI; expand to pseudo-messages server-side:

- Pros: Clean UI (“11 files”), deduplication, hashing.
- Cons: More schema and migration work.

---

## Phased roadmap

### Phase 0 — Product vocabulary (no API change)

- Define **mode catalog** in config: `modeId` → `{ defaultChainId, description, hookPreset }`.
- Beam: replace opaque chain dropdown with **mode** dropdown that **sets** `chainId` (current API). Document that this is **UI-only** mapping.

**Boundary:** Beam config / constants only.

### Phase 1 — Extend the chat request contract

- Add optional fields to the chat request body, e.g.:

  ```json
  {
    "message": "…",
    "mode": "chat" | "prompt" | "plan",
    "context": {
      "artifacts": [ { "kind": "…", "payload": { } } ],
      "fileRefs": [ { "path": "…", "sha": "…" } ]
    }
  }
  ```

- Server: validate `mode`, resolve default `chainId` if not overridden, run **injection** (strategy A) to produce the `ChatHistory` passed to `Execute`.

**Boundary:** **internalchatapi** + OpenAPI/spec updates; Beam sends new fields.

### Phase 2 — Persist artifacts for meaningful follow-ups

- Store **per-message or per-session** artifact blobs (or references) so “outputs of those modes” are not re-sent manually by the client each time.
- Optional: **summarize** long artifacts into a rolling `context_summary` system message on the server.

**Boundary:** `chatservice` / `messagestore` schema extensions; API for listing artifacts for UI strip.

### Phase 3 — Review & autonomy (optional)

- **Review** as a separate resource or message type: pending edits keyed by `requestId`, approve/reject endpoints.
- **Auto** mode: server-side loop or client-orchestrated multiple `chat` calls with shared `mode` — policy lives in **mode definition**, not scattered in Beam.

**Boundary:** New routes or sub-resources; still one execution engine underneath.

---

## Mapping to existing Contenox features

| Feature | Role in this design |
|---------|---------------------|
| **Task chains** | Mode resolves to `chainId`; execution unchanged. |
| **`plan_manager` hook** | One way “Agent/Plan” mode gets tools; artifacts can include `plan_snapshot` after each run. |
| **`contenox plan` / plan API** | Can stay parallel; unified UX means **linking** active plan to chat session in Phase 2+. |
| **Template vars** | Optional enrichment from mode + injection layer; not the only path. |

---

## Open decisions

1. **Mode scope**: per **session** vs per **message** (per-message is more flexible; session default reduces UI noise).
2. **Artifact schema**: start with a small closed set (`file_excerpt`, `plan_snapshot`, `tool_trace_summary`) and version it.
3. **Token budget**: server-side truncation vs client-side preview vs async summarization chain.

---

## Implementation (runtime)

The chat turn pipeline lives in the **`chatsessionmodes`** package ([`chatsessionmodes/service.go`](../../chatsessionmodes/service.go)), not in HTTP handlers.

- **`chatsessionmodes.Service.SendTurn`** loads history, runs **mode-scoped injectors** (see [`registry.go`](../../chatsessionmodes/registry.go)), resolves the chain via **`MapChainResolver`** ([`chain_resolve.go`](../../chatsessionmodes/chain_resolve.go)), builds template vars (including `mode`), calls **`taskService.Execute`**, and persists via **`chatservice.Manager`**.
- **`ClientArtifactInjector`** maps request `context.artifacts` to system messages ([`inject_artifacts.go`](../../chatsessionmodes/inject_artifacts.go)).
- **`ActivePlanInjector`** injects `[Context kind=active_plan]\n{json}` from **`planservice.Service.Active`** when mode is **`plan`** ([`inject_plan.go`](../../chatsessionmodes/inject_plan.go)).
- Product modes **`chat`**, **`prompt`**, **`plan`** default to **`default-chain.json`** until separate VFS chains are configured ([`DefaultChainByMode`](../../chatsessionmodes/chain_resolve.go)).
- **[`internal/internalchatapi`](../../internal/internalchatapi/chatroutes.go)** decodes JSON, maps errors to HTTP status, and delegates **`POST /chats/{id}/chat`** to **`SendTurn`** only.

---

## Summary

- **Mode + injection** are implemented in **`chatsessionmodes`**, composed at construction (`New` + **`ModeRegistry`**), so HTTP stays thin.
- **Context bundles** from the client and **active plan snapshots** for **plan** mode both use the same injection string format (`[Context kind=…]`).
- **Phased** delivery: persisted artifact rows / review / further mode-specific chains can build on this service boundary.
