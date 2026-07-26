# Why V1 looks like this

This file explains the reasoning behind the V1 reshape — for contributors and
users wondering where things went, and for steering: if the motives written
here look wrong, that's a bug worth filing.

## The bet

Contenox V1 is a bet on **one product surface: the terminal**. The `contenox`
CLI, ACP editor sessions, and the `contenox beam` TUI (a real coding TUI, not
a control panel) are the product. Everything else was removed.

## Why cut so much?

**Focus beats breadth for product-market fit.** The project had grown seven
surfaces — a web UI, an HTTP API + framework + spec generator, a VS Code
extension, a local-inference daemon, a UI library, and the CLI. Seven surfaces
means no crisp answer to "what is this?". V1 picks the surface where the pull
actually is — terminal-native agent work — and commits to it.

**Everything visible must actually work.** A small team cannot honestly
certify a web UI, a REST API, a marketplace extension, and a native inference
daemon at once. Shrinking the surface is what makes the quality bar real
instead of aspirational.

**Every extra surface taxes every change.** The API layer alone coupled 24
route packages into the CLI binary; modeld carried its own C/C++ build
matrix, release runbooks, and dependency bundles. That tax was being paid on
every commit, for surfaces few people used.

**The cut work was R&D, not waste.** Beam (the web UI), modeld, the VS Code
extension, and the API stack taught us what the product actually is — and
they remain presented as research in the website docs' R&D section (the one
place past work lives, by policy). modeld's job is now done better for us by
Ollama and vLLM; the Beam name was good enough to keep — it now names the TUI.

**Enjoyable by default, safe where it matters.** The product language shifts
from security lecture to working tool: sensible human-in-the-loop defaults
and chains that run without nagging. Confinement (sandboxing, env scrubbing,
HITL gates) stays on by default — it just stops being the headline.

**A clean history is part of a clean V1.** Compiled binaries that were
accidentally committed are being purged from git history, and website media
moves to S3; a public V1 repo should clone fast and contain source.

## The heart

The surfaces are reach; the heart is the kernel. Eighteen months of matured
execution machinery — typed-IO chains, policy envelopes, an embeddable fleet
kernel, tracing through every step — in the way Dagger's heart is its engine
rather than its CLI. Users meet contenox in a terminal or an editor; what
they're actually holding is a programmable substrate for agentic work that
runs the same unit in a chat turn, a detached mission, or a headless pipeline
loop. The V1 cut removed surfaces, never the heart.

## What this is NOT

- Not an abandonment of multi-surface ambitions forever — it's sequencing.
  The R&D section documents what was learned for whoever picks a thread back up.
- Not a security retreat — defaults stay safe; the *copy* stops nagging.
