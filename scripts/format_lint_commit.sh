#!/usr/bin/env bash
set -euo pipefail

# Run lint + format in enterprise/site, then stage and commit.
# Safe to re-run; commit will be skipped if no changes.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "Running lint and format in enterprise/site ..."
cd "$ROOT_DIR/enterprise/site"

if [ ! -d node_modules ]; then
  echo "Installing dependencies with npm ci ..."
  npm ci
fi

# Lint first (fail if the project enforces it)
if npm run | grep -qE '(^| )lint( |$)'; then
  echo "Running npm run lint ..."
  npm run lint
else
  echo "No lint script found; skipping lint."
fi

# Then format (prefer package script; fallback to prettier)
if npm run | grep -qE '(^| )format( |$)'; then
  echo "Running npm run format ..."
  npm run format
else
  echo "No format script found; running Prettier fallback ..."
  npx prettier --write .
fi

cd "$ROOT_DIR"

echo "Staging and committing changes ..."
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git add -A
  git commit -m 'Landing: JSON-LD aligned with hero; docs/cookbook frontmatter updated; build + search index regenerated' || echo "No changes to commit."
else
  echo "Not inside a Git repository; skipping git add/commit."
fi

echo "Done."