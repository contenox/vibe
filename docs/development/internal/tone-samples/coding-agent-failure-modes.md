---
title: "Six Failure Modes in Coding Agents, and the Staged Loop That Changed Them"
description: A real migration task exposed six specific ways coding agents lose the plot — filesystem flood, cwd loss, write loops, placeholder collapse, destructive recovery, and continue-prompt amnesia — and the staged loop that turned reckless mutation into contained failure.
---

# Six Failure Modes in Coding Agents, and the Staged Loop That Changed Them

I stress-tested a local AI runtime against a real coding task: migrate an existing static, bilingual (English/German) portfolio site to Astro, preserving the original content and styling while moving case studies into Markdown content collections, keeping both language routes intact, and verifying the result actually built and rendered the original content.

The task reads as simple: inspect the existing HTML/CSS/assets, stand up the Astro structure, move entries into Markdown, preserve the EN/DE routes, run the build, confirm the rendered page still holds the original content. What mattered wasn't which model ran the loop — swapping the frontier model produced the same shape of failure. What mattered were the failures themselves, and they were specific enough to name.

## The six failure modes

**Filesystem context flood.** When the agent lost its footing, it called a directory listing on the project root. The tool obliged and returned `.git` and `node_modules` along with everything else — thousands of irrelevant paths dumped straight into context. After that, the agent's sense of where anything was got measurably worse.

**CWD / path-state loss.** The agent became confused about where it had actually written files, at one point acting as though a subdirectory it had created was the project root, then continuing to patch paths against that mistaken belief.

**Repeated write/tool loops.** The agent rewrote the same file over and over with empty or near-empty diffs. It read as "still working." No real progress was happening underneath it.

**Placeholder collapse.** Once the task grew past what the agent could hold, it quietly substituted real migration work with placeholder content — "Placeholder TLDR," "This is the about page." The build passed. The actual task had not.

**Destructive recovery.** When verification failed, the agent's own fix sometimes moved or copied files in a way that risked overwriting a language variant, or the original content it was supposed to preserve.

**Continue-prompt state loss.** After the agent stalled, telling it to "step back and continue" was enough to make a naive classifier read it as a fresh, general chat message instead of a resumption of the active coding task — dropping exactly the task state that mattered most.

## The staged loop

The one change that measurably helped was giving up on the open-ended loop — user, model, tools, model, tools, forever — for something staged: classify, then inspect (read-only), then patch (mutate), then verify (read-only), then audit (read-only), with an explicit revise-or-block step folded in. It didn't magically solve the task. The model still made mistakes. But it changed the failure mode: confusion stopped automatically becoming destructive mutation, because mutation was no longer the loop's default next move.

## Where this lives now

That staged loop is not a one-off finding. It is, in shape, the chain architecture the runtime ships today. Both `internal/surfaces/contenoxcli/chain-acp.json` (the ACP editor integration) and `chain-beam.json` (the terminal UI) route a request through a `classify_request` stage first, into a bounded chat/tool-execution loop for the coding path, into a separate recovery loop once that loop exhausts its tool-call budget, and finally into a `summarise_failure` stage that reports the actual state honestly rather than claiming a completion that never happened. The vocabulary changed — inspect, patch, verify, and audit became a single tool-using loop bounded by an edge-count budget, and recovery absorbed what used to be a distinct revise-or-block step — but the underlying discipline is the same one this stress test forced into the open: never let confusion default into unattended mutation.

---

*Originally published on r/ollama and r/opencodeCLI, May 2026.*
