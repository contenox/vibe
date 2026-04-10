#!/usr/bin/env bash
# Clean CI runners have no generated embed inputs. //go:embed requires files on disk
# before any `go list ./...` (e.g. tools/openapi-gen). This script:
#   1) builds the Beam SPA into internal/web/beam/dist
#   2) writes a minimal OpenAPI JSON stub into internal/openapidocs/openapi.json
# `make docs-gen` then replaces the stub with the real spec from codegen.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

npm ci
make build-web

mkdir -p "$ROOT/internal/openapidocs"
# Minimal OAS JSON so the openapidocs embed is non-empty; overwritten by docs-gen.
printf '%s' '{"openapi":"3.1.0","info":{"title":"ci-pre-stub","version":"0"},"paths":{}}' \
  >"$ROOT/internal/openapidocs/openapi.json"
