#!/usr/bin/env bash
# Generate modeld release index metadata sidecars for existing packaged archives.
#
# This is the reproducible backfill path for archives produced before
# modeld-package-release.sh wrote *.build.json. It reads manifest.json from the
# package archive and writes <archive>.build.json, which push-modeld-release uses
# to regenerate modeld/index.json.
set -euo pipefail

fail() { echo "modeld-release-metadata: $*" >&2; exit 1; }
[ "$#" -gt 0 ] || fail "usage: modeld-release-metadata.sh <archive>..."

extract_manifest() {
  archive=$1
  base=$(basename -- "$archive")
  top=$base
  case "$base" in
    *.tar.gz)
      top=${base%.tar.gz}
      tar -xOf "$archive" "$top/manifest.json"
      ;;
    *.zip)
      top=${base%.zip}
      command -v unzip >/dev/null 2>&1 || fail "unzip is required to read $archive"
      unzip -p "$archive" "$top/manifest.json"
      ;;
    *)
      fail "unsupported archive type: $archive"
      ;;
  esac
}

json_string_field() {
  field=$1
  sed -n "s/.*\"$field\": *\"\\([^\"]*\\)\".*/\\1/p" | head -1
}

json_number_field() {
  field=$1
  sed -n "s/.*\"$field\": *\\([0-9][0-9]*\\).*/\\1/p" | head -1
}

for archive in "$@"; do
  [ -f "$archive" ] || continue
  base=$(basename -- "$archive")
  manifest=$(extract_manifest "$archive") || fail "could not read manifest.json from $archive"
  version=$(printf '%s' "$manifest" | json_string_field modeld_version)
  platform=$(printf '%s' "$manifest" | json_string_field platform)
  protocol=$(printf '%s' "$manifest" | json_number_field protocol)
  backends_json=$(printf '%s' "$manifest" | tr -d '\n' | sed -n 's/.*"backends": *\(\[[^]]*\]\).*/\1/p' | tr -s ' ')
  [ -n "$version" ] || fail "manifest in $archive is missing modeld_version"
  [ -n "$platform" ] || fail "manifest in $archive is missing platform"
  [ -n "$backends_json" ] || fail "manifest in $archive is missing backends"
  # Archives that predate the protocol field spoke the original transport
  # contract. New package-modeld-release builds hard-fail if protocol is absent.
  [ -n "$protocol" ] || protocol=${MODELD_RELEASE_PROTOCOL:-1}
  channel=${MODELD_RELEASE_CHANNEL:-stable}
  size=$(wc -c < "$archive" | tr -d ' ')

  cat > "$archive.build.json" <<EOF
{
  "version": "$version",
  "platform": "$platform",
  "protocol": $protocol,
  "backends": $backends_json,
  "channel": "$channel",
  "archive": "$version/$base",
  "sha256": "$version/$base.sha256",
  "size": $size
}
EOF
  echo "modeld-release-metadata: wrote $archive.build.json"
done
