//! Missions, reports, plans and the inbox — the unattended half of the product.
//!
//! A mission is fired from outside, drives itself with nobody watching, and
//! leaves durable rows behind. Every case here fires one through the shipped
//! binary and then reads it back the way an operator does: `mission list`,
//! `mission show`, `mission reports`, `mission plan`, `approvals list`,
//! `inbox list`, and the exit code a script would branch on. The unit's own
//! transcript is read through `session list --all` + `session show`, which is
//! the only place a tool's reply to the model is visible from outside.

use contenox_e2e::{Acp, Instance, Script, Table, ToolCall, Turn, Usage};
use serde_json::json;
use std::time::Duration;

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/// A cold instance compiles its agents before the first turn, so every fire
/// gets the same generous ceiling.
fn patiently() -> Duration {
    Duration::from_secs(180)
}

fn instance(label: &str, script: &Script) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(script).expect("scripted-test backend");
    cx
}

/// `contenox mission fire <agent> "<intent>" --wait --policy <envelope>`.
fn fire(cx: &Instance, envelope: &str, intent: &str) -> contenox_e2e::CmdOutput {
    fire_within(cx, envelope, intent, "10m")
}

fn fire_within(
    cx: &Instance,
    envelope: &str,
    intent: &str,
    timeout: &str,
) -> contenox_e2e::CmdOutput {
    cx.cmd([
        "mission",
        "fire",
        "run",
        intent,
        "--wait",
        "--policy",
        envelope,
        "--timeout",
        timeout,
    ])
    .timeout(patiently())
    .output()
    .expect("contenox mission fire")
}

/// The one mission this case fired.
fn only_mission(cx: &Instance) -> contenox_e2e::MissionRow {
    let missions = cx.missions().expect("contenox mission list");
    match missions.as_slice() {
        [only] => only.clone(),
        other => panic!("expected exactly one mission, got {other:?}"),
    }
}

/// The dispatched unit's own transcript, read back through the product's
/// session commands — a unit's session lives in the runtime namespace, so it is
/// reachable by id rather than by the active workspace's name.
fn unit_transcript(cx: &Instance) -> String {
    let rows = cx.sessions_all().expect("contenox session list --all");
    let unit = rows
        .iter()
        .find(|row| row.name.starts_with("contenoxruntime-"))
        .unwrap_or_else(|| panic!("no unit session among {rows:?}"));
    cx.session_show(&unit.id).ok().stdout
}

/// `contenox inbox list`, as rows.
fn inbox(cx: &Instance, all: bool) -> Vec<contenox_e2e::table::Row> {
    let out = if all {
        cx.run(["inbox", "list", "--all"])
    } else {
        cx.run(["inbox", "list"])
    };
    out.clone().ok();
    if out.stdout.contains("No unacknowledged inbox items")
        || out.stdout.contains("Operator inbox is empty")
    {
        return Vec::new();
    }
    Table::parse(
        &out.stdout,
        &["ID", "REASON", "MISSION", "KIND", "SUMMARY", "AGE", "ACKED"],
    )
    .expect("contenox inbox list prints its table")
    .rows
}

/// Write `.contenox/agents.toml` in the workspace — where an operator declares
/// the envelopes a mission can be bounded by.
fn declare(cx: &Instance, body: &str) {
    cx.write_file(".contenox/agents.toml", body)
        .expect("write agents.toml");
}

/// The dialog that files one result and lands.
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

/// The dialog of a unit that answers in prose and never reaches its operator.
/// Exactly two turns: a third prompt would exhaust the script and fail loudly,
/// which is what makes "never a third" observable at all.
fn mute() -> Script {
    Script::new()
        .turn("I looked at everything and here is my prose answer.")
        .turn("Still talking, and still nobody is listening.")
}

/// One `/mission` at the preseeded `run` agent, under the envelope its own
/// declaration renders. The slash command takes the rendered filename.
const MISSION_COMMAND: &str = "/mission --policy hitl-policy-run.json run hold the fleet open";

/// Block until the mute unit has run out of turns: its mission is open, its
/// blocker is filed, and it holds fleet width for good.
fn wait_for_a_mute_unit(cx: &Instance) -> contenox_e2e::MissionRow {
    cx.wait_for(Duration::from_secs(120), |cx| {
        cx.missions()
            .ok()
            .and_then(|missions| missions.first().cloned())
            .map(|mission| {
                cx.mission_reports(&mission.id)
                    .stdout
                    .contains("unit ended two turns without reporting")
            })
            .unwrap_or(false)
    })
    .expect("the unit stops reporting and keeps its mission open");
    only_mission(cx)
}

const NUDGE: &str = "Your last turn ended without reaching outside this session";

// ===========================================================================
// One intent, one agent, one envelope named at fire time
// ===========================================================================

/// `--wait` is not a convenience: the unit is this process's child, so a
/// detached fire would tear down its own mission. The refusal says that, and
/// nothing is fired.
#[test]
fn a_fire_without_wait_is_refused_and_says_why_the_flag_exists() {
    let cx = instance("mission-needs-wait", &lands_reporting("never reached"));

    cx.cmd([
        "mission",
        "fire",
        "run",
        "check the tree",
        "--policy",
        "run",
    ])
    .timeout(patiently())
    .output()
    .expect("contenox mission fire")
    .expect_failure()
    .expect_stderr(
        "mission fire requires --wait: the dispatched unit is a child subprocess of this command",
    )
    .expect_stderr("fire from a long-lived host (an editor session's /mission)");

    assert!(
        cx.missions().expect("mission list").is_empty(),
        "a refused fire must cost nothing"
    );
}

/// Nothing runs unbounded. With neither `--policy` nor the config key, the fire
/// is refused rather than guessed at, and both ways to fix it are named.
#[test]
fn a_fire_with_no_envelope_is_refused_rather_than_guessed_at() {
    let cx = instance("mission-needs-envelope", &lands_reporting("never reached"));

    cx.cmd(["mission", "fire", "run", "check the tree", "--wait"])
        .timeout(patiently())
        .output()
        .expect("contenox mission fire")
        .expect_failure()
        .expect_stderr("no mission envelope: pass --policy <policy>")
        .expect_stderr("contenox config set default-mission-policy");

    assert!(
        cx.missions().expect("mission list").is_empty(),
        "an unbounded mission must fire nothing"
    );
}

/// The envelope is bound AT FIRE TIME and written into the record: changing the
/// default afterwards cannot rewrite what a finished mission ran under.
#[test]
fn the_envelope_named_at_fire_time_is_written_into_the_record() {
    let cx = instance("mission-envelope-bound", &lands_reporting("bounded once"));

    fire(&cx, "run", "check the tree")
        .ok()
        .expect_stdout("Mission fired at agent \"run\" under envelope \"run\".");

    cx.run(["config", "set", "default-mission-policy", "strict"])
        .ok();

    let mission = only_mission(&cx);
    assert_eq!(
        mission.envelope, "run",
        "the record keeps the envelope the fire named, not today's default"
    );
    let shown = cx.mission_show(&mission.id).expect("mission show");
    assert_eq!(shown.get("Envelope"), "run");
    assert_eq!(shown.get("Intent"), "check the tree");
    assert_eq!(shown.get("Agent"), "run");
}

/// The agent has to be a declared one. An unknown name is refused by name
/// before any record exists — there is no mission without an agent.
#[test]
fn an_undeclared_agent_is_refused_before_a_mission_record_exists() {
    let cx = instance("mission-unknown-agent", &lands_reporting("never reached"));

    cx.cmd([
        "mission",
        "fire",
        "notanagent",
        "do the thing",
        "--wait",
        "--policy",
        "run",
    ])
    .timeout(patiently())
    .output()
    .expect("contenox mission fire")
    .expect_failure()
    .expect_stderr("resolve agent \"notanagent\"");

    assert!(
        cx.missions().expect("mission list").is_empty(),
        "an unresolved agent must fire nothing at all"
    );
}

/// A `--policy` name is resolved against envelopes this command renders itself:
/// an envelope declared since the last run is fired under without any other
/// command having been run in between.
#[test]
fn a_fire_renders_every_declared_envelope_before_resolving_the_name() {
    let cx = instance("mission-renders-envelope", &lands_reporting("ran under it"));
    declare(
        &cx,
        r#"[envelopes.freshly]
description = "Declared after every other command already ran."
default_action = "approve"
files.read = "allow"
"#,
    );

    let rendered = cx.generated("hitl-policy-freshly.json");
    assert!(
        !rendered.exists(),
        "nothing has rendered {} yet — the fire is what must",
        rendered.display()
    );

    fire(&cx, "freshly", "run under the new envelope")
        .ok()
        .expect_stdout("under envelope \"freshly\"");

    assert!(
        rendered.exists(),
        "the fire renders the declared envelope to {}",
        rendered.display()
    );
    assert_eq!(only_mission(&cx).envelope, "freshly");
}

// ===========================================================================
// The exit code a script branches on
// ===========================================================================

/// Landed is the only zero. The reports ride on stdout with it, so a caller
/// that branches on the status can also read what came back.
#[test]
fn a_landed_mission_exits_zero_and_prints_its_reports() {
    let cx = instance("mission-lands", &lands_reporting("the tag is cut"));

    fire(&cx, "run", "cut the tag")
        .ok()
        .expect_stdout("finished: landed")
        .expect_stdout("Reports:")
        .expect_stdout("[result] the tag is cut")
        .expect_stdout("Full detail: contenox mission reports");

    assert_eq!(only_mission(&cx).status, "landed");
}

/// A mission that ends anywhere but `landed` exits non-zero, with the terminal
/// status and its reason on the same line.
#[test]
fn a_derailed_mission_exits_nonzero_naming_the_status_and_the_reason() {
    let cx =
        instance(
            "mission-derails",
            &Script::new()
                .turn(Turn::new().text("Giving up.").call(
                    ToolCall::mission_finish("derailed").arg("reason", "the tag does not exist"),
                ))
                .turn("Mission finished."),
        );

    fire(&cx, "run", "cut the tag")
        .expect_code(1)
        .expect_stdout("finished: derailed — the tag does not exist");

    assert_eq!(only_mission(&cx).status, "derailed");
}

/// The wait running out is not a terminal status: the unit dies with this
/// process, the record stays open, and the exit code says the mission did not
/// land.
#[test]
fn a_wait_that_runs_out_exits_nonzero_and_leaves_the_record_open() {
    let cx = instance("mission-wait-timeout", &mute());

    fire_within(&cx, "run", "say something", "20s")
        .expect_code(1)
        .expect_stdout("did not finish within 20s")
        .expect_stdout("Its record and any reports so far survive");

    assert_eq!(
        only_mission(&cx).status,
        "open",
        "the record outlives the process that waited on it"
    );
}

// ===========================================================================
// The drive loop: two prompt turns, never a third
// ===========================================================================

/// A unit that answers in prose is nudged exactly once, and when the nudge
/// changes nothing the RUNTIME files the blocker rather than prompting again.
/// The script holds exactly two turns, so a third prompt would fail loudly as
/// an exhausted script — getting the silent-turn blocker instead is the proof
/// that no third prompt was sent.
#[test]
fn a_mute_unit_gets_one_nudge_and_then_the_runtime_files_the_blocker_itself() {
    let cx = instance("mission-mute-nudge", &mute());

    fire_within(&cx, "run", "say something", "20s").expect_code(1);

    let mission = only_mission(&cx);
    assert_eq!(
        mission.status, "open",
        "a mute unit reaches no verdict of its own"
    );

    let reports = cx.mission_reports(&mission.id).ok();
    reports
        .clone()
        .expect_stdout("[blocker]")
        .expect_stdout("unit ended two turns without reporting")
        .expect_stdout(
            "The unit produced no mission report across two turns (its first turn and one runtime nudge).",
        )
        .expect_stdout("Attach to session contenoxruntime-");
    assert_eq!(
        reports.stdout.matches("[blocker]").count(),
        1,
        "one blocker, not one per turn:\n{}",
        reports.render()
    );

    let transcript = unit_transcript(&cx);
    assert_eq!(
        transcript.matches(NUDGE).count(),
        1,
        "exactly one nudge reached the unit:\n{transcript}"
    );
    assert!(
        transcript.contains("Still talking, and still nobody is listening."),
        "and the unit answered it:\n{transcript}"
    );
}

/// A turn that ERRORS is not nudged: the runtime files the blocker, derails the
/// mission and reaps the unit. The context ceiling is what makes the first turn
/// fail here — the unit's own preamble does not fit.
#[test]
fn a_turn_that_errors_skips_the_nudge_and_derails_the_mission() {
    let cx = instance(
        "mission-turn-error",
        &Script::new().context_length(1).turn("never reached"),
    );

    fire(&cx, "run", "do something")
        .expect_code(1)
        .expect_stdout("finished: derailed — unit turn failed");

    let mission = only_mission(&cx);
    assert_eq!(mission.status, "derailed");

    let reports = cx.mission_reports(&mission.id).ok();
    reports
        .clone()
        .expect_stdout("[blocker]")
        .expect_stdout("unit turn failed")
        .expect_stdout("exceeds context length")
        .expect_stdout(
            "The mission was finished derailed and the unit stopped, so it no longer holds fleet width.",
        );
    assert_eq!(
        reports.stdout.matches("[blocker]").count(),
        1,
        "one blocker for one failed turn:\n{}",
        reports.render()
    );

    let transcript = unit_transcript(&cx);
    assert!(
        !transcript.contains(NUDGE),
        "an erroring turn is never nudged:\n{transcript}"
    );
}

// ===========================================================================
// One running state, four terminal ones
// ===========================================================================

/// Finishing is immutable: the first terminal status is the mission's outcome
/// and a later, different verdict cannot become it. The unit is reaped the
/// instant its mission comes to rest, so a second call in the same turn may be
/// cancelled before it reaches the tool at all — what must hold either way is
/// that nothing it said was recorded.
#[test]
fn a_second_terminal_verdict_never_replaces_the_first() {
    let cx = instance(
        "mission-finish-conflict",
        &Script::new()
            .turn(
                Turn::new()
                    .text("Wrapping up.")
                    .call(ToolCall::mission_finish("landed").id("first"))
                    .call(
                        ToolCall::mission_finish("derailed")
                            .id("second")
                            .arg("reason", "changed my mind"),
                    ),
            )
            .turn("Mission finished."),
    );

    fire(&cx, "run", "finish twice").ok();

    let shown = cx
        .mission_show(&only_mission(&cx).id)
        .expect("mission show");
    assert_eq!(
        shown.get("Status"),
        "landed",
        "the first verdict stands, and the second's reason is nowhere on the record"
    );

    let transcript = unit_transcript(&cx);
    assert!(
        transcript.contains("mission finished as landed"),
        "the first verdict was recorded:\n{transcript}"
    );
    assert!(
        !transcript.contains("mission finished as derailed"),
        "the second was never applied:\n{transcript}"
    );
}

/// The same terminal status twice is a no-op, not an error: an operator who
/// repeats a stop gets the same answer and the record keeps the reason it
/// already carries.
#[test]
fn stopping_the_same_mission_twice_is_an_idempotent_no_op() {
    let cx = instance("mission-stop-twice", &mute());

    fire_within(&cx, "run", "say something", "20s").expect_code(1);
    let mission = only_mission(&cx);

    let stopped = "Pending asks are closed; any live unit is being reaped by its host process.";
    cx.run(["mission", "stop", &mission.id])
        .ok()
        .expect_stdout(stopped);
    cx.run([
        "mission",
        "stop",
        &mission.id,
        "--reason",
        "second thoughts",
    ])
    .ok()
    .expect_stdout(stopped);

    assert_eq!(
        cx.mission_show(&mission.id)
            .expect("mission show")
            .get("Status"),
        "abandoned — stopped by operator",
        "the terminal status was written once; the repeat rewrote nothing"
    );
}

/// The guard holds against an operator too: stopping a mission that already
/// landed is a conflict, not a silent overwrite of its outcome.
#[test]
fn stopping_a_finished_mission_is_refused_as_a_conflict() {
    let cx = instance("mission-stop-finished", &lands_reporting("already done"));

    fire(&cx, "run", "land it").ok();
    let mission = only_mission(&cx);

    cx.run(["mission", "stop", &mission.id])
        .expect_failure()
        .expect_stderr("already finished as \"landed\"; cannot re-finish as \"abandoned\"");

    assert_eq!(
        only_mission(&cx).status,
        "landed",
        "a refused stop changes nothing"
    );
}

// ===========================================================================
// A report, an ask and an inbox item are three different things
// ===========================================================================

/// One unit files a report and asks a question in the same turn. They land in
/// two different places with two different lifecycles, and the report also
/// lands in a third — the operator inbox, because a mission an operator fired
/// has no session listening. Acknowledging the inbox copy closes neither the
/// report nor the ask.
#[test]
fn a_report_an_ask_and_an_inbox_item_are_three_rows_with_three_lifecycles() {
    let cx = instance(
        "mission-three-rows",
        &Script::new()
            .turn(
                Turn::new()
                    .text("Working.")
                    .call(ToolCall::mission_report("progress", "read the changelog").id("report"))
                    .call(
                        ToolCall::new("mission_ask_attention")
                            .id("ask")
                            .arg("summary", "which database should I target?")
                            .arg("detail", "staging or production"),
                    ),
            )
            .turn("Carrying on.")
            .turn("Still here."),
    );

    fire_within(&cx, "run", "target the right database", "20s").expect_code(1);
    let mission = only_mission(&cx);

    // 1. the report: durable on the mission, whatever happens to the others
    cx.mission_reports(&mission.id)
        .ok()
        .expect_stdout("[progress]")
        .expect_stdout("read the changelog");

    // 2. the ask: pending in the live queue, waiting on a human's own words
    let asks = cx.approvals().expect("contenox approvals list");
    assert_eq!(asks.len(), 1, "one pending ask, got {asks:?}");
    assert_eq!(asks[0].kind, "question");
    assert_eq!(asks[0].tool, "mission.mission_ask_attention");
    assert_eq!(asks[0].summary, "which database should I target?");
    assert_eq!(asks[0].mission, mission.id);

    // 3. the inbox item: the report with nobody there to read it
    let items = inbox(&cx, false);
    assert_eq!(items.len(), 1, "one inbox item, got {items:?}");
    assert_eq!(items[0].get("REASON"), "operator-fired (no session)");
    assert_eq!(items[0].get("KIND"), "progress");
    assert_eq!(items[0].get("SUMMARY"), "read the changelog");
    assert_eq!(items[0].get("MISSION"), mission.id);
    assert_eq!(items[0].get("ACKED"), "no");

    // Acking is the inbox's own lifecycle and touches neither of the others.
    let item = items[0].get("ID").to_string();
    cx.run(["inbox", "ack", &item])
        .ok()
        .expect_stdout(&format!("Inbox item {item} acknowledged."));
    assert!(
        inbox(&cx, false).is_empty(),
        "an acked item leaves the list"
    );
    assert_eq!(
        inbox(&cx, true)[0].get("ACKED"),
        "yes",
        "--all still shows it, marked read"
    );
    assert_eq!(
        cx.approvals().expect("approvals list").len(),
        1,
        "the ask is untouched by an inbox ack"
    );
    cx.mission_reports(&mission.id)
        .ok()
        .expect_stdout("read the changelog");
}

/// A `result` may carry a structured hand-over for the mission that follows it.
/// Every claim in it is a reference — a path — never the artifact's content.
#[test]
fn a_result_report_carries_a_structured_handover_of_references() {
    let cx = instance(
        "mission-handover",
        &Script::new()
            .turn(
                Turn::new().text("Filing.").call(
                    ToolCall::mission_report("result", "the release is cut")
                        .arg("refs", json!(["real.txt"]))
                        .arg(
                            "handover",
                            json!({
                                "outcome": "the tag was cut and pushed",
                                "artifacts": ["real.txt"],
                                "handoverForNext": "pick up at the changelog",
                                "caveats": "the tests were not run",
                            }),
                        ),
                ),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    );
    cx.write_file("real.txt", "the artifact the result points at\n")
        .expect("the artifact");

    fire(&cx, "run", "cut the release").ok();

    let mission = only_mission(&cx);
    let reports = cx.mission_reports(&mission.id).ok();
    reports
        .clone()
        .expect_stdout("[result]")
        .expect_stdout("Refs: real.txt")
        .expect_stdout("Handover:")
        .expect_stdout("Outcome:  the tag was cut and pushed")
        .expect_stdout("Artifacts: real.txt")
        .expect_stdout("For next: pick up at the changelog")
        .expect_stdout("Caveats:  the tests were not run");
    assert!(
        !reports.stdout.contains("the artifact the result points at"),
        "a report references an artifact; it never carries its content:\n{}",
        reports.render()
    );
}

/// A result claiming an artifact that is not there is downgraded to progress
/// before it is stored, and `mission show` prints the warning beside it — so
/// the summary line and the durable row never disagree.
#[test]
fn mission_show_flags_a_report_the_verification_gate_downgraded() {
    let cx = instance(
        "mission-verification",
        &Script::new()
            .turn(
                Turn::new()
                    .text("Filing.")
                    .call(
                        ToolCall::mission_report("result", "wrote the migration notes")
                            .id("missing")
                            .arg("refs", json!(["notes/missing-migration.md"])),
                    )
                    .call(
                        ToolCall::mission_report("result", "wrote the real file")
                            .id("present")
                            .arg("refs", json!(["real.txt"])),
                    )
                    .call(ToolCall::mission_finish("landed").id("done")),
            )
            .turn("Mission finished."),
    );
    cx.write_file("real.txt", "this one exists\n")
        .expect("the artifact that is there");

    fire(&cx, "run", "write the notes").ok();

    let shown = cx
        .mission_show(&only_mission(&cx).id)
        .expect("mission show");
    assert!(
        shown.body.contains("[progress] wrote the migration notes"),
        "the unverifiable result is stored as progress:\n{}",
        shown.body
    );
    assert!(
        shown
            .body
            .contains("⚠ claimed artifacts not found: \"notes/missing-migration.md\""),
        "and mission show warns beside it:\n{}",
        shown.body
    );
    assert!(
        shown.body.contains("[result] wrote the real file"),
        "a result whose artifact is there is left alone:\n{}",
        shown.body
    );
}

// ===========================================================================
// The living plan
// ===========================================================================

/// Each call replaces the whole plan. `mission show` prints one summary line;
/// `mission plan` prints the plan itself and the durable revision history.
#[test]
fn the_plan_is_a_full_snapshot_and_mission_plan_prints_its_revisions() {
    let entry = |content: &str, status: &str, priority: &str| json!({"content": content, "status": status, "priority": priority});
    let cx = instance(
        "mission-plan",
        &Script::new()
            .turn(
                Turn::new().text("Planning.").call(
                    ToolCall::new("mission_plan")
                        .id("first")
                        .arg(
                            "entries",
                            json!([
                                entry("read the changelog", "completed", "high"),
                                entry("cut the release tag", "in_progress", "high"),
                                entry("announce it", "pending", "low"),
                            ]),
                        )
                        .arg("explanation", "first pass over the release"),
                ),
            )
            .turn(
                Turn::new()
                    .text("Revising.")
                    .call(
                        ToolCall::new("mission_plan")
                            .id("second")
                            .arg(
                                "entries",
                                json!([
                                    entry("read the changelog", "completed", "high"),
                                    entry("cut the release tag", "completed", "high"),
                                ]),
                            )
                            .arg(
                                "explanation",
                                "dropped the announcement, the release is internal",
                            ),
                    )
                    .call(ToolCall::mission_report("result", "release cut").id("report"))
                    .call(ToolCall::mission_finish("landed").id("done")),
            )
            .turn("Mission finished."),
    );

    fire(&cx, "run", "cut the release").ok();
    let mission = only_mission(&cx);

    let shown = cx.mission_show(&mission.id).expect("mission show");
    assert_eq!(
        shown.get("Plan"),
        "revision 2 (0 pending, 0 in progress, 2 completed)",
        "mission show carries the plan as one summary line"
    );

    cx.run(["mission", "plan", &mission.id])
        .ok()
        .expect_stdout("plan revision 2")
        .expect_stdout("Rationale: dropped the announcement, the release is internal")
        .expect_stdout("completed  high      cut the release tag")
        .refute_stdout("announce it")
        .expect_stdout("Revision history (oldest first):")
        .expect_stdout(
            "rev 1: +3/-0 (1 pending, 1 in progress, 1 completed) — first pass over the release",
        )
        .expect_stdout("rev 2: +2/-3 (0 pending, 0 in progress, 2 completed)");
}

/// A mission nothing planned says so plainly rather than printing an empty
/// table.
#[test]
fn a_mission_with_no_plan_says_so_rather_than_printing_an_empty_table() {
    let cx = instance("mission-no-plan", &lands_reporting("no planning needed"));

    fire(&cx, "run", "just do it").ok();
    let mission = only_mission(&cx);

    cx.run(["mission", "plan", &mission.id]).ok().expect_stdout(
        "has no plan yet (revision 0) — no resident planner has run for this mission",
    );
}

// ===========================================================================
// Stopping one
// ===========================================================================

/// `mission stop` is the way out of a mission waiting on an answer nobody will
/// give: it abandons the record, closes the pending ask, and reads as the
/// operator's own act rather than as a reclaim.
#[test]
fn mission_stop_abandons_the_mission_and_closes_its_pending_ask() {
    let cx = instance(
        "mission-stop",
        &Script::new()
            .turn(
                Turn::new().text("I need a decision.").call(
                    ToolCall::new("mission_ask_attention")
                        .arg("summary", "which bucket should I use?")
                        .arg("detail", "staging or production"),
                ),
            )
            .turn("Carrying on.")
            .turn("Still here."),
    );

    fire_within(&cx, "run", "pick the bucket", "20s").expect_code(1);
    let mission = only_mission(&cx);
    assert_eq!(mission.status, "open");
    assert_eq!(
        cx.approvals().expect("approvals list").len(),
        1,
        "the question is pending before the stop"
    );

    cx.run(["mission", "stop", &mission.id])
        .ok()
        .expect_stdout(&format!(
            "Mission {} abandoned. Pending asks are closed; any live unit is being reaped by its host process.",
            mission.id
        ));

    let shown = cx.mission_show(&mission.id).expect("mission show");
    assert_eq!(
        shown.get("Status"),
        "abandoned — stopped by operator",
        "a stopped mission reads as the operator's act"
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and its pending ask is closed with it"
    );
}

/// The reason an operator gives is the record's reason.
#[test]
fn mission_stop_records_the_reason_the_operator_gave() {
    let cx = instance("mission-stop-reason", &mute());

    fire_within(&cx, "run", "say something", "20s").expect_code(1);
    let mission = only_mission(&cx);

    cx.run([
        "mission",
        "stop",
        &mission.id,
        "--reason",
        "superseded by the release branch",
    ])
    .ok();

    assert_eq!(
        cx.mission_show(&mission.id)
            .expect("mission show")
            .get("Status"),
        "abandoned — superseded by the release branch"
    );
}

// ===========================================================================
// Fleet width
// ===========================================================================

/// The cap is admission, not a queue: a dispatch past it is refused on the
/// spot, naming how many units are open and the key that raises the ceiling.
/// It takes a long-lived host to have two units in flight at once, so this is
/// the editor's `/mission`, twice, over one live unit that never concludes.
#[test]
fn a_dispatch_past_the_fleet_width_cap_is_refused_naming_the_count_and_the_key() {
    let cx = instance("mission-fleet-cap", &mute());
    cx.run(["config", "set", "fleet-max-parallel", "1"]).ok();

    let mut acp: Acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    let first = acp
        .prompt(&session, MISSION_COMMAND)
        .expect("the first /mission");
    assert!(
        first
            .text()
            .contains("Mission fired at named agent \"run\""),
        "the first fire is admitted: {}",
        first.text()
    );

    // The unit is mute, so it holds width for good — but only once its drive
    // loop has run out of turns, which is what this waits for.
    wait_for_a_mute_unit(&cx);

    let second = acp
        .prompt(&session, MISSION_COMMAND)
        .expect("the second /mission");
    let told = second.text();
    assert!(
        told.contains(
            "fleet admission refused: 1 units are already open, at the fleet-width cap (fleet-max-parallel=1)"
        ),
        "the refusal names the count and the key: {told}"
    );
    assert!(
        told.contains("contenox config set fleet-max-parallel"),
        "and how to raise it: {told}"
    );

    assert_eq!(
        cx.missions().expect("mission list").len(),
        1,
        "the refused dispatch left no record behind"
    );
    acp.close().expect("the editor hangs up");
}

/// `mission stop` reaches a unit hosted by another process: the record says
/// abandoned everywhere, and the width the unit held is given back — which only
/// the host that owns the subprocess can do, and which nothing but a later
/// dispatch can prove from outside.
#[test]
fn mission_stop_reaches_the_host_that_owns_the_unit_and_gives_its_width_back() {
    let cx = instance("mission-stop-reaps", &mute());
    cx.run(["config", "set", "fleet-max-parallel", "1"]).ok();

    let mut acp: Acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    acp.prompt(&session, MISSION_COMMAND)
        .expect("the first /mission");
    let mission = wait_for_a_mute_unit(&cx);

    // Full: the host is holding one unit that will never conclude on its own.
    let refused = acp.prompt(&session, MISSION_COMMAND).expect("/mission");
    assert!(
        refused.text().contains("fleet admission refused"),
        "the fleet is at its cap before the stop: {}",
        refused.text()
    );

    // Stopped from a second process entirely.
    cx.run(["mission", "stop", &mission.id]).ok();
    assert_eq!(
        cx.mission_show(&mission.id)
            .expect("mission show")
            .get("Status"),
        "abandoned — stopped by operator"
    );

    // The reap travels to the host over the bus, so the width comes back a
    // moment later rather than instantly.
    let mut admitted = String::new();
    for _ in 0..8 {
        admitted = acp
            .prompt(&session, MISSION_COMMAND)
            .expect("/mission")
            .text();
        if admitted.contains("Mission fired at named agent") {
            break;
        }
        std::thread::sleep(Duration::from_secs(2));
    }
    assert!(
        admitted.contains("Mission fired at named agent"),
        "the stopped unit was reaped by its host, so the fleet has room again: {admitted}"
    );
    acp.close().expect("the editor hangs up");
}

/// The session surface refuses an unbounded mission in its own words, naming
/// the flag it takes and the config key behind it.
#[test]
fn the_session_surface_refuses_a_fire_with_no_envelope_too() {
    let cx = instance("mission-slash-no-envelope", &mute());

    let mut acp: Acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "/mission run check the tree")
        .expect("/mission");
    let told = turn.text();
    assert!(
        told.contains("no mission envelope: name one as `/mission --policy <envelope> <intent>`"),
        "the refusal is in the session's own vocabulary: {told}"
    );
    assert!(
        told.contains("contenox config set default-mission-policy"),
        "and names the durable way to set one: {told}"
    );
    assert!(
        cx.missions().expect("mission list").is_empty(),
        "an unbounded mission fires nothing here either"
    );
    acp.close().expect("the editor hangs up");
}

// ===========================================================================
// Compute bounds
// ===========================================================================

/// `maxTurns = 1` is the only value with an effect: it drops the nudge. The
/// mute unit then crosses the bound after its single turn and the mission is
/// finished `stuck`, with the reason led by the bound that was crossed.
#[test]
fn max_turns_of_one_drops_the_nudge_and_finishes_the_mission_stuck() {
    let cx = instance("mission-max-turns", &mute());
    declare(
        &cx,
        r#"[envelopes.oneturn]
description = "One turn only: no nudge."
default_action = "approve"
files.read = "allow"

[envelopes.oneturn.compute]
max_turns = 1
on_exhausted = "finish_stuck"
"#,
    );

    fire(&cx, "oneturn", "say something")
        .expect_code(1)
        .expect_stdout(
            "finished: stuck — compute bound exhausted: maxTurns=1 — the mission spent its turn budget without reaching its operator.",
        );

    let mission = only_mission(&cx);
    assert_eq!(mission.status, "stuck");
    assert_eq!(mission.envelope, "oneturn");
    assert!(
        !unit_transcript(&cx).contains(NUDGE),
        "the dropped nudge never reached the unit"
    );
}

/// The token budget is checked between turns, on the usage the session
/// reported — best effort by construction, and enough to stop a runaway.
#[test]
fn the_token_budget_is_enforced_between_turns() {
    let cx = instance(
        "mission-max-tokens",
        &Script::new()
            .turn(Turn::new().text("Thinking out loud.").usage(Usage {
                prompt_tokens: 900,
                completion_tokens: 100,
                total_tokens: 1000,
            }))
            .turn("Still thinking."),
    );
    declare(
        &cx,
        r#"[envelopes.pennies]
description = "A tiny token budget."
default_action = "approve"

[envelopes.pennies.compute]
max_tokens = 10
on_exhausted = "finish_stuck"
"#,
    );

    let out = fire(&cx, "pennies", "burn the budget").expect_code(1);
    assert!(
        out.stdout_has("finished: stuck — compute bound exhausted: maxTokens=10 (reported usage "),
        "the reason names the bound and what was spent:\n{}",
        out.render()
    );
    assert_eq!(only_mission(&cx).status, "stuck");
}

/// `maxToolCalls` is DECLARED, not enforced. A unit that makes five calls under
/// a ceiling of one still lands — and the envelope's own summary says as much
/// wherever it is offered, so the disclosure is where the choice is made.
#[test]
fn max_tool_calls_is_declared_and_not_enforced() {
    let cx = instance(
        "mission-max-tool-calls",
        &Script::new()
            .turn(
                Turn::new()
                    .text("Looking around.")
                    .call(ToolCall::new("list_dir").id("one").arg("path", "."))
                    .call(ToolCall::new("list_dir").id("two").arg("path", "."))
                    .call(ToolCall::new("list_dir").id("three").arg("path", "."))
                    .call(ToolCall::mission_report("result", "looked three times").id("four"))
                    .call(ToolCall::mission_finish("landed").id("five")),
            )
            .turn("Mission finished."),
    );
    declare(
        &cx,
        r#"[envelopes.onecall]
description = "One tool call, declared."
default_action = "approve"
files.read = "allow"

[envelopes.onecall.compute]
max_tool_calls = 1
on_exhausted = "finish_stuck"
"#,
    );

    fire(&cx, "onecall", "look three times")
        .ok()
        .expect_stdout("finished: landed")
        .refute_stdout("compute bound exhausted");

    assert_eq!(
        only_mission(&cx).status,
        "landed",
        "five calls under a ceiling of one: the ceiling is a declaration"
    );
}

/// The model allowlist is enforced where the model is resolved, before any
/// request is sent — the refusal says which model the envelope permits, which
/// one resolution picked, and that nothing went out.
#[test]
fn a_model_outside_the_envelopes_allowlist_is_refused_at_the_resolution_seam() {
    let cx = instance("mission-model-allowlist", &lands_reporting("never reached"));
    declare(
        &cx,
        r#"[envelopes.wrongmodel]
description = "Only a model this host does not have."
default_action = "approve"

[envelopes.wrongmodel.compute]
model_allowlist = ["gpt-does-not-exist"]
"#,
    );

    fire(&cx, "wrongmodel", "use a model I do not have").expect_code(1);

    let mission = only_mission(&cx);
    assert_eq!(mission.status, "derailed");
    cx.mission_reports(&mission.id)
        .ok()
        .expect_stdout("compute bound refused: modelAllowlist")
        .expect_stdout("this mission's envelope permits only \"gpt-does-not-exist\"")
        .expect_stdout("Nothing was sent: resolution outside the mission envelope");
}

/// Compute bounds a mission could never honour are refused at load: the fire is
/// turned away and nothing runs, and `contenox vet` is where the reason is
/// spelled out.
#[test]
fn compute_bounds_that_cannot_be_honoured_bound_no_mission() {
    let cx = instance("mission-bad-bounds", &lands_reporting("never reached"));
    declare(
        &cx,
        r#"[envelopes.twoturns]
description = "More turns than a mission has."
default_action = "approve"

[envelopes.twoturns.compute]
max_turns = 2

[envelopes.pausey]
description = "An exhaustion behaviour this build does not implement."
default_action = "approve"

[envelopes.pausey.compute]
max_tool_calls = 10
on_exhausted = "pause_ask"
"#,
    );

    for envelope in ["twoturns", "pausey"] {
        fire(&cx, envelope, "run under an impossible envelope")
            .expect_failure()
            .expect_stderr(&format!("hitl policy \"{envelope}\" could not be loaded"));
    }
    assert!(
        cx.missions().expect("mission list").is_empty(),
        "nothing runs under bounds the runtime will not accept"
    );

    let vet = cx.run(["vet"]);
    assert_eq!(
        vet.code,
        Some(1),
        "vet must exit non-zero so a pipeline can branch on it\n{}",
        vet.render()
    );
    vet.expect_stdout(
        "compute: maxTurns is 2, but a mission runs at most two prompt turns (its own, plus one runtime nudge when it reports nothing): only 1 has an effect",
    )
    .expect_stdout("compute: pause_ask is not implemented; use finish_stuck");
}

// ===========================================================================
// Reclaim
// ===========================================================================

/// The abandoned sweep is lazy and runs on the reads an operator makes — but it
/// has a floor of six hours of silence, so a mission whose unit only just went
/// quiet is left alone by every one of them, and none of them says otherwise.
#[test]
fn a_mission_that_only_just_went_quiet_is_not_reclaimed_by_the_lazy_sweep() {
    let cx = instance("mission-no-early-reclaim", &mute());

    fire_within(&cx, "run", "say something", "20s").expect_code(1);
    let mission = only_mission(&cx);

    let list = cx.run(["mission", "list"]).ok();
    let show = cx.run(["mission", "show", &mission.id]).ok();
    let doctor = cx.doctor().ok();
    for out in [&list, &show, &doctor] {
        assert!(
            !out.stdout.contains("reclaimed"),
            "a fresh silence reclaims nothing, and the sweep says nothing:\n{}",
            out.render()
        );
    }

    assert_eq!(
        only_mission(&cx).status,
        "open",
        "the mission is still the runtime's to finish"
    );
}

// ===========================================================================
// Naming what does not exist
// ===========================================================================

/// Every mission read names the id it could not find and where to look.
#[test]
fn a_mission_id_that_does_not_exist_is_named_along_with_where_to_look() {
    let cx = instance("mission-unknown-id", &Script::new().turn("nothing to do"));

    for verb in ["show", "reports", "plan"] {
        cx.run(["mission", verb, "m-nope"])
            .expect_failure()
            .expect_stderr(
                "mission \"m-nope\" not found: libdb: not found — see 'contenox mission list' for recorded missions",
            );
    }
    cx.run(["inbox", "show", "nope"])
        .expect_failure()
        .expect_stderr(
            "no inbox item \"nope\" exists — 'contenox inbox list --all' shows every item, acknowledged or not",
        );
}

/// The empty states are sentences, not blank output.
#[test]
fn the_empty_mission_list_and_inbox_say_what_would_fill_them() {
    let cx = instance("mission-empty-states", &Script::new().turn("nothing to do"));

    cx.run(["mission", "list"]).ok().expect_stdout(
        "No missions recorded. Fire one with 'contenox mission fire <agent> \"<intent>\" --wait', or /mission from an editor session.",
    );
    cx.run(["inbox", "list"])
        .ok()
        .expect_stdout("No unacknowledged inbox items.");
    cx.run(["inbox", "list", "--all"]).ok().expect_stdout(
        "Operator inbox is empty. A mission's report lands here only when it had no live session to reach",
    );
}

// ===========================================================================
// Quarantined: documented, not delivered
// ===========================================================================

/// `/mission --policy` is documented as taking `<envelope>`, and every other
/// surface that takes an envelope accepts the bare name — `contenox mission
/// fire --policy run`, `--hitl-policy read_only`, `config set
/// default-mission-policy`. Only the slash command insists on the filename.
#[test]
#[ignore = "confirmed defect: '/mission --policy <envelope>' accepts only the rendered FILENAME. \
acpsvc's MissionEnvelopeSource.LookupEnvelope (internal/surfaces/contenoxcli/mission_envelopes.go) \
looks the name up verbatim, while every other envelope seam normalises a bare name through \
hitlservice.policyFileName ('run' -> 'hitl-policy-run.json'). So 'contenox mission fire --policy \
run' fires and '/mission --policy run' is refused with 'unknown mission envelope \"run\": no such \
policy file on the search path' — while listing hitl-policy-run.json as available in the same \
sentence. docs/reference/contenox-cli.md documents the flag as '--policy <envelope>'"]
fn the_mission_slash_command_takes_a_bare_envelope_name_like_every_other_surface() {
    let cx = instance(
        "mission-slash-bare-name",
        &lands_reporting("fired from the editor"),
    );

    let mut acp: Acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "/mission --policy run run read the changelog")
        .expect("/mission");
    assert!(
        turn.text().contains("under envelope \"run\""),
        "the bare envelope name resolves the same way it does on the command line: {}",
        turn.text()
    );
    acp.close().expect("the editor hangs up");
}
