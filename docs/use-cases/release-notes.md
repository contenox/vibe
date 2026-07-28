---
description: Pipe git log into contenox run and get grouped, categorized release notes back — no custom chain, no setup.
---

# Automated Release Notes

Pipe `git log` into `contenox run` and get grouped markdown release notes back. No custom chain, no setup — just shell composition.

---

## CI pipeline

```bash
PREV_TAG=v0.9.0
git log --oneline "$PREV_TAG"..HEAD | \
  contenox run "Group these commits under ## Features, ## Bug Fixes, ## Improvements, ## Documentation. Omit empty sections. No preamble." \
  > RELEASE_NOTES.md
```

Output example:

```markdown
## Bug Fixes
- Fix lockfile
- Fix naming drift

## Improvements
- Improve naming

## Documentation
- Update docs
```

The shell produces the commit list; contenox is just the formatter. No `--shell` flag is needed because contenox never runs the command itself — the model only sees the commit lines.

---

## Tips

- **Use a stronger model** for better grouping. Override per-invocation:
  ```bash
  git log --oneline "$PREV_TAG"..HEAD | \
    contenox run --model gpt-4o --provider openai \
    "Group these commits under ## Features, ## Bug Fixes, ## Improvements, ## Documentation. Omit empty sections. No preamble." \
    > RELEASE_NOTES.md
  ```
- **Point at a specific tag**: set `PREV_TAG` to whichever tag covers the range you want. For the very first release, use the root commit hash instead of a tag.
- **Filter merge commits**: add `--no-merges` to the `git log` command before piping.
- **Upload to GitHub**:
  ```bash
  gh release create v0.9.1 --notes-file RELEASE_NOTES.md
  ```
