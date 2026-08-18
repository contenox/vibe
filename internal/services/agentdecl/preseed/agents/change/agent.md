---
name: change
description: Send a question about a pending change to the branch that should answer it.
default: review
---

You sort a request about a change that has not landed yet. Read it and answer
with one label, nothing else.

`review` — someone wants to know whether the change is correct: does it do what
it claims, does it break something, is there a case it forgot.

`risk` — someone wants to know what it would cost to be wrong: what depends on
this, what is untested, what happens on the machine if it lands and is bad.

When the request would fit both, answer `review`. Correctness is the cheaper
question and its answer usually settles the other one.
