---
title: "The HTTP API layer and OpenAPI generator"
description: apiframework and a from-scratch OpenAPI 3.1 generator that derived a byte-deterministic spec straight from Go route annotations, with CI gates against drift.
---

# The HTTP API layer and OpenAPI generator

Contenox ran on a shared HTTP layer, `apiframework`: authentication, CORS, a structured error envelope, request IDs, and the query/path parameter helpers every route package built on. On top of it sat a from-scratch OpenAPI 3.1 generator that read the route registrations and inline `@request` / `@response` / `@param` annotations directly out of the Go source and turned them into `openapi.json` — embedded into the binary and served live at `/api/openapi.json`, with a bundled RapiDoc viewer at `/api/docs`.

The generator was strict by design, not best-effort. A coverage gate rejected any documented operation missing a `@response` annotation, or any non-GET/DELETE operation missing `@request` — with explicit `none`/`binary`/`sse`/`redirect` forms as the only escape hatches, each requiring its own stated reason. A freshness test regenerated the spec from the working tree on every `go test ./...` run and byte-compared it against the committed file, so a route could never silently drift out of sync with its documented contract. Generation was fully deterministic — the same tree always produced identical output — and a full `apitests/` pytest suite exercised the compiled routes against that same described contract: backends, missions, chains, tools, HITL policies, MCP servers, model registry, and health, end to end.

## What it proved

- **A spec generated straight from source annotations can be both fully automatic and byte-deterministic** — no hand-maintained YAML drifting away from the code it describes.
- **Coverage and freshness gates make spec drift a CI failure, not an eternal chore.** Every route stayed honestly documented because the build refused to pass otherwise.
- **Typed request/response contracts, declared once at the handler, are worth enforcing structurally** — not just documenting and hoping.

## Where this lives now

The same typed, declared-once discipline lives on in the chain engine's task contracts — every handler's fields are still validated and documented from one source of truth, just Go-native instead of HTTP-native.
