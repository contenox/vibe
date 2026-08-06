---
title: Ollama
description: Connect Contenox to a local Ollama instance or Ollama Cloud.
---

# Ollama

Ollama runs models locally on your machine — no API key, no data leaving your network. Together with contenox's local SQLite state, this is the fully sovereign deployment option — see [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/).

## Local Ollama

Install Ollama from [ollama.com](https://ollama.com), pull a model, then register it:

```bash
ollama pull qwen3:8b

contenox backend add ollama --type ollama
contenox config set default-model qwen3:8b
contenox config set default-provider ollama
```

## Ollama Cloud

Get an API key at [ollama.com/settings/keys](https://ollama.com/settings/keys), then:

```bash
export OLLAMA_API_KEY=your-key

contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
contenox model list
contenox config set default-model <name-from-list>
contenox config set default-provider ollama
```

## See also

- [Configuration reference](/docs/reference/config/)
