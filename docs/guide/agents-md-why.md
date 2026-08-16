---
title: Why a project needs an AGENTS.md
description: An AGENTS.md carries what is true about a repository but not inferable from its code — read-only boundaries, silent tooling contracts, traps already paid for, and where prior art lives — because a wrong assumption in an agent brief executes at scale.
order: 7
---

# Why a project needs an AGENTS.md

A human who is wrong writes one wrong line. An agent that is wrong writes forty files. That asymmetry is the whole argument for `AGENTS.md`; everything else here follows from it.

The mechanics are covered separately. [AGENTS.md](/docs/guide/agents-md/) explains how the file is located, how nested files resolve, the size cap, and when an edit takes effect. This page is about why the file earns its place in a repository, what specifically breaks without it, and where its authority ends.

## An assumption in a brief executes at scale

A wrong belief held by a person is bounded by how much that person types. A wrong belief held at the start of an agent session is not. It is carried into the plan, into every file the plan touches, and — worse — into the brief handed to the next agent, which will implement it faithfully and at speed. We have watched a single unverified claim about our own tree get written into a brief and cost real work to unwind, when the command that would have falsified it was one line away.

This is why the file is not optional politeness. The cost of a missing fact is not "the agent asks a question." The cost is a confident, internally consistent, wrong implementation, delivered fast enough that review is the first place anyone notices.

An `AGENTS.md` does not make a model correct. It removes a category of guesses by making the answer present before the guessing starts.

## The four things that are not in the code

The useful test for what belongs in the file is not "is this important?" — most important things are already in the code, and repeating them there is how the file rots. The test is: **is this true, and is it impossible to infer from reading the tree?** Four kinds of fact pass that test.

### Where the thing you are about to build already lives

The most expensive failure is not a bug. It is a competent implementation of something that already existed twenty files away.

In one day, on one repository, an agent built a migration mechanism that a predecessor system had already solved more simply, hand-rolled a parser nothing else in the tree used, wrote a version-gated runner to survive a constraint that should not have existed, and introduced mutable status columns where two prior systems had already settled on event logs. None of that was carelessness. Each was a reasonable solution to the problem as stated. The brief described a solution instead of pointing at the one already in the tree, and the agent did exactly as briefed.

An agent's search is shallower than it appears. It finds what its query names, and its query is shaped by the solution it has already begun imagining. Prior art that lives under a different vocabulary — or in a deleted directory still reachable through git history — is effectively invisible. A line in `AGENTS.md` that says *this pattern is already solved in X, copy it* converts an invisible fact into a visible one, and it is the highest-leverage sentence the file can contain.

The same rule applies to reasoning that arrives from a subagent. Treat it as a claim to check against existing work, not a conclusion to forward.

### Contracts enforced by tooling that says nothing

Some repositories contain a generator that reads source text rather than a parsed model: it recognizes a shape and, for anything shaped differently, emits nothing at all. No error, no warning, no failing build. A registrar under a different name, a route written as a constant instead of a literal, an annotation moved two lines, and the generated output silently omits that surface.

Nobody infers that rule from reading the code, because the code that would have complained does not exist. The generator is not lying; it simply has no vocabulary for the case. A human learns this once, painfully, and remembers. An agent has no such memory, and will violate the rule forever, plausibly, once per session.

Rules of this shape — invisible, unenforced, and load-bearing — are the clearest case for writing something down. State the rule, name the file that holds the detail, and say what the failure looks like, because the failure is silence and nobody will recognize it otherwise.

### Boundaries whose reason is not visible from inside

A directory can be a read-only mirror of another repository. From inside that directory everything looks like ordinary source: it compiles, it has tests, a fix in passing would be easy and would look correct in review. The reason it must not be edited — that it is a copy, that changes belong somewhere else, that things written there leave the boundary they were meant to stay inside — is nowhere in the files themselves.

The same holds for generated files that are committed, for directories whose contents are snapshots and must not be "corrected," and for any structure whose rationale lives one level above the code. If the boundary is not stated, an agent will cross it politely and helpfully, and the diff will look like an improvement.

### Traps already paid for, and actions no agent should take unattended

Every long-lived repository accumulates knowledge that was purchased with an outage. Declarative apply that never deletes, so a removed manifest keeps running in production indefinitely. A storage class that quietly dies with its node. A signing path where an encoding mismatch produced a full run of invalid signatures — every one of them, not a sampled few.

These are worth writing down precisely because they are counter-intuitive: the code looks right, and the mental model that produces the mistake is the reasonable one. Keep the fact, shorten the story, never delete it.

A subset of these facts is not a trap but a rule: **some commands must never run without a human executing them.** We write that rule down because we have already been on the wrong side of it. A suggested command carrying a safety flag was mistyped into a real deletion and took a production site down. The lesson is not "be careful with the flag." It is that a destructive command should not appear in an agent's output as something it may run at all — it goes to a person, or it goes through a gate. [HITL policies](/docs/guide/hitl/) are the enforcement; the line in `AGENTS.md` is the intent, and it covers the cases policy has not caught up with yet.

## Stale documentation is worse than none

A document that is missing produces a question. A document that is wrong produces work.

Our own tree has produced all three of the standard rots. A sentence that said "five workflows" was accurate when written and stayed on the page for months while the number grew. Two live documents each stated a count of the same thing and disagreed. Cross-references written as "see step 4" broke silently the first time anything was reordered, and nobody noticed because nobody reads by number.

A human reading any of these shrugs and moves on; the surrounding context makes the intent obvious. An agent reads the same line as a specification and acts on it. That is the difference that makes a stale `AGENTS.md` more dangerous than an absent one, and it forces a particular way of writing:

- Prefer the invariant to the inventory. "Nothing else can change anything" needs no maintenance; a list of what can does.
- Do not enumerate what a command can list. Point at the source of truth, or give the command.
- Cross-reference by section name, never by number, and quote enough of the target to survive a rename.
- Date decisions and say what they replace, so a reader can separate current from historic without reading the git log.

Every line in the file should be one you would still want there in six months without having touched it.

## The limits, stated plainly

`AGENTS.md` is a small instrument and it is easy to over-trust.

It loads once, at session start, and persists in the conversation for the rest of the run. That is efficient, and it means a stale file misleads with full authority for an entire session; an edit made mid-session changes nothing until a new one begins. It also competes for the same context as your actual task, which is the real reason for the size cap — the file's cost is paid on every session, so a paragraph that earns nothing is a paragraph taken from the work.

More importantly, it is the weakest available enforcement mechanism. It is reference material the model consults, not a constraint the runtime imposes. A rule the compiler could enforce should be enforced by the compiler; a rule a test could catch should be a test; a rule about which operations are dangerous should be a [HITL policy](/docs/guide/hitl/); a boundary that must hold against an untrusted process should be [the sandbox](/docs/guide/confinement/sandbox/), which does not depend on cooperation at all. Writing a rule in prose when the code could refuse the mistake outright is choosing the version that can be forgotten.

What is left after those subtractions is exactly what the file is for: the things that are true, that matter, and that no tool in the repository can tell you.

## What the file actually is

An `AGENTS.md` is not documentation for humans that agents happen to read. It is a ledger of what a codebase knows about itself but cannot say in code — its invariants, its boundaries, its prior art, and the traps it has already paid for once and does not intend to pay for again.

Write it the day after something goes wrong, from the specific thing that went wrong. A file assembled that way stays short, stays true, and earns its place in every session it loads into.

## Next steps

- [AGENTS.md](/docs/guide/agents-md/) — the mechanics: loading, closest-wins precedence, the size cap, and staleness.
- [HITL policies](/docs/guide/hitl/) — turning "never run this unattended" from a written intent into an enforced gate.
- [Why contenox confines agents](/docs/guide/confinement/why/) — the case for controls that hold without the agent's cooperation.
