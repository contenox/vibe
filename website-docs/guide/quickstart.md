# Quickstart

Get a working AI agent on your machine in 5 minutes.

## Prerequisites

- [Ollama](https://ollama.com) running locally with at least one model pulled:
  ```bash
  ollama pull qwen2.5:7b
  ```
- Go 1.22+ (for building from source) **or** a pre-built `vibe` binary

## Install vibe

**From source:**
```bash
git clone https://github.com/contenox/runtime
cd runtime
go install ./cmd/vibe
```

## Initialize a workspace

```bash
mkdir my-agent && cd my-agent
vibe init
```

This creates `.contenox/` with a default config and chain:
```
.contenox/
├── config.yaml          ← model, backend settings
└── default-chain.json   ← the default task chain
```

## Start chatting

```bash
vibe "what is the capital of France?"
# → Paris.

vibe "list files in the current directory" --enable-local-exec
# → the model calls local_shell with `ls` and returns the result
```

`vibe` without a subcommand is interactive — type your message and press Enter. `Ctrl+D` exits.

## Run a chain explicitly

```bash
vibe exec --chain .contenox/default-chain.json --input-type chat "explain recursion briefly"
```

## Add a remote API as a tool

```bash
# US National Weather Service — free, no API key
vibe hook add nws --url https://api.weather.gov --timeout 15000
vibe hook show nws       # lists 60 discovered tools

vibe exec --chain .contenox/chain-nws.json --input-type chat \
  "how many active weather alerts are there right now?"
```

## Next steps

- [Core Concepts](/guide/concepts) — understand chains, tasks, and hooks
- [Chains reference](/chains/) — write your own chains
- [vibe CLI reference](/reference/vibe-cli) — all flags and subcommands
