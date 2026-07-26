# ACP Registry Submission

This directory contains the files to copy into a fork of
`agentclientprotocol/registry`:

```text
contenox/
  agent.json
  icon.svg
```

The manifest version must match `internal/version/version.txt`.

Before opening the registry PR, verify the release asset URLs return `200` and
run the registry validator from the registry checkout:

```bash
uv run --with jsonschema .github/workflows/build_registry.py
uv run --with jsonschema --with agent-client-protocol \
  .github/workflows/verify_agents.py --auth-check --agent contenox
```

Release facts:

- `agent.json` targets `v0.32.8`.
- The `v0.32.8` release asset URLs for darwin-arm64, linux-arm64, linux-amd64,
  and windows-amd64 return `200`.
- `contenox-linux-amd64.tar.gz` contains a single executable named `contenox`.
- Windows releases should publish `contenox-windows-amd64.zip` containing
  `contenox.exe`; the registry `cmd` must be `./contenox.exe`.
- `icon.svg` is 16x16, square, and uses `fill="currentColor"`.
