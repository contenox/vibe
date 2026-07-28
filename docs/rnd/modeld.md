---
title: "modeld: the local inference daemon"
description: Contenox's cross-backend local inference daemon — a live-hardware capacity planner, lease-based multi-client ownership, and llama.cpp/OpenVINO backends behind one gRPC contract.
---

# modeld: the local inference daemon

`modeld` was Contenox's dedicated local inference daemon: a separate process that ran on your machine (or a remote GPU box) and owned hardware resources on behalf of every frontend — the CLI, the VS Code extension, Beam, and any ACP editor — talking to the pure-Go runtime over gRPC. It served GGUF models through llama.cpp and OpenVINO GenAI models through a shared session contract, so the runtime itself never linked native inference code.

Its most distinctive piece was the capacity planner: on opening a session it took a live free-memory snapshot of the target device, parsed the model's own metadata (layer count, KV heads, sliding-window pattern), and computed exactly what context window actually fit right now — shedding GPU layers if needed to guarantee a usable "hot" context floor rather than guessing from a static config. A cooperative, cross-process lease file gave one daemon per data root a single owner, so multiple frontends could share one resident model and one GPU without fighting over it, while an idle reaper returned memory to the system after inactivity. Sessions kept KV state warm across turns, could snapshot and restore for branching, and a single `modeld` binary registered as a remote backend served a laptop and a dedicated GPU box through the exact same protocol. Later builds added native vision (VLM) support through llama.cpp's multimodal stack, with capability truth reported per model rather than inferred from its name.

## What it proved

- **A live-hardware capacity planner beats static config.** Computing effective context from an actual memory snapshot plus real KV math — including GQA and sliding windows — solved the "why did this just OOM" problem instead of hand-waving it.
- **Lease-based ownership lets many frontends share one local model safely.** CLI, editor, and browser sessions could all point at the same resident model without a central server process owning everything.
- **A stateful session boundary, not a stateless provider interface, is the right shape for local inference.** Warm KV, prefix reuse, and durable snapshots only make sense once the daemon owns session lifetime.
- **Refuse-don't-spill beats silent degradation.** An explicit, typed context-overflow refusal proved more useful to an agent than a quietly truncated or CPU-spilled response.

## Where this lives now

Local inference now rides Ollama and vLLM directly; the capacity-aware, session-first thinking modeld proved out still shapes how the runtime treats any local backend.
