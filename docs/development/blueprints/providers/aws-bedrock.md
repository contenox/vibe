# Blueprint: AWS Bedrock provider

Status: implemented — the provider ships in `internal/models/modelrepo/bedrock`. Kept as the assessment and design record.

Bedrock is doable and fits the provider/codec pattern. The central choice is
**whether to take the `aws-sdk-go-v2` dependency** — it flips which parts are
easy vs. hard. There is currently no AWS dependency in `go.mod` and no Bedrock
code in the tree.

## 1. What Bedrock is

Multi-model marketplace (Claude, Llama, Mistral, Amazon Nova/Titan, Cohere,
AI21, …) on `bedrock-runtime.<region>.amazonaws.com`. Two API styles:
- **InvokeModel / …WithResponseStream** — vendor-native body per model (same
  per-publisher divergence as Vertex; Claude here uses Anthropic Messages with
  `anthropic_version: "bedrock-2023-05-31"`).
- **Converse / ConverseStream** — AWS's **unified** schema (messages, content
  blocks, `toolConfig`, `system`, `inferenceConfig`) across nearly all models.
  This is the abstraction Vertex never built. **Recommended target.**

## 2. THE decision: take aws-sdk-go-v2 or stay zero-dep

The original draft notes (preserved in §6) recommend the SDK path, and for
Bedrock specifically that's a reasonable call — unlike the Vertex/direct
providers where a bearer/api-key header was trivial, Bedrock's native auth and
streaming are genuinely easier with the SDK.

| Concern | With `aws-sdk-go-v2` (draft's path) | Zero-dep (Bedrock API key) |
|---|---|---|
| **Auth** | Full credential chain: env, profile, IAM role, instance metadata, AssumeRole; auto-refresh. SigV4 handled. | Bedrock API key (long-lived bearer) → `Authorization: Bearer`. No IAM roles/instance creds. |
| **Streaming** | `ConverseStream` returns a **typed event union** — switch on event type, SDK decodes the binary `vnd.amazon.eventstream` framing for you. | Must hand-roll the binary eventstream decoder (prelude + headers + payload + CRC32), ~200–300 LOC. |
| **Dependency** | New, large transitive tree in `go.mod`. | None. |
| **Fits existing transport** | No — uses the SDK client, not our `net/http` pattern. | Yes — same shape as anthropic/mistral providers. |

So: **SDK = less of our own code, standard AWS auth, streaming free, but a big
dependency and a different transport shape.** **Zero-dep = consistent with the
rest of modelrepo and dependency-free, but we own SigV4-avoidance (bearer only)
and the eventstream decoder.**

Recommendation: if IAM/role-based auth or instance credentials matter to
deployments, take the SDK (the draft's call). If the deployment can use Bedrock
API keys and we value a zero-dep, uniform `net/http` transport, go zero-dep and
land non-streaming first.

## 3. Codec story (cleaner than Vertex either way)

Target **Converse**: one new `internal/models/modelrepo/codec/converse` package —
build the Converse request (messages→content blocks, `system`, `toolConfig`
from neutral `Tool`, `inferenceConfig` from `ChatConfig`), decode the response
(`output.message.content[]` → text + `toolUse`), and a ConverseStream decoder
(`contentBlockDelta` / `messageStop` → text/thinking + tool-arg assembly).
Covers nearly every Bedrock model with one codec. Mirrors `codec/messages`.

Tool protocol (from the draft, confirmed): function defs go in `toolConfig`,
calls come back as `toolUse` content blocks → map to/from contenox's neutral
(OpenAI-shaped) `ToolCall`. Straightforward but real code.

Fallback: legacy InvokeModel-only models can reuse the `messages` codec for
Claude. Pick the Converse subset and skip the rest (draft §6).

## 4. Two real-world traps (must handle, or it "looks broken")

1. **Account-level model enablement** (draft): AWS requires explicit per-model
   enablement in the Bedrock console before any API call works. New backends
   return empty model lists / `AccessDeniedException: model not enabled` until
   the customer enables models. Surface this clearly in setupcheck — otherwise
   it reads as a broken backend. (Analogous to, but worse than, the Vertex
   region/enablement gotcha.)
2. **Per-region model IDs**: model IDs differ by region; `--url`/region is
   required and cannot be inferred. `bedrock.ListFoundationModels` reports what's
   actually accessible per region (control-plane perms needed) — else use a
   curated static list.

## 5. Implementation checklist (when greenlit)

New code:
- `internal/models/modelrepo/codec/converse/` — codec + golden/stream tests.
- `internal/models/modelrepo/bedrock/` — `client.go` (transport per §2), `provider.go`,
  `catalog.go` (register `"bedrock"`; ListModels via `ListFoundationModels` or
  static; default chat caps; map `AccessDeniedException` → enablement hint),
  `types.go`, httptest round-trip tests (zero-dep path) or SDK-mock tests.

Wiring (new type `bedrock`, wired nowhere today — same checklist the direct
providers used):
- `runtimestate/catalogimports.go` — blank import
- `runtimestate/state.go` — dispatch (bearer path can reuse `processOpenAIBackend`; SDK/SigV4 path likely needs its own handler)
- `runtimestate/provider.go` — `BedrockKey`
- `runtimestate/catalogstate.go` — `providerConfigKey`
- `backendservice/backendservice.go` — type validation
- `contenoxcli/backend_cmd.go` — region-templated URL + `--type` help (require `--url`/`--region`)
- `contenoxcli/init.go` — `providerConfigs`
- `internal/setupcheck/setupcheck.go` — display name + diagnostic/help/enablement cases

Backend-type shape: a single `bedrock` (Converse unifies everything) is
preferred over the Vertex-style `bedrock-anthropic`/`bedrock-meta` split.

## 6. Original draft notes (preserved)

> Easy parts:
> - Auth: AWS SDK Go v2 credential chain (env vars, profile, IAM role, instance metadata, AssumeRole) is mature — drop-in similar to Vertex's ADC but without the refresh-token nightmare. Credentials renew automatically through the SDK.
> - The Converse API (bedrockruntime.Converse / ConverseStream) normalizes across Anthropic, Meta, Mistral, Cohere, AI21, Titan. One request/response shape for almost all models — far less translation than per-model formats.
> - Streaming events are a typed union; switch on event type, done.
>
> Friction parts:
> - Some legacy Bedrock models are InvokeModel-only, not Converse. Pick the Converse subset and skip the rest.
> - Tool-use protocol differs from OpenAI's: function definitions go in ToolConfig, tool calls come back as ToolUse content blocks. Mapping to contenox's internal tool-call format is straightforward but real code.
> - Per-region per-model availability — Bedrock model IDs differ by region. The catalog endpoint (bedrock.ListFoundationModels) tells you what's actually accessible per region.
> - Account-level model enablement is the canonical UX trap. AWS requires explicit enablement of each model in the Bedrock console before any API call works. New backends will return empty model lists until customers do that. UX needs clear handling for AccessDeniedException: model not enabled — otherwise it looks like the backend is broken.
>
> Maps onto internal/modelrepo/bedrock/ cleanly — mirror the vertex package structure (auth, catalog, chat, stream, types, tests). Probably 2500-4000 LOC including tests. Backend types: either a single bedrock since Converse unifies them, or bedrock-anthropic / bedrock-meta / etc. matching the Vertex family-split pattern.

## 7. Open decisions
1. **Dependency** (§2) — `aws-sdk-go-v2` (SDK auth + free eventstream) vs zero-dep (Bedrock API key + hand-rolled eventstream). *Gates everything.*
2. **Converse vs InvokeModel** (§3) — recommend Converse.
3. **Streaming now or follow-up** — non-streaming Converse is cheap; the eventstream decoder is the costly part only on the zero-dep path.
4. **Catalog source** — `ListFoundationModels` (control-plane perms) vs curated static list.
5. **Backend-type shape** — single `bedrock` (recommended) vs `bedrock-*` split.
