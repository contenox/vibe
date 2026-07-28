---
title: "blueprints: the schema-validated app generator"
description: A declarative page format, a validator that never stops at the first problem, and a bounded AI draft-and-repair loop that turned a plain-English ask into a working page bound to real backend operations.
---

# blueprints: the schema-validated app generator

blueprints defined a small, closed document format for describing a page as data: a tree of typed blocks, each field either a literal or a template reference into a query or mutation, and every query or mutation bound to a real backend operation by its stable operation ID rather than a URL. One validator checked a document against every rule the format could express structurally and every rule it couldn't — id uniqueness, variable scoping, an operation id that actually existed — and it never stopped at the first problem: it collected every finding in one pass, each with an exact path to where it lived, so anything reading the result got the complete picture at once. The same expression language that resolved those references ran identically on the server, inside an embedded JS interpreter, and in the browser, off one shared implementation — zero drift between the two hosts by construction.

On top of that sat a generator: a plain-English description went in, a model drafted a page document, the validator's complete diagnostics fed straight back as the repair signal, and the loop closed — bounded to a handful of rounds — on a document proven valid against a real target API. A step further, the same loop could synthesize the backend operations themselves and merge them safely into an existing API surface, so a page and the endpoints it needed could both come from one description. Underneath both, a human's edit was sacred: a single explicit marker froze exactly the node a person had touched, so the next regeneration rebuilt everything around it without ever clobbering what they'd changed. The system dogfooded itself end to end — it rendered its own marketing site's landing page as one such document.

## What it proved

- **Bounding an AI generation loop to a few draft-and-repair rounds against a strict, complete-diagnostics validator turned "describe it in plain English" into a page that reliably validated against a real API** — not just a demo that worked once.
- **A single explicit per-node marker was enough to make regeneration edit-preserving:** a human's hand-tuned change survived every future AI pass over the same document.
- **One expression-language implementation, shared byte-for-byte between the server and the browser, proved a single evaluator can serve two hosts with zero drift between them.**
- **The generator could extend the backend it targeted, not just consume it** — synthesized operations merged safely into a live API surface with the same validation the hand-written ones got.

## Where this lives now

The edit-is-sacred rule it proved out — freeze exactly the node a person touched, regenerate everything else around it — is the same instinct the runtime's human-in-the-loop model runs on today: an approval is never something the next turn quietly redoes out from under you.
