---
title: What contenox is
description: The rules an AI agent runs under are a file you wrote, in your repo, that you can read — and nothing runs that the file does not allow. What contenox is, why it exists, what it is typically used for, and what it is not.
---

# What contenox is

**The rules your AI agent runs under are a file you wrote — and nothing runs
that the file does not allow.**

That file, the approvals it pauses for, and the things allowed to start a run
are the product. Three parts, and they have names:

- **The rule format.** One JSON file — the envelope — that says what passes
  silently, what stops for a human, and what is refused outright. Anything no
  rule covers fails closed: it asks.
- **Approvals that wait.** A question the agent may not answer alone
  checkpoints the run. The process is released. You answer days later from any
  terminal — `contenox approvals respond` — and it continues exactly once.
- **Declared starts.** What sets work going — a schedule, a signed message from
  another system, a form on your own site — is something you write down, not
  something that happens to you.

Everything you can see is a wrapper around that. The terminal UI, the CLI, the
browser app: all of them are example implementations, and they change. The
three things above are what stays.

## Why I built it

I wanted to hand an agent a job at three in the morning and go back to sleep.

Not because unattended work is impressive — because the alternative is sitting
and watching, and an agent's runtime and a human's attention are never going to
line up. What stopped me was not whether the model was good enough. It was that
I had no way to say *this directory is untouchable* and know it would hold when
something decided a cleanup was in order.

So the answer had to be a file I wrote, that I could read back, that was
enforced by something other than the model's good intentions. Once that file
existed, the rest followed: approvals had to survive me closing the laptop, and
the things that trigger work had to be declared rather than discovered.

There is a second reason, and it is not smaller. I did not want the record of
how my work behaves to live in someone else's dashboard — a product that can
pivot, exit, leak, or turn my usage into their next feature. It runs on my
machine. That is a choice about who holds the evidence, not a deployment
preference.

## What it is typically used for

- An overnight mission under a strict envelope — review a diff, run the tests,
  draft the patch — that stops and asks before it touches the branch.
- Work that fires from something else: a webhook, a schedule, a form on your
  own site, each running a chain on a machine you already own.
- Local inference where nothing may leave the network, or a hosted model on
  your own key and your own region when that is the honest trade.
- The unglamorous case, and the most common one: a repeatable job you would
  otherwise have written a shell script for, except this one can ask you a
  question halfway through.

## What it is not

- **Not a chat product.** There is no thread to keep warm and nothing that
  wants you back tomorrow.
- **Not a dashboard.** No screen summarises what an agent did. If you want to
  know, you read the captured execution state.
- **Not a hosted agent.** Nothing of yours runs on our machines. The optional
  relay carries a session to your browser and stores none of it.
- **Not a compliance badge.** It does not make a deployment compliant with
  anything. It produces controls your own assessment can point at — rules you
  wrote, approvals recorded, execution state captured.
- **Not autonomous.** It does exactly what you declared and stops where you
  said stop. That is the feature.

## Why this matters now

An agent that only writes text is a text problem. An agent that runs shell
commands, edits files and calls APIs is an operations problem, and the question
stops being *was the answer good* and becomes *what was it allowed to touch,
and who decided that*.

That second question has an answer only if the answer existed before the run
and was enforced during it. Written afterwards, it is a story. Written
beforehand, in a file under review like any other change, it is a control.

This is also the part that regulators, insurers and your own incident review
will ask about, in that order — and none of them accept "the model usually
behaves". You do not need a policy for that. You need a file, and something
that refuses to run past it.

## Where to go next

- [Core concepts](/docs/guide/concepts/) — chains, tasks, tools, transitions.
- [Human-in-the-loop policies](/docs/guide/hitl/) — the full envelope format.
- [Missions](/docs/guide/missions/) — unattended runs, start to finish.
- [Sovereignty](/docs/guide/sovereignty/) — local inference, EU regions, and
  what the EU AI Act asks of whoever deploys.
