# R&D section — asset manifest

Media referenced by `docs/rnd/**` (unpublished notes — see `website/src/lib/remark-md-links.mjs`
for the rewrite mechanism and `S3_MEDIA` allowlist). None of these files have been uploaded;
this table exists so the maintainer knows exactly what to source and where each one is used.
The S3 target base is the existing bucket already referenced by the site:
`https://contenox-website-assets-573643652148.s3.amazonaws.com/media/`.

The maintainer uploads; nothing here was touched in S3, and no binary was added to this repo.

| Filename | Used on | Source | S3 target |
|---|---|---|---|
| `beam-demo.webm` | `docs/rnd/beam-web.md` | Recovered from the pre-purge `docs/guide/beam.md` (this repo, commit `126ac7ca`, before `7ffadb05`), which embedded this exact filename as the product-tour demo video. The file itself was not found in this repo's history (video assets were never committed to git) — maintainer has it. | `.../media/beam-demo.webm` |
| `beam-video-cover.png` | `docs/rnd/beam-web.md` | Same source doc as above (poster frame for the demo video). Not found in this repo's history — maintainer has it. | `.../media/beam-video-cover.png` |
| `beam-login.png` | `docs/rnd/beam-web.md` | Referenced by filename in the pre-purge `docs/guide/beam.md` (commit `126ac7ca`) as the login-page screenshot. Image file not located — maintainer has it, or a fresh screenshot of the archived build is needed. | `.../media/beam-login.png` |
| `beam-new-chat.png` | `docs/rnd/beam-web.md` | Referenced by filename in the same pre-purge doc (new-session screenshot with the per-session controls). Image file not located — maintainer has it, or needs a fresh capture. | `.../media/beam-new-chat.png` |
| `modeld-console.png` | `docs/rnd/modeld.md` | No prior asset exists under this name; modeld had no UI of its own; the referenced page (`docs/integrations/providers/modeld.md`, commit `126ac7ca`) only ever mentioned the Beam "modeld console" panel in passing. New screenshot needed — maintainer to capture from an archived build, or drop the slot. | `.../media/modeld-console.png` |
| `vscode-extension-icon.png` | ~~`docs/rnd/vscode-extension.md`~~ — page removed 2026-07-28 when the extension was revived; asset stays uploaded, currently unreferenced | Recoverable directly from this repo's git history: `packages/vscode/media/contenox-icon.png` at commit `126ac7ca` (512x512 PNG, still a valid blob — `git show 126ac7ca:packages/vscode/media/contenox-icon.png > vscode-extension-icon.png`). Rename on upload to match the filename used on the page. | `.../media/vscode-extension-icon.png` |
| `ui-library-storybook.png` | `docs/rnd/ui-library.md` | No prior asset exists; `packages/ui` shipped Storybook stories (recoverable as source at commit `126ac7ca`) but no exported static screenshot was ever committed. New screenshot needed — maintainer to capture from an archived Storybook build, or drop the slot. | `.../media/ui-library-storybook.png` |

## Notes for the maintainer

- Two additional Beam screenshots exist in the maintainer's own archive but are **not** listed above as ready to use: one embeds a visible local shell prompt (username/hostname in the terminal pane) and another prints a directory listing of a local `.contenox` config dir. Both need cropping/redaction before they could go anywhere public — they are intentionally left out of this manifest and off the pages.
- Other small brand assets (extension activity-bar icons, light/dark logo SVGs, the beam wordmark) are also still recoverable the same way, from `packages/vscode/media/*` and `packages/beam/src/assets/*` at commit `126ac7ca` — pull any of those the same way if a page needs a second icon variant.
- Once uploaded, add each filename to the `S3_MEDIA` set in `website/src/lib/remark-md-links.mjs` (already done in code for the seven above, pending the actual uploads) so the build's root-relative references resolve to the bucket.

## Upload log 2026-07-28 (assistant, maintainer-authorized AWS session)

- UPLOADED `beam-demo.webm` (source: maintainer's personal backup, `demo.webm`) → live, verified HTTP 200
- UPLOADED `beam-video-cover.png` (poster frame extracted at t=5s from the demo) → live
- UPLOADED `vscode-extension-icon.png` (recovered via `git show 126ac7ca:packages/vscode/media/contenox-icon.png`) → live
- UPLOADED `backend-manager-demo.mp4` (source: maintainer's personal backup, `product-demo.mp4`; NEW slot, embedded on beam-web page) → live
- Still needed: beam-login.png, beam-new-chat.png, modeld-console.png, ui-library-storybook.png (captures from archived builds)
- CORRECTED 2026-07-28: beam-demo.webm on S3 replaced with the true production demo (368KB, fetched from the live site root); poster regenerated from it. The earlier 2.9MB backup recording (old chain-runner web UI) preserved as media/chain-runner-demo.webm — unused, available for a future lab embed.

## Lab screenshot harvest — uploaded 2026-07-28

15 screenshots from the archive stack (launched locally via compose) uploaded as `lab-*.png`:
bob-{dashboard,connectors,search,beam,files,members}, site-{home,bob-signup,admin-login},
admin-bob-{tenants,tenants2,worker-pools,apps}, minio-console, mailpit-inbox.
Embedded so far: bob.md (5), api-layer.md (2), ui-library.md (1), vald-operator.md (2).
Unused-but-available: bob-files, bob-members, admin-bob-tenants, minio-console, mailpit-inbox.
NOT uploaded (flagged): admin-dashboard.png (checkout-order record with an email address),
admin-analytics.png (404 page), registry-catalog.png (raw JSON).
