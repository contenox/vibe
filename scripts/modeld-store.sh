#!/usr/bin/env bash
# Thin transfer wrapper for the modeld artifact store (native dep bundles and final
# packages). The store is S3 in production, but the backend is chosen by URI scheme:
# an `s3://...` URI uses the aws CLI, anything else is treated as a local directory.
#
# This keeps the only S3-specific code in one place. Every other step — bundle
# production, fingerprinting, dedup decisions, consumer preflight, packaging, and
# smoke gates are exercised by pointing the store at a local directory, so they test
# without credentials. The literal `aws s3` transfer is the only part that needs a
# real bucket.
#
# Verbs:
#   exists <uri>            exit 0 if the object/prefix exists
#   put    <localdir> <uri> mirror a directory up (sync)
#   get    <uri> <localdir> mirror a directory down (sync)
#   cp     <localfile> <uri> upload a single file
set -euo pipefail

verb=${1:?usage: modeld-store.sh exists|put|get|cp ...}; shift
is_s3() { case "$1" in s3://*) return 0;; *) return 1;; esac; }

case "$verb" in
  exists)
    uri=${1:?exists <uri>}
    if is_s3 "$uri"; then aws s3 ls "$uri" >/dev/null 2>&1; else [ -e "$uri" ]; fi
    ;;
  put)
    src=${1:?put <localdir> <uri>}; uri=${2:?put <localdir> <uri>}
    [ -d "$src" ] || { echo "modeld-store: not a directory: $src" >&2; exit 1; }
    if is_s3 "$uri"; then
      aws s3 sync --delete "$src" "$uri"
    else
      rm -rf "$uri"; mkdir -p "$uri"; cp -a "$src"/. "$uri"/
    fi
    ;;
  get)
    uri=${1:?get <uri> <localdir>}; dst=${2:?get <uri> <localdir>}
    mkdir -p "$dst"
    if is_s3 "$uri"; then
      aws s3 sync "$uri" "$dst"
    else
      [ -d "$uri" ] || { echo "modeld-store: source not found: $uri" >&2; exit 1; }
      cp -a "$uri"/. "$dst"/
    fi
    ;;
  cp)
    src=${1:?cp <localfile> <uri>}; uri=${2:?cp <localfile> <uri>}
    [ -f "$src" ] || { echo "modeld-store: not a file: $src" >&2; exit 1; }
    if is_s3 "$uri"; then
      aws s3 cp "$src" "$uri"
    else
      mkdir -p "$(dirname -- "$uri")"; cp -a "$src" "$uri"
    fi
    ;;
  *)
    echo "modeld-store: unknown verb '$verb' (want: exists|put|get|cp)" >&2; exit 2
    ;;
esac
