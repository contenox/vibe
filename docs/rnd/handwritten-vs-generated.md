---
title: "The generation loop"
description: Two codebases in the same domain by the same author, one written by hand in 2025 and one AI-assisted in 2026, measured side by side. Size was never the variable. Readability, deletion, and naming were.
---

# The generation loop

Use AI to write code and you end up generating code that generates more code. Then you try to use the thing, and it wants another round.

That is not a complaint about volume. It is the shape of the work. Generated code needs generated tests to pin it down, because nobody read it closely enough to know what it does. The tests need generated docs to say why they exist. The docs drift, so you generate a gate to check them. The gate flags things, so you generate a sweep to clean up what it found. Each of those is real work product and each is now a thing you maintain. And the first time you try to *use* the capability, wiring it to a second surface or calling it from a script, the seam does not quite fit, and the cheapest fix is to generate an adapter.

I have two codebases that let me look at this with numbers instead of impressions. Same domain, same author, about a year apart. The first was written by hand in 2025: the [Telegram and GitHub MVP](/docs/rnd/mvp/). The second was written in 2026 with heavy AI assistance, and it is the one that ships.

| | Hand-written, 2025 | AI-assisted, 2026 |
|---|---|---|
| Production Go | 17,267 lines | 134,226 lines |
| Packages | 39 | 123 |
| Test code | 7,735 lines (0.45:1) | 124,615 lines (0.93:1) |
| Surfaces | 2 | 7 |
| Model providers | 1 | 8 |

The obvious read is "AI writes too much code." That read is wrong, and I want to kill it before it gets comfortable. The 2025 codebase was only 17,267 lines because it imported an engine that was already 113,931 lines. Counting honestly, the hand-written system was about 130,000 lines of Go the day it shipped. The successor is 134,226. Size did not move. What moved is that two surfaces became seven and one provider became eight. The bigger thing does more.

So the numbers are not the finding. The finding is what happened to the habits underneath them once typing got cheap.

## A function used to be a complete thought

In the old codebase, creating a chat instance is 17 lines. Not a wrapper around the real thing, the whole operation: take the request, resolve who is asking, store the opening instruction, record the first message, return. Every line names something from the problem. Chat, instruction, message, identity. You read it once, top to bottom, and you are done. If it is wrong, it is wrong somewhere you can point at.

The equivalent entry point in the new codebase is four lines. Shorter. It dispatches into five concepts, and not one of them can be evaluated without leaving the file. The words are transport, driver, seam, envelope, dropped content kinds. All five are the implementation describing itself. None of them came from the problem. Reading the 17 lines is reading a page. Reading the four is traversing a graph, and you come out the other side holding a model of the machinery rather than a model of what the code does.

That is the tax that shows up in no line count. There is not more code here. There is more *distance* per operation.

## Refactoring means deleting the old shape

When I refactor by hand, the old shape goes away. That is most of the value. Afterwards there is one way the thing works, and the way it used to work is gone from the tree and lives in the history, which is where it belongs.

AI-assisted refactoring does something else. It puts the new shape next to the old one, keeps both working, and writes a comment reconciling them. There is a one-line function in the new codebase whose comment explains that it is byte-identical to the path that existed before a particular seam was introduced. The comment is accurate. It is also documenting a refactor that no longer exists anywhere except in that sentence. The function is a fossil with a plaque on it.

Do that for a few weeks and the codebase becomes sediment: every intermediate state it passed through, preserved in order, all of it compiling. The comments give it away first. They stop describing the thing and start justifying the diff. A comment explaining why this differs from what was here before is written for a reviewer who read the previous version, and that reviewer will never exist again.

## The prefix is where the filing system should have been

One package in the new codebase holds 136 files, 15,236 lines, and 1,002 functions in a single flat namespace. For scale: that one package is 88% of the entire production code of the hand-written system. The hand-written system had 136 files in total, spread across 39 packages.

Nothing forces a package boundary, so none appeared. The cost turns up in the names, which is the part I did not expect. In that package, 54 function names begin with `run`, 20 with `resolve`, 18 with `render`, 15 with `print`. One small feature contributes twelve identifiers, eleven of which open by restating which feature they belong to.

That prefix is doing the job a package name does. It is a filing system typed out by hand, at every definition and every call site, forever. In the old codebase the equivalent type is `ChatRequest` in a package called `chatservice`. The package carries the qualifier so the name does not have to. That is not a style preference. It is the difference between paying for structure once and paying for it at every reference.

## Nothing gets removed because nothing gets disproved

The old codebase was built in month-long slices, each with a written record of what it was for. Roughly 40% of that record is struck through and marked deprecated. Features built, shipped, used, then killed on purpose. The record shows a codebase getting smaller by intent, repeatedly.

There is no equivalent column in the new work and no equivalent act. Not because deletion got harder. Because nothing ever proves itself unnecessary. When adding is close to free, the thing that used to force the decision, which was simply that I did not have time to maintain both, stops firing. Every capability that was ever plausible is still in the tree, still compiling, still in the suite, still in the docs, still costing.

Deletion was never a cleanup task. It was the feedback signal. Losing it is the expensive part.

## Tests as a substitute for users

Test code against source code in the new tree, by layer:

| Layer | Test lines per source line |
|---|---|
| Presentation surfaces | 1.11, 1.12, 0.89 |
| Capability layer | 0.57, 0.66 |

The parts a person looks at are tested about twice as heavily as the parts that do the work. That is backwards, and the reason is not carelessness. A surface is where behavior is observable without understanding the system, so it is where a test can be written confidently by someone, or something, that has not understood the system. The capability layer requires knowing what correct means, and there is no shortcut to that.

Some of those tests are the good kind. A test that remembers a specific failure — this input, this crash, never again — is worth more than the code it guards, and it should outlive several rewrites of that code. But most of the volume is not that. Most of it pins internal shape: this function is called with these arguments in this order. That is concrete poured around whatever shape happened to exist on the day it was poured.

Here is the loop worth naming honestly, because it does not have a clean exit. Code that cannot be read makes its tests load-bearing; they become the only place the intended behavior is written down. Load-bearing tests make the shape permanent, because changing the shape now breaks the specification. So the unreadable thing becomes the thing you cannot change. Writing better tests does not break this loop. The tests are downstream of it.

## Pointed at a working solution, it reinterprets

One more, short, because it caught me out. I asked for a shipped, working component to be brought back with one part of it removed. What came back had kept the replacement for the part that was supposed to go, and had dropped several pieces that were load-bearing. Each loss was documented in a header comment as a deliberate change.

The failure is not laziness. The work was careful and the comments were honest about what changed. The failure is that a plausible improvement is always available, and nothing in the process ever asks whether this was already solved. Restoration and reinterpretation look identical from the inside. Someone who wrote the original recognises the shape and puts it back; anything else rebuilds from the description, which is lossy, and what gets lost is exactly the parts that were there for a reason nobody wrote down.

## What I do differently now

None of this is an argument against generation. I build an agent harness. I would not be shipping it if I thought the tools were a mistake, and generation is genuinely good at the part that used to be slow. What it does not do is the other half, and the other half is smaller than it sounds: deleting, bounding, naming.

So, in practice.

Work in slices small enough to finish, and keep the written record. The record matters more than it sounds like it should, because it is the only place a deprecation can be written down.

Keep the deprecation column and use it. If nothing has been struck through this month, nothing has been disproved this month, and that is information about the process rather than about the code.

When the shape changes, delete the old shape in the same change. Two shapes and a reconciling comment is not a refactor. It is a third thing to maintain.

Make readability the acceptance criterion, not correctness. Correctness is what the tests are for, and the tests will pass. The question that has to be asked out loud is whether the next person can read the operation top to bottom without opening five files. If not, the change is not done, however green the run was.

Keep the tests that remember a failure. Be suspicious of the ones that only remember a shape.

None of that is new. It is what everyone already knew about writing code by hand. What changed is that generation removed the friction that used to enforce it, so now it has to be enforced on purpose.
