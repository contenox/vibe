# Configuration

The Contenox runtime is configured via `.contenox/config.yaml`.

When you run `vibe init`, a default configuration is generated. `vibe` looks for this directory in your current working directory, then walks up to the root, and finally checks your home directory (`~/.contenox/`).

## Default `config.yaml`

```yaml
version: 1
default_provider: ollama
default_model: qwen2.5:7b
providers:
  ollama:
    base_url: http://localhost:11434
  openai:
    api_key: ""
```

## Settings

| Key | Description | Example |
|-----|-------------|---------|
| `default_provider` | The fallback provider if a chain doesn't specify one | `ollama` |
| `default_model` | The fallback model if a chain doesn't specify one | `gpt-4o` |
| `providers.<name>` | Provider-specific connection settings | see below |

## Supported Providers

Contenox uses a unified translation layer, meaning you can swap providers per-task in your chains without changing the prompt format or tool schemas.

1. **`ollama`**: Requires `base_url` (usually `http://localhost:11434`).
2. **`openai`**: Requires `api_key` (or uses `OPENAI_API_KEY` from environment).
3. **`vllm`**: Exposes an OpenAI-compatible endpoint. Requires `base_url`.
4. **`gemini`**: Requires `api_key`.
