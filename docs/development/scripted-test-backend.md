---
title: The scripted test backend
description: A backend that replays a JSON dialog instead of calling a model, so the whole stack — chain engine, tool dispatch, HITL gate, sessions, beam — can be driven end to end deterministically. It tests the machinery, not the agent's judgement.
---

# The scripted test backend

`scripted-test` is a model backend that answers from a file instead of a model.
You register it the way you register Ollama or OpenAI, point it at a JSON
dialog, and every model turn in the run is served by the next turn in that file
— in order, byte for byte, every time.

Everything else stays real. The chain engine loops for real, tools dispatch for
real, the HITL gate asks for real, sessions persist for real, `beam` renders for
real. Only the one nondeterministic part — the model — is replaced. That is what
makes an end-to-end test of the product possible: you assert on what the
operator actually sees, not on a mock asserting that the implementation matches
itself.

It ships in the ordinary binary. There is no build tag and no separate test
build, because a backend you have to rebuild to reach is a backend nobody uses.

## What a scripted run proves, and what it does not

It proves the **machinery**. That the chain looped. That the tool name the model
emitted resolved to a real tool. That the arguments survived the round trip.
That the HITL gate held the call it was supposed to hold and released it on
approval. That the session recorded what happened and `beam` drew it. That is
most of what actually breaks in this product, and none of it needs a model to
be real.

It proves nothing about the **agent's judgement**. The script did not decide to
call `git_diff` — you did, when you wrote the turn. A scripted run cannot tell
you whether a real model would pick that tool, fill those arguments, recover
from a tool error, or stop when it should. Prompt wording, tool descriptions and
the agent declaration are invisible to it: the dialog is fixed before the run
starts, and the same turn comes back no matter what the prompt said.

So a green scripted suite is a regression net for the harness, not evidence that
the agent works. Editing an agent's Markdown or a tool's description cannot fail
a scripted test — that change has to be tried against a real model.

## It is never mistaken for a real model

The type is called `scripted-test`, and that string is what every surface
prints:

- `contenox backend list` shows the type in its own column.
- `contenox doctor` prints the type and the dialog file on the backend's
  diagnostics line — `scripted (scripted-test, /abs/dialog.json)` — and raises a
  warning for as long as it is the default provider: *"replies are replayed from
  the backend's script file and NO model is called."* A scripted backend that is
  registered but not the default gets a warning of its own.
- `beam`'s welcome header prints `model <name> · scripted-test`.
- Every model call it serves is logged on stderr as
  `subject=scripted-test model=<name> script=<path>`, and `contenox state show
  <req> --raw` records `providerType: "scripted-test"` for the turn it served.
- A script that names no model is exposed as the model **`scripted-test`**, so
  even a surface that prints only a model name says it.
- `contenox backend add` prints a `WARNING:` line naming the script file.

## Register it

```bash
contenox backend add scripted --type scripted-test --script ./dialog.json
contenox config set default-provider scripted-test
contenox config set default-model scripted-test
```

That is the ordinary backend path — no special flag on `beam`, `run`, `chat` or
`acp`, and no special-casing inside them. `--script` is validated when you add
the backend: a missing or malformed dialog file is refused there, not three
commands later.

`--script` takes a filesystem path (a `file://` URL is accepted too) and stores
it as the backend's base URL, so `contenox backend show scripted` tells you
which dialog a backend replays.

Remove it exactly like any other backend:

```bash
contenox backend remove scripted
```

## The script

A script is an ordered list of assistant turns. Each turn is text, or one or
more tool calls, or both.

```json
{
  "model": "scripted-test",
  "context_length": 32768,
  "turns": [
    {
      "text": "Let me look at what changed.",
      "tool_calls": [
        { "name": "git_diff", "arguments": { "path": "." } }
      ]
    },
    {
      "text": "Two files changed: README.md and main.go."
    }
  ]
}
```

Turn 1 calls a tool. The engine dispatches it for real, feeds the real result
back, and loops. Turn 2 is the answer written against that result. Tool names
are the ones the agent's roster advertises — the leaf names `contenox doctor`
prints under "Tool roster" (`git_diff`, `mission_report`, `local_shell`), not
the toolset they came from. A bare array of turns is also a valid script when
you need nothing else:

```json
[{ "text": "hello" }, { "text": "goodbye" }]
```

### Document fields

| Field | Default | Meaning |
|---|---|---|
| `model` | `scripted-test` | The model name the backend exposes. Set it when a chain or agent pins a specific name. |
| `context_length` | `32768` | Reported context window; a request needing more is refused, as with a real model. |
| `max_output_tokens` | unset | Reported output ceiling; unset means no clamp. |
| `embed_dimensions` | `64` | Width of the deterministic embedding vector. |
| `capabilities` | all `true` | Per-capability overrides: `chat`, `stream`, `prompt`, `embed`, `think`, `vision`, `audio`. Set one to `false` to test the refusal path. |
| `turns` | required | The dialog, in order. At least one. |

### Turn fields

| Field | Meaning |
|---|---|
| `text` | The assistant's visible reply. Streamed in chunks on the streaming path. |
| `thinking` | A reasoning trace, delivered on the thinking channel and never sent back as history. |
| `tool_calls` | `[{ "id": "...", "name": "...", "arguments": {...} }]`. `id` is generated when omitted. `arguments` is a JSON object, or a JSON **string** when you want to hand the engine raw (even malformed) argument text. |
| `finish_reason` | Overrides the reported finish reason. Defaults to `tool_calls` when the turn calls tools, otherwise `stop`. Set `"length"` to exercise truncation handling. |
| `usage` | `{ "prompt_tokens": …, "completion_tokens": …, "total_tokens": … }`, reported as the turn's token accounting. |

A turn with none of `text`, `thinking` or `tool_calls` is rejected when the
script loads: an empty turn is an authoring mistake, not a silent no-op.

## How turns are consumed

One turn per model turn, in order, across the whole run. Chat, streaming and
prompt-execution calls all draw from the same cursor, so a chain that streams
its main loop and prompt-executes a summarizer consumes turns in the order the
chain actually runs them.

Prompt execution needs a turn with `text`. It returns a string and has nowhere
to put a tool call, so landing it on a tool-call-only turn fails naming the
script and the turn index rather than returning an empty answer.

The cursor belongs to the script *file*, for the life of the process. Two chain
tasks in one `contenox run` therefore continue the same dialog rather than each
restarting at turn 1 — which is what lets you script a multi-task chain end to
end. Embedding calls never consume a turn; an embedding is not a model turn, and
spending one would desync the dialog. Embeddings are a deterministic function of
the input text.

Editing the script file rewinds it: the next call notices the file changed,
reloads it, and starts again at turn 1.

## A worked run

```bash
cat > dialog.json <<'JSON'
{
  "turns": [
    {
      "text": "Filing what I found.",
      "tool_calls": [{"name": "mission_report",
                      "arguments": {"kind": "result", "summary": "scripted run reporting home"}}]
    },
    {"tool_calls": [{"name": "mission_finish", "arguments": {"status": "landed"}}]},
    {"text": "Mission finished."}
  ]
}
JSON

contenox backend add scripted --type scripted-test --script ./dialog.json
contenox config set default-provider scripted-test
contenox config set default-model scripted-test

contenox run --policy run "report what you know"
# stdout: scripted run reporting home
# stderr: Mission … finished: landed
```

The mission really ran: the engine dispatched `mission_report` and
`mission_finish` through the mission service, the HITL envelope bounded them,
and the report is in the local store afterwards (`contenox mission reports <id>`).

Note the third turn. A chat loop asks the model again after the tool result
comes back, so a dialog that ends on a tool call is one turn short of the run —
the terminal tool call lands, and the *next* request finds the script exhausted.
Close a script with a plain text turn.

A tool call the envelope holds for approval suspends the run exactly as a real
model's would: the approval is raised against the scripted call id and the run
waits for `contenox approvals respond`. Pick a policy that grants what the
script calls, or answer the ask.

## Running past the end fails loudly

There is no fallback reply. When the run asks for a turn nobody wrote, it fails
with the script and the turn index in the message:

```
scripted-test script "/home/you/proj/dialog.json" is exhausted: stream asked for
turn 4 but the script holds 4 turn(s); add the missing turn or shorten the run
```

That is the point. A silent "mock response" would let a broken chain look green;
an exhausted script tells you exactly which turn to write next.

## Who drives it

The black-box CLI suite in
[`tools/contenox-e2e`](https://github.com/contenox/contenox/tree/main/tools/contenox-e2e)
is built on this backend: every case registers a scripted-test backend, runs
the shipped binary against it, and asserts on stdout, stderr, exit codes and
state read back through contenox's own commands. Cases write the dialog as a
typed Rust value rather than raw JSON, so the two documented traps below — a
script that ends on a tool call, and the `acp` chain consuming turn 1 as a
router label — are the only ones left to remember. Run it with `task e2e-cli`.

## Limitations

- One dialog per script file, one cursor per file per process. Two concurrent
  sessions in one process share the cursor.
- The dialog is fixed: turns are not matched against what the model was asked.
  If the chain takes a different branch than you scripted, you get the next turn
  in the file anyway — and usually an exhausted-script error shortly after,
  which is the signal that the branch changed.
- Token counts come from the estimating tokenizer, not the script, unless a turn
  declares `usage`.
