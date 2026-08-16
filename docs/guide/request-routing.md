---
title: Request routing
description: One prompt, several specialist loops. The router is where a workflow's expertise is spent — teaching the system what a request means, instead of asking the user to prompt harder.
---

# Request routing

A single agent loop cannot be good at everything. The instruction that makes a model a careful editor of files is not the instruction that makes it a careful reader of them, and the tools one needs are the tools the other must not have. The usual answer is to push the difference onto the user: write a longer prompt, spell out the method, remember to say "do not change anything". That answer scales badly and fails silently — a reasonable sentence gets a shallow answer and nobody can see why.

Contenox puts the difference in the chain instead. A `route` task reads the request, labels it, and hands it to the loop built for that label. The expertise lives in the configuration, authored once, and the user goes on writing ordinary sentences.

This is the layer above [the agentic loop](/docs/guide/agentic-loop/): that page is about how one loop is built, this one is about choosing which loop runs.

## The shape

A routing agent declaration — the shipped `acp` example among them — compiles to a chain that opens the same way:

```json
{
  "id": "classify_request",
  "handler": "route",
  "system_instruction": "Classify the user's latest message. Use 'coding_change' for requests to create, edit, migrate, refactor, fix, test, build, or otherwise change code/files. Use 'general' for questions, explanations, brainstorming, setup help... Use 'review_change' for requests to review, audit, critique, or assess code that already exists...",
  "execute_config": {
    "model": "{{var:alt_model|var:model}}",
    "provider": "{{var:alt_provider|var:provider}}",
    "tools": []
  },
  "transition": {
    "on_failure": "acp_chat",
    "branches": [
      { "operator": "equals", "when": "coding_change", "goto": "coding_chat" },
      { "operator": "equals", "when": "general",       "goto": "acp_chat" },
      { "operator": "equals", "when": "review_change", "goto": "review_chat" },
      { "operator": "default", "when": "",             "goto": "acp_chat" }
    ]
  }
}
```

Four properties are worth naming, because each is a decision:

- **The classifier holds no tools.** `"tools": []`. It decides and nothing else; it cannot act on a request it has not yet understood.
- **It may run on a smaller model.** `{{var:alt_model|var:model}}` falls back to the main model, but labelling one sentence is cheap work and does not need the expensive one.
- **`on_failure` names a real loop.** If classification fails, the request goes to the general assistant rather than to an error. A router that can refuse to route is a new way to fail.
- **The `default` branch is the safe branch.** An unrecognised label lands on the least-powerful loop, never the most.

## What a specialist is

A branch target is not a differently-worded prompt. Each loop carries its own:

- **System instruction** — the method, stated as a contract: what counts as input, what decision is being made, what the output must contain.
- **Tool scope** — `tools` selects toolsets; `hide_tools` withholds individual tools by `toolset.tool` name. This is enforced when a call executes, not merely by omitting the tool from what the model was offered, so a model that asks for a withheld tool anyway still cannot run it.
- **Budget** — the `edge_traversed_at_least` ceiling on its own chat→tools edge, sized for the work that loop does.
- **Recovery** — where it goes when it stalls, and with how much power. Each stage after a failure gets fewer capabilities and a narrower mandate.

The review loop is the clearest illustration. It withholds every mutating tool:

```json
"hide_tools": [
  "local_fs.write_file", "local_fs.edit_file", "local_fs.sed"
]
```

"This loop does not modify anything" is therefore a property of the configuration, not a promise in a prompt — and the same withholding is repeated on the paired `execute_tool_calls` task, because a tool withheld only where the model is asked would still run where calls are executed.

## This is not a coding feature

Routing is what lets one deployment serve requests that need different treatment, in any domain:

- **A support assistant** routes billing questions to a loop with the billing API and no ability to write, and account changes to a loop that can write but pauses for approval.
- **A moderation gate** ([see the story](/docs/use-cases/moderation-gate/)) is a router whose unsafe branch simply never reaches the expensive model.
- **A data workflow** routes "summarise this" and "extract these fields into JSON" to loops with different output contracts, so the caller gets parseable output without asking for it every time.
- **A governed deployment** routes anything touching regulated data to a loop bound by a stricter HITL policy, regardless of how the request was phrased.

In each case the point is the same: the workflow author knows something the user should not have to restate — which tools this kind of request may use, what a good answer looks like, what must never happen. The router is where that knowledge is applied.

## Adding a branch

1. **Name the label** and add one sentence to the classifier's instruction describing it. Say what belongs to it *and* what does not — the sentence that keeps "review and then fix this" out of the review branch is the one that earns its place.
2. **Add the `equals` branch**, above the `default` branch.
3. **Author the loop**: a `chat_completion` task with the method as its instruction and its tool scope declared, plus an `execute_tool_calls` task with `input_var` naming the chat task and a `default` branch back to it.
4. **Give it a ceiling and a failure exit** — an `edge_traversed_at_least` branch ahead of the `tool_call` branch, pointing at a recovery task or `summarise_failure`.

Then check it: `contenox vet path/to/chain.json` refuses a chain whose branches name tasks that do not exist, and a listener that can never fire.

## Reading which branch ran

The chain is data, so the route it took is observable rather than inferred. Each task attempt is logged with its id — `~/.contenox/telemetry.log` records `operation=task_attempt subject=review_chat` — and `contenox events` shows the same journey for a dispatched run. When an answer disappoints, the first question is not "how should I have phrased it" but "which loop got it", and that question has an answer you can read.
