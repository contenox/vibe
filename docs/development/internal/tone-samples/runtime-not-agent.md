---
title: "AI Coding Is Becoming a Runtime Problem, Not an Agent Problem"
description: The agent UI is the visible part of AI coding. The hard problem — model routing, tool execution, policy, logs, approvals, and session isolation — has moved into the runtime underneath it.
---

# AI Coding Is Becoming a Runtime Problem, Not an Agent Problem

A few things happened in the same stretch of months, and the pattern was easy to miss if you were only watching product announcements.

AI coding is moving from "a cloud feature bolted onto an IDE" to "runtime infrastructure." The agent UI — the chat pane, the inline diff, the accept/reject buttons — is the visible part. The hard problem has moved underneath it, into the runtime: how a request gets routed to a model, how a tool call actually gets executed, what policy governs that execution, what gets logged, what needs a human's approval before it runs, and how one session stays isolated from another.

A few signals point the same direction. Billing across coding-assistant vendors has been shifting toward usage-based, per-token pricing, with only the narrowest completions still bundled into flat plans — chat, CLI agents, cloud agents, and third-party coding agents are now visibly inside the token economy, not a free extra bolted onto a subscription. Hosted-agent APIs keep growing new surface area: tool calling, file search, code execution, remote MCP servers, background execution, tracing. That is not "just another endpoint." That is runtime shape — model plus tools plus state plus orchestration — being sold as a product in its own right. Remote frontier-model access keeps proving itself an unstable primitive: availability can change on policy, region, account rules, pricing, or plain capacity, with no warning built into the interface that depends on it. And deskside, team-local AI compute is being marketed again as a serious category, not a hobbyist fallback.

None of this is really about picking a winner. The old question was: which AI coding agent should I use? The new question is: where does the agent actually run?

Because serious agent work needs, at minimum:

- model routing across OpenAI- and Ollama-compatible APIs
- tool execution against the filesystem and the shell
- policy enforcement over what a tool call is allowed to do
- logs and audit trails a security team can actually read
- approval gates a human sits in front of
- session isolation between concurrent runs
- model capability metadata, not vendor marketing copy
- fallbacks for when a provider changes terms, quota, or availability

That is why the ecosystem is more interesting than any single winner. Ollama is the easiest local-model entry point. Open-source coding-agent layers like OpenCode and Kilo Code are where that entry-level model gets assembled into something usable. vLLM is the serving path once a team needs to run this for real, at real concurrency. Frontier hosted APIs still earn their place when a task genuinely needs top-tier capability. None of these replace each other — they read as layers of the same stack, not competing final answers.

I used to frame this mostly as an agent problem. I now think that framing was too small.

The agent is the proof workload. The runtime is the product.

---

*Originally published on r/LLMDevs and r/ollama, June 2026.*
