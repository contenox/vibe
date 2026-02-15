# Contenox Vibe

![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)
![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)

Vibe is the local CLI for task-chain orchestration: SQLite, in-memory bus. No Postgres or NATS required. Default setup uses [Ollama](https://ollama.com); you can add OpenAI, vLLM, or Gemini in `.contenox/config.yaml`.

## Install (copy-paste)

Set the release version and download the binary for your OS/arch. The snippet below uses the version in the next line; after a new release, update it from [Releases](https://github.com/contenox/vibe/releases).

```bash
TAG=v0.0.76
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac
curl -sL "https://github.com/contenox/vibe/releases/download/${TAG}/vibe_${TAG}_${OS}_${ARCH}.tar.gz" -o vibe.tar.gz
tar xzf vibe.tar.gz
./vibe init
./vibe list files in my home directory
```

The default chain is **vibes**: natural language → shell commands (the model uses `local_shell` to run e.g. `ls`, `pwd`). Run Ollama (`ollama serve`) and pull a tool-capable model (e.g. `ollama pull qwen2.5:7b`). Run `vibe init` once to create `.contenox/` with the vibes default; then use `vibe <input>`, `vibe --input '…'`, or pipe stdin. To use OpenAI, vLLM, or Gemini, add `backends` and `default_provider` / `default_model` in `.contenox/config.yaml` (see [docs/contenox-vibe.md](docs/contenox-vibe.md)).

Manual download: [Releases](https://github.com/contenox/vibe/releases) — pick `vibe_<tag>_<os>_<arch>.tar.gz` for your platform.

-----

> for further information contact: **hello@contenox.com**
