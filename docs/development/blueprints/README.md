# Blueprints

Design records, decision documents, and R&D directions for the Contenox
runtime. Blueprints capture the *why* behind the implementation; user-facing
how-to docs live one level up in `docs/`.

## Active subsystems

| Area | What it covers |
| --- | --- |
| [acp/](acp/README.md) | Agent Client Protocol surface: contenox as agent (registry submission artifacts, sandbox architecture) and as client (the client-side engine, fleet and mission machinery) |
| [providers/](providers/README.md) | Cloud/hosted provider integrations |

## Beam TUI and engine designs

| Doc | Status | What it covers |
| --- | --- | --- |
| [beam-tui.md](beam-tui.md) | active blueprint | The beam TUI component blueprint: constitutional decisions, build order, testability doctrine |
| [beam-tui-crush-mining.md](beam-tui-crush-mining.md) | mining report | Clean-room study of charmbracelet/crush (FSL — implementers use this report, never the repo) |
| [pando-mining.md](pando-mining.md) | mining report | Clean-room study of digiogithub/pando; mission re-entry design input (§F1-G2/G3) |
| [eino-evaluation.md](eino-evaluation.md) | decision record | The replace-vs-learn evaluation of cloudwego/eino, with the revisit trigger |
| [provider-kv-cache.md](provider-kv-cache.md) | active design | Provider KV-cache utilization: prefix determinism, breakpoints, usage extraction |
| [tool-hardening.md](tool-hardening.md) | decision record | The ten tool-hardening recommendations (Rec 4/5/7 landed in `localtools`); the retired eval-harness design |

## Past R&D

| Area | What it covers |
| --- | --- |
| [retired/](retired/README.md) | Retired R&D: the Beam web UI, the modeld local inference daemon, the VS Code extension, and the HTTP API surface — what was built, what was learned, and why V1 ships without them |
