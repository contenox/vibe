# Release pipelines

**You don't need anything on this page to build or contribute to this repo.**
It documents how the two things this repo ships — the `contenox` CLI and the
website — actually get released, who can trigger that, and which external
services (GitHub Actions secrets) are involved. See
[build-requirements.md](build-requirements.md) for what you need to build any
of this locally.

The two pipelines are independent, have different automation levels, and
don't share credentials:

| Pipeline | Automated? | External deps |
|---|---|---|
| `contenox` CLI + GitHub Release | Yes — `release.yml` on tag push | none (`GITHUB_TOKEN` only) |
| Website (contenox.com) | No — no CI wired up at all | possibly an external platform building `website/Dockerfile` (not in this repo) |

## 1. `contenox` CLI + GitHub Release — fully automated

`.github/workflows/release.yml` runs on every push of a `vX.Y.Z` tag:

1. **verify** — the tag must equal `internal/version/version.txt` exactly, or
   the run fails with the correction steps.
2. **build** — cross-compiles the pure-Go CLI (`CGO_ENABLED=0`) for
   `linux/amd64`, `linux/arm64`, `darwin/arm64`, `windows/amd64` from one
   Ubuntu runner, and packages both the raw binary (for `install.sh`) and an
   ACP-registry archive (`.tar.gz`/`.zip`) per target.
3. **release** — downloads every artifact and runs `gh release create`,
   authenticated with the ambient `GITHUB_TOKEN`. No other secret is used.

Maintainer-side, cutting a release is:

```bash
task version:bump-patch   # or bump-minor / bump-major
# review the generated release commit + tag
git push && git push origin vX.Y.Z
```

(`task release` prints this same runbook.) Note `darwin/amd64` is not one of
the four raw CLI release targets above — Intel Mac users build from source.

## 2. Website (contenox.com) — no automated pipeline

Confirmed by both `.github/workflows/ci.yml` (compiles/tests the CLI only,
never touches `website/`) and `website/README.md` itself: *"Deployment (CI
push of `dist/` to a GitHub Pages repo) is not wired yet; builds are
local-only."* There is no workflow that builds or deploys the site.

`website/Dockerfile` builds the Astro site and serves the static output via
nginx on port 3000 (its own header comment: *"to satisfy the existing site
deployment contract (containerPort + GET / probes)"*), but nothing in this
repo invokes that Dockerfile automatically. Today, publishing the site is
either a manual `task website:build` + hand-deploy of `website/dist`, a
manual `docker build -f website/Dockerfile .` + push, or an external
platform configured (outside this repo) to build that Dockerfile directly on
push to `main`.

**A separate, unrelated S3 bucket** hosts heavy website media (demo gifs,
screenshots) referenced by the docs — a public bucket
(`contenox-website-assets-*`), read over plain HTTPS with no credentials
needed to build or view the site. `website/src/lib/remark-md-links.mjs`
rewrites root-relative markdown image paths (`/hero.gif`) to that bucket at
build time via its `S3_MEDIA` filename allowlist. Uploading a new asset there
is a manual, out-of-band step (no in-repo script does it) — add the filename
to `S3_MEDIA` after uploading.

## See also

- [build-requirements.md](build-requirements.md) — per-platform toolchain requirements to build any of this
- `Taskfile.yml` (`task release`) — prints the CLI release runbook
