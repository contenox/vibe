//! The scripted-test backend: the enabler every other case in this suite
//! stands on.
//!
//! It is registered like any other backend, it replays a JSON dialog from the
//! path stored as its base URL, and it says so on every surface that names a
//! provider. These cases drive that from outside — `backend add`, `backend
//! list`, `backend show`, `doctor`, `model list`, `beam`, `run`, `acp` — and
//! read the consequences back through the product's own commands.

use contenox_e2e::{Capabilities, CmdOutput, Instance, Script, Table, ToolCall, Turn, Usage};
use serde_json::Value;
use std::time::Duration;

/// A cold `contenox run` compiles its agents before the first model call.
fn patiently() -> Duration {
    Duration::from_secs(240)
}

fn started(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx
}

/// `contenox backend add` for a dialog already on disk, run raw so a refusal
/// is a value this case can assert on rather than a panic.
fn add_backend(cx: &Instance, name: &str, script: &str) -> CmdOutput {
    cx.run([
        "backend",
        "add",
        name,
        "--type",
        "scripted-test",
        "--script",
        script,
    ])
}

/// The dialog a `run` needs to land: file a result, finish, then answer the
/// loop's last question.
fn reports_and_lands(summary: &str) -> Script {
    Script::new()
        .turn(
            Turn::new()
                .text("Filing what I found.")
                .call(ToolCall::mission_report("result", summary)),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

/// A router declared in the workspace: one classifier and two branches. The
/// classifier is a `route` task, which is the only handler that reaches the
/// backend through prompt execution rather than chat.
fn declare_a_router(cx: &Instance) {
    cx.write_file(
        ".contenox/agents/sorter/agent.md",
        "---\nname: sorter\ndescription: Send a request to the branch that should handle it.\n\
         default: docs\n---\nYou sort an incoming request.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/sorter/code/agent.md",
        "---\nname: code\ndescription: Change code\n---\nYou change code.\n",
    )
    .expect("write the code branch");
    cx.write_file(
        ".contenox/agents/sorter/docs/agent.md",
        "---\nname: docs\ndescription: Change docs\n---\nYou change docs.\n",
    )
    .expect("write the docs branch");
}

/// Both streams of one command, for a failure the product prints on whichever
/// of the two the caller happens to be reading.
fn both_streams(out: &CmdOutput) -> String {
    format!("{}\n{}", out.stdout, out.stderr)
}

/// The captured units of the one run this case made, as `contenox state show
/// --raw` prints them. The table form drops the provider, the model and the
/// finish reason; `--raw` is the operator's command that keeps them.
fn captured_units(cx: &Instance) -> Vec<Value> {
    let requests = cx.state_requests().expect("contenox state list");
    let [request] = requests.as_slice() else {
        panic!("expected one captured run, got {requests:?}");
    };
    let out = cx.run(["state", "show", request.as_str(), "--raw"]);
    serde_json::from_str(&out.stdout).unwrap_or_else(|err| {
        panic!(
            "contenox state show --raw is not JSON ({err}):\n{}",
            out.render()
        )
    })
}

// ---------------------------------------------------------------------------
// Registered like any other backend, and validated where it is registered
// ---------------------------------------------------------------------------

/// The ordinary `backend add` path, plus the thing that separates this type
/// from every other one: it says out loud that it is a fake, names the file it
/// will replay, and tells you the two commands that point the defaults at it.
#[test]
fn adding_a_scripted_backend_prints_the_test_warning_and_the_dialog_it_replays() {
    let cx = started("scripted-add");
    let dialog = cx
        .write_script("dialog.json", &Script::new().turn("hello"))
        .expect("write the dialog");
    let absolute = dialog.display().to_string();

    // Added by the relative path a person would actually type.
    add_backend(&cx, "scripted", "./dialog.json")
        .ok()
        .expect_stdout(&format!(
            "Backend \"scripted\" added (scripted-test → {absolute})."
        ))
        .expect_stdout(&format!(
            "WARNING: scripted-test is a TEST backend. It calls no model — every reply is replayed from {absolute} in order."
        ))
        .expect_stdout("contenox config set default-provider scripted-test")
        .expect_stdout("contenox config set default-model scripted-test");
}

/// `--script` is stored as the backend's base URL — resolved to an absolute
/// path, so the record still points at the dialog from any directory.
#[test]
fn the_dialog_path_is_the_backends_url_so_show_says_which_dialog_it_replays() {
    let cx = started("scripted-url");
    let dialog = cx
        .write_script("dialog.json", &Script::new().turn("hello"))
        .expect("write the dialog");
    add_backend(&cx, "scripted", "./dialog.json").ok();

    let backends = cx.backends().expect("contenox backend list");
    assert_eq!(backends.len(), 1, "one backend, got {backends:?}");
    assert_eq!(backends[0].name, "scripted");
    assert_eq!(backends[0].kind, "scripted-test");
    assert_eq!(backends[0].url, dialog.display().to_string());

    let shown = cx.run(["backend", "show", "scripted"]).ok();
    let record: Value = serde_json::from_str(&shown.stdout)
        .unwrap_or_else(|err| panic!("backend show is not JSON ({err}):\n{}", shown.render()));
    assert_eq!(record["type"], Value::from("scripted-test"));
    assert_eq!(record["baseUrl"], Value::from(dialog.display().to_string()));
}

/// Validated at add time, not three commands later: a dialog that is not there
/// is refused where you named it, and nothing is registered.
#[test]
fn a_dialog_that_is_not_there_is_refused_at_add_time_and_registers_nothing() {
    let cx = started("scripted-missing");

    let out = add_backend(&cx, "scripted", "./nope.json").expect_failure();
    assert!(
        out.stderr.contains("nope.json") && out.stderr.contains("cannot be read"),
        "the refusal names the file it could not read:\n{}",
        out.render()
    );
    assert!(
        cx.backends().expect("contenox backend list").is_empty(),
        "a refused add must leave no backend behind"
    );
}

#[test]
fn a_dialog_that_is_not_valid_json_is_refused_at_add_time_and_registers_nothing() {
    let cx = started("scripted-malformed");
    cx.write_file("dialog.json", "{not json")
        .expect("write the dialog");

    let out = add_backend(&cx, "scripted", "./dialog.json").expect_failure();
    assert!(
        out.stderr.contains("is not a valid script"),
        "the refusal says the file is not a script:\n{}",
        out.render()
    );
    assert!(
        cx.backends().expect("contenox backend list").is_empty(),
        "a refused add must leave no backend behind"
    );
}

#[test]
fn a_dialog_with_no_turns_is_refused_at_add_time() {
    let cx = started("scripted-no-turns");
    cx.write_file("dialog.json", "{\"turns\":[]}")
        .expect("write the dialog");

    add_backend(&cx, "scripted", "./dialog.json")
        .expect_failure()
        .expect_stderr("declares no turns");
}

/// An empty turn is an authoring mistake, not a silent no-op, and the loader
/// says which turn and what to write in it.
#[test]
fn a_turn_with_neither_text_thinking_nor_tool_calls_is_refused_at_add_time() {
    let cx = started("scripted-empty-turn");
    cx.write_script(
        "dialog.json",
        &Script::new().turn("hello").turn(Turn::new()),
    )
    .expect("write the dialog");

    add_backend(&cx, "scripted", "./dialog.json")
        .expect_failure()
        .expect_stderr("turn 1 is empty: give it \"text\", \"thinking\" or \"tool_calls\"");
}

/// The type carries no default endpoint, because the dialog file IS the
/// endpoint. Omitting it is refused with the flag that fixes it.
#[test]
fn the_scripted_type_without_a_dialog_is_refused_and_names_the_flag() {
    let cx = started("scripted-no-flag");

    cx.run(["backend", "add", "scripted", "--type", "scripted-test"])
        .expect_failure()
        .expect_stderr(
            "--script is required for scripted-test backends (it carries the dialog file)",
        )
        .expect_stderr("--script ./dialog.json");
    assert!(cx.backends().expect("contenox backend list").is_empty());
}

#[test]
fn the_script_flag_on_any_other_backend_type_is_refused() {
    let cx = started("scripted-wrong-type");
    cx.write_script("dialog.json", &Script::new().turn("hello"))
        .expect("write the dialog");

    cx.run([
        "backend",
        "add",
        "local",
        "--type",
        "ollama",
        "--script",
        "./dialog.json",
    ])
    .expect_failure()
    .expect_stderr("--script only applies to --type scripted-test backends");
    assert!(cx.backends().expect("contenox backend list").is_empty());
}

// ---------------------------------------------------------------------------
// It names itself everywhere
// ---------------------------------------------------------------------------

/// `doctor` is the command an operator runs to ask "can I work right now". It
/// must never let a scripted backend pass for a model: the type and the dialog
/// are on the diagnostics line, and a warning stands for as long as it is the
/// default provider.
#[test]
fn doctor_names_the_type_the_dialog_and_warns_while_it_is_the_default() {
    let cx = started("scripted-doctor-default");
    let dialog = cx
        .scripted(&Script::new().turn("hello"))
        .expect("scripted backend");

    let out = cx.doctor().ok();
    assert!(
        out.stdout_has(&format!("scripted (scripted-test, {})", dialog.display())),
        "the diagnostics line names the type and the dialog it replays:\n{}",
        out.render()
    );
    out.clone()
        .expect_stdout("Status: reachable; 1 chat model(s)")
        .expect_stdout("Chat models: scripted-test")
        .expect_stdout(
            "[warning]  Default provider is \"scripted-test\": replies are replayed from the backend's script file and NO model is called.",
        )
        .expect_stdout("switch the default before doing real work.");

    let report = cx.doctor_json().expect("contenox doctor --json");
    let codes: Vec<&str> = report["issues"]
        .as_array()
        .map(|issues| {
            issues
                .iter()
                .filter_map(|issue| issue["code"].as_str())
                .collect()
        })
        .unwrap_or_default();
    assert!(
        codes.contains(&"scripted_test_provider_active"),
        "the machine-readable report carries the same warning: {codes:?}"
    );
}

/// Registered but not the default is still worth saying out loud, with the
/// command that removes it once the test run is done.
#[test]
fn doctor_warns_about_a_scripted_backend_that_is_registered_but_not_default() {
    let cx = started("scripted-doctor-registered");
    cx.write_script("dialog.json", &Script::new().turn("hello"))
        .expect("write the dialog");
    add_backend(&cx, "scripted", "./dialog.json").ok();

    cx.doctor()
        .ok()
        .expect_stdout(
            "[warning]  Backend scripted is a scripted-test backend: it replays a script file instead of calling a model. Remove it once the test run is done.",
        )
        .expect_stdout("Try: contenox backend remove scripted");
}

/// The dialog is the backend's endpoint, so losing it is the scripted
/// equivalent of a dead endpoint — and `doctor` reports it as one, naming the
/// file rather than a generic connection error.
#[test]
fn doctor_reports_a_dialog_deleted_after_registration_as_the_backends_error() {
    let cx = started("scripted-doctor-gone");
    let dialog = cx
        .scripted(&Script::new().turn("hello"))
        .expect("scripted backend");
    std::fs::remove_file(&dialog).expect("delete the dialog");

    let out = cx.doctor().ok();
    assert!(
        out.stdout_has("Reachable backends:    0"),
        "a backend whose dialog is gone is not reachable:\n{}",
        out.render()
    );
    assert!(
        out.stdout_has(&format!(
            "Error: scripted-test script \"{}\" cannot be read",
            dialog.display()
        )),
        "the error names the dialog file:\n{}",
        out.render()
    );
}

/// A dialog that names no model is exposed as the model `scripted-test`, so
/// even a surface that prints nothing but a model name still says "test".
#[test]
fn a_script_that_names_no_model_is_exposed_as_the_model_scripted_test() {
    let cx = started("scripted-model-name");
    cx.scripted(&Script::new().turn("hello"))
        .expect("scripted backend");

    let out = cx.run(["model", "list"]).ok();
    let table = Table::parse(
        &out.stdout,
        &[
            "BACKEND", "MODEL", "CHAT", "EMBED", "PROMPT", "THINK", "VISION", "CTX",
        ],
    )
    .unwrap_or_else(|err| panic!("contenox model list ({err}):\n{}", out.render()));

    assert_eq!(table.len(), 1, "one model, got {:?}", table.rows);
    let row = &table.rows[0];
    assert_eq!(row.get("BACKEND"), "scripted");
    assert!(
        row.get("MODEL").starts_with("scripted-test"),
        "the model name says test: {:?}",
        row.get("MODEL")
    );
    assert_eq!(row.get("CTX"), "32768", "the documented default context");
}

/// beam's welcome header is the first thing an attended user reads. It names
/// the model the dialog declares and the provider serving it, so a scripted
/// session cannot be mistaken for a real one.
#[test]
fn beams_welcome_header_names_the_scripted_model_and_its_provider() {
    let cx = started("scripted-beam-header");
    let dialog = cx
        .write_script(
            "dialog.json",
            &Script::new().model("dialog-model").turn("hello"),
        )
        .expect("write the dialog");
    add_backend(&cx, "scripted", &dialog.display().to_string()).ok();
    cx.run(["config", "set", "default-provider", "scripted-test"])
        .ok();
    cx.run(["config", "set", "default-model", "dialog-model"])
        .ok();

    let pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    let screen = pty
        .wait_for("type / for commands", Duration::from_secs(90))
        .expect("beam's composer hint");

    assert!(
        screen.contains("model dialog-model"),
        "the header takes the model name from the dialog:\n{screen}"
    );
    assert!(
        screen.contains("model dialog-model · scripted-test")
            || screen.contains("model dialog-model - scripted-test"),
        "and names scripted-test as the provider beside it:\n{screen}"
    );
}

/// Every model call the backend serves is logged as its own type, with the
/// model and the dialog file, so a run's stderr always says a file answered.
#[test]
fn every_scripted_model_call_names_the_backend_the_model_and_the_script_on_stderr() {
    let cx = started("scripted-stderr-naming");
    let dialog = cx
        .scripted(&reports_and_lands("the dialog answered"))
        .expect("scripted backend");

    let out = cx
        .cmd(["run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .ok();

    assert!(
        out.stderr.contains("subject=scripted-test"),
        "the backend type names itself on every model call:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains("model=scripted-test"),
        "and so does the model:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains(&format!("script={}", dialog.display())),
        "and the dialog it read the reply from:\n{}",
        out.render()
    );
}

/// The captured run keeps the provider that served each turn, which is how an
/// operator reading `contenox state show --raw` afterwards can tell a scripted
/// run from a real one.
#[test]
fn the_captured_run_records_scripted_test_as_the_provider_that_served_the_turn() {
    let cx = started("scripted-captured-provider");
    cx.scripted(&reports_and_lands("captured provider"))
        .expect("scripted backend");

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .ok();

    let units = captured_units(&cx);
    let model_calls: Vec<&Value> = units
        .iter()
        .filter(|unit| unit["taskHandler"] == Value::from("chat_completion"))
        .collect();
    assert!(!model_calls.is_empty(), "the run captured no model call");
    for unit in model_calls {
        assert_eq!(
            unit["providerType"],
            Value::from("scripted-test"),
            "every captured model call names the scripted provider: {unit:#}"
        );
        assert_eq!(unit["modelName"], Value::from("scripted-test"));
    }
}

/// `--trace` is documented to print `provider=scripted-test` on every turn.
///
/// The renderer that would print it (`formatTraceUnit` in
/// internal/surfaces/contenoxcli/trace_render.go) exists and is unit-tested,
/// but `startTraceStream`, the only thing that would feed it, has no caller
/// anywhere in the binary. `--trace` therefore only widens the slog output; no
/// `[trace]` line is ever written. The provider IS recorded — see
/// `the_captured_run_records_scripted_test_as_the_provider_that_served_the_turn`
/// — so this is a lost wire, not a lost fact.
#[test]
#[ignore = "confirmed defect: --trace prints no [trace] lines at all; startTraceStream (internal/surfaces/contenoxcli/trace_render.go) has no caller"]
fn trace_prints_provider_scripted_test_on_every_turn() {
    let cx = started("scripted-trace");
    cx.scripted(&reports_and_lands("traced"))
        .expect("scripted backend");

    let out = cx
        .cmd(["--trace", "run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run --trace")
        .ok();

    assert!(
        out.stderr.contains("[trace] task="),
        "--trace streams task-step events to stderr:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains("provider=scripted-test"),
        "and names the provider on every turn:\n{}",
        out.render()
    );
}

// ---------------------------------------------------------------------------
// How turns are consumed
// ---------------------------------------------------------------------------

/// The cursor belongs to the script file for the life of ONE process. Two
/// invocations of the same command against the same dialog therefore both
/// start at turn 1 — the property every other case in this suite depends on,
/// since each of them registers a dialog exactly as long as one run.
#[test]
fn each_process_replays_the_dialog_from_turn_one() {
    let cx = started("scripted-per-process-cursor");
    cx.scripted(&reports_and_lands("replayed from the top"))
        .expect("scripted backend");

    for attempt in 1..=2 {
        let out = cx
            .cmd(["run", "--policy", "run", "report what you know"])
            .timeout(patiently())
            .output()
            .expect("contenox run")
            .ok();
        assert_eq!(
            out.stdout.trim(),
            "replayed from the top",
            "run {attempt} should replay turn 1, not continue where the last process stopped:\n{}",
            out.render()
        );
    }

    let missions = cx.missions().expect("contenox mission list");
    assert_eq!(missions.len(), 2, "two runs, two missions: {missions:?}");
    assert!(missions.iter().all(|m| m.status == "landed"));
}

/// One turn per model turn, in order, across the whole run — including across
/// handlers. A router's classifier reaches the backend through a different
/// path than the agent loop that follows it, and both draw from the one
/// cursor: turn 1 picks the branch, turns 2-4 are the branch's conversation.
#[test]
fn a_classifier_and_the_agent_loop_draw_from_one_cursor_in_script_order() {
    let cx = started("scripted-one-cursor");
    declare_a_router(&cx);
    cx.scripted(
        &Script::new()
            .route("code")
            .turn(
                Turn::new()
                    .text("Filing what I found.")
                    .call(ToolCall::mission_report(
                        "result",
                        "the code branch answered",
                    )),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted backend");

    cx.cmd(["run", "sorter", "sort this", "--policy", "run"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .ok()
        .expect_stdout("the code branch answered");

    let steps = cx.executed_tasks().expect("contenox state show");
    let route = steps
        .iter()
        .find(|step| step.handler == "route")
        .unwrap_or_else(|| panic!("no route step in {steps:?}"));
    assert_eq!(route.task, "sorter-route");
    assert_eq!(
        route.transition, "code",
        "turn 1 was consumed as the classifier's answer: {steps:?}"
    );
    assert!(
        steps.iter().any(|step| step.task == "sorter-code-agent"),
        "and the branch it named ran the rest of the dialog: {steps:?}"
    );
    assert!(
        !steps.iter().any(|step| step.task == "sorter-docs-agent"),
        "the other branch never ran: {steps:?}"
    );
}

/// Editing the dialog rewinds it. The next call notices the file changed,
/// reloads it and starts again at turn 1 — inside a process that has already
/// consumed turns, which is the only place the behaviour is visible.
#[test]
fn editing_the_dialog_rewinds_it_to_turn_one_inside_a_live_process() {
    let cx = started("scripted-rewind");
    let dialog = cx
        .scripted(&Script::new().route("general").turn("the first answer"))
        .expect("scripted backend");

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    let first = acp
        .prompt(&session, "what changed?")
        .expect("session/prompt");
    assert_eq!(first.text(), "the first answer");

    // The same file, rewritten under a process that has already read it.
    std::fs::write(
        &dialog,
        Script::new()
            .route("general")
            .turn("the second answer, written while the process was live")
            .to_json(),
    )
    .expect("rewrite the dialog");

    let second = acp
        .prompt(&session, "and now?")
        .expect("a rewound dialog answers again rather than reporting itself exhausted");
    assert_eq!(
        second.text(),
        "the second answer, written while the process was live",
        "the edit rewound the dialog to turn 1"
    );
}

/// There is no fallback reply. Running past the end fails with the dialog and
/// the turn index, and the failure reaches the caller of the shipped command:
/// a nonzero exit code, the message in what the command printed, and the same
/// message left in the session the run wrote.
#[test]
fn running_past_the_end_names_the_script_and_the_turn_instead_of_inventing_a_reply() {
    let cx = started("scripted-exhausted");
    let dialog = cx
        .scripted(&Script::new().turn("the only turn there is"))
        .expect("scripted backend");

    let out = cx
        .cmd(["run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .expect_failure();

    let said = both_streams(&out);
    assert!(
        said.contains(&format!(
            "scripted-test script \"{}\" is exhausted: chat asked for turn 1 but the script holds 1 turn(s); add the missing turn or shorten the run",
            dialog.display()
        )),
        "the exhaustion names the dialog and the turn nobody wrote:\n{}",
        out.render()
    );

    let missions = cx.missions().expect("contenox mission list");
    assert_eq!(missions.len(), 1, "{missions:?}");
    assert_eq!(
        missions[0].status, "derailed",
        "the run derails rather than being papered over with a fallback reply: {missions:?}"
    );

    // And the same message is what the conversation records, so it is still
    // there for whoever reads the session afterwards.
    let sessions = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(sessions.len(), 1, "one run, one session: {sessions:?}");
    let transcript = cx.session_show(&sessions[0].id).ok().stdout;
    assert!(
        transcript.contains("(chat_completion) failed:") && transcript.contains("is exhausted"),
        "the exhaustion is persisted into the session history:\n{transcript}"
    );
}

// ---------------------------------------------------------------------------
// Overrides: capabilities, finish reason, raw arguments
// ---------------------------------------------------------------------------

/// A capability set false in the dialog is reported false wherever the product
/// prints capabilities — which is what makes a refusal path testable at all.
#[test]
fn a_capability_the_dialog_switches_off_is_reported_off_on_every_surface() {
    let cx = started("scripted-capability-off");
    cx.scripted(
        &Script::new()
            .capabilities(Capabilities::new().chat(false).embed(false))
            .turn("hello"),
    )
    .expect("scripted backend");

    let out = cx.run(["model", "list"]).ok();
    let table = Table::parse(
        &out.stdout,
        &[
            "BACKEND", "MODEL", "CHAT", "EMBED", "PROMPT", "THINK", "VISION", "CTX",
        ],
    )
    .unwrap_or_else(|err| panic!("contenox model list ({err}):\n{}", out.render()));
    let row = &table.rows[0];
    assert_eq!(
        row.get("CHAT"),
        "-",
        "chat was switched off: {:?}",
        row.cells()
    );
    assert_eq!(
        row.get("EMBED"),
        "-",
        "embed was switched off: {:?}",
        row.cells()
    );
    assert_ne!(
        row.get("PROMPT"),
        "-",
        "everything else stays on by default"
    );

    cx.doctor()
        .ok()
        .expect_stdout("Status: reachable; 0 chat model(s), 1 total model(s)");
}

/// The point of the override: the refusal path is reachable without a real
/// model that lacks the capability. A run whose only model cannot chat is
/// refused, with an exit code a script can branch on and a message naming the
/// capability that was missing.
#[test]
fn a_run_whose_only_model_cannot_chat_is_refused_and_names_the_capability() {
    let cx = started("scripted-cannot-chat");
    cx.scripted(
        &Script::new()
            .capabilities(Capabilities::new().chat(false).stream(false))
            .turn("hello"),
    )
    .expect("scripted backend");

    let out = cx
        .cmd(["run", "--policy", "run", "say something"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .expect_failure();

    let said = both_streams(&out);
    assert!(
        said.contains("no models matched requirements"),
        "the run is refused rather than answered:\n{}",
        out.render()
    );
    assert!(
        said.contains("canchat: false"),
        "and the report names the capability the only model lacks:\n{}",
        out.render()
    );
}

/// Prompt execution has nowhere to put a tool call, so it refuses a
/// tool-call-only turn naming the dialog and the turn index rather than
/// returning an empty answer.
///
/// Reaching that path from outside takes the `stream` override: the engine
/// prefers streaming for a classifier and only falls back to prompt execution
/// when stream setup fails.
#[test]
fn switching_streaming_off_falls_back_to_prompt_execution_which_refuses_a_tool_call_turn() {
    let cx = started("scripted-prompt-tool-turn");
    declare_a_router(&cx);
    let dialog = cx
        .scripted(
            &Script::new()
                .capabilities(Capabilities::new().stream(false))
                .turn(Turn::new().call(ToolCall::mission_report(
                    "result",
                    "a classifier cannot call tools",
                )))
                .turn(Turn::new().call(ToolCall::mission_finish("landed")))
                .turn("Mission finished."),
        )
        .expect("scripted backend");

    cx.cmd(["run", "sorter", "sort this", "--policy", "run"])
        .timeout(patiently())
        .output()
        .expect("contenox run");

    let requests = cx.state_requests().expect("contenox state list");
    let [request] = requests.as_slice() else {
        panic!("expected one captured run, got {requests:?}");
    };
    let shown = cx.run(["state", "show", request.as_str()]).ok();
    assert!(
        shown.stdout_has(&format!(
            "scripted-test script \"{}\" turn 0 has no \"text\": a prompt call cannot replay a tool-call turn",
            dialog.display()
        )),
        "the classifier's failure names the dialog and the turn it landed on:\n{}",
        shown.render()
    );
    assert!(
        shown.stdout_has("sorter-route"),
        "and the step it failed:\n{}",
        shown.render()
    );
}

/// `finish_reason` overrides what the turn reports it stopped for, which is
/// how a truncated answer is exercised without a model that truncates. It
/// reaches the operator verbatim in the captured run.
///
/// The declaration rides the FIRST turn deliberately. A mission is over the
/// moment `mission_finish` lands, and the run cancels its context there — so a
/// turn scripted after it is raced against teardown and is captured with
/// `error: "context canceled"` and no finish reason at all when the machine is
/// busy. Asserting on the last unit made this case fail under the parallelism
/// of its own file while passing alone; the promise is the same on a turn the
/// run is guaranteed to complete.
#[test]
fn a_turns_finish_reason_reaches_the_captured_run_verbatim() {
    let cx = started("scripted-finish-reason");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Filing what I found.")
                    .finish_reason("length")
                    .call(ToolCall::mission_report("result", "truncated on purpose")),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted backend");

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .ok();

    let reasons: Vec<String> = captured_units(&cx)
        .iter()
        .filter(|unit| unit["taskHandler"] == Value::from("chat_completion"))
        .map(|unit| {
            unit["finishReason"]
                .as_str()
                .unwrap_or_default()
                .to_string()
        })
        .collect();
    assert_eq!(
        reasons.first().map(String::as_str),
        Some("length"),
        "the turn that declared a truncation reports it verbatim: {reasons:?}"
    );
    assert!(
        reasons.iter().skip(1).any(|reason| reason == "tool_calls"),
        "and an unset finish reason still defaults to tool_calls on a tool turn: {reasons:?}"
    );
}

/// `arguments` written as a JSON *string* hands the engine raw argument text,
/// malformed included — the only way to exercise what the runtime does with a
/// model that emits broken arguments. The tool never runs and the parse error
/// is what comes back as its result.
#[test]
fn arguments_written_as_a_json_string_reach_the_tool_as_raw_text() {
    let cx = started("scripted-raw-arguments");
    cx.write_file("evidence.txt", "the-quick-brown-fox\n")
        .expect("plant a file the tool would have listed");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Looking around.")
                    .call(ToolCall::new("list_dir").raw_arguments("{not json")),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted backend");

    cx.cmd(["run", "--policy", "run", "list the working directory"])
        .timeout(patiently())
        .output()
        .expect("contenox run");

    let sessions = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(sessions.len(), 1, "one run, one session: {sessions:?}");
    let transcript = cx.session_show(&sessions[0].id).ok().stdout;
    assert!(
        transcript.contains(
            "failed to unmarshal tool arguments for native-fs-browse.list_dir: invalid character 'n'"
        ),
        "the raw text reached the dispatcher and was refused as arguments:\n{transcript}"
    );
}

/// A turn that declares `usage` is documented to have that reported as the
/// turn's token accounting.
///
/// It is not: the captured run carries the estimating tokenizer's numbers
/// (0 / 6 / 6 for the 27-character reply below) and the declared 11 / 7 / 18
/// never appears on any surface. Scripting a token budget therefore cannot be
/// used to drive the compute bounds a mission declares.
#[test]
#[ignore = "confirmed defect: a turn's declared usage is dropped; the estimating tokenizer's count is what the run records"]
fn a_turns_declared_usage_is_the_token_accounting_the_run_records() {
    let cx = started("scripted-usage");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Filing what I found.")
                    .call(ToolCall::mission_report("result", "accounted for")),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn(
                Turn::new()
                    .text("Mission finished halfway th")
                    .usage(Usage {
                        prompt_tokens: 11,
                        completion_tokens: 7,
                        total_tokens: 18,
                    }),
            ),
    )
    .expect("scripted backend");

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .ok();

    let units = captured_units(&cx);
    let last = units
        .iter()
        .filter(|unit| unit["taskHandler"] == Value::from("chat_completion"))
        .next_back()
        .expect("a captured model call");
    assert_eq!(last["tokenUsage"]["prompt"], Value::from(11));
    assert_eq!(last["tokenUsage"]["completion"], Value::from(7));
    assert_eq!(last["tokenUsage"]["total"], Value::from(18));
}

// ---------------------------------------------------------------------------
// A gated scripted call is a gated call
// ---------------------------------------------------------------------------

/// A tool call the envelope holds suspends the run exactly as a real model's
/// would, and the ask is raised against the id the dialog gave the call — so a
/// case can name the ask it expects before the run has started.
#[test]
fn a_gated_scripted_call_suspends_the_run_against_the_scripted_call_id() {
    let cx = started("scripted-gated-call-id");
    std::process::Command::new("git")
        .args(["init", "-q"])
        .current_dir(cx.work())
        .status()
        .expect("git is on PATH");
    cx.write_file("note.txt", "hello\n").expect("the note");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new().text("Staging the note.").call(
                    ToolCall::new("git_add")
                        .id("the-dialogs-own-call-id")
                        .arg("paths", "note.txt"),
                ),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted backend");

    let fired = cx
        .cmd(["run", "--policy", "default", "stage the note"])
        .timeout(patiently())
        .start()
        .expect("contenox run");

    let ask = cx
        .await_approval(Duration::from_secs(120))
        .expect("the gated scripted call reaches 'contenox approvals list'");
    assert_eq!(ask.tool, "native-git.git_add");
    assert!(
        ask.id.starts_with("the-dialogs-own-call-id"),
        "the ask is raised against the scripted call id, got {:?}",
        ask.id
    );

    cx.approve(&ask.id).ok();
    fired
        .wait_timeout(patiently())
        .expect("the approved run finishes")
        .ok();

    let staged = std::process::Command::new("git")
        .args(["diff", "--cached", "--name-only"])
        .current_dir(cx.work())
        .output()
        .expect("git diff --cached");
    assert_eq!(
        String::from_utf8_lossy(&staged.stdout).trim(),
        "note.txt",
        "the released call really ran"
    );
}
