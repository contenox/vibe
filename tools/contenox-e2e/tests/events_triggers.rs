//! Events & triggers (beta) — the durable event log, the operator-authored
//! trigger files that bind an event type to a chain, and `contenox events
//! dispatch` run as its own process.
//!
//! Every case here appends real events the only way an operator can — by firing
//! a real mission through the shipped binary — writes `trigger-*.json` and its
//! chain into the workspace the way the guide says to, and then runs the
//! dispatcher as a separate process it starts, watches and stops. What a firing
//! did is read back through `contenox events list`, `events firings`,
//! `state list|show`, `approvals list` and `doctor`; the dispatcher's own
//! stdout line is the other half of the evidence. Nothing here opens the
//! database.
//!
//! Two chains carry most of the weight, and neither needs a model: a `noop`
//! chain that succeeds, and a `raise_error` chain that always fails. A chain
//! that must actually *do* something (hold the process, run a shell command)
//! pins a second scripted-test backend by model name, so the dispatcher's
//! dialog and the mission unit's dialog never share a cursor.

use contenox_e2e::{CmdOutput, Instance, Script, Table, ToolCall, Turn};
use serde_json::Value;
use std::path::Path;
use std::time::{Duration, Instant};

/// A cold instance compiles its agents before the first turn, and a mission
/// fire spawns a unit subprocess; both get a generous ceiling.
const PATIENTLY: Duration = Duration::from_secs(240);

/// How long a case waits for the dispatcher to catch up on a backlog.
const CATCH_UP: Duration = Duration::from_secs(90);

// ---------------------------------------------------------------------------
// the instance, the dialogs, the files an operator writes
// ---------------------------------------------------------------------------

/// The dialog a mission needs to land: file one result, finish, then answer the
/// loop's last question.
fn lands_reporting(summary: &str) -> Script {
    Script::new()
        .turn(
            Turn::new()
                .text("Filing what I found.")
                .call(ToolCall::mission_report("result", summary)),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&lands_reporting("the tree is clean"))
        .expect("scripted-test backend");
    cx
}

/// The event tier is beta: nothing below happens without the opt-in.
fn opted_in(label: &str) -> Instance {
    let cx = instance(label);
    cx.run(["config", "set", "opt-in-beta", "true"]).ok();
    cx
}

/// `contenox mission fire run "<intent>" --wait --policy run` — the only way to
/// put an event in the log from outside, since V1 emits from the mission tier.
fn fire(cx: &Instance, intent: &str) -> CmdOutput {
    cx.cmd([
        "mission",
        "fire",
        "run",
        intent,
        "--wait",
        "--policy",
        "run",
        "--timeout",
        "3m",
    ])
    .timeout(PATIENTLY)
    .output()
    .expect("contenox mission fire")
}

/// A trigger file, as the guide documents it.
fn trigger_file(name: &str, listen_for: &str, chain: &str, policy: Option<&str>) -> String {
    let policy = match policy {
        Some(policy) => format!(",\n  \"policy\": \"{policy}\""),
        None => String::new(),
    };
    format!(
        "{{\n  \"name\": \"{name}\",\n  \"description\": \"written by the e2e suite\",\n  \
         \"listen_for\": {{\"type\": \"{listen_for}\"}},\n  \"type\": \"fire_chain\",\n  \
         \"chain\": \"{chain}\"{policy}\n}}\n"
    )
}

/// A chain that succeeds and needs no model: one `noop` task.
const NOOP_CHAIN: &str = r#"{
  "id": "chain-note",
  "description": "Record that the event arrived.",
  "tasks": [
    {
      "id": "note_the_event",
      "handler": "noop",
      "transition": {"on_failure": "", "branches": [{"operator": "default", "when": "", "goto": "end"}]}
    }
  ],
  "token_limit": 4096
}
"#;

/// A chain that always fails, deterministically and without a model.
const FAILING_CHAIN: &str = r#"{
  "id": "chain-boom",
  "description": "A trigger chain that always fails.",
  "tasks": [
    {
      "id": "always_fails",
      "handler": "raise_error",
      "prompt_template": "this trigger chain always fails",
      "transition": {"on_failure": "", "branches": [{"operator": "default", "when": "", "goto": "end"}]}
    }
  ],
  "token_limit": 4096
}
"#;

/// A chain that actuates: the pinned scripted model answers with one tool call
/// and `execute_tool_calls` runs it. `prompt_template` is load-bearing — a
/// `chat_completion` task refuses the event envelope as input.
const ACTUATING_CHAIN: &str = r#"{
  "id": "chain-act",
  "description": "Trigger chain that actuates through local_shell.",
  "tasks": [
    {
      "id": "decide",
      "handler": "chat_completion",
      "system_instruction": "You are a task processing engine talking to other machines.",
      "prompt_template": "Event {{.input.nid}} of type {{.input.type}} arrived.",
      "execute_config": {
        "model": "trigger-model",
        "provider": "scripted-test",
        "tools": ["local_shell"],
        "max_tokens": 4096
      },
      "transition": {"on_failure": "", "branches": [
        {"operator": "equals", "when": "tool_call", "goto": "act"},
        {"operator": "default", "when": "", "goto": "end"}
      ]}
    },
    {
      "id": "act",
      "handler": "execute_tool_calls",
      "input_var": "decide",
      "execute_config": {"tools": ["local_shell"]},
      "transition": {"on_failure": "", "branches": [{"operator": "default", "when": "", "goto": "end"}]}
    }
  ],
  "token_limit": 32768
}
"#;

/// Register the second scripted backend the actuating chain pins by model name,
/// so a fired chain's dialog never shares a cursor with the mission unit's.
fn actuating_backend(cx: &Instance, calls: &[Turn]) {
    let mut script = Script::new().model("trigger-model");
    for turn in calls {
        script = script.turn(turn.clone());
    }
    cx.scripted_backend("triggerscript", &script)
        .expect("the trigger's own scripted backend");
}

fn shell_turn(command: &str, args: &str) -> Turn {
    Turn::new().text("Acting on it.").call(
        ToolCall::new("local_shell")
            .arg("command", command)
            .arg("args", args),
    )
}

// ---------------------------------------------------------------------------
// reading the tier back through its own commands
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq)]
struct EventRow {
    nid: String,
    kind: String,
    source: String,
    subject: String,
    hop: String,
    time: String,
}

fn events_in(cx: &Instance, dir: &Path) -> Vec<EventRow> {
    let out = cx
        .cmd(["events", "list"])
        .cwd(dir)
        .output()
        .expect("contenox events list")
        .ok();
    if out.stdout.contains("(no events)") {
        return Vec::new();
    }
    Table::parse(
        &out.stdout,
        &["NID", "TYPE", "SOURCE", "SUBJECT", "HOP", "TIME", "DATA"],
    )
    .expect("contenox events list prints its table")
    .rows
    .iter()
    .map(|row| EventRow {
        nid: row.get("NID").to_string(),
        kind: row.get("TYPE").to_string(),
        source: row.get("SOURCE").to_string(),
        subject: row.get("SUBJECT").to_string(),
        hop: row.get("HOP").to_string(),
        time: row.get("TIME").to_string(),
    })
    .collect()
}

fn events(cx: &Instance) -> Vec<EventRow> {
    events_in(cx, cx.work())
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct FiringRow {
    nid: String,
    trigger: String,
    status: String,
    request: String,
    error: String,
}

fn firings_in(cx: &Instance, dir: &Path, args: &[&str]) -> Vec<FiringRow> {
    let mut argv = vec!["events", "firings"];
    argv.extend_from_slice(args);
    let out = cx
        .cmd(argv)
        .cwd(dir)
        .output()
        .expect("contenox events firings")
        .ok();
    if out.stdout.contains("(no firings)") {
        return Vec::new();
    }
    Table::parse(
        &out.stdout,
        &["NID", "TRIGGER", "STATUS", "REQUEST", "TIME", "ERROR"],
    )
    .expect("contenox events firings prints its table")
    .rows
    .iter()
    .map(|row| FiringRow {
        nid: row.get("NID").to_string(),
        trigger: row.get("TRIGGER").to_string(),
        status: row.get("STATUS").to_string(),
        request: row.get("REQUEST").to_string(),
        error: row.get("ERROR").to_string(),
    })
    .collect()
}

fn firings(cx: &Instance) -> Vec<FiringRow> {
    firings_in(cx, cx.work(), &[])
}

/// The `fired …` lines the dispatcher printed, one per firing.
fn fired_lines(out: &CmdOutput) -> Vec<&str> {
    out.stdout
        .lines()
        .filter(|line| line.starts_with("fired "))
        .collect()
}

// ---------------------------------------------------------------------------
// driving the dispatcher
// ---------------------------------------------------------------------------

/// Start `contenox events dispatch` in `dir`, wait until `ready` holds, then
/// stop it with the Ctrl-C an operator would use and return everything it
/// printed. A dispatcher that never reaches `ready` fails the case with its own
/// output attached rather than hanging the suite.
fn dispatch_until(
    cx: &Instance,
    dir: &Path,
    args: &[&str],
    mut ready: impl FnMut(&Instance) -> bool,
) -> CmdOutput {
    let mut argv = vec!["events", "dispatch"];
    argv.extend_from_slice(args);
    let child = cx
        .cmd(argv)
        .cwd(dir)
        .timeout(PATIENTLY)
        .start()
        .expect("spawn contenox events dispatch");

    let deadline = Instant::now() + CATCH_UP;
    let mut reached = false;
    while Instant::now() < deadline {
        if ready(cx) {
            reached = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(250));
    }
    child.interrupt();
    let out = child
        .wait_timeout(Duration::from_secs(60))
        .expect("the dispatcher stops on Ctrl-C");
    assert!(
        reached,
        "the dispatcher never reached the expected state within {CATCH_UP:?}\n{}",
        out.render()
    );
    out
}

/// Run the dispatcher long enough to catch up on the whole backlog and stop it.
/// Used where the assertion is that nothing fires: the settle is the same one a
/// firing needs, and the returned stderr proves the dispatcher really started.
fn dispatch_and_settle(cx: &Instance, dir: &Path, args: &[&str]) -> CmdOutput {
    let mut argv = vec!["events", "dispatch"];
    argv.extend_from_slice(args);
    let child = cx
        .cmd(argv)
        .cwd(dir)
        .timeout(PATIENTLY)
        .start()
        .expect("spawn contenox events dispatch");
    std::thread::sleep(Duration::from_secs(20));
    child.interrupt();
    let out = child
        .wait_timeout(Duration::from_secs(60))
        .expect("the dispatcher stops on Ctrl-C");
    assert!(
        out.stderr.contains("Dispatching (catch-up, then live)"),
        "the dispatcher never got as far as dispatching\n{}",
        out.render()
    );
    out
}

/// The workspace the dispatcher bound itself to, as it prints it.
fn dispatcher_workspace(out: &CmdOutput) -> String {
    out.stderr
        .lines()
        .find_map(|line| line.trim().strip_prefix("Workspace: "))
        .unwrap_or_else(|| {
            panic!(
                "the dispatcher never printed its workspace\n{}",
                out.render()
            )
        })
        .trim()
        .to_string()
}

/// The `evt-` request id off one of the dispatcher's firing lines.
fn request_id(line: &str) -> String {
    line.split_whitespace()
        .find_map(|word| word.strip_prefix("request="))
        .unwrap_or_else(|| panic!("no request id on {line:?}"))
        .to_string()
}

// ===========================================================================
// The beta gate
// ===========================================================================

/// Beta means invisible, not absent: without the opt-in `contenox events` is
/// off the help, and setting the opt-in is all it takes to put it back.
#[test]
fn events_is_hidden_from_help_until_the_beta_opt_in() {
    let cx = instance("events-beta-help");

    let hidden = cx.run(["--help"]).ok();
    assert!(
        !hidden
            .stdout
            .lines()
            .any(|line| line.trim_start().starts_with("events ")),
        "`contenox events` must not be listed without the opt-in:\n{}",
        hidden.stdout
    );

    // Hidden gates visibility only: the command still runs, and an empty log is
    // an answer rather than a failure.
    cx.run(["events", "list"]).ok().expect_stdout("(no events)");

    let shown = cx
        .cmd(["--help"])
        .env("CONTENOX_OPT_IN_BETA", "1")
        .output()
        .expect("contenox --help")
        .ok();
    assert!(
        shown
            .stdout
            .lines()
            .any(|line| line.trim_start().starts_with("events ")),
        "CONTENOX_OPT_IN_BETA=1 must reveal `contenox events`:\n{}",
        shown.stdout
    );
}

/// The whole tier is off without the opt-in: the trigger file on disk is not
/// loaded, `contenox vet` will not even look at it, and a mission that would
/// have produced two events produces none.
#[test]
fn without_the_opt_in_no_trigger_loads_and_nothing_reaches_the_log() {
    let cx = instance("events-beta-off");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    let vetted = cx.run(["vet"]).ok();
    assert!(
        vetted.stdout.contains("skip") && vetted.stdout.contains("trigger-note.json"),
        "vet must skip a trigger file while the tier is off:\n{}",
        vetted.stdout
    );

    fire(&cx, "check the tree").ok();
    assert!(
        events(&cx).is_empty(),
        "no event may be appended without the opt-in, got {:?}",
        events(&cx)
    );

    let dispatched = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    dispatched
        .clone()
        .expect_stderr("opt-in-beta is off: no triggers are loaded")
        .expect_stderr("Loaded 0 trigger(s)");
    assert!(
        fired_lines(&dispatched).is_empty(),
        "nothing may fire without the opt-in:\n{}",
        dispatched.stdout
    );
    assert!(firings(&cx).is_empty(), "and nothing may be recorded");
}

// ===========================================================================
// The event, and what a fired chain is handed
// ===========================================================================

/// V1's catalog is the mission tier's own four types, and `events list` is
/// where an operator reads them: one landing mission files a report and changes
/// status, one that plans and asks revises a plan and raises an attention ask.
#[test]
fn the_v1_event_types_are_the_mission_tiers_own() {
    let cx = opted_in("events-catalog");

    fire(&cx, "check the tree").ok();

    // A unit that plans and then asks a question suspends on the ask, so this
    // fire never lands — the two events it appended are the point.
    cx.write_script(
        "scripted.json",
        &Script::new()
            .turn(
                Turn::new()
                    .text("Planning.")
                    .call(ToolCall::new("mission_plan").arguments(serde_json::json!({
                    "entries": [
                        {"content": "read the tag", "status": "in_progress", "priority": "high"},
                        {"content": "file the result", "status": "pending", "priority": "medium"}
                    ],
                    "explanation": "first plan"
                }))),
            )
            .turn(
                Turn::new().text("Asking.").call(
                    ToolCall::new("mission_ask_attention")
                        .arg("summary", "which tag should I cut?")
                        .arg("detail", "the repo carries v1 and v2"),
                ),
            ),
    )
    .expect("rewrite the dialog");
    cx.cmd([
        "mission",
        "fire",
        "run",
        "cut the tag",
        "--wait",
        "--policy",
        "run",
        "--timeout",
        "20s",
    ])
    .timeout(PATIENTLY)
    .output()
    .expect("contenox mission fire")
    .expect_failure();

    let logged = events(&cx);
    let types: Vec<&str> = logged.iter().map(|row| row.kind.as_str()).collect();
    for wanted in [
        "missionservice.events.report_added",
        "missionservice.events.status_changed",
        "missionservice.events.plan_revised",
        "missionservice.events.attention_asked",
    ] {
        assert!(
            types.contains(&wanted),
            "{wanted} is missing from the log: {types:?}"
        );
    }
    assert!(
        logged.iter().all(|row| row.source == "missionservice"),
        "every V1 event comes from the mission tier: {logged:?}"
    );
    assert!(
        logged.iter().all(|row| row.hop == "0"),
        "ordinary operation is hop 0: {logged:?}"
    );
    assert!(
        logged.iter().all(|row| !row.subject.is_empty()),
        "every mission event names its mission as the subject: {logged:?}"
    );

    let nids: Vec<i64> = logged
        .iter()
        .map(|row| row.nid.parse::<i64>().expect("nid is a number"))
        .collect();
    let mut sorted = nids.clone();
    sorted.sort_unstable();
    assert_eq!(nids, sorted, "the log lists in append (nid) order");

    // --since is the cursor an operator reads with: strictly greater than.
    let since = cx
        .run(["events", "list", "--since", &nids[0].to_string()])
        .ok();
    let listed = Table::parse(
        &since.stdout,
        &["NID", "TYPE", "SOURCE", "SUBJECT", "HOP", "TIME", "DATA"],
    )
    .expect("events list --since prints its table");
    let after: Vec<String> = listed
        .rows
        .iter()
        .map(|row| row.get("NID").to_string())
        .collect();
    assert!(
        !after.contains(&nids[0].to_string()),
        "--since {} excludes that event: {after:?}",
        nids[0]
    );
    assert_eq!(
        after.len(),
        nids.len() - 1,
        "and keeps every later one: {after:?}"
    );
}

/// The envelope the log stores is exactly what the fired chain gets as input —
/// read back off the run's own captured state, field for field against the row
/// `contenox events list` printed.
#[test]
fn a_fired_chain_receives_the_stored_envelope_verbatim() {
    let cx = opted_in("events-envelope");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());
    let workspace = dispatcher_workspace(&dispatched);

    let report = events(&cx)
        .into_iter()
        .find(|row| row.kind == "missionservice.events.report_added")
        .expect("the mission filed a report");
    let request = request_id(fired_lines(&dispatched)[0]);

    let raw = cx.run(["state", "show", &request, "--raw"]).ok();
    let units: Value = serde_json::from_str(&raw.stdout)
        .unwrap_or_else(|err| panic!("state show --raw is JSON: {err}\n{}", raw.stdout));
    let input = units[0]["input"]
        .as_object()
        .unwrap_or_else(|| panic!("the fired chain recorded no input\n{}", raw.stdout));

    let mut fields: Vec<&str> = input.keys().map(String::as_str).collect();
    fields.sort_unstable();
    assert_eq!(
        fields,
        vec![
            "data",
            "hop",
            "nid",
            "source",
            "subject",
            "time",
            "type",
            "workspace_id"
        ],
        "the chain is handed the stored envelope, no more and no less"
    );
    assert_eq!(input["nid"].to_string(), report.nid);
    assert_eq!(input["type"], Value::from(report.kind.as_str()));
    assert_eq!(input["source"], Value::from(report.source.as_str()));
    assert_eq!(input["subject"], Value::from(report.subject.as_str()));
    assert_eq!(input["hop"].to_string(), report.hop);
    assert_eq!(input["workspace_id"], Value::from(workspace.as_str()));
    let stored_time = input["time"].as_str().expect("time is a string");
    assert_eq!(
        &stored_time[..19],
        &report.time[..19],
        "the envelope carries the event time the log lists"
    );
    assert!(
        input["data"]["missionId"] == Value::from(report.subject.as_str()),
        "the producer's payload rides along verbatim: {}",
        input["data"]
    );
}

/// `listen_for.type` takes a dotted prefix pattern: one trigger reacting to
/// `missionservice.events.*` fires on every type under it.
#[test]
fn a_prefix_pattern_trigger_fires_on_every_type_under_it() {
    let cx = opted_in("events-pattern");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-all.json",
        &trigger_file(
            "note-every-mission-event",
            "missionservice.events.*",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    cx.run(["vet"])
        .ok()
        .expect_stdout("ok")
        .expect_stdout("trigger-all.json");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| firings(cx).len() >= 2);

    let fired: Vec<String> = fired_lines(&dispatched)
        .iter()
        .map(|line| line.to_string())
        .collect();
    assert!(
        fired
            .iter()
            .any(|line| line.contains("missionservice.events.report_added")),
        "the pattern must catch report_added: {fired:?}"
    );
    assert!(
        fired
            .iter()
            .any(|line| line.contains("missionservice.events.status_changed")),
        "the pattern must catch status_changed: {fired:?}"
    );
    assert!(
        firings(&cx)
            .iter()
            .all(|row| row.trigger == "note-every-mission-event"),
        "one trigger, both events: {:?}",
        firings(&cx)
    );
}

// ===========================================================================
// Exactly once, and where the cursor stands
// ===========================================================================

/// Set up a workspace whose report_added trigger fires a chain that holds the
/// process (a scripted `sleep`), and whose status_changed trigger fires the
/// instant `noop` chain. Returns after one mission has put both events in the
/// log.
fn instance_with_a_holding_trigger(label: &str) -> Instance {
    let cx = opted_in(label);
    actuating_backend(&cx, &[shell_turn("sleep", "45"), shell_turn("sleep", "45")]);
    cx.write_file(".contenox/chain-act.json", ACTUATING_CHAIN)
        .expect("write the actuating chain");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the noop chain");
    cx.write_file(
        ".contenox/trigger-hold.json",
        &trigger_file(
            "hold-on-report",
            "missionservice.events.report_added",
            "chain-act.json",
            None,
        ),
    )
    .expect("write the holding trigger");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-on-status",
            "missionservice.events.status_changed",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the noop trigger");
    fire(&cx, "check the tree").ok();
    cx
}

/// A dispatcher killed mid-chain leaves its claim standing, and the next one
/// does not fire that pair again: the claim, not the cursor, is what makes a
/// firing at-most-once.
#[test]
fn a_killed_dispatchers_claim_is_never_fired_twice() {
    let cx = instance_with_a_holding_trigger("events-claim-once");

    let mut held = cx
        .cmd(["events", "dispatch", "--auto"])
        .timeout(PATIENTLY)
        .start()
        .expect("spawn the first dispatcher");
    cx.wait_for(Duration::from_secs(90), |cx| {
        !firings_in(cx, cx.work(), &["--status", "running"]).is_empty()
    })
    .expect("the first dispatcher claims the report event before running its chain");
    let claimed = firings_in(&cx, cx.work(), &["--status", "running"]);
    assert_eq!(claimed.len(), 1, "one claim, mid-chain: {claimed:?}");
    assert_eq!(claimed[0].trigger, "hold-on-report");
    let claim = claimed[0].clone();

    // The host dies with the chain still running — no graceful stop, nothing
    // recorded.
    held.kill();
    let _ = held.wait_timeout(Duration::from_secs(30));

    let second = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    assert!(
        !second.stdout.contains(&format!("nid={}", claim.nid)),
        "the next dispatcher must not fire a claimed pair again:\n{}",
        second.stdout
    );

    let after: Vec<FiringRow> = firings_in(&cx, cx.work(), &["--trigger", "hold-on-report"]);
    assert_eq!(
        after,
        vec![claim.clone()],
        "the claim is untouched and alone — one firing per (trigger, event)"
    );
    assert_eq!(
        after[0].status, "running",
        "a claim whose host died stays running until it goes stale"
    );
}

/// The cursor advances only after an event is handled, so the dispatcher that
/// takes over resumes at the event the killed one stopped on rather than
/// skipping the rest of the backlog.
#[test]
fn the_next_dispatcher_resumes_at_the_event_the_killed_one_stopped_on() {
    let cx = instance_with_a_holding_trigger("events-cursor-resume");

    let mut held = cx
        .cmd(["events", "dispatch", "--auto"])
        .timeout(PATIENTLY)
        .start()
        .expect("spawn the first dispatcher");
    cx.wait_for(Duration::from_secs(90), |cx| {
        !firings_in(cx, cx.work(), &["--status", "running"]).is_empty()
    })
    .expect("the first dispatcher takes the report event");
    held.kill();
    let _ = held.wait_timeout(Duration::from_secs(30));

    // The status_changed event was appended before the kill and never handled:
    // a dispatcher that resumed anywhere past it would never fire it.
    let second = dispatch_until(&cx, cx.work(), &["--auto"], |cx| {
        !firings_in(cx, cx.work(), &["--trigger", "note-on-status"]).is_empty()
    });
    let fired = fired_lines(&second);
    assert_eq!(
        fired.len(),
        1,
        "exactly the one unhandled event is picked up: {fired:?}"
    );
    assert!(
        fired[0].contains("note-on-status")
            && fired[0].contains("missionservice.events.status_changed")
            && fired[0].contains("status=ok"),
        "the resumed dispatcher fires the event after the one it died on: {fired:?}"
    );
}

// ===========================================================================
// Workspace scope
// ===========================================================================

/// The log, the cursor and the firing record are workspace-scoped: a dispatcher
/// in another project sees none of this one's events, however loudly its own
/// trigger is loaded.
#[test]
fn one_workspaces_triggers_never_fire_on_another_workspaces_events() {
    let cx = opted_in("events-workspace-scope");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    let other = cx.work().join("other");
    std::fs::create_dir_all(&other).expect("create the other project");
    cx.cmd(["init", "--project"])
        .cwd(&other)
        .output()
        .expect("contenox init --project")
        .ok();
    cx.write_file("other/.contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the other chain");
    cx.write_file(
        "other/.contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the other trigger");

    fire(&cx, "check the tree").ok();
    assert!(
        !events(&cx).is_empty(),
        "the fire must land events in its own workspace"
    );

    let elsewhere = dispatch_and_settle(&cx, &other, &["--auto"]);
    elsewhere
        .clone()
        .expect_stderr("Loaded 1 trigger(s)")
        .expect_stderr("note-report");
    assert!(
        fired_lines(&elsewhere).is_empty(),
        "another workspace's dispatcher must fire nothing:\n{}",
        elsewhere.stdout
    );
    assert!(
        events_in(&cx, &other).is_empty(),
        "and must not even see the events"
    );
    assert!(
        firings_in(&cx, &other, &[]).is_empty(),
        "and must record nothing"
    );
    // The control: the same trigger, chain and events fire at once at home, so
    // the silence above was the workspace boundary and not a dead trigger.
    let home = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    assert_eq!(
        fired_lines(&home).len(),
        1,
        "the event was fireable all along:\n{}",
        home.stdout
    );
    assert_ne!(
        dispatcher_workspace(&elsewhere),
        dispatcher_workspace(&home),
        "the two dispatchers bound different workspaces"
    );
}

// ===========================================================================
// Failure, and the loop that keeps going
// ===========================================================================

/// A chain that fails is recorded on its own firing and stops nothing: the next
/// trigger on the same event still fires.
#[test]
fn a_failing_chain_is_recorded_and_the_loop_keeps_going() {
    let cx = opted_in("events-chain-fails");
    cx.write_file(".contenox/chain-boom.json", FAILING_CHAIN)
        .expect("write the failing chain");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the noop chain");
    cx.write_file(
        ".contenox/trigger-boom.json",
        &trigger_file(
            "boom-report",
            "missionservice.events.report_added",
            "chain-boom.json",
            None,
        ),
    )
    .expect("write the failing trigger");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the surviving trigger");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| firings(cx).len() >= 2);

    let recorded = firings(&cx);
    let boom = recorded
        .iter()
        .find(|row| row.trigger == "boom-report")
        .expect("the failing trigger recorded a firing");
    assert_eq!(boom.status, "error");
    assert!(
        boom.error.contains("always_fails"),
        "the chain's own error is on the record: {boom:?}"
    );
    let note = recorded
        .iter()
        .find(|row| row.trigger == "note-report")
        .expect("the second trigger fired anyway");
    assert_eq!(note.status, "ok");
    assert_eq!(
        note.nid, boom.nid,
        "both fired on the same event, one after the other"
    );

    assert!(
        fired_lines(&dispatched)
            .iter()
            .any(|line| line.contains("status=error") && line.contains("boom-report")),
        "the failure is on the dispatcher's own line too:\n{}",
        dispatched.stdout
    );

    let filtered = firings_in(&cx, cx.work(), &["--status", "error"]);
    assert_eq!(
        filtered.len(),
        1,
        "--status error selects the failure alone: {filtered:?}"
    );
}

/// An event whose hop is past the limit is refused rather than fired, and the
/// refusal says which event and which limit.
#[test]
fn an_event_past_the_hop_limit_is_refused_not_fired() {
    let cx = opted_in("events-hop-limit");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    // A chain the dispatcher fires stamps hop+1 on what it appends, and carries
    // that hop across a process boundary in CONTENOX_EVENT_HOP — which is how a
    // fifth-generation event is produced from outside.
    cx.cmd([
        "mission",
        "fire",
        "run",
        "loop me",
        "--wait",
        "--policy",
        "run",
        "--timeout",
        "3m",
    ])
    .env("CONTENOX_EVENT_HOP", "5")
    .timeout(PATIENTLY)
    .output()
    .expect("contenox mission fire")
    .ok();

    let logged = events(&cx);
    assert!(
        logged.iter().all(|row| row.hop == "5"),
        "the fire must append fifth-generation events: {logged:?}"
    );

    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());
    let recorded = firings(&cx);
    assert_eq!(recorded.len(), 1, "one refusal, recorded: {recorded:?}");
    assert_eq!(recorded[0].status, "refused");
    assert!(
        recorded[0].error.contains("hop 5 exceeds limit 4"),
        "the refusal names the hop and the limit: {:?}",
        recorded[0]
    );
    assert!(
        fired_lines(&dispatched)[0].contains("status=refused"),
        "and the dispatcher says so on its line:\n{}",
        dispatched.stdout
    );

    assert!(
        cx.state_requests()
            .expect("contenox state list")
            .iter()
            .all(|id| !id.starts_with("evt-")),
        "a refused event runs no chain at all"
    );
}

// ===========================================================================
// The asks a firing raises
// ===========================================================================

/// Set up a workspace whose report_added trigger fires a chain that makes one
/// approve-tier `local_shell` call, under `policy` when it names one.
fn instance_with_a_gated_firing(label: &str, policy: Option<&str>) -> Instance {
    let cx = opted_in(label);
    actuating_backend(&cx, &[shell_turn("touch", "MARKER")]);
    cx.write_file(".contenox/chain-act.json", ACTUATING_CHAIN)
        .expect("write the actuating chain");
    cx.write_file(
        ".contenox/trigger-act.json",
        &trigger_file(
            "act-on-report",
            "missionservice.events.report_added",
            "chain-act.json",
            policy,
        ),
    )
    .expect("write the trigger");
    fire(&cx, "check the tree").ok();
    cx
}

/// Nobody is attached to a firing, so an approve-tier call must not be decided
/// by that absence: the ask is recorded, the process is handed back, and
/// `contenox approvals list` shows the ask — answering it later, from another
/// process, is what runs the call.
#[test]
fn a_firing_records_its_gated_ask_and_hands_the_process_back() {
    let cx = instance_with_a_gated_firing("events-detached-ask", Some("hitl-policy-run.json"));

    let dispatched = dispatch_until(&cx, cx.work(), &[], |cx| {
        !cx.approvals().expect("contenox approvals list").is_empty()
    });
    assert!(
        !dispatched
            .stderr
            .contains("requires approval but no terminal is attached"),
        "an unattended firing must not be denied for want of a terminal:\n{}",
        dispatched.render()
    );

    let pending = cx.approvals().expect("contenox approvals list");
    assert_eq!(pending.len(), 1, "the firing left one ask: {pending:?}");
    assert!(
        pending[0].tool.contains("local_shell"),
        "the ask names the gated call: {:?}",
        pending[0]
    );
    assert!(
        !cx.work().join("MARKER").exists(),
        "the call must not run before it is answered"
    );
    assert_eq!(
        firings(&cx).len(),
        1,
        "the firing itself is recorded and done: {:?}",
        firings(&cx)
    );

    // The dispatcher that raised it is long gone: answering from another
    // process is what resumes the fired run.
    cx.approve(&pending[0].id).ok();
    cx.wait_for(Duration::from_secs(90), |cx| {
        cx.work().join("MARKER").exists()
    })
    .expect("the answered ask resumes the fired chain wherever it can be resumed");
    assert!(
        cx.approvals().expect("contenox approvals list").is_empty(),
        "and the ask is closed behind it"
    );
}

/// `policy` is optional on a trigger — omitted, the standard resolution applies
/// — so a firing that names no envelope must park its gated ask exactly like
/// one that does.
#[test]
#[ignore = "confirmed defect: a firing whose trigger names NO policy does not detach its ask. \
The same chain, dispatcher and gate that park an ask under `\"policy\": \"hitl-policy-run.json\"` \
(see a_firing_records_its_gated_ask_and_hands_the_process_back, which passes) instead route the \
call to the terminal asker, which prints `[denied: local_shell.local_shell requires approval but \
no terminal is attached]`, tells the model `User denied the operation.` and records the firing \
status=ok — `contenox approvals list` stays empty, so a human is left nothing to answer and the \
call is silently lost. Seam: internal/surfaces/contenoxcli/events_cmd.go chainFiringRunner.RunChain \
sets taskengine.WithDetachedAsks either way and only adds hitlservice.WithPolicyName when \
t.Policy != \"\". Promised in docs/guide/events.md (`policy` optional, and \"a firing\'s asks are \
DETACHED\")."]
fn a_firing_without_a_named_policy_still_parks_its_gated_ask() {
    let cx = instance_with_a_gated_firing("events-detached-ask-default", None);

    let dispatched = dispatch_until(&cx, cx.work(), &[], |cx| {
        !cx.approvals().expect("contenox approvals list").is_empty()
    });
    assert!(
        !dispatched
            .stderr
            .contains("requires approval but no terminal is attached"),
        "an unattended firing must not be denied for want of a terminal:\n{}",
        dispatched.render()
    );
    let pending = cx.approvals().expect("contenox approvals list");
    assert_eq!(pending.len(), 1, "the firing left one ask: {pending:?}");
}

/// `--auto` buys unattended operation, not a bypass: the guide states the
/// trigger's policy still applies, so a call its envelope denies must not run.
#[test]
#[ignore = "confirmed defect: `contenox events dispatch --auto` drops the HITL gate entirely, so a \
fired chain's local_shell call runs even under a policy that denies it. Reproduced: trigger policy \
hitl-policy-read_only.json (local_shell -> deny), chain calls local_shell `touch MARKER`, the marker \
is created and the firing records status=ok. Seam: internal/surfaces/contenoxcli/engineopts.go sets \
EffectiveHITL = !autoMode, which builds the engine with no policy gate at all. Promised in \
docs/guide/events.md (\"the trigger's policy (or the default) still applies\", \"a trigger grants \
timing, never capability\") and in the --auto flag help."]
fn dispatch_auto_still_bounds_a_fired_chain_by_its_policy() {
    // read_only denies local_shell outright — no ask, no terminal, nothing to
    // answer: the call simply may not run.
    let cx = instance_with_a_gated_firing("events-auto-policy", Some("hitl-policy-read_only.json"));

    dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());

    assert!(
        !cx.work().join("MARKER").exists(),
        "the envelope denies local_shell, so --auto must not run it"
    );
}

// ===========================================================================
// The firing record an operator reads
// ===========================================================================

/// `events firings` is the dispatched half of the story: one row per claim with
/// the columns an operator greps, and an empty answer that is not a failure.
#[test]
fn events_firings_prints_every_claim_and_an_empty_match_exits_zero() {
    let cx = opted_in("events-firings-table");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());

    let listed = cx.run(["events", "firings"]).ok();
    let header = listed
        .stdout
        .lines()
        .next()
        .expect("events firings prints a header");
    assert_eq!(
        header.split_whitespace().collect::<Vec<_>>(),
        vec!["NID", "TRIGGER", "STATUS", "REQUEST", "TIME", "ERROR"]
    );

    let recorded = firings(&cx);
    assert_eq!(recorded.len(), 1, "one claim, one row: {recorded:?}");
    assert_eq!(recorded[0].trigger, "note-report");
    assert_eq!(recorded[0].status, "ok");
    assert_eq!(
        recorded[0].request,
        request_id(fired_lines(&dispatched)[0]),
        "the row carries the request id the dispatcher printed"
    );
    assert!(
        recorded[0].error.is_empty(),
        "a clean firing records no error: {recorded:?}"
    );

    cx.run(["events", "firings", "--trigger", "nosuchtrigger"])
        .ok()
        .expect_stdout("(no firings)");
    cx.run(["events", "firings", "--status", "refused"])
        .ok()
        .expect_stdout("(no firings)");
    cx.run(["events", "firings", "--status", "sideways"])
        .expect_failure()
        .expect_stderr("unknown --status");
}

/// The typo case leaves no error row to find: a trigger whose type matches
/// nothing records nothing at all, and `doctor` still lists it as loaded — which
/// is the only way to tell a typo from a trigger that never had an event.
#[test]
fn a_trigger_that_matches_nothing_records_nothing_and_doctor_still_lists_it() {
    let cx = opted_in("events-typo-trigger");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-typo.json",
        &trigger_file(
            "typo-report",
            "missionservice.events.reports_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger with the typo");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    dispatched
        .clone()
        .expect_stderr("Loaded 1 trigger(s)")
        .expect_stderr("typo-report → missionservice.events.reports_added → chain-note.json");
    assert!(
        fired_lines(&dispatched).is_empty(),
        "a type that never happens fires nothing:\n{}",
        dispatched.stdout
    );

    assert!(
        firings(&cx).is_empty(),
        "and records nothing at all — not even an error"
    );
    cx.run(["events", "firings", "--trigger", "typo-report"])
        .ok()
        .expect_stdout("(no firings)");

    cx.doctor()
        .ok()
        .expect_stdout("Event triggers (1 loaded)")
        .expect_stdout("typo-report");
    assert!(
        !events(&cx).is_empty(),
        "the events it should have matched are in the log to compare against"
    );
}

/// Retention is manual and blunt: `events prune` drops whole day partitions,
/// never runs by itself, and leaves the cursor and the firing record alone.
#[test]
fn events_prune_drops_nothing_it_was_not_asked_to_and_leaves_the_cursor_alone() {
    let cx = opted_in("events-prune");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());

    let before_events = events(&cx);
    let before_firings = firings(&cx);
    assert!(!before_events.is_empty() && !before_firings.is_empty());

    cx.run(["events", "prune", "--keep-days", "30"])
        .ok()
        .expect_stdout("Nothing to prune: no event partitions older than 30 day(s).");
    cx.run(["events", "prune", "--keep-days", "-1"])
        .expect_failure()
        .expect_stderr("--keep-days must be non-negative");

    assert_eq!(
        events(&cx),
        before_events,
        "today's events are not a retention candidate"
    );
    assert_eq!(
        firings(&cx),
        before_firings,
        "and the firing record is untouched"
    );

    // The cursor survives too: a dispatcher started after a prune re-fires
    // nothing it already handled.
    let again = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    assert!(
        fired_lines(&again).is_empty(),
        "prune must not rewind the cursor:\n{}",
        again.stdout
    );
    assert_eq!(firings(&cx), before_firings);
}

/// A fired run is an ordinary run: its `evt-` request id is on the dispatcher's
/// line, in the firing record, and in `contenox state`, where its steps read
/// back like any other chain's.
#[test]
fn a_fired_run_is_inspectable_under_its_evt_request_id() {
    let cx = opted_in("events-state-inspect");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());
    let request = request_id(fired_lines(&dispatched)[0]);
    assert!(
        request.starts_with("evt-"),
        "a fired run carries an evt- request id, got {request}"
    );

    let requests = cx.state_requests().expect("contenox state list");
    assert!(
        requests.contains(&request),
        "the fired run is listed among the captured ones: {requests:?}"
    );

    let steps = cx.state_steps(&request).expect("contenox state show");
    assert_eq!(steps.len(), 1, "the chain has one task: {steps:?}");
    assert_eq!(steps[0].task, "note_the_event");
    assert_eq!(steps[0].handler, "noop");
    assert_eq!(steps[0].status, "OK");
}

/// Hard invariant: firing observability is a read. Looking at what fired must
/// never append an event, or an incident would feed itself.
#[test]
fn reading_the_firing_record_never_appends_an_event() {
    let cx = opted_in("events-observe-is-a-read");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    let dispatched = dispatch_until(&cx, cx.work(), &["--auto"], |cx| !firings(cx).is_empty());
    let request = request_id(fired_lines(&dispatched)[0]);

    let before = events(&cx);
    assert!(!before.is_empty());

    for _ in 0..3 {
        cx.run(["events", "firings"]).ok();
        cx.run(["events", "firings", "--status", "ok"]).ok();
        cx.run(["events", "list"]).ok();
        cx.run(["state", "show", &request]).ok();
        cx.doctor().ok();
    }

    assert_eq!(
        events(&cx),
        before,
        "observing the firing record must append nothing"
    );
    assert_eq!(
        firings(&cx).len(),
        1,
        "and must fire nothing: {:?}",
        firings(&cx)
    );
}

// ===========================================================================
// The live, in-process path
// ===========================================================================

/// The guide's other firing path: a host that runs an engine fires matching
/// triggers the moment it appends an event, with no dispatcher involved.
#[test]
#[ignore = "confirmed defect: no in-process firing is observable for the events a `contenox run` \
(or `contenox mission fire`) host appends. The host prints `in-process event dispatch: 1 trigger(s) \
fire live in this process`, the run appends report_added and status_changed (`contenox events list` \
shows them), and yet `contenox events firings` stays empty and `contenox state list` gains no evt- \
run; a `contenox events dispatch` started afterwards fires those very events. The mission tier's \
events are appended by the dispatched unit subprocess, not by the host that loaded the triggers. \
Promised in docs/guide/events.md (\"Live, in-process … fires matching triggers the moment it \
appends an event\")."]
fn an_engine_running_host_fires_the_events_it_appends_itself() {
    let cx = opted_in("events-live-path");
    cx.write_file(".contenox/chain-note.json", NOOP_CHAIN)
        .expect("write the chain");
    cx.write_file(
        ".contenox/trigger-note.json",
        &trigger_file(
            "note-report",
            "missionservice.events.report_added",
            "chain-note.json",
            None,
        ),
    )
    .expect("write the trigger");

    fire(&cx, "check the tree").ok();
    assert!(
        events(&cx)
            .iter()
            .any(|row| row.kind == "missionservice.events.report_added"),
        "the host appended the event it should have fired on"
    );

    cx.wait_for(Duration::from_secs(30), |cx| !firings(cx).is_empty())
        .expect("the host fires its own event in-process, with no dispatcher running");
    let live = firings(&cx);
    assert_eq!(live.len(), 1, "one live firing: {live:?}");
    assert_eq!(live[0].trigger, "note-report");
    assert_eq!(live[0].status, "ok");

    // And the catch-up dispatcher must not fire it a second time.
    let dispatched = dispatch_and_settle(&cx, cx.work(), &["--auto"]);
    assert!(
        fired_lines(&dispatched).is_empty(),
        "the shared claim dedups the live/catch-up overlap:\n{}",
        dispatched.stdout
    );
    assert_eq!(firings(&cx), live);
}
