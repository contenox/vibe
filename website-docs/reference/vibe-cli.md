# vibe CLI Reference

`vibe` is the local AI agent CLI. It runs the Contenox chain engine entirely on your machine.

## Global Flags

| Flag | Description |
|------|-------------|
| `--trace` | Print verbose chain execution logs |
| `--steps` | Stream intermediate task names and tool executions |
| `--enable-local-exec` | Opt-in to allow the model to run shell commands (`local_shell` hook) |
| `--model <name>` | Override the model defined in `.contenox/config.yaml` |

## Subcommands

### `vibe run` (or just `vibe`)

Starts an interactive chat session using the default chain (`.contenox/default-chain.json`).

```bash
vibe "what is the capital of France?"
vibe   # enters interactive REPL mode
```

### `vibe exec`

Executes a specific chain non-interactively. Useful for wiring Contenox into bash scripts or CI pipelines.

```bash
vibe exec --chain .contenox/chain-nws.json --input-type chat "how is the weather?"
```

- `--chain <path>`: Required. Path to the chain JSON file.
- `--input-type <type>`: How to parse the positional argument. `chat` treats it as a user message. `string` treats it as raw string input. Defaults to `string`.

### `vibe plan`

Autonomous multi-step execution using a separate "planner" model that directs an "executor" model.

```bash
vibe plan "analyze main.go, find the bug, and write a fix to patch.diff" --enable-local-exec
```

- `--planner-model`: Override the model used for planning.
- `--executor-model`: Override the model used for executing steps.

### `vibe hook`

Manage remote OpenAPI hooks. See [Remote Hooks](/hooks/remote).

```bash
vibe hook add <name> --url <url>
vibe hook list
vibe hook show <name>
vibe hook remove <name>
```

### `vibe init`

Initializes a new `.contenox/` configuration directory in the current path.
