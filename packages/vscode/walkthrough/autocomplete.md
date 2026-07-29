# Inline Autocomplete

Contenox requests inline editor suggestions from a **separate autocomplete
model** — not your chat model. Use a FIM/coder model when available.

This means chat can stay on a larger hosted model while ghost text comes from a
local edge model, a local-network Ollama server, or a dedicated cloud code model:

```sh
# Local modeld llama autocomplete:
contenox config set default-autocomplete-provider llama
contenox config set default-autocomplete-model qwen3-coder-30b-a3b

# OpenVINO/modeld autocomplete:
contenox config set default-autocomplete-provider openvino
contenox config set default-autocomplete-model qwen2.5-coder-1.5b-ov

# Local-network Ollama autocomplete:
contenox config set default-autocomplete-provider ollama
contenox config set default-autocomplete-model qwen2.5-coder:7b

# Hosted code model:
contenox config set default-autocomplete-provider mistral
contenox config set default-autocomplete-model codestral-latest
```

If you leave the autocomplete provider/model empty, Contenox uses runtime
defaults and may auto-route to a configured code backend. Set them explicitly
when you want predictable local, edge, or cloud behavior. Run `Contenox: Test
Autocomplete At Cursor` to check what the model returned.

Useful commands:

- `Contenox: Test Autocomplete At Cursor`
- `Contenox: Trigger Autocomplete`
- `Contenox: Enable Autocomplete`
- `Contenox: Disable Autocomplete`

Autocomplete uses workspace settings for prefix length, suffix length, debounce,
and maximum output tokens.
