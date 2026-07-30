# Release pipelines

**You don't need anything on this page to build or contribute to this repo.**
It documents how the three things this repo ships — the `contenox`
CLI/VS Code extension, `modeld`, and the website — actually get released, who
can trigger that, and which external services (GitHub Actions secrets, AWS S3)
are involved. See [build-requirements.md](build-requirements.md) for what you
need to build any of this locally.

The three pipelines are independent, have different automation levels, and
don't share credentials:

| Pipeline | Automated? | External deps | Needs `.env`/S3? |
|---|---|---|---|
| `contenox` CLI + GitHub Release | Yes — `release.yml` on tag push | none (`GITHUB_TOKEN` only) | No |
| VS Code Marketplace publish | Yes, gated — `vscode-marketplace.yml` | `VSCE_PAT` secret, protected `vscode-marketplace` environment | No |
| `modeld` native build/release | No — fully manual, maintainer-run via `make` | AWS S3 (or a local directory for testing) | **Yes** |
| Website (contenox.com) | No — no CI wired up at all | possibly an external platform building `website/Dockerfile` (not in this repo) | Indirectly — a separate, unrelated public media bucket |

## 1. `contenox` CLI + GitHub Release — fully automated

`.github/workflows/release.yml` runs on every push of a `vX.Y.Z` tag:

1. **verify** — the tag must equal `internal/version/version.txt` exactly, or
   the run fails with the correction steps.
2. **build** — cross-compiles the pure-Go CLI (`CGO_ENABLED=0`) for
   `linux/amd64`, `linux/arm64`, `darwin/arm64`, `windows/amd64` from one
   Ubuntu runner, and packages both the raw binary (for `install.sh`) and an
   ACP-registry archive (`.tar.gz`/`.zip`) per target.
3. **vscode** — builds a native VSIX per target (`linux-x64`, `linux-arm64`,
   `darwin-arm64`, `darwin-x64`, `win32-x64`) — each bundles the `contenox`
   binary for the machine the extension host runs on, so these are built
   natively per matrix entry (including on real `macos-15`/`macos-15-intel`
   runners for the two Darwin targets), not cross-compiled.
4. **release** — downloads every artifact and runs `gh release create`,
   authenticated with the ambient `GITHUB_TOKEN`. No other secret is used.

Maintainer-side, cutting a release is:

```bash
task version:bump-patch   # or bump-minor / bump-major
# review the generated release commit + tag
git push && git push origin vX.Y.Z
```

(`task release` prints this same runbook.) Note `darwin/amd64` is not one of
the four raw CLI release targets above — Intel Mac users get the binary via
the VS Code extension's `darwin-x64` VSIX (built natively on
`macos-15-intel`) or by building from source.

## 2. VS Code Marketplace publish — automated but gated

Separate from the GitHub Release above. `.github/workflows/vscode-marketplace.yml`
runs on the same `v*` tag push, or manually via `workflow_dispatch`
(`publish`/`pre_release` inputs):

1. Rebuilds every target VSIX from scratch (same 5-target matrix), verifying
   package contents each time — it never reuses `release.yml`'s artifacts.
2. The `publish` job only runs on a tag push or an explicit
   `workflow_dispatch` with `publish=true`, requires the protected
   `vscode-marketplace` GitHub Environment (reviewer approval), and calls
   `vsce publish` using the `VSCE_PAT` repository secret.

Full setup checklist, publisher identity, and the marketplace-listing
requirements: [vscode-marketplace-release.md](vscode-marketplace-release.md).
No S3 or `.env` involved here either — the only secret is `VSCE_PAT`.

## 3. `modeld` native build/release — manual, S3-backed

This is the one pipeline **not** wired into GitHub Actions at all — no CI
builds or ships `modeld`. It needs per-OS native toolchains (see the
`modeld` section of [build-requirements.md](build-requirements.md)) in
combinations no single CI runner covers cleanly once OpenVINO GenAI and the
optional CUDA/HIP variants are counted, so the release model is
**distributed build, centralized store**, entirely driven by the top-level
`Makefile`:

1. **Each device that can build a given platform+accelerator variant builds
   it locally** and produces a *native dependency bundle* — a relocatable
   sysroot of the compiled llama.cpp runtime, llama.cpp reference headers,
   and (except on macOS) the OpenVINO GenAI SDK/headers/libs, tagged by a
   deterministic fingerprint of its build inputs (llama.cpp commit, build
   type, runtime ABI, CUDA/HIP/OpenVINO flags):

   ```bash
   make deps-modeld                                # or deps-openvino only
   make bundle-modeld-deps                          # dispatches by host OS:
   #   bundle-modeld-deps-linux / -darwin / -windows
   make modeld-deps-fingerprint                      # what this bundle's key is
   ```

2. **The bundle is pushed to a shared artifact store** — this is where S3
   comes in:

   ```bash
   make push-modeld-deps
   ```

   A fingerprint already present in the store is skipped, so a device never
   rebuilds/re-uploads a variant that's already there.

3. **Release assembly** happens on any single machine (it doesn't need to be
   one that can build natively for every platform) by pulling a bundle and
   linking the final binary against it, without rebuilding llama.cpp/OpenVINO:

   ```bash
   make pull-modeld-deps                                       # or: deps-modeld-prebuilt
   make package-modeld-release-linux MODELD_DEPS_ROOT=...       # -darwin / -windows
   make push-modeld-release                                     # uploads + refreshes index.json
   ```

   `package-modeld-release-*` smoke-tests the packaged binary (`modeld
   version --json`) and hard-fails if the reported backend set doesn't match
   what was requested — it refuses to silently ship a reduced backend set
   (e.g. missing OpenVINO on a platform that's supposed to have it).

**Why S3, specifically:** native dependency bundles are large, per-variant
blobs that need cheap incremental sync and skip-if-present dedup by
fingerprint — that's a poor fit for GitHub Releases (which is for versioned,
immutable, comparatively small release assets), so both the intermediate
bundles and the final packaged `modeld` archives live in an S3-backed (or
local-dir, for testing) store instead.

**The `.env` file** is how a maintainer's local `make` run gets pointed at
that store — this is the actual thing the release process needs that nothing
else in this repo does:

```bash
# .env at the repo root — gitignored, never commit this
MODELD_DEPS_S3_URI=s3://<bucket>/modeld-deps
MODELD_RELEASE_S3_URI=s3://<bucket>/modeld-releases
AWS_REGION=...
```

The `Makefile` sources `.env` automatically if present (see its
`LOCAL_ENV_FILE` block) and exports every variable it defines. Actual AWS
credentials are **not** handled by this repo's tooling at all — `aws s3` is
shelled out to by `scripts/modeld-store.sh`, so credentials come from
whatever the `aws` CLI normally uses (`aws configure`, `AWS_PROFILE`,
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, or an assumed role).

Two things worth knowing:

- **Every store-touching target fails loudly, not silently**, when
  `MODELD_DEPS_S3_URI`/`MODELD_RELEASE_S3_URI` is unset — `push-modeld-deps`,
  `pull-modeld-deps`, `check-modeld-deps-store`, `deps-modeld-prebuilt`, and
  `push-modeld-release` all check first and print `set
  MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)` rather than
  doing nothing or erroring obscurely.
- **Both URIs also accept a plain local directory** instead of an `s3://`
  URI — `scripts/modeld-store.sh` dispatches purely on that prefix — which is
  how the entire push/pull/dedup/fingerprint flow is exercised without any
  AWS access at all.
- **None of this is required for local `modeld` development.**
  `make build-modeld` / `make run-modeld` / `make package-modeld` never touch
  the store; it only matters when publishing a `modeld` build for others to
  consume without compiling llama.cpp/OpenVINO themselves.

## Website (contenox.com) — no automated pipeline

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
to `S3_MEDIA` after uploading. This bucket has nothing to do with `modeld`'s
release store; don't confuse the two `MODELD_*_S3_URI` vars above with it.

## See also

- [build-requirements.md](build-requirements.md) — per-platform toolchain requirements to build any of this
- [vscode-marketplace-release.md](vscode-marketplace-release.md) — full Marketplace setup/checklist
- `Taskfile.yml` (`task release`) — prints the CLI/extension release runbook
- Top-level `Makefile` (`make help`) — `modeld` build/package/release targets
