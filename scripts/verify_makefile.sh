#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
make help >/dev/null
make -n build-cli >/dev/null
echo "verify_makefile.sh: OK"
