# contenox-e2e

The black-box end-to-end suite. It builds the shipped `contenox` binary, runs it
in a scratch home, and asserts on what a user or an integrator can actually see.

It is a sibling of [`tools/acp-validator`](../acp-validator/README.md): a Rust
crate that drives contenox from outside the Go module. Unlike acp-validator it
has no external checkout to clone — `cargo test` in this directory is the whole
story.

## Why this suite is not written in Go

An end-to-end test written in the language of the thing it tests can cheat. It
can construct internals, reach unexported state, swap a fake in, and still call
itself end-to-end. A second language makes that impossible by construction
rather than by discipline: from here there is no `internal/` to import, no
unexported field to poke, no in-process runtime to stand up.

The rules that follow from that, and that a case must not break:

- **The suite drives the shipped binary.** It never links Go code and never
  imports anything from the module.
- **It asserts only on what is observable from outside**: stdout, stderr, exit
  codes, files the run wrote, and state read back *through the product's own
  commands* — `contenox approvals list`, `mission show`, `session list`,
  `backend list`, `doctor`.
- **It never opens the SQLite file.** Reading `local.db` asserts the
  implementation, not the behaviour. If something is only observable through
  internal state, that is a finding — the product cannot show the user
  something it does — not a licence to open the database. This crate therefore
  ships no database helper and never will.
- **No case may need a network or cloud credentials.** The model is replaced by
  the [scripted-test backend](../../docs/development/scripted-test-backend.md),
  which replays a JSON dialog and calls nothing.

## Running it

```sh
task e2e-cli                 # the gate: builds bin/contenox, then runs cargo test
task e2e-cli -- run_puts     # pass anything through to cargo test
```

Or directly, from this directory:

```sh
cargo test
cargo test --test smoke                       # one file
cargo test a_gated_call_raises_an_ask         # one case
cargo test -- --nocapture                     # see the child processes talk
```

`task e2e-cli` sets `CONTENOX_BIN` to the binary it just built. A bare
`cargo test` has no such gift, so it finds the binary itself:

| Knob | Effect |
|---|---|
| `CONTENOX_BIN=/path/to/contenox` | Use this binary. Nothing is built. |
| *(unset)* | Build `bin/contenox` from `cmd/contenox` once per test process, skipping the build when the binary is already newer than every `.go` file, `go.mod` and `go.sum` in the tree. |
| `CONTENOX_E2E_REBUILD=1` | Build even when the binary looks current. |
| `CONTENOX_E2E_NO_BUILD=1` | Never build; fail if `bin/contenox` is missing. |
| `CONTENOX_E2E_KEEP=1` | Keep each case's scratch directory and print its path. |

The build is taken under an exclusive `flock` on `bin/.contenox-e2e-build.lock`
and installed with an atomic rename, so parallel test binaries cannot race or
tear a binary another case is executing.

## Writing a case

A case is an ordinary `#[test]` in `tests/`. One behaviour per test, and a name
that says the area and the behaviour: `run_puts_the_scripted_report_on_stdout_and_exits_zero`,
`beam_without_a_terminal_refuses_and_names_run`.

```rust
use contenox_e2e::{Instance, Script, ToolCall, Turn};
use std::time::Duration;

#[test]
fn run_puts_the_scripted_report_on_stdout_and_exits_zero() {
    let cx = Instance::named("run-smoke").unwrap();
    cx.init().ok();

    cx.scripted(
        &Script::new()
            .turn(Turn::new().text("Filing what I found.").call(
                ToolCall::mission_report("result", "scripted run reporting home"),
            ))
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .unwrap();

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .timeout(Duration::from_secs(180))
        .output()
        .unwrap()
        .ok()
        .expect_stdout("scripted run reporting home")
        .expect_stderr("finished: landed")
        .refute_stdout("Mission fired at agent");

    let missions = cx.missions().unwrap();
    assert_eq!(missions[0].envelope, "run");
    assert_eq!(missions[0].status, "landed");
}
```

### Quarantining a case for a confirmed defect

A promise the product does not keep is the point of this suite, not an
embarrassment to hide. Write the case as if the promise held, then mark it
`#[ignore = "confirmed defect: …"]` with the reason and the seam. The gate stays
green, `cargo test` prints the reason on every run, `cargo test -- --ignored`
reproduces the failure in one command, and the case is already written for
whoever fixes it. Never rewrite the assertions to match the bug — a green test
asserting broken behaviour is how the bug becomes the specification.

### The hermetic instance

`Instance::new()` (or `named`, which only labels the scratch path) creates a
case-private world under the system temp directory:

```
contenox-e2e-<pid>-<label>-<n>-<nanos>/
  home/    ->  $HOME, so the global store is home/.contenox/local.db
  work/    ->  the working directory every command runs in
  tmp/     ->  $TMPDIR
```

Every `CONTENOX_*` and `XDG_*` variable in the ambient environment is removed
from the child, along with the provider API-key variables, so a developer's own
configuration cannot reach a case. The real `~/.contenox` is never read and
never written; `Instance::new` refuses to start if the scratch home would land
inside it. `Drop` removes the directory unless `CONTENOX_E2E_KEEP=1`.

`Instance::started()` is `new()` plus `contenox init`.

`cx.write_file(rel, text)` writes into the working directory and
`cx.write_home_file(rel, text)` into the scratch `~/.contenox/`, both creating
parents — the two places a declaration can live. `cx.generated(name)` is the
path under the workspace `.contenox/.generated/`, which is what
`contenox agent show <name>` prints for a declared agent.

### Running commands

| Call | What it does |
|---|---|
| `cx.run(["doctor"])` | Runs to completion, returns `CmdOutput`. |
| `cx.cmd([...])` | The builder: `.stdin(…)`, `.env(…)`, `.cwd(…)`, `.timeout(…)`, then `.output()`. |
| `cx.start([...])` | Spawns and returns a `Running` you can leave in the background — this is what a cross-process case uses. `Running` also carries `write_stdin`, `interrupt`, `kill`, `wait`, `wait_timeout`. |
| `cx.pty([...])` | Spawns under a real pty (see below). |

Every command carries a timeout (two minutes by default). A command that
overruns is killed and the case fails with its partial output rather than
hanging `cargo test`.

`CmdOutput` asserts by chaining, and prints both streams on failure: `ok()`,
`expect_code(n)`, `expect_failure()`, `expect_stdout`, `expect_stderr`,
`refute_stdout`. `notices()` returns stderr with the `time=… level=…` log lines
dropped, which is usually the only part of stderr a case cares about.

### The script the model reads

The scripted-test backend replays a JSON dialog. A case writes that dialog as a
typed Rust value, never as a raw string:

```rust
Script::new()
    .model("scripted-test")
    .context_length(32768)
    .capabilities(Capabilities::new().think(false))   // exercise a refusal path
    .route("general")                                 // see the ACP note below
    .turn(Turn::new().text("Looking.").call(ToolCall::new("git_diff").arg("path", ".")))
    .turn(Turn::new().call(ToolCall::new("write_file").raw_arguments("{not json")))
    .turn("Done.");
```

`cx.scripted(&script)` writes it into the work directory, registers it with
`contenox backend add scripted --type scripted-test --script …`, and points
`default-provider` / `default-model` at it. `cx.scripted_backend(name, &script)`
does the same under another name.

Three facts a case gets wrong exactly once:

- **Close a script with a plain text turn.** The chat loop asks the model again
  after a tool result comes back, so a dialog that ends on a tool call is one
  turn short and the run dies with an exhausted-script error.
- **The `acp` chain is a router tree.** Turn 1 is consumed as a classifier
  label and any tool call on it is discarded, so an ACP case opens with
  `Script::new().route("general")` before the turn that calls a tool.
- **Script only the tools the shape actually has.** `local_fs` (`read_file`,
  `write_file`, `edit_file`, `sed`) and `local_shell` are forwarded to a
  connected ACP client, so they exist under `beam` and an editor but NOT under
  `run`, `mission fire` or `serve`. Scripting one there comes back `tool
  <name> not found`, the scripted model shrugs and files its next turn anyway,
  and the case passes while proving nothing. What an unattended run can call is
  the in-process sets: `native-fs-browse` (`list_dir`, `grep`, `find_files`,
  `stat_file`, `count_stats`), `native-git`, `native-go`, `native-jq`,
  `native-goja` and the `mission` tools. `contenox doctor` prints the roster
  with the client capability each entry needs.

### Reading state back through the product

These are the only way a case may look at durable state. Each shells out to the
command an operator would run and parses the table it prints.

| Helper | Command behind it |
|---|---|
| `cx.missions()` | `contenox mission list` → `Vec<MissionRow>` |
| `cx.mission_show(id)` | `contenox mission show <id>` → labelled fields |
| `cx.mission_reports(id)` | `contenox mission reports <id>` |
| `cx.approvals()` | `contenox approvals list` → `Vec<ApprovalRow>` |
| `cx.await_approval(timeout)` | polls `approvals list` until an ask appears |
| `cx.approve(id)` / `deny(id)` / `answer(id, text)` | `contenox approvals respond …` |
| `cx.sessions()` / `cx.sessions_all()` | `contenox session list` / `--all` |
| `cx.session_show(name)` | `contenox session show <name>` |
| `cx.backends()` | `contenox backend list` |
| `cx.state_requests()` / `cx.state_steps(id)` | `contenox state list` / `state show <id>` |
| `cx.executed_tasks()` | `state show` for the one captured run → `Vec<StateStep>` |
| `cx.doctor()` / `cx.doctor_json()` | `contenox doctor` / `--json` |

Empty states are values, not errors: `contenox mission list` printing
*"No missions recorded…"* comes back as an empty `Vec`.

`Table::parse(text, &["ID", "KIND", …])` is underneath all of them. It locates
the header row and slices every following line at the header's own column
offsets, so a `SUMMARY` cell containing spaces survives intact. Use it directly
for a table with no typed helper yet.

### Driving an interactive surface

`contenox beam` refuses to start without a terminal, so a case that wants beam
runs it under a real pty:

```rust
let mut pty = cx.pty(["beam", "--plain"]).unwrap();
pty.wait_for("type / for commands", Duration::from_secs(60)).unwrap();
pty.send_line("what changed?").unwrap();
pty.wait_for("scripted run reporting home", Duration::from_secs(60)).unwrap();
pty.send_ctrl('c').unwrap();
```

`--plain` exists for exactly this: no colour and no redrawing. `screen()`
returns everything rendered so far with the escape sequences stripped, and
`raw()` returns the bytes themselves for a case whose subject is the escape
sequences; `wait_for` polls the screen and fails with the whole of it attached.
`interrupt()` sends SIGINT to the child's own process group and `hangup()` sends
the SIGHUP a closing terminal window sends; `wait_exit(timeout)` collects the
exit code.

The default terminal is 40x120. A line beam clamps to the width — the approval
card's policy line, with a long scratch path in it — needs a wider one:
`Pty::spawn_sized(cx.cmd(["beam", "--plain"]), 40, 220)`.

Everything beam does that is *not* rendering is observable without a pty —
`contenox acp` over stdio is the same session machinery — so reach for the pty
only when the assertion is about what the terminal shows.

### Driving the ACP surface

`cx.acp([...])` spawns `contenox acp` / `acpx` and speaks the protocol to it
over stdio, which is the position an editor is in — no terminal involved:

```rust
let mut acp = cx.acp(["acp"]).unwrap();
let init = acp.initialize().unwrap();               // full client capabilities
let session = acp.new_session(cx.work()).unwrap();  // the cwd the editor has open
let turn = acp.prompt(&session, "what changed?").unwrap();

assert_eq!(turn.stop_reason, "end_turn");
assert_eq!(turn.text(), "Two files changed.");
assert!(acp.offers("mission"));
```

`prompt` returns the whole turn as the client saw it: `updates` (every
`session/update` — `agent_message_chunk`, `tool_call`, `tool_call_update`,
`usage_update`, `session_info_update`), `permissions` (every
`session/request_permission` the agent sent), and `client_calls` (every
`fs/*` or `terminal/*` the agent asked *this process* to perform). `text()`
reassembles the chunks, `of_kind`/`tool_call_updates` select updates,
`tool_outputs()` is what the model was told about its calls,
`read_paths()` / `written_paths()` are the files the agent asked the client to
read and write, and `terminal_commands()` is every command line it asked the
client to open a terminal for. An editor keeps listening after the result, so
`prompt` does too — updates the agent flushes behind the response are part of
the turn.

This client rules on permission the way the editor's UI does, and the case
chooses how: `acp.answers(Verdict::Allow | Verdict::Deny | Verdict::Defer |
Verdict::Cancel)`. `Defer` never answers, which is how a case proves the ask
outlives the editor — answer it from another process with `cx.approve(&ask.id)`
while the turn waits. `Cancel` answers `cancelled`, the card an operator
dismissed without deciding, which is how a case proves a dropped card is not a
verdict.

This client also *performs* the work it is asked for, which is what makes the
forwarding contract checkable: it reads and writes the files the agent names and
runs the commands the agent asks it to open a terminal for — `terminal/create`
runs the command to completion, and the `wait_for_exit` / `output` / `release`
calls that follow answer from what it captured. Two knobs turn that around:
`acp.performs_writes(false)` takes `fs/write_text_file` and does nothing, which
is how a case proves the agent has no filesystem of its own — the request
arrives and the file still never appears — and `acp.cannot_read(path)` makes one
path unreadable, an editor holding a buffer it will not hand over.

`initialize_with(json!({}))` declares a client with no filesystem, so a case can
assert what a client is served when it grants nothing. `close()` hangs up and
collects the exit code; `stderr()` / `notices()` read the log stream the surface
writes beside the protocol.

The chain that actually ran is read back through `contenox state`:
`cx.executed_tasks()` returns the steps of the one captured run, which is how a
case tells the router tree from a flat chain without opening a chain file, and
`cx.captured_system_prompts()` returns what the model was actually sent with its
macros already expanded — the only way from outside to see what `{{tools}}`
rendered to.

### A service the agent calls out to

Some claims are about what contenox puts on the wire, not what it says
afterwards — a header sent on every call, a login performed after a 401. The
`openapi_stub` helper binary is a one-operation OpenAPI service a case can
register with `contenox tools add`: it listens on 127.0.0.1, serves its own
spec, records every request it receives as a JSON line, and optionally demands a
login before it answers. It reaches nothing, so a case that uses it still needs
no network and no credentials. `tests/tools_remote_service.rs` shows the shape.

### A relay to pair with

Pairing is the other direction: the machine dials out. Redemption is one POST,
so `relay_stub` is a loopback stand-in for the relay a case points
`CONTENOX_RELAY_ENDPOINT` (or `contenox pair <key> <endpoint>`) at. It answers
`/v1/pair/redeem`, records every request as a JSON line, can refuse a key the
way a spent or expired one is refused, and notes the TLS connections a paired
machine opens when it dials out — which is how a case tells "pairing attaches the
machine" from "something running keeps it reachable". It listens on 127.0.0.1
and reaches nothing. `tests/pairing.rs` shows the shape.

What the stand-in cannot stand in for is the connection itself: `relaylink`
dials `https` only and verifies the relay's Ed25519 signature, so a case here
can observe that a machine dialled and nothing past it. Anything downstream of a
completed handshake — a revoked instance refused at its next dial, an ask
answered from the app — needs a relay that can finish one.

## Layout

```
src/binary.rs     find or build the shipped binary, once
src/instance.rs   the hermetic scratch instance
src/cmd.rs        run a subcommand, capture stdout/stderr/exit code, assert on them
src/script.rs     the typed scripted-test dialog
src/probe.rs      read state back through the product's own commands
src/table.rs      the column-offset reader every table helper uses
src/pty.rs        drive an interactive surface
src/acp.rs        speak ACP to the editor surface over stdio
src/bin/openapi_stub.rs  a loopback OpenAPI service a case can register
src/bin/relay_stub.rs    a loopback stand-in for the relay, for the pairing cases
tests/            the cases
```
