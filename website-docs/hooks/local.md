# Local Hooks

Local hooks execute directly on the machine where the Contenox runtime is hosted. Unlike remote hooks, they do not require an external OpenAPI service.

## Security Warning

> **CAUTION:** Local hooks give the AI model direct access to your host system. Never enable them on a public-facing server or when exposing the agent to untrusted users without a secure sandbox (like a container).

For the `vibe` CLI, local hooks are disabled by default. You must explicitly opt-in:
```bash
vibe run "list my files" --enable-local-exec
vibe exec --chain mychain.json --enable-local-exec "do something"
```

## Available Local Hooks

### `local_shell`

Executes Bash commands on the host. 

**Tools exposed:**
- `run_command(command: string)`: Executes the given string in `bash -c`.

**Example chain usage:**
```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "hooks": ["local_shell"]
}
```

### `local_fs`

Reads and writes files on the host filesystem.

**Tools exposed:**
- `read_file(path: string)`
- `write_file(path: string, content: string)`
- `list_dir(path: string)`

**Example chain usage:**
```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "hooks": ["local_fs"]
}
```

## Adding custom local hooks

Currently, adding new local hooks requires modifying the Contenox Go source code. If you need a custom capability without writing Go, build a small HTTP service and register it as a [Remote Hook](/hooks/remote) instead.
