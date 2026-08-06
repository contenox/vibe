---
title: Multi-provider fallback as authored resilience
description: The vendor doesn't choose your failure mode. You do.
---

# Multi-provider fallback as authored resilience

Chain a candidate list of models and providers with a retry policy, so a rate limit or outage on one provider falls through to the next instead of failing the task.

## Prerequisites

- `contenox init` has run in this project.
- A backend registered for each provider in the candidate list (e.g. `ollama`, `openai`, `gemini`) — see [Quickstart](/docs/guide/quickstart/) and the provider integration pages.

## Steps

1. Add `models`, `providers`, and `retry_policy` to a task's `execute_config`:

   ```json
   {
     "id": "summarise",
     "handler": "chat_completion",
     "system_instruction": "Summarise the input in two sentences.",
     "execute_config": {
       "models": ["qwen3:8b", "gpt-5-mini", "gemini-flash-latest"],
       "providers": ["ollama", "openai", "gemini"],
       "retry_policy": {
         "max_attempts": 3,
         "initial_backoff": "1s",
         "max_backoff": "10s",
         "jitter": 0.25,
         "rate_limit_min_wait": "10s"
       }
     },
     "transition": {
       "on_failure": "summarise_locally",
       "branches": [{ "operator": "default", "goto": "end" }]
     }
   }
   ```

2. Add the `on_failure` target task (`summarise_locally` above) — what runs when every candidate in the list is exhausted. It can call a smaller local model, truncate the input, or return a fixed message.

3. Run the chain as usual. If the primary model/provider fails transiently, the engine retries with backoff before falling through to the next candidate — no separate flag needed.

## Expected outcome

One task now encodes a four-level resilience policy:

1. Try `qwen3:8b` on Ollama. On a transient failure, retry with backoff and jitter (3 attempts).
2. If Ollama's retries exhaust, fall over to `gpt-5-mini` on OpenAI and retry there.
3. If OpenAI also exhausts, try `gemini-flash-latest` on Gemini.
4. If all three fail, `transition.on_failure` routes to `summarise_locally`.

## Customize

- **Provider order** — the `providers` array order is the fallback order; put your primary provider first.
- **Retry shape** — `max_attempts`, `initial_backoff`, `max_backoff`, `jitter`, and `rate_limit_min_wait` are all yours to tune. Tighter for a CI step with a hard deadline; looser for an overnight batch job.
- **Terminal behavior** — `on_failure` names the task that runs when the whole candidate list is exhausted; write whatever degradation makes sense (local fallback, truncation, a "try later" message).

## Where to next

- [The moderation gate](/docs/use-cases/moderation-gate/) — routing on model output rather than on failure.
- [HITL policies](/docs/guide/hitl/) — the same authored-policy pattern applied to tool approval instead of retries.
