---
title: "runnerd: the self-hosted simulation runner"
description: A self-hosted agent that enrolled once, dialed home outbound-only, and turned a scenario into many branching AI-driven exercises with an auditable evidence trail.
---

# runnerd: the self-hosted simulation runner

runnerd was the self-hosted half of a tabletop-exercise simulation product: a small binary a customer ran on their own Linux box, enrolled once against a hosted panel by exchanging a one-time token for a long-lived credential, then only ever dialed out — heartbeat, poll, execute, upload — with no inbound port ever opened. Given a scenario, it drove one or many independent branches through a pluggable engine: a hermetic mode that spawned a fresh agent subprocess per branch over the runtime's own ACP client machinery, needing no GPU or model at all, and a real local-inference mode that drove a running `modeld` daemon directly over its own session seam.

A scenario could carry participant roles and a scripted timeline of injected events, turning a single-shot run into a bounded multi-turn exercise — and when a role was marked human, the runner parked mid-run and waited for a live person to submit their move before the AI played its own reaction, rotating through every human party in turn. Every run closed with a size-capped evidence bundle: a full transcript, timings, and outcomes, with confidential participant goals and any grounding material redacted from the recorded form even though they had shaped what the model saw — an explicit redaction seam rather than an accidental leak.

## What it proved

- **An outbound-only, enroll-once dial-home model is a workable way to run agent work on infrastructure you don't own and never need to reach** — the same shape self-hosted CI runners use, applied to AI-driven exercises.
- **Sharing one prefilled prompt prefix across every branch of a run, and only re-prefilling the part that diverged, measured out real:** 2.5x faster wall-clock and 16x fewer prompt tokens processed at 16 branches, with flat memory instead of one resident copy per branch.
- **A redaction seam — state that shapes the model's prompt without ever appearing in the recorded evidence — proved a workable pattern** for exercises that need to be both realistic and safe to hand to an auditor.
- **Local, lexical-only grounding proved a run could draw on an organization's own documents while keeping a real promise:** their contents never left the machine that read them, only the filenames a run cited.

## Where this lives now

The shape lives on: dispatch a bounded, policy-governed unit of unattended work, let it park for a human answer mid-run, and collect a durable report — exactly what the runtime's own fleet does today. `contenox mission fire` dispatches an enveloped unit, and `mission asks` surfaces precisely the kind of paused, waiting-on-a-human moment runnerd's parked branches needed.
