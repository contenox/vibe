---
title: "The God Model Fallacy: Why the AI Future Looks Exactly Like 1987"
description: Betting everything on ever-larger frontier models repeats the Lisp-machine collapse of 1987. The durable future is commodity models chained into specialists, paid for in engineering effort instead of token bills.
---

# The God Model Fallacy: Why the AI Future Looks Exactly Like 1987

I spent the better part of a year building what I privately called "Kubernetes for GenAI" before I understood what I was actually watching: a rerun of the 1987 Lisp-machine collapse, in real time.

The 1987 playbook: Lisp machines were sold as the only hardware capable of "real AI" — the expert systems of that era. Then ordinary Sun and Apollo workstations, running the exact same Lisp code, became good enough at a fraction of the price. Every specialized AI-hardware company that had bet on the expensive box went to zero. The technology didn't die; it survived inside Python, Java, and JavaScript — ordinary languages, running on ordinary machines.

The mapping onto today is direct. Today's flagship frontier models are the Lisp machines. The GPU racks they run on are the six-figure Symbolics boxes. Commodity open-weight models, fine-tuned and chained for a narrow job, are the Sun workstations that are already good enough.

The real future isn't a bigger brain. It's Unix philosophy applied to model serving: a tiny router, a retriever, a narrow specialist for the actual sub-task — code, math, vision, whatever it is — and a synthesizer to assemble the answer. Run well, that whole chain fits on ordinary consumer hardware for pennies per call, not a metered subscription to somebody else's frontier model.

## The Integration Tax

What stands between "commodity models are good enough" and "everyone actually runs this way" isn't cost. It's what I call the Integration Tax. A monolithic frontier model buys high token bills and comparatively little engineering pain — point a prompt at it and it mostly works. A chained-specialist system flips that trade: token bills fall toward zero, but the systems-engineering burden — routing, state, retries, evaluation, the glue between steps — becomes the whole job.

That tax is the bubble popper. It kills the cute demo built on one clever prompt. It does not kill the boring pipelines whose payoff was never the demo in the first place — document processing, discovery, ticket triage, and the like — because those were always paying for throughput, not for a magic trick.

## Chaining specialists isn't magic

I built the model-calls-model piece of this myself, and it isn't magic. The interesting part isn't wiring one model up as a callable tool for another; it's that a model can't reliably improvise the combinatorics of an arbitrary new request every single time. What worked was closer to a compiler: pre-run the scenarios you actually expect, turn that into a blueprint, and let the chain execute the blueprint instead of re-deriving it from scratch on every call. That requires knowing the use case and the shape of its answer ahead of time. It is not AGI. It is engineering.

My own scar tissue: I over-invested in the "one model to rule them all" story, and learned the hard way that magic is expensive and depreciates fast. The real engineering — the boring kind that actually ships and keeps shipping — is only starting now. A model small enough to run on ordinary hardware will, for most of what people actually do day to day, be more than enough.

---

*Originally published on r/LLMDevs, November 2025.*
