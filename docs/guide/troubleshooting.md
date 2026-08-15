---
title: Troubleshooting & recovery
description: Symptom, cause, and fix for the failures operators actually hit — plus what survives a crash, what resumes it, and how to file a diagnostics bundle.
---

# Troubleshooting & recovery

Contenox keeps its state in one local database, so almost nothing is lost when a process dies. What it does not have is a daemon that notices. Recovery is something you run, and this page is the list of what to run.

Start here for anything that is not obviously a code problem:

```bash
contenox doctor
```

> **`doctor` is not a read-only probe.** On its text output it also reclaims missions whose host process is gone, finishing them `abandoned` and printing how many. It says so on the line it prints. `contenox doctor --json` does **not** sweep — the JSON payload has no field to report the count in, and a mutation a diagnostic cannot mention stays silent.

`doctor` leads with the one question you ran it to answer:

```
Ready: no — No default model is set. Internal chat and chains using {{var:model}} need it.
Next:  contenox setup
```

The `Next:` line is the single command that moves you closest to yes. When everything is fine it reads `Ready: yes`.

| Flag | Effect |
|---|---|
| `--json` | Machine-readable payload; no mission reclaim, no text sections |
| `--skip-cycle` | Skip the backend sync first — faster, but reachability may be stale |
| `--bundle` | Also write a redacted diagnostics zip and print a pre-filled issue URL |
| `--bundle-out <path>` | Where `--bundle` writes (default `./contenox-doctor-<timestamp>.zip`) |

---

## Chat won't answer: no model configured

**Symptom.** `contenox chat`, an editor session, or a chain run refuses to resolve a model. `doctor` reports `Ready: no`.

**Cause.** One of the defaults is unset, or the default names something the reachable backends do not serve. `doctor` distinguishes these:

| What `doctor` says | What it means |
|---|---|
| No default model is set | `default-model` is empty |
| No default provider is set | `default-provider` is empty |
| No LLM backends are registered yet | Saving defaults does not create a backend — you still need one |
| Default provider *X* is set, but no registered backend uses that provider | The provider and the backends disagree |
| Default model *X* is not currently available for provider *Y* | The backend is reachable; the model pin is wrong. `doctor` lists the chat models that *are* available |
| Default provider *X* is reachable, but runtime state contains no chat-capable models | The backend answered but serves nothing that can chat |

**Fix.** Take the `Next:` command. For the guided path:

```bash
contenox setup
```

Or set them directly:

```bash
contenox config set default-provider ollama
contenox config set default-model qwen3:8b
contenox config get default-model
```

## Provider unreachable, or a bad key

**Symptom.** Backends are registered, but `doctor` reports every one of them in error.

**Cause.** `doctor` separates the failure kinds rather than lumping them into "unreachable":

| What `doctor` says | Cause |
|---|---|
| …cannot be used because its backend credentials are missing | No API key stored for that backend |
| …rejected the stored credentials | The key is present and wrong, expired, or revoked |
| …is registered, but runtime state has not produced an entry | The sync has not finished yet — rerun `contenox doctor` |
| No reachable backend is available for default provider *X* | Network, wrong URL, or a local server that is not running |

**Fix.** Confirm what is registered and where it points, then repair the one that failed:

```bash
contenox backend list
```

For a local Ollama, `doctor` will probe your Ollama URL (`OLLAMA_HOST`, or `http://127.0.0.1:11434`) when no Ollama backend is ready and print the exact commands to pull a model and register the backend. For a hosted provider, see the [provider integration pages](/docs/integrations/providers/openai/) for the key it expects.

If you just fixed a key, rerun `doctor` without `--skip-cycle` so the backend cycle actually re-runs.

## `doctor` says a HITL policy preset is stale

**Symptom.** A line naming a policy file by full path, saying it predates one or more toolsets.

**Cause.** The policy file on disk carries no rule for a toolset the same-named preset in this build rules on — the envelope is older than the tools. This is invisible from the inside: it is not an error, it is calls to those tools quietly falling through to the file's `default_action`. With a fail-closed default that means an approval card per read; with `default_action: allow` it means those calls run unreviewed.

**Fix.**

```bash
contenox init --refresh-policies
```

This rewrites **only** the `hitl-policy-*.json` presets, in `~/.contenox` and in any workspace `.contenox` copy that shadows it. Chains, config, and sessions are untouched — but your own edits to those policy files are replaced. If you hand-authored a preset, copy it out first, or add the missing rules yourself: the notice stops the moment the file gains them, by refresh or by your own hand.

A policy never gates which tools the model can *see* — that is the chain's tool allowlist. It gates what happens when one is called.

## `doctor` says a trusted-binary declaration stopped matching

**Symptom.** A line reporting trusted-binary drift on a policy file.

**Cause.** A `trusted_binaries` block pins a command name to an absolute path and a SHA256. The binary changed — usually a legitimate upgrade. The allow is withdrawn and the call now asks a human, which is correct but gives no clue that a binary moved underneath it.

**Fix.** Look at the declarations first, then re-read them:

```bash
contenox hitl trust --list      # every declaration and its state on this host
contenox hitl trust --refresh   # re-read every declaration and rewrite its hash
```

`--refresh` is the upgrade path: it re-resolves and re-hashes what is already declared, and adds nothing new. See [Trusted binaries](/docs/guide/trusted-binaries/).

## A chain or policy file fails to load

**Symptom.** A run fails at load time, or an envelope name is refused.

**Cause.** A structural defect in the file — a dangling `goto`, a handler signature mismatch, an unknown field, a rule pattern that can never match.

**Fix.** Lint before anything runs it:

```bash
contenox vet                 # every .json in the workspace .contenox/
contenox vet --all           # plus ~/.contenox/
contenox vet ./my-chain.json # one file
```

Unlike `doctor`, **`vet` is a pass/fail gate**: it exits non-zero when any vetted file fails, so it belongs in CI. A `WARN` line is not a failure — it means a field parses and is accepted but is not enforced as strongly as it reads, and it names what to rely on instead.

Files are classified by content: a `"tasks"` array is a chain, a `"rules"` array (or a `hitl-policy-*.json` name) is an envelope; anything else is skipped. If your file is being skipped, that is why.

## Every tool call returns `tool_result_too_large`

**Symptom.** The agent reports it cannot read anything. Tool results come back as
`{"error":"tool_result_too_large", ...}` with a `max_bytes` of `0`, even for a
file of a few hundred bytes.

**Cause.** The chain has no `token_limit`. The per-call tool-result cap is
derived from what is left of that budget, so an absent one leaves nothing to
spend and every result is over the limit.

**Fix.** Set it at the top level of the chain file:

```json
{ "id": "my-chain", "token_limit": 131072, "tasks": [ ... ] }
```

Use the context window of the model you configured. The shipped chains use
`131072`. See [chain structure](/docs/specification/#chain-structure).

## Recovering after a crash or a restart

**Nothing resumes a *run*.** Reopening a session is not resuming a run — resuming a run is not a verb, it is a side effect of the two commands you would run anyway.

**`contenox approvals respond <id> …`** records the verdict and, when a checkpoint exists under that ask, resumes the suspended run *in the responding process*. Ordering matters here and is deliberate: for a checkpointed run, the process proves it can build an engine **before** anything is recorded. A process with no usable model configuration is refused outright and the ask stays pending, answerable from a terminal that can reach your models — because a checkpointed run's verdict is one-shot and must not be spent by a process that cannot act on it.

**`contenox approvals list`** is the reconciling read. Every run of it does three things:

1. Applies expired asks' `on_timeout` verdicts.
2. Finds **stranded checkpoints** — an answered ask whose run was never claimed, or whose claim is older than the 10-minute resume staleness bound — and carries them to completion in this process. It only builds an engine when a strand actually exists.
3. Lists what is still pending.

If this process cannot build an engine, it says so and leaves the strands for the next capable one, rather than failing them.

A resume that itself fails is not lost: its checkpoint is retained with the failure recorded and becomes reclaimable again once its claim goes stale. And a resume that died *after* the approved tool call ran does not re-run it — the claiming resumer records the gate call's result on the checkpoint the moment it completes, so the retry replays that result instead of executing the side effect twice.

### What survives what

| Failure | What survives | What you run |
|---|---|---|
| **The firing CLI exits** (`mission fire --wait` timed out, or you hit Ctrl-C) | The mission record and every report filed so far. The unit is a child of that process and is torn down with it. | `contenox mission show <id>` to read it; `contenox mission stop <id>` to close it now instead of waiting for reclaim |
| **A host dies** (`contenox acp` or the firing CLI is killed or crashes) | Everything durable. Its units die with it; their mission rows stay `open` with a heartbeat that will never advance. | `contenox mission list` (or `mission show`, or `doctor`) — each reclaims dead-host missions on the way. A host booting sweeps too. |
| **The asking process dies** while an ask is pending | The ask row, and the run's checkpoint once the park window has elapsed and the run released its process. | `contenox approvals respond <id> …` — it resumes the run here. If nothing was checkpointed under it, the verdict is recorded and it says so plainly. |
| **A resumer dies mid-resume** | The checkpoint, with its claim. The claim goes stale after 10 minutes. | `contenox approvals list` — it re-derives the stranded set and finishes them in that process |
| **The machine restarts** | All of it — missions, reports, asks, checkpoints, inbox, config are in the local database. Nothing resumes on its own; there is no daemon. | `contenox approvals list`, then `contenox mission list` (or `contenox doctor`) |

## A mission is stuck in `open` and nothing is running it

**Symptom.** `contenox mission list` shows an `open` mission, its heartbeat is hours old, and no process is driving it.

**Cause.** A mission unit is a child subprocess of the host that fired it. The host is gone; the row is not.

**Fix — wait, or close it yourself.** The runtime reclaims it automatically, but on a deliberately generous bound: **six hours of heartbeat silence**, widened further when the mission has an ask parked on it whose own wait window is longer than that. Reaping live work is unrecoverable; reaping late only delays a row you were already ignoring.

The sweep is lazy, not scheduled. It runs on a host coming up, on `contenox mission list`, on `contenox mission show`, and on `contenox doctor` (text output). With none of those happening, the row simply waits.

A reclaim is never silent — it finishes the mission `abandoned` with the reason `reclaimed: host process gone` plus the measured silence, and files a blocker report explaining it. That reads differently from a mission you stopped yourself (`stopped by operator`), so the two are never confused.

If you do not want to wait out the six hours:

```bash
contenox mission stop <mission-id> --reason "host is gone"
```

This abandons it now, closes its pending asks, and reaps any live unit through its host. Note what a reclaim does **not** touch: any run the mission checkpointed is untouched, and any ask it filed is left pending on its own expiry. Those are `contenox approvals list`'s business, not the mission sweep's.

See [Missions](/docs/guide/missions/) for the full lifecycle.

## Filing a bug: `doctor --bundle`

When something is wrong in a way this page does not cover, produce the attachment a maintainer will ask for:

```bash
contenox doctor --bundle
contenox doctor --bundle --bundle-out /tmp/report.zip
contenox doctor --json --bundle          # bundle notes go to stderr; stdout stays parseable JSON
```

It prints:

```
Bundle: /home/you/contenox-doctor-20260805-142230.zip
  Contents: doctor.json, build.txt, and any telemetry.log found.
  Redacted: 3 credential-shaped value(s). Review the file before sharing it.
  Report:   https://github.com/contenox/contenox/issues/new?title=…&body=…
```

**What is in the zip:**

| Member | Contents |
|---|---|
| `doctor.json` | The full `doctor` report, exactly as `--json` renders it |
| `build.txt` | contenox version, Go version, platform, module, and VCS/build settings |
| `logs/<source>/<name>` | The **last 256 KB** of each `telemetry.log` found, looked for in the workspace `.contenox`, beside the database, and in `~/.contenox` — the member name records which |

**What is redacted.** Every member is passed through the same credential scrubber on the way in, including `doctor.json`, whose backend URLs can carry a key in the query string. It matches named assignments (`api_key=`, `token:`, `authorization=`, …), URL userinfo (`scheme://user:secret@host`), bearer tokens, and recognizable provider key shapes — OpenAI, Anthropic, Google, GitHub, AWS access key ids, Google OAuth tokens. The field names and punctuation survive so a redacted log stays greppable; the value becomes `[REDACTED]`.

It errs toward over-redaction on purpose: a false positive costs one diagnostic line, a false negative costs a key. It is still a heuristic. **Review the file before sharing it** — the printed redaction count is there so you can sanity-check it rather than trust it silently.

The issue URL is pre-filled with the environment facts a maintainer asks for first — version, platform, ready verdict, provider/model, backend counts — and names the bundle as the attachment. It carries **no log content**: only you decide what leaves your machine.

## Next

- [Missions](/docs/guide/missions/) — the mission lifecycle, states, and reclaim in full
- [HITL policies](/docs/guide/hitl/) — the policy format and the shipped presets
- [Trusted binaries](/docs/guide/trusted-binaries/) — what a pin protects and what it does not
- [Quickstart](/docs/guide/quickstart/) — first-run setup, if `doctor` says nothing is configured
- [`contenox doctor` reference](/docs/reference/contenox-cli/#contenox-doctor) · [`contenox approvals` reference](/docs/reference/contenox-cli/#contenox-approvals)
