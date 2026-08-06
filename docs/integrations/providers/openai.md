---
title: OpenAI
description: Connect Contenox to OpenAI or any OpenAI-compatible endpoint.
---

# OpenAI

Any OpenAI-compatible endpoint works — OpenAI, vLLM, LM Studio, or your own proxy.

```bash
export OPENAI_API_KEY=your-key

contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
contenox config set default-model gpt-5-mini
contenox config set default-provider openai
```

For a custom endpoint (vLLM, LM Studio, etc.) add `--url`:

```bash
contenox backend add local-vllm --type openai --url http://localhost:8000/v1
```

Local servers typically need no key; if yours does, add `--api-key-env <VAR>`.

Contenox also has a native `vllm` backend type (`contenox backend add myvllm --type vllm --url http://localhost:8000`); the `--type openai` route works for any OpenAI-compatible server. If you want a commercially supported vLLM for production, Red Hat ships one as [Red Hat AI Inference Server](https://www.redhat.com/en/products/ai/inference).

## EU data residency

OpenAI offers European data residency for eligible API customers. It is configured per project: create a **new** project with Europe as its region (existing projects cannot be switched), and requests for that project go through the regional endpoint `eu.api.openai.com`, where OpenAI states eligible traffic is handled in-region with zero data retention. Point contenox at it with `--url`:

```bash
contenox backend add openai-eu --type openai --url https://eu.api.openai.com/v1 --api-key-env OPENAI_API_KEY
```

Eligibility, covered endpoints, and mechanics are OpenAI's to define — check [OpenAI's data residency documentation](https://help.openai.com/en/articles/10503543-data-residency-for-the-openai-api) before relying on it. See [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) for how region pinning compares to self-hosted inference.

## See also

- [Configuration reference](/docs/reference/config/)
