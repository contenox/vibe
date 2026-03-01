# Introduction

Contenox is a self-hostable runtime for building **observable, deterministic AI workflows** as explicit state machines.

Instead of wiring prompts together with ad-hoc Python glue, you define your AI behaviour as a **task chain** — a JSON graph of typed tasks, transitions, and tool calls. Every step is inspectable, replayable, and testable.

## Three editions

| Edition | Use case | Entry point |
|---------|----------|-------------|
| **vibe** CLI | Local AI agent on your machine | `vibe run` |
| **Runtime API** | Self-hosted REST backend for apps | Docker / `go run` |
| **Enterprise (EE)** | Multi-tenant, dashboard, RBAC | `enterprise/` |

**vibe is the flagship.** This documentation focuses on it first. The runtime API and EE share the same chain / hook / task engine underneath.

## How it works

```
User input
    │
    ▼
┌─────────────────────┐
│   Task Chain (JSON) │  ← you define this
│  task → task → …   │
└─────────────────────┘
    │
    ▼
Model (Ollama / OpenAI / vLLM / Gemini)
    │
    ├─ tool call? → Hook (local shell, remote API)
    │                    │
    └─ text reply ←──────┘
```

Each task has a **handler** (what it does), an optional **LLM config** (which model, which hooks), and a **transition** (where to go next). The chain engine drives the loop — the model doesn't.

## Next steps

- [Quickstart](/guide/quickstart) — install vibe and run your first chain in 5 minutes
- [Core Concepts](/guide/concepts) — chains, tasks, hooks, transitions explained
- [Chains reference](/chains/) — build your own chains from scratch
