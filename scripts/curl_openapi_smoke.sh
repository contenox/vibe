#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BIN="$ROOT/bin/contenox-runtime"
TMP=$(mktemp -d)
cleanup() { pkill -f '[/]bin/contenox-runtime beam' 2>/dev/null || true; rm -rf "$TMP"; }
trap cleanup EXIT
mkdir -p "$TMP/.contenox"
"$BIN" --data-dir "$TMP/.contenox" config set default-model phi3:3.8b
"$BIN" --data-dir "$TMP/.contenox" config set default-provider ollama
"$BIN" beam --data-dir "$TMP/.contenox" &
PID=$!
for i in $(seq 1 45); do
  if curl -sf -o /dev/null http://127.0.0.1:8081/api/health; then echo "beam ready (${i}s)"; break; fi
  sleep 1
  if [ "$i" -eq 45 ]; then echo "timeout"; exit 1; fi
done
echo "GET /api/health -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/api/health)"
echo "GET /openapi.json -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/openapi.json)"
curl -sf http://127.0.0.1:8081/openapi.json | python3 -c "import sys,json; d=json.load(sys.stdin); assert d.get('openapi'); print('openapi field', d['openapi'])"
echo "GET /docs -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/docs)"
curl -sf http://127.0.0.1:8081/docs | grep -q rapi-doc && echo "docs contains rapi-doc"
echo "GET / -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/)"
kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
echo "curl_openapi_smoke.sh: OK"
