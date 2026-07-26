# TODO — V1 Release

**Direction:** contenox V1 is terminal-first. The only product surface is the
`contenox` CLI + ACP editor sessions + the upcoming `contenox beam` TUI.
We only keep code that directly supports this. Motives: see WHY.md.
Past work is presented ONLY in the website-docs R&D page
(`docs/development/blueprints/retired/README.md`) — no other historic records
in the repo; details live in git history.

**This file also serves follow-up sessions as the hunting list for stale
strings, features, and code branches — see §8.**

## 1. Kill product branches — DONE (this session, 2026-07-26)

- [x] Beam web UI — `packages/beam`, `runtime/internal/web`, serve/beam wiring
- [x] UI library — `packages/ui` (website now vendors its own tokens.css)
- [x] API layer — `runtime/serverapi` + 24 `runtime/internal/*api` pkgs
- [x] API framework — `apiframework/` deleted; error taxonomy folded into
      `internal/errdefs` (fleet/mission/missionchanges/terminal services)
- [x] API spec generator — `internal/openapigen`, `tools/openapi-gen`
- [x] modeld — COMPLETELY: daemon AND client half (modeldinstall/probe/conn,
      `runtime/transport`, modelrepo llama+openvino, local backend types,
      snapshot/warmcache, statetype Resolved*/LiveEngine fields).
      Local inference for users = Ollama or vLLM.
- [x] VS Code extension — `packages/vscode`, `runtime/vscodeagent`,
      `contenox code`, `vscode-agent` cmd
- [x] apitests/, `runtime/benchreport`
- [x] REST-lever CLI verbs died with serve: `fleet`, `mission`, `approvals`,
      `workspace access`, `model push/pull/local/snapshot`, `setup --web`,
      `--setup-web`, `CONTENOX_SERVER_URL` mission forwarding (acpsvc
      MissionForwardConfig removed). Missions/HITL live on IN-PROCESS
      (ACP `/mission`, future TUI).
- [x] Gates: `go build ./...` + `go vet ./...` green; CLI help smoke green
      (21 surviving subcommands); website builds
- [ ] Full `go test -short ./...` — running at session end; record outcome

## 2. Public docs — DONE except final review

- [x] R&D policy implemented: single narrative page
      `blueprints/retired/README.md`; ALL retired blueprint records deleted
      (2026-07-26 decision) — details only in git history
- [x] docs/ sweep: dead user manuals deleted, quickstart/CLI-ref/config/
      editors/hitl/env-scrubbing de-Beam/de-modeld, v1-feature-map +
      windows blueprints rewritten terminal-first, internal/ records of killed
      products deleted
- [x] website/: Landing Beam-web section → `contenox beam` TUI section (EN+DE),
      Base meta, legacy redirects → retired/readme/, media → S3
- [x] README rewritten (terminal-first, zero killed-product mentions)
- [x] CONTRIBUTING rewritten (Task workflow, current commands, voice note)
- [x] WHY.md created (motive record; steer via this file)
- [ ] USER REVIEW: R&D page tone, landing copy (EN+DE), README
- [ ] Landing: replace the old-way/contenox table with a three-frame terminal
      strip (mission fired → detach → envelope interrupt; commands as the only
      captions) once beam exists to record — the CEO-copy pass's real
      recommendation; the terse table is the interim

## 3. `contenox beam` — the new TUI (NEXT MILESTONE)

Decision: NOT a port of the old vibe TUI (that was a control panel). The new
`contenox beam` is a true coding TUI — easy copy/paste etc. — built ON TOP of
the ACP services (acpsvc / agent sessions), not necessarily ACP-native.
Plan in a dedicated session.

- [ ] Design doc / blueprint for the beam TUI (prior art: old vibe TUI,
      1530-LOC bubbletea app — find via `git log --all -i --grep='vibe'`;
      hashes change after the history rewrite). Design inspiration the
      user likes: the xAI/Grok TUI — study its layout/interaction patterns
      when planning beam
- [ ] **Modularization:** define a proper reusable component / module
      structure for the CLI and TUI — this is now the core bet, so it must be
      very maintainable, but pragmatic: no overengineering. Candidate seams:
      command wiring vs. engine bootstrap vs. rendering; shared session/
      approval components used by both CLI verbs and TUI panes; acpsvc as the
      one service boundary both talk to. Decide before the TUI grows.
- [ ] Implement + register the `beam` subcommand (docs already describe it —
      README says "in development"; ship before V1 or soften docs)

### 3b. Vision e2e (MUST be intact for V1 — currently has NO entry point)

Audit 2026-07-26: the engine back half is fully intact — `taskengine.Message.
Images` flows through taskexec into every surviving provider (ollama, gemini,
openai chat+responses, anthropic messages codec), llmresolver routes on
`CanVision`, and `taskexec_images_test.go` covers the conversion. But every
image PRODUCER died with the purge: the only writers of `Images` were the
killed compatapi HTTP endpoints. Today:
- ACP advertises `PromptCapabilities.Image: false` (acpsvc/initialize.go:77)
  and the native driver flattens prompts via `libacp.FlattenContent`, which
  DROPS image blocks (only telemetry records `dropped_content_kinds`).
- The CLI has no attach mechanism.

Work items:
- [ ] ACP: advertise `Image: true`; native driver must map image content
      blocks → `ImagePart{Data, MimeType}` on the user message instead of
      flattening them away (turn input becomes text + images, not a string)
- [ ] CLI: `contenox chat --attach <img>` (repeatable) feeding ImageParts
- [ ] beam TUI: image paste/attach is a design requirement from day one
- [ ] Tests: e2e ACP-image-block → chain → provider-wire assertion (mock
      provider), plus a gated real-model vision test (ollama container,
      vision-capable model) replacing the deleted llama full-stack vision e2e
- [ ] Capability truth: `model list` / doctor should make CanVision visible
      so a failed vision route teaches instead of confusing

## 4. Git history & assets

- [x] Website media → S3: bucket `contenox-website-assets-573643652148`
      (us-east-1, public read), `media/` live + `retired/` archive; local
      copies in scratchpad assets-export/
- [x] git-filter-repo fetched; purge inventory finalized:
      root binaries (contenox-runtime, contenox, contenox-cli, runtime, beam,
      vibe ×2, acp-stub-agent ×2) + historical media (website/public dead+
      migrated files, website/assets/, scripts/demos video dirs,
      website/public/demo.webm, old hitl-*.png)
- [x] .gitignore: binary names blocked
- [ ] **USER: commit the staged V1 reshape** (Claude stages only, never
      commits). The rewrite needs a clean committed tree.
- [ ] Then: git bundle backup → run filter-repo (purge list above) → verify
      sizes/paths → force-push main + announce re-clone
- [ ] AWS: decommission old bucket `contenox-modeld-artifacts-573643652148`?

## 5. Build system — DONE

- [x] Taskfile.yml (build, tests, ACP harnesses, website, version, dev-link)
- [x] Makefile, Makefile.version, mk/ deleted; scripts/verify_cli_help.sh
      updated to the 21 surviving commands
- [x] tools/version: VS Code metadata sync removed (README TAG mechanism kept)
- [x] ci.yml + release.yml: task-based, beam/ui/vscode jobs removed
- [ ] Install `task` locally: `go install github.com/go-task/task/v3/cmd/task@latest`

## 6. Open decisions (USER)

- [x] **Repo/product rename** — DECIDED: `beam`. Module is now
      `github.com/contenox/beam` (rewrite done 2026-07-26).
      - [x] GitHub repo renamed to contenox/beam (2026-07-26); local origin
            updated; install.sh REPO var, landing badges, and the ACP
            registry manifest repointed
      - [ ] after next release: verify install.sh end-to-end against
            contenox/beam release assets (old URLs redirect meanwhile)
      - [ ] scripts/demos/*.tape still reference the old local checkout path
            (~/src/github.com/contenox/runtime) — refresh when demos are
            re-recorded (or chop with §6 orphans)
- [x] **Repo restructure** — DONE 2026-07-26 (one atomic rewrite with the
      rename; 730 files moved). Layout: `libacp/` public; `internal/kernel/`
      (taskengine, agentinstance, nativeturn, contextasm, enginesvc,
      reasoning, llmresolver, tools) · `internal/models/` (modelrepo+
      providers, llmrepo, runtimestate, statetype, backend/provider services,
      modelregistry*, modelcapability, hostcapacity, ollamatokenizer) ·
      `internal/services/` (~38 domain pkgs) · `internal/store/runtimetypes` ·
      `internal/surfaces/` (contenoxcli, acpsvc; beamtui to come) ·
      `internal/{libbus,libdbexec,libkvstore,libsandbox,libtracker}` (dirs
      keep package names — mechanical move) · `internal/errdefs` ·
      `internal/version` · `internal/tooleval`. Gates green post-move: build,
      vet, gofmt, unit suite, CLI help smoke, website build.
      - [ ] Fold statetype INTO runtimestate (deferred: package merge is
            surgery, not a move)
      - [ ] statetype/tooleval/version placement review after beam TUI lands
- [x] **taskengine** — RESOLVED (2026-07-26): keep the engine (it executes
      every chat/ACP/mission turn and owns the Message/ImagePart vocabulary);
      demote the public chain-authoring story. Follow-ups:
      - [ ] reframe docs/specification/ + landing "Chain is the contract" cap:
            users author AGENTS (agent-*.json) and ENVELOPES (HITL policies) —
            chains are the substrate, documented for power users
      - [ ] move `internal/kernel/taskengine` → `runtime/internal/taskengine` during
            the modularization milestone (mechanical import rewrite; not in
            this commit)
      - [x] core/kernel layering stays: thin surfaces → services → engine is
            the build-on-services rule; agentinstance kernel carries missions
            and the beam engine-bridge
- [ ] **Active blueprints cull** — user hinted "99% of blueprints can go";
      kept for now: acp/ (5), providers/ (2), windows/ (2), v1-feature-map,
      product-surface-truth, tool-hardening, local-coding-node-goals, README.
      Say the word and they go too.
- [ ] **Orphaned engineering** (outside CLI closure — chop or keep?):
      `internal/tooleval` (+fixtures; Taskfile tool-eval uses it), `libcipher`,
      `libprocess`, `libroutine`, `scripts/demos/`,
      `scripts/contenox-agentic-bench.ps1` + bench chain json.
      KEEP (harness/tooling): tools/acp-validator, libacp/cmd/acp-stub-agent,
      tools/version.

## 7. Verification gates before calling V1 prep done

- [x] go build ./... && go vet ./... green
- [ ] go test -short ./... green (or documented pre-existing failures)
- [x] website builds post-purge (100+ pages, retired index live)
- [x] `contenox --help` shows only surviving commands
- [ ] grep gates in §8 clean (first pass done; re-run after TUI lands)

## 8. Stale-hunt list (follow-up sessions)

Run these; every hit must be justified or killed:
- `grep -rniE 'modeld|openvino|llamacpp|llama\.cpp' --include='*.go' .`
  (known acceptable: modelregistry curated-catalog data labels — decide
  whether the GGUF/IR catalog itself survives without local inference!)
- `grep -rniE '"serve"|serverapi|/api/' runtime/ --include='*.go'` (judge each)
- `grep -rniE 'vscode|vs code' --include='*.go' .` (taskengine/tasktype.go
  prose comments; localtools skip-dir `.vscode` default is legit)
- `grep -rniE 'beam' docs/ website/ README.md` (must mean the TUI or retired page)
- `grep -rn 'make ' docs/ CONTRIBUTING.md README.md .github/` (should be task)
- `contenox model registry-*` commands: registry lists GGUF/OpenVINO curated
  models but `model pull` is gone — dead feature branch? decide
- `internal/models/hostcapacity` — former modeld capacity helper; check
  remaining importers, likely dead
- `internal/services/agentsmd`, `internal/services/agentview`, `internal/services/accessview`,
  `internal/services/presence`, `internal/services/operatorinbox` — verify still reachable from
  CLI closure after serve removal
- examples/ chains re-verify against surviving providers
- `.contenox/` in-repo chain configs — check for killed-cmd references
- go.mod: charmbracelet deps should arrive only with the new TUI; grpc is
  indirect-only now
- `internal/version` + install.sh + release.yml artifact-name consistency
- SUPPORT.md issue templates (.github/ISSUE_TEMPLATE?) for killed surfaces
- Known load-flaky tests (pass isolated, flake under full-suite parallel
  load — harden or serialize): runtimetypes `TestUnit_Backend_
  DeletesSuccessfully` (Postgres testcontainer startup timeout — also ask:
  why does the SQLite-era store still test against a Postgres container?),
  acpsvc `TestE2E_Wire_ExternalAgent_CommandMenuAfterNewSessionResult` and
  `TestLoopback_ExternalAgent_LazyRespawnPushesConfigOptions` (subprocess
  EOF under load)
