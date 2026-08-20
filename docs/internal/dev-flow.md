# The dev flow: trunk is a knife fight, main is the release ledger

Decided 2026-08-19. Two branches, one rule each.

## The model

**`trunk`** is where everything happens: commit straight to it, fifty
"Checkpoint" commits a day, agents and humans alike. Commit-then-review,
revert fast — the OpenBSD posture. CI on trunk is the fast hermetic lane
(ci.yml: hygiene, units, substrate smoke, e2e-cli), advisory by design:
post-commit signal in minutes, nothing gating, nothing waited on. Trunk may
be briefly broken; that costs a revert, not a customer.

**`main`** moves only by release. A PR from trunk into main has exactly one
meaning — "release this" — and merging it IS the release:

1. Bump `internal/version/version.txt` on trunk.
2. Open a PR trunk → main titled `Release vX.Y.Z`. The PR body is the
   release notes — the one document in this flow worth writing well, and the
   only PR etiquette that exists here.
3. The required check is the full release claim (release-gate.yml,
   `task test-all`) — the single lane where an hour of wall clock is
   acceptable, because merging ships.
4. Merge (merge commit — never squash, see below). release-on-merge.yml
   verifies the title against version.txt, tags the merge commit, and calls
   release.yml to build, attest, and publish with the PR body as notes.

An outsider reading main sees one merge bubble per release and nothing else.
The knife fight stays on trunk.

## Why these mechanics and not others

- **Merge commits only into main.** Squash or rebase would rewrite the
  trunk commits' identity; the next release PR then re-shows everything
  already released, and the branches diverge forever (the classic
  develop/main wound). With merge commits the divergence is structurally
  impossible.
- **Any merged PR into main is a release — no title magic deciding IF.**
  The title only names the version, and two independent checks (before the
  tag exists, and again in release.yml against the tag) refuse a version
  that does not match version.txt. A convention that can be silently gotten
  wrong at 2am is not a convention, it is a trap.
- **The tag is created by the workflow and the build is a workflow_call.**
  A tag pushed with the default GITHUB_TOKEN cannot trigger the tag-push
  release path (GitHub's recursion guard) — calling release.yml directly is
  the only PAT-free design that works, and it keeps one definition of how a
  release is built. The tag-push trigger remains as break-glass.
- **The release claim is a required check only here.** A gate nobody waits
  for gets made non-required and then ignored; a gate on the release button
  is waited on happily, because pressing the button is rare and deliberate.

## One-time GitHub settings (not in any file — do these in the UI/API)

- [ ] Create `trunk` from current `main`: `git push origin main:trunk`.
- [ ] Default branch: keep `main` (outsiders land on the clean ledger).
      Cost: strangers' PRs default to targeting main; the release workflow
      ignores non-trunk PRs, and a ruleset (below) blocks their merge.
- [ ] Ruleset on `main`: require a PR, require the `test-all` status check,
      block force pushes, block direct pushes. Do NOT require approvals —
      self-approving your own release PR forever is ceremony without value.
- [ ] Repo merge settings: allow merge commits, disallow squash and rebase
      merging. (Repo-wide; trunk sees no PRs in practice, so nothing is lost.)
- [ ] Ruleset on tags `v*`: restrict creation to the GitHub Actions
      integration (break-glass: repo admin bypass). This is "no manual tags".
- [ ] Later, when a second human joins: add an auto-merge lane for their
      branches into trunk. Nothing about the release machinery changes.

## What stays deliberately absent

- Required approvals anywhere.
- Conventional commits, changelog bots, commit-message linting. Release
  notes are written once, by a human, in the release PR body, at exactly the
  moment they are cheapest to write and most likely to be true.
- Per-push heavyweight suites. The claim runs nightly (release-gate
  schedule) and on the release button. GitHub is the operations runner, not
  the test farm; if the claim ever outgrows shared runners, it moves to
  owned compute without this flow changing shape.
