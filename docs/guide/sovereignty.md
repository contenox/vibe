---
title: AI Sovereignty & the EU AI Act
description: What sovereignty means operationally with contenox — you choose the hosting, state stays in local SQLite, oversight is a policy you authored — and how those controls map to the EU AI Act's oversight and transparency themes.
---

# AI Sovereignty & the EU AI Act

Sovereignty over an AI system (in German: **AI-Souveränität**) is not a feature you switch on. It is a set of concrete operational questions: where does inference run, where does state live, who holds the credentials, what can the agent reach, and who decides when a human must intervene. Contenox is built so that every one of those answers belongs to the operator — and each answer is a file, a flag, or a grant you can read, version, and revoke.

That matters most where contenox is typically embedded: tool-heavy orchestration, request-processing and analytics chains, scripts and pipelines — the places where an agent's effects reach real systems and the operator has to account for them.

## What sovereignty means operationally

- **You pick the hosting.** Run inference fully locally via [Ollama](/docs/integrations/providers/ollama/) or [vLLM](/docs/integrations/providers/openai/) — no prompt or response leaves your network — or register a cloud backend against your own account, pinned to a region you choose. Providers are configuration, not architecture: the same chain runs against either.
- **State stays on your machine.** Sessions, configuration, run logs, and captured execution state live in a local SQLite database (`~/.contenox/local.db` by default). No account is required, and no contenox service is in the loop unless you deliberately [pair a machine with the relay](/docs/guide/pairing/) — an install that never pairs contacts none; telemetry is opt-in and off by default.
- **Secrets resolve from your environment at request time.** Backends reference credentials by environment-variable name (`--api-key-env`); the value is read when a request is made and never lands in a config file on disk.
- **Agents see what you grant, not what you have.** Every agent-reachable shell gets a [scrubbed, least-privilege environment](/docs/guide/environment-scrubbing/). Chains expose tools through a [per-invocation allowlist](/docs/guide/concepts/#tools). Sessions run only inside the [workspace roots](/docs/reference/contenox-cli/#workspace-roots) the operator configured — the launch directory, the roots granted with `contenox workspace add`, and any passed for that run — and never inside the runtime's own config, database, or policies, which no setting can open. And for foreign agent code, contenox carries a kernel-enforced, fail-closed [sandbox](/docs/guide/agent-sandbox/) — see the [threat model](/docs/guide/agent-threat-model/) for why that wall is structural rather than cooperative, and the sandbox guide for exactly what it does and does not confine.

## Mapping to the EU AI Act's oversight themes

The EU AI Act (Regulation (EU) 2024/1689) asks, among other things, that high-risk AI systems be designed for **effective human oversight** — its Article 14 language includes the ability of the natural persons overseeing a system to understand it, to intervene in its operation, and to interrupt it — alongside obligations around transparency, record-keeping (Article 12), and risk management. These are the questions an operator asks anyway before leaving an agent alone with real work; the Act happens to ask the same ones. Contenox does not interpret the Act for you. What it gives you are operator-authored mechanisms that map naturally onto those themes:

| Oversight theme | Contenox mechanism |
|---|---|
| **Human oversight** — a person can intervene in or interrupt the system's operation | [HITL policies](/docs/guide/hitl/) as human-in-the-loop checkpoints: authored allow/approve/deny rules evaluated before any tool call executes, failing closed to approval when nothing matches. The [durable approvals inbox](/docs/reference/contenox-cli/#contenox-approvals) checkpoints an unanswered ask instead of timing out — the question waits for a human, and answering it from any terminal resumes the run exactly once. A [parked turn announces](/docs/guide/hitl/#what-a-parked-approval-looks-like) that it is suspended rather than ending silently, and re-presents the open question to a client that reconnects, so a waiting decision stays visible to the person responsible for making it. [Attention bounds](/docs/guide/hitl/#who-may-answer-a-units-question-attention) state who may answer an escalated question: a human by default, an agent only if the envelope says so, and only a bounded number of times. |
| **Traceability and record-keeping** | The audit trail is local and readable: [`contenox state`](/docs/reference/contenox-cli/#contenox-state) inspects the captured execution state of past runs — per-task steps, handlers, transitions, and timings per request. `--trace` emits structured operation telemetry on stderr. Durable asks record who answered — and whether it was a person or an agent. Chains and policies are plain versioned files, so the configuration that produced a run is diffable. |
| **Risk controls** | Authored deny rules and condition operators (path globs, host matching, command blacklists, substitution detection) in the [policy file](/docs/guide/hitl/#policy-file-format). An LLM [moderation gate](/docs/use-cases/moderation-gate/) as an ordinary chain step, on a model you choose. [Compute bounds](#compute-and-attention-bounds) capping a mission's total spend. Per-invocation [tool allowlists](/docs/guide/concepts/#tools) and [scoped workflow credentials](/docs/use-cases/nested-permission-bomb/). `contenox vet` validates chains and envelopes before anything runs them, and warns on fields that read stronger than they are enforced. |
| **Data governance** | Local SQLite state, [environment scrubbing](/docs/guide/environment-scrubbing/), secrets resolved from env at request time, region-pinned backends ([below](#sovereign-deployment-options)), and [workspace roots](/docs/reference/contenox-cli/#workspace-roots) bounding where sessions — and the missions they dispatch — may operate. |

> **Note:**
> This is not legal advice, and using contenox does not make a deployment compliant with the EU AI Act. Whether the Act's obligations apply to your system, and whether a given configuration satisfies them, depends on what you build and deploy — that assessment is yours and your counsel's. What contenox provides are the operational controls such an assessment can point at: authored, versioned, and inspectable rather than implicit.

## Compute and attention bounds

An envelope — the same HITL policy file that gates tool calls — can also carry a `compute` block that puts a ceiling on a mission's total spend, and an `attention` block that says who may answer the unit's questions:

```json
{
  "default_action": "approve",
  "rules": [],
  "compute": {
    "maxTurns": 40,
    "maxToolCalls": 200,
    "maxTokens": 2000000,
    "modelAllowlist": ["qwen3:8b"],
    "backendAllowlist": ["ollama"],
    "onExhausted": "finish_stuck"
  },
  "attention": { "allowAgentAnswers": false }
}
```

- Every compute bound is a **ceiling and opt-in**: absent or zero means unbounded, and bounds only ever restrict — they never grant.
- `maxTurns` is enforced host-side. `maxToolCalls` is validated but not yet enforced by the shipped hosts. `maxTokens` is best-effort, enforced when the unit reports usage.
- `modelAllowlist` and `backendAllowlist` are enforced at the point where a model is resolved, covering chat, prompt, streaming, and embedding calls. A unit cannot switch itself to a model or backend you did not name — which is how you pin an unattended mission to local inference only.
- Exhaustion is never silent: a mission that crosses a bound finishes as stuck rather than running on. (`onExhausted: "pause_ask"` is not implemented and is rejected at validation — an envelope that sets it fails to load and fails `contenox vet`; use `finish_stuck`.)
- The `attention` block is documented in the [HITL guide](/docs/guide/hitl/#who-may-answer-a-units-question-attention): by default only a human may answer a unit's escalated question; an envelope can hand a bounded number of routine questions to the firing agent, and the durable record always shows who answered.

Unknown fields in a `compute` block fail the policy load rather than silently running the mission unbounded.

## Sovereign deployment options

**Fully local: Ollama.** [Ollama](/docs/integrations/providers/ollama/) runs models on your own machine — no API key, no data leaving your network. Combined with local SQLite state and env-resolved secrets, nothing about the deployment depends on an external party. This is the strongest sovereignty posture contenox supports, and the default path in the [Quickstart](/docs/guide/quickstart/). It is also what makes contenox a self-hosted Copilot alternative: your rules, your models, your machine — instead of an assistant whose behavior and telemetry belong to the vendor.

**Self-hosted serving: vLLM.** For serving open models on your own GPUs at higher throughput, contenox has a native `vllm` backend type and also speaks to vLLM through its [OpenAI-compatible endpoint](/docs/integrations/providers/openai/). vLLM is an open-source project with substantial backing from Red Hat, which ships a hardened, commercially supported distribution as [Red Hat AI Inference Server](https://www.redhat.com/en/products/ai/inference) — the route to take if you want a vendor on the hook behind your self-hosted inference.

**EU-region cloud.** When you use hosted models, you can still pin where requests are processed, on your own account and keys:

- [AWS Bedrock — EU regions](/docs/integrations/providers/bedrock/#eu-regions): a `bedrock-runtime.eu-central-1.amazonaws.com` (Frankfurt) or other EU-region URL, with `eu.`-prefixed inference profiles.
- [Vertex AI — EU regions and data residency](/docs/integrations/providers/vertex/#eu-regions-and-data-residency): regional endpoints such as `europe-west4` (Netherlands) or `europe-west3` (Frankfurt) keep ML processing in the pinned region; the global endpoint does not.
- [OpenAI — EU data residency](/docs/integrations/providers/openai/#eu-data-residency): eligible API projects created with Europe as their region, served via `eu.api.openai.com`.

A region-pinned cloud backend is a weaker posture than local inference — the provider's terms and infrastructure are still in the loop — but the account, the region, the keys, and the decision remain yours, and swapping to a local backend later is a configuration change, not a rewrite.

## Human + AI collaboration

Sovereignty is not only about where computation happens — it is about who decides. Contenox treats Human + AI collaboration as an authored artifact: the [HITL policy](/docs/guide/hitl/) you wrote decides which actions run unattended, which pause for a person, and which are denied outright. Because asks are durable, that collaboration survives process boundaries — a question a unit cannot decide alone waits in the [approvals inbox](/docs/reference/contenox-cli/#contenox-approvals) for a human answer instead of timing out into a default. The division of labor between you and the agent is a file you can read, review, and change — not a vendor's default you discovered after the fact. That is what "trustworthy AI" means mechanically here: written rules instead of hidden prompts, budgets instead of hope, traces instead of guesswork.

## Next steps

- [HITL policies](/docs/guide/hitl/) — the policy format, condition operators, presets, and attention bounds.
- [Why contenox confines agents](/docs/guide/agent-threat-model/) — the threat model behind structural confinement.
- [Least-privilege shell environment](/docs/guide/environment-scrubbing/) — scrub-and-inject for every agent-reachable shell.
- [The pause is yours to define](/docs/use-cases/authored-approval/) — writing and activating your own approval policy.
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — scoped workflow credentials instead of inherited human access.
