//! Envelopes: the gate, and the wait.
//!
//! An envelope is a `[envelopes.<name>]` section an operator writes in
//! `agents.toml`. The runtime transpiles it into the HITL policy the approval
//! engine evaluates before every tool call, and the name is the whole identity:
//! the section, the rendered `hitl-policy-<name>.json`, and the value passed to
//! `--hitl-policy` / `--policy` are all the same thing.
//!
//! Every case here writes an envelope the way an operator does, runs the
//! shipped binary, and looks at what a person can see: the rendered file, the
//! refusal the model was handed, the row in `contenox approvals list`, and the
//! exit code a script would branch on.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn};
use serde_json::Value;
use std::path::PathBuf;
use std::time::Duration;

// ---------------------------------------------------------------- the fixtures

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx
}

/// Write the operator's own `~/.contenox/agents.toml`.
///
/// The shipped table is compiled into the binary and merges underneath, so a
/// file declaring one envelope leaves the eight shipped ones exactly where they
/// were — which is a promise of its own, asserted below.
fn declare(cx: &Instance, body: &str) {
    let path = cx.home_file("agents.toml");
    std::fs::write(&path, format!("version = 1\n\n{body}\n"))
        .unwrap_or_else(|err| panic!("write {}: {err}", path.display()));
}

/// The file an envelope renders to, in the directory every surface reads.
fn render_path(cx: &Instance, envelope: &str) -> PathBuf {
    cx.home_file(format!(".generated/hitl-policy-{envelope}.json"))
}

fn rendered(cx: &Instance, envelope: &str) -> Value {
    let path = render_path(cx, envelope);
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|err| panic!("read {}: {err}", path.display()));
    serde_json::from_str(&text)
        .unwrap_or_else(|err| panic!("{} is not JSON: {err}", path.display()))
}

/// Render every declared envelope without running a session. `contenox init`
/// re-transpiles on every invocation, which is the same code path a surface
/// startup takes.
fn render(cx: &Instance) -> contenox_e2e::CmdOutput {
    cx.run(["init"])
}

fn handshake(cx: &Instance, argv: &[&str]) -> (Acp, Value) {
    let mut acp = cx.acp(argv).expect("spawn the ACP surface");
    let init = acp.initialize().expect("initialize");
    (acp, init)
}

/// The dialog an unattended run needs: make the call under test, file a result,
/// land, and answer the loop's last question.
fn run_calling(call: ToolCall, summary: &str) -> Script {
    Script::new()
        .turn(Turn::new().text("Trying it.").call(call))
        .turn(
            Turn::new()
                .text("Filing.")
                .call(ToolCall::mission_report("result", summary)),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

/// The one editor turn: make the call under test, then answer.
fn editor_calling(call: ToolCall) -> Vec<Turn> {
    vec![
        Turn::new().text("Trying it.").call(call),
        Turn::new().text("That is what came back."),
    ]
}

/// Everything the run told its model, read back through `contenox session show`.
fn transcript(cx: &Instance) -> String {
    let sessions = cx.sessions_all().expect("contenox session list --all");
    let row = sessions.last().expect("the run recorded a session");
    let shown = cx.session_show(&row.id);
    assert!(
        shown.success(),
        "contenox session show {} failed\n{}",
        row.id,
        shown.render()
    );
    shown.stdout
}

// ============================================================== the three verdicts
//
// One envelope of the operator's own, saying a different word on each axis. The
// three cases below are the three things those words do.

const THREE_VERDICTS: &str = r#"[envelopes.casebook]
description = "One word per axis, and three different outcomes."
default_action = "deny"
files.read = "allow"
files.write = "approve"
shell = "deny"
"#;

#[test]
fn an_allow_grant_runs_the_call_and_asks_nobody() {
    let cx = instance("env-allow");
    declare(&cx, THREE_VERDICTS);
    cx.write_file("readme.txt", "the tree as it stands\n")
        .expect("a file to read");
    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", "readme.txt"),
    )))
    .expect("scripted-test backend");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "casebook"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read the readme").expect("prompt");

    assert!(
        !turn.asked_permission(),
        "an allow grant runs silently; nothing may be put in front of a person"
    );
    assert!(
        turn.methods().contains(&"fs/read_text_file"),
        "and the call actually ran: {:?}",
        turn.methods()
    );
    assert!(
        turn.tool_outputs().contains("the tree as it stands"),
        "the model was handed the file, got {}",
        turn.tool_outputs()
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "an allowed call parks nothing for anyone to answer"
    );
}

#[test]
fn an_approve_grant_stops_the_call_in_front_of_a_person() {
    let cx = instance("env-approve");
    declare(&cx, THREE_VERDICTS);
    cx.scripted(
        &Script::new().route("coding").turns(editor_calling(
            ToolCall::new("write_file")
                .arg("path", "notes.txt")
                .arg("content", "asked first\n"),
        )),
    )
    .expect("scripted-test backend");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "casebook"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "write a note").expect("prompt");

    let ask = turn
        .permissions
        .first()
        .unwrap_or_else(|| panic!("an approve grant must reach a person: {:#?}", turn.updates));
    assert_eq!(
        ask["toolCall"]["_meta"]["policyName"], "hitl-policy-casebook.json",
        "and the ask names the envelope that raised it"
    );
    assert_eq!(
        cx.read_file("notes.txt").expect("the approved write"),
        "asked first\n",
        "the answered call then runs"
    );
}

#[test]
fn a_deny_grant_refuses_the_call_outright_and_tells_the_model_why() {
    let cx = instance("env-deny");
    declare(&cx, THREE_VERDICTS);
    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("local_shell").arg("command", "ls"),
    )))
    .expect("scripted-test backend");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "casebook"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "list the directory").expect("prompt");

    assert!(
        !turn.asked_permission(),
        "a denied call is refused, not offered: {:#?}",
        turn.permissions
    );
    let told = turn.tool_outputs();
    assert!(
        told.contains("Denied by the active policy hitl-policy-casebook.json"),
        "the refusal names the envelope that refused it, got {told}"
    );
    assert!(
        told.contains("This is the envelope refusing the capability"),
        "and says it is the envelope rather than a transient failure, got {told}"
    );
    assert!(
        !turn.methods().contains(&"terminal/create"),
        "nothing may reach the client's terminal: {:?}",
        turn.methods()
    );
}

// ================================================================== failing closed

#[test]
fn an_envelope_that_states_no_default_action_asks_about_what_it_did_not_name() {
    let cx = instance("env-fail-closed");
    declare(
        &cx,
        r#"[envelopes.partial]
description = "Says one thing and leaves the rest unsaid."
files.read = "allow"
"#,
    );
    cx.scripted(
        &Script::new().route("coding").turns(editor_calling(
            ToolCall::new("write_file")
                .arg("path", "unnamed.txt")
                .arg("content", "unnamed\n"),
        )),
    )
    .expect("scripted-test backend");

    render(&cx).ok();
    assert_eq!(
        rendered(&cx, "partial")["default_action"],
        "approve",
        "an omitted default_action fails closed to approve, never to allow"
    );

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "partial"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "write a file").expect("prompt");

    assert!(
        turn.asked_permission(),
        "a capability the envelope never mentioned stops for a person: {:#?}",
        turn.updates
    );
}

// =============================================================== first match wins

#[test]
fn the_first_matching_rule_wins_so_a_deny_path_outranks_the_grant_beneath_it() {
    let cx = instance("env-order");
    declare(
        &cx,
        r#"[envelopes.carved]
description = "Writes are free, except where they are not."
default_action = "deny"
files.read = "allow"

[envelopes.carved.files.write]
grant = "allow"
deny_paths = ["**/vault/**"]
"#,
    );
    cx.scripted(
        &Script::new()
            .route("coding")
            .turn(
                Turn::new().text("Into the vault.").call(
                    ToolCall::new("write_file")
                        .arg("path", "vault/key.txt")
                        .arg("content", "secret\n"),
                ),
            )
            .turn(
                Turn::new().text("Then the note.").call(
                    ToolCall::new("write_file")
                        .arg("path", "notes.txt")
                        .arg("content", "ordinary\n"),
                ),
            )
            .turn("Both attempts are in."),
    )
    .expect("scripted-test backend");
    cx.write_file("vault/placeholder", "").expect("the vault");

    render(&cx).ok();
    let policy = rendered(&cx, "carved");
    let rules = policy["rules"].as_array().expect("rules");
    assert_eq!(
        rules[0]["action"], "deny",
        "the carve-out is emitted ahead of the grant it carves out of:\n{policy:#}"
    );
    assert_eq!(rules[0]["tools"], "local_fs");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "carved"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "write both files").expect("prompt");

    let told = turn.tool_outputs();
    assert!(
        told.contains("Denied by the active policy hitl-policy-carved.json (rule 0)"),
        "the denial names the rule index that decided it, got {told}"
    );
    assert!(
        cx.read_file("vault/key.txt").is_err(),
        "the carved-out path is never written"
    );
    assert_eq!(
        cx.read_file("notes.txt").expect("the ordinary write"),
        "ordinary\n",
        "while the grant below still stands for everything else"
    );
    assert!(
        !turn.asked_permission(),
        "neither outcome needed a person: one allowed, one refused"
    );
}

// ================================================================ the name is all

#[test]
fn every_run_renders_the_declared_envelope_to_the_file_its_name_says() {
    let cx = instance("env-render");
    declare(
        &cx,
        r#"[envelopes.review]
description = "Read the tree, change nothing."
default_action = "deny"
files.read = "allow"
"#,
    );

    let path = render_path(&cx, "review");
    assert!(
        !path.exists(),
        "nothing is rendered before a run: {}",
        path.display()
    );

    render(&cx).ok();

    assert!(
        path.exists(),
        "[envelopes.review] renders to {}",
        path.display()
    );
    let policy = rendered(&cx, "review");
    assert_eq!(policy["default_action"], "deny");
    assert!(
        policy["//"]
            .as_str()
            .unwrap_or_default()
            .contains("Read the tree, change nothing."),
        "the render carries the envelope's own description: {policy:#}"
    );

    // The product's own linter, on the file the product wrote.
    cx.run(["vet", path.to_str().expect("policy path")])
        .ok()
        .expect_stdout("ok");
}

#[test]
fn the_bare_name_and_the_rendered_filename_name_the_same_envelope() {
    let cx = instance("env-name-forms");
    cx.write_file("readme.txt", "readable, if the envelope allowed it\n")
        .expect("a file to read");
    declare(
        &cx,
        r#"[envelopes.locked]
description = "Refuse everything."
default_action = "deny"
"#,
    );
    // One registration, replayed from the top by each surface process.
    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", "readme.txt"),
    )))
    .expect("scripted-test backend");

    let refusal = |flag: &str| {
        let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", flag]);
        let session = acp.new_session(cx.work()).expect("session/new");
        let turn = acp.prompt(&session, "read it").expect("prompt");
        turn.tool_outputs()
    };

    for flag in ["locked", "hitl-policy-locked.json"] {
        let told = refusal(flag);
        assert!(
            told.contains("Denied by the active policy hitl-policy-locked.json"),
            "--hitl-policy {flag} must resolve to the same envelope, got {told}"
        );
    }
}

// ============================================================ the search path

#[test]
fn a_hand_written_policy_shadows_the_render_and_is_never_rewritten() {
    let cx = instance("env-shadow");
    cx.write_file("readme.txt", "readable, if the envelope allowed it\n")
        .expect("a file to read");
    declare(
        &cx,
        r#"[envelopes.mine]
description = "The rendered one allows everything."
default_action = "allow"
"#,
    );
    // The operator's own copy, at the top level where it outranks the render.
    let own = cx.home_file("hitl-policy-mine.json");
    let hand_written =
        "{\"version\":1,\"default_action\":\"deny\",\"//\":\"written by hand\",\"rules\":[]}";
    std::fs::write(&own, hand_written).expect("write the operator's own policy");

    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", "readme.txt"),
    )))
    .expect("scripted-test backend");
    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "mine"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read it").expect("prompt");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-mine.json"),
        "the copy on the path is what gates, not the render beneath it: {}",
        turn.tool_outputs()
    );
    assert_eq!(
        std::fs::read_to_string(&own).expect("the operator's own policy"),
        hand_written,
        "and it is never rewritten by the render"
    );
    assert_eq!(
        rendered(&cx, "mine")["default_action"],
        "allow",
        "the render is still produced beside it, derived and disposable"
    );
}

#[test]
#[ignore = "confirmed defect: an envelope declared in the WORKSPACE .contenox/agents.toml never gates. contenoxcli/acp_cmd.go resolves contenoxDir with globalContenoxDir(), so every evaluating surface (acp, acpx, beam, and the mission unit a run dispatches) searches only ~/.contenox and ~/.contenox/.generated — the two workspace directories docs/guide/hitl.md#policy-resolution-order names as the STRONGEST entries are on nobody's path. `contenox run --policy <workspace envelope>` passes validation (mission_cmd.go resolves the workspace dir) and is then silently gated by hitl-policy-default.json instead, logging 'hitl: policy \"x\" unreadable, gating on default'."]
fn an_envelope_declared_in_the_workspace_gates_the_run_that_names_it() {
    let cx = instance("env-workspace-path");
    // The workspace file, which the resolution order calls the strongest source.
    cx.write_file(
        ".contenox/agents.toml",
        "version = 1\n\n[envelopes.wsonly]\ndescription = \"declared where the work is\"\ndefault_action = \"deny\"\n",
    )
    .expect("the workspace agents.toml");

    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "tried the tree",
    ))
    .expect("scripted-test backend");

    cx.cmd(["run", "--policy", "wsonly", "check the tree"])
        .timeout(Duration::from_secs(180))
        .output()
        .expect("contenox run")
        .ok();

    assert!(
        transcript(&cx).contains("Denied by the active policy hitl-policy-wsonly.json"),
        "the envelope the run named is the one that gates it:\n{}",
        transcript(&cx)
    );
}

#[test]
#[ignore = "confirmed defect, same seam as the case above: a hand-written hitl-policy-<name>.json in the WORKSPACE .contenox/ is the strongest entry in docs/guide/hitl.md#policy-resolution-order and is read by nobody. Every evaluating surface builds its policy source from globalContenoxDir(), so the global copy wins over the workspace copy that is supposed to outrank it."]
fn a_workspace_policy_copy_outranks_the_global_one() {
    let cx = instance("env-workspace-copy");
    cx.write_file("readme.txt", "the global copy let this through\n")
        .expect("a file to read");
    // The same name, written twice, saying opposite things.
    cx.write_file(
        ".contenox/hitl-policy-mine.json",
        "{\"version\":1,\"default_action\":\"deny\",\"rules\":[]}",
    )
    .expect("the workspace copy");
    std::fs::write(
        cx.home_file("hitl-policy-mine.json"),
        "{\"version\":1,\"default_action\":\"allow\",\"rules\":[]}",
    )
    .expect("the global copy");

    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", "readme.txt"),
    )))
    .expect("scripted-test backend");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "mine"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read it").expect("prompt");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-mine.json"),
        "the workspace copy is the strongest source, so its deny is the verdict: {}",
        turn.tool_outputs()
    );
}

// ============================================================= self-healing render

#[test]
fn a_broken_envelope_beside_a_readable_copy_is_reported_and_survived() {
    let cx = instance("env-self-healing");
    cx.write_file("readme.txt", "readable, if the envelope allowed it\n")
        .expect("a file to read");
    declare(
        &cx,
        r#"[envelopes.mine]
description = "Broken on purpose."
shel = "deny"
"#,
    );
    std::fs::write(
        cx.home_file("hitl-policy-mine.json"),
        "{\"version\":1,\"default_action\":\"deny\",\"rules\":[]}",
    )
    .expect("a readable copy on the path");
    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", "readme.txt"),
    )))
    .expect("scripted-test backend");

    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "mine"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read it").expect("prompt");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-mine.json"),
        "the copy on the path still gates while the render is broken: {}",
        turn.tool_outputs()
    );
    assert!(
        acp.notices().contains("rendering envelopes:"),
        "and the failed render is reported rather than swallowed:\n{}",
        acp.notices()
    );

    let code = acp
        .close()
        .expect("the surface exits when its client hangs up");
    assert_eq!(
        code,
        Some(0),
        "a stale envelope is survivable, so the surface must not refuse to start:\n{}",
        acp.notices()
    );
}

#[test]
fn a_name_that_resolves_to_nothing_is_fatal_and_says_where_it_looked() {
    let cx = instance("env-unresolvable");

    let out = cx
        .cmd(["acp", "--hitl-policy", "nowhere"])
        .timeout(Duration::from_secs(60))
        .output()
        .expect("contenox acp");

    out.expect_failure()
        .expect_stderr("no envelope \"nowhere\" resolves")
        .expect_stderr("hitl-policy-nowhere.json is on none of")
        .expect_stderr("agents.toml declares no [envelopes.nowhere]");
}

// ============================================================== the shipped eight

#[test]
fn the_shipped_envelopes_survive_an_operator_config_that_declares_none() {
    let cx = instance("env-shipped");
    // An operator file that mentions no envelope at all.
    declare(&cx, "[chain]\ntoken_limit = 65536");

    render(&cx).ok();

    for name in [
        "read_only",
        "ask_always",
        "auto_edit",
        "default",
        "strict",
        "acpx",
        "oracle",
        "serve",
    ] {
        let path = render_path(&cx, name);
        assert!(
            path.exists(),
            "the shipped envelope {name} must still resolve: {}",
            path.display()
        );
    }
    // The floor every unresolvable name lands on cannot be removed.
    assert_eq!(rendered(&cx, "default")["default_action"], "approve");

    // Each one still has the character its name promises.
    assert_eq!(
        verdict(&rendered(&cx, "read_only"), "local_fs", "read_file"),
        "allow",
        "read_only reads the workspace"
    );
    assert_eq!(
        verdict(&rendered(&cx, "read_only"), "local_fs", "write_file"),
        "deny",
        "and changes nothing"
    );
    assert_eq!(
        verdict(&rendered(&cx, "ask_always"), "local_shell", "local_shell"),
        "approve",
        "ask_always asks before running anything"
    );
    assert_eq!(
        verdict(&rendered(&cx, "auto_edit"), "local_fs", "write_file"),
        "allow",
        "auto_edit edits without asking"
    );
    assert_eq!(
        verdict(&rendered(&cx, "auto_edit"), "local_shell", "local_shell"),
        "approve",
        "and still asks before running anything"
    );
    assert_eq!(
        rendered(&cx, "strict")["default_action"],
        "deny",
        "strict refuses what it was not asked about"
    );
    assert!(
        !conditions(&rendered(&cx, "strict")).contains("go test"),
        "and its shell keeps no allowlist tier, so even a read-only command asks"
    );
    assert!(
        conditions(&rendered(&cx, "default")).contains("go test"),
        "which is exactly what default does keep"
    );
    assert_eq!(
        rendered(&cx, "acpx")["default_action"],
        "deny",
        "acpx refuses rather than asking a driver's user"
    );
    assert_eq!(
        verdict(&rendered(&cx, "acpx"), "local_fs", "write_file"),
        "deny",
        "including writes"
    );
    assert_eq!(
        rendered(&cx, "oracle")["default_action"],
        "deny",
        "the oracle reaches nothing it was not granted"
    );
    assert_eq!(
        verdict(&rendered(&cx, "oracle"), "oracle", "submit_verdict"),
        "allow",
        "except the one toolset it exists for"
    );
}

/// What a policy decides for one tool, first match wins, default_action last —
/// the engine's own order, replayed over the rendered file.
fn verdict(policy: &Value, toolset: &str, tool: &str) -> String {
    for rule in policy["rules"].as_array().expect("rules") {
        if rule.get("when").is_some() {
            continue; // conditional: it decides some calls, not this question
        }
        let rule_tools = rule["tools"].as_str().unwrap_or_default();
        let rule_tool = rule["tool"].as_str().unwrap_or_default();
        let matches =
            (rule_tools == toolset || rule_tools == "*") && (rule_tool == tool || rule_tool == "*");
        if matches {
            return rule["action"].as_str().unwrap_or_default().to_string();
        }
    }
    policy["default_action"]
        .as_str()
        .unwrap_or_default()
        .to_string()
}

/// Every condition value a policy carries, as one blob to look for a tier in.
fn conditions(policy: &Value) -> String {
    let mut out = String::new();
    for rule in policy["rules"].as_array().expect("rules") {
        for condition in rule["when"].as_array().unwrap_or(&Vec::new()) {
            out.push_str(condition["value"].as_str().unwrap_or_default());
            out.push('\n');
        }
    }
    out
}

#[test]
fn the_credential_quarantine_denies_a_key_read_under_the_most_permissive_posture() {
    let cx = instance("env-quarantine");
    cx.write_file(".ssh/id_rsa", "PRIVATE KEY MATERIAL\n")
        .expect("a key to quarantine");
    cx.scripted(&Script::new().route("coding").turns(editor_calling(
        ToolCall::new("read_file").arg("path", ".ssh/id_rsa"),
    )))
    .expect("scripted-test backend");

    // auto_edit is the widest shipped posture: it hands out file writes without
    // even asking. The quarantine rides beneath it all the same.
    let (mut acp, _) = handshake(&cx, &["acp", "--hitl-policy", "auto_edit"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read the key").expect("prompt");

    let told = turn.tool_outputs();
    assert!(
        told.contains("Denied by the active policy hitl-policy-auto_edit.json"),
        "key material is refused, not offered for approval: {told}"
    );
    assert!(
        !turn.asked_permission(),
        "and no approval is offered that could consent to it: {:#?}",
        turn.permissions
    );
    assert!(
        !told.contains("PRIVATE KEY MATERIAL"),
        "the key's contents must never reach the model: {told}"
    );
    assert!(
        turn.client_calls.is_empty(),
        "the client is never even asked to open it: {:?}",
        turn.methods()
    );

    // The rule the refusal named is a quarantine rule, not some other deny that
    // happens to sit in front of it.
    let rule = rule_index(&told);
    let quarantine = &rendered(&cx, "auto_edit")["rules"][rule];
    assert_eq!(quarantine["tools"], "local_fs");
    assert_eq!(
        quarantine["tool"], "*",
        "the quarantine binds every local_fs tool, not one of them: {quarantine:#}"
    );
    assert_eq!(quarantine["action"], "deny");
    assert!(
        quarantine["when"][0]["value"]
            .as_str()
            .unwrap_or_default()
            .contains(".ssh"),
        "rule {rule} of the widest posture is the key-store deny: {quarantine:#}"
    );
}

/// The rule index a policy refusal names: "… (rule 7)".
fn rule_index(refusal: &str) -> usize {
    let tail = refusal
        .split_once("(rule ")
        .unwrap_or_else(|| panic!("no rule index in {refusal}"))
        .1;
    tail.split(')')
        .next()
        .and_then(|digits| digits.trim().parse().ok())
        .unwrap_or_else(|| panic!("no rule index in {refusal}"))
}

// ==================================================================== the wait

#[test]
fn a_bounded_wait_counts_down_in_the_queue_and_denies_when_it_runs_out() {
    let cx = instance("env-wait-bounded");
    declare(
        &cx,
        r#"[envelopes.waiter]
description = "Asks, and does not wait long."
default_action = "allow"

[envelopes.waiter.tools]
"native-git.git_status" = { grant = "approve", timeout = "10s", on_timeout = "deny" }
"#,
    );
    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "waited for an answer nobody gave",
    ))
    .expect("scripted-test backend");

    let fired = cx
        .cmd(["run", "--policy", "waiter", "check the tree"])
        .timeout(Duration::from_secs(180))
        .start()
        .expect("contenox run");

    let ask = cx
        .await_approval(Duration::from_secs(60))
        .expect("the bounded ask reaches the queue");
    assert_eq!(ask.tool, "native-git.git_status");
    assert_ne!(
        ask.expires_in, "never",
        "a bounded ask shows the deadline the envelope wrote, got {:?}",
        ask.expires_in
    );

    let out = fired.wait().expect("the run finishes on its own");
    assert!(
        out.success(),
        "the expired ask releases the run rather than hanging it:\n{}",
        out.render()
    );

    let policy = rendered(&cx, "waiter");
    assert_eq!(policy["rules"][0]["timeout_s"], 10);
    assert_eq!(policy["rules"][0]["on_timeout"], "deny");

    assert!(
        transcript(&cx).contains("Approval timed out"),
        "and the model is told the wait ran out, not that a person refused:\n{}",
        transcript(&cx)
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "the expired ask leaves the queue"
    );
}

#[test]
fn an_ask_that_waits_for_ever_renders_minus_one_and_reads_never_in_the_queue() {
    let cx = instance("env-wait-never");
    declare(
        &cx,
        r#"[envelopes.patient]
description = "Waits until somebody answers."
default_action = "allow"

[envelopes.patient.tools]
"native-git.git_status" = { grant = "approve", timeout = "never" }
"#,
    );
    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "somebody answered in the end",
    ))
    .expect("scripted-test backend");

    let fired = cx
        .cmd(["run", "--policy", "patient", "check the tree"])
        .timeout(Duration::from_secs(180))
        .start()
        .expect("contenox run");

    let ask = cx
        .await_approval(Duration::from_secs(60))
        .expect("the unbounded ask reaches the queue");
    assert_eq!(
        ask.expires_in, "never",
        "an ask with no deadline says so where an operator reads it"
    );

    let rule = &rendered(&cx, "patient")["rules"][0];
    assert_eq!(rule["timeout_s"], -1, "timeout = \"never\" renders as -1");
    assert!(
        rule.get("on_timeout").is_none(),
        "and carries no on_timeout beside it: {rule:#}"
    );

    // Nothing resolves it on the operator's behalf, so the case must.
    cx.deny(&ask.id).ok();
    fired.wait().expect("the answered run finishes");
}

#[test]
fn an_ask_with_no_wait_written_falls_to_the_hosts_approval_ceiling() {
    let cx = instance("env-wait-ceiling");
    declare(
        &cx,
        r#"[envelopes.unbounded]
description = "Asks, and says nothing about how long."
default_action = "allow"

[envelopes.unbounded.tools]
"native-git.git_status" = "approve"
"#,
    );
    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "asked with no deadline of its own",
    ))
    .expect("scripted-test backend");

    let fired = cx
        .cmd(["run", "--policy", "unbounded", "check the tree"])
        .timeout(Duration::from_secs(180))
        .start()
        .expect("contenox run");

    let ask = cx
        .await_approval(Duration::from_secs(60))
        .expect("the ask reaches the queue");
    assert_eq!(
        ask.expires_in, "6d",
        "an unbounded ask shows the seven-day ceiling, not 'never'"
    );

    let rule = &rendered(&cx, "unbounded")["rules"][0];
    assert_eq!(rule["action"], "approve");
    assert!(
        rule.get("timeout_s").is_none(),
        "the rule itself carries no deadline: {rule:#}"
    );

    cx.deny(&ask.id).ok();
    fired.wait().expect("the answered run finishes");
}

// ======================================================= what an envelope refuses
//
// Every refusal below is reported where an operator sees it — on the surface's
// own stderr — and leaves the name unrenderable, so a run that names it stops.

/// Start `contenox acp` with a broken envelope and collect what it said.
fn render_refusal(cx: &Instance) -> String {
    let out = cx
        .cmd(["acp"])
        .stdin("")
        .timeout(Duration::from_secs(60))
        .output()
        .expect("contenox acp");
    out.notices()
}

#[test]
fn an_unknown_envelope_key_is_refused_and_names_the_keys_that_exist() {
    let cx = instance("env-unknown-key");
    declare(
        &cx,
        r#"[envelopes.mine]
files.read = "allow"
shel = "deny"
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("[envelopes.mine]: unknown key \"shel\""),
        "the refusal names the envelope and the key: {said}"
    );
    assert!(
        said.contains("known keys: extends, description, default_action"),
        "and lists what could have been written instead: {said}"
    );
    assert!(
        !render_path(&cx, "mine").exists(),
        "a refused envelope renders nothing"
    );
}

#[test]
fn an_envelope_name_outside_the_pattern_is_refused() {
    let cx = instance("env-bad-name");
    declare(
        &cx,
        r#"[envelopes.MyEnvelope]
files.read = "allow"
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("[envelopes.MyEnvelope]: name must match ^[a-z0-9][a-z0-9_-]*$"),
        "the refusal names the pattern: {said}"
    );
    assert!(
        said.contains("a dot would collide with TOML sub-table syntax"),
        "and why dots are excluded: {said}"
    );
}

#[test]
fn a_dotted_name_is_read_as_a_sub_table_rather_than_an_envelope() {
    let cx = instance("env-dotted-name");
    declare(
        &cx,
        r#"[envelopes.my.env]
files.read = "allow"
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("[envelopes.my]: unknown key \"env\""),
        "a dot makes TOML a sub-table, so the name reads as an envelope `my` \
         carrying a key `env` — which is why the pattern excludes dots: {said}"
    );
    assert!(
        !render_path(&cx, "my").exists() && !render_path(&cx, "my.env").exists(),
        "and neither reading of the name renders anything"
    );
}

#[test]
fn a_comma_inside_a_joined_axis_list_is_refused_rather_than_read_as_two() {
    let cx = instance("env-comma");
    declare(
        &cx,
        r#"[envelopes.mine.shell]
grant = "approve"
prefix_allowlist = ["go test, go vet"]
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("shell.prefix_allowlist[0] \"go test, go vet\" contains a comma"),
        "the refusal quotes the entry that would have been split: {said}"
    );
    assert!(
        said.contains("the engine reads this list as one comma-separated value"),
        "and says why one entry cannot carry one: {said}"
    );
}

#[test]
fn a_wait_on_a_grant_that_never_asks_is_refused_naming_the_envelope_and_the_axis() {
    let cx = instance("env-wait-no-ask");
    declare(
        &cx,
        r#"[envelopes.mine]
files.read = { grant = "allow", timeout = "30m", on_timeout = "deny" }
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("[envelopes.mine]: files.read: timeout/on_timeout apply to an ask"),
        "the refusal names both the envelope and the axis: {said}"
    );
    assert!(
        said.contains("grant = \"allow\" never asks"),
        "and the grant that cannot carry a wait: {said}"
    );
}

#[test]
fn on_timeout_beside_a_wait_that_never_expires_is_refused() {
    let cx = instance("env-never-plus-on-timeout");
    declare(
        &cx,
        r#"[envelopes.mine]
files.write = { grant = "approve", timeout = "never", on_timeout = "deny" }
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("[envelopes.mine]: files.write: on_timeout = \"deny\" cannot apply to timeout = \"never\""),
        "the refusal names the envelope and the axis: {said}"
    );
    assert!(
        said.contains("drop one of the two"),
        "and says what to do about it: {said}"
    );
}

#[test]
fn on_timeout_allow_is_refused_because_an_ask_may_not_allow_itself() {
    let cx = instance("env-on-timeout-allow");
    declare(
        &cx,
        r#"[envelopes.mine]
files.write = { grant = "approve", timeout = "30m", on_timeout = "allow" }
"#,
    );

    let said = render_refusal(&cx);
    assert!(
        said.contains("files.write.on_timeout is \"allow\", which the policy schema refuses"),
        "the refusal names the value: {said}"
    );
    assert!(
        said.contains("bypasses the approval it exists to require"),
        "and why no envelope may write it: {said}"
    );
    assert!(
        said.contains("write \"deny\""),
        "and names the only value that works: {said}"
    );
}

#[test]
fn a_refused_envelope_is_not_a_name_a_run_can_be_bounded_by() {
    let cx = instance("env-refused-run");
    declare(
        &cx,
        r#"[envelopes.mine]
files.write = { grant = "approve", timeout = "half an hour" }
"#,
    );
    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "never reached",
    ))
    .expect("scripted-test backend");

    cx.cmd(["run", "--policy", "mine", "do the thing"])
        .timeout(Duration::from_secs(120))
        .output()
        .expect("contenox run")
        .expect_failure()
        .expect_stderr("hitl policy \"mine\" could not be loaded");

    assert!(
        cx.missions().expect("mission list").is_empty(),
        "nothing may run under an envelope that would not compile"
    );
}

// ========================================================================== vet

#[test]
fn vet_fails_the_policy_files_that_can_never_do_what_they_say() {
    let cx = instance("env-vet-bad");
    let cases: [(&str, &str, &str); 5] = [
        (
            "unknownfield",
            r#"{"version":1,"default_action":"approve","rules":[],"compute":{"max_tool_kalls":5}}"#,
            "compute: unknown field \"max_tool_kalls\"",
        ),
        (
            "badrule",
            r#"{"version":1,"default_action":"approve","rules":[{"tools":"local_fs","tool":"write_file","action":"maybe"}]}"#,
            "rule 0: unknown action \"maybe\"",
        ),
        (
            "partialglob",
            r#"{"version":1,"default_action":"approve","rules":[{"tools":"native-git","tool":"git_*","action":"allow"}]}"#,
            "rule 0: tool \"git_*\" can never match",
        ),
        (
            "longwait",
            r#"{"version":1,"default_action":"approve","rules":[{"tools":"local_fs","tool":"write_file","action":"approve","timeout_s":999999999}]}"#,
            "timeout_s 999999999 is out of range",
        ),
        (
            "waitonallow",
            r#"{"version":1,"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow","timeout_s":60,"on_timeout":"deny"}]}"#,
            "timeout_s/on_timeout only apply when action is \"approve\"",
        ),
    ];

    for (name, body, _) in cases {
        cx.write_file(format!("bad/hitl-policy-{name}.json"), body)
            .expect("a policy to vet");
    }

    let out = cx.run(["vet", "./bad"]);
    assert_eq!(
        out.code,
        Some(1),
        "vet must exit non-zero so a pipeline can branch on it\n{}",
        out.render()
    );
    for (name, _, message) in cases {
        assert!(
            out.stdout_has(message),
            "vet must say what is wrong with hitl-policy-{name}.json, got\n{}",
            out.stdout
        );
    }
    assert!(
        out.stdout_has("vet: 5 of 5 file(s) failed"),
        "and count them:\n{}",
        out.stdout
    );
}

#[test]
fn vet_passes_the_envelope_the_runtime_rendered() {
    let cx = instance("env-vet-good");
    declare(
        &cx,
        r#"[envelopes.review]
description = "Read the tree, ask before the shell."
default_action = "deny"
files.read = "allow"

[envelopes.review.shell]
grant = "approve"
timeout = "30m"
on_timeout = "deny"
prefix_allowlist = ["go test", "go vet"]
"#,
    );
    render(&cx).ok();

    let path = render_path(&cx, "review");
    cx.run(["vet", path.to_str().expect("policy path")])
        .ok()
        .expect_stdout("ok");
}

// ============================================================ one namespace, two families

#[test]
fn an_envelope_that_takes_a_declared_agents_name_owns_the_file_and_says_so() {
    let cx = instance("env-agent-collision");
    // `reviewer` is a preseeded declared agent, and a declaration's own posture
    // renders to the same filename an envelope of that name renders to.
    declare(
        &cx,
        r#"[envelopes.reviewer]
description = "Takes a declared agent's name."
default_action = "deny"
"#,
    );

    // The next run is what re-renders, and it renders the envelope rather than
    // the declaration's own posture.
    render(&cx).ok();

    let listed = cx.run(["agent", "list"]);
    let said = format!("{}{}", listed.stdout, listed.stderr);
    assert!(
        listed.success(),
        "the collision is reported, not fatal:\n{}",
        listed.render()
    );
    assert!(
        said.contains(
            "reviewer: posture — \"reviewer\" is also an envelope in agents.toml, which owns hitl-policy-reviewer.json"
        ),
        "the report names both families and the file they share:\n{said}"
    );
    assert!(
        said.contains("this agent runs under the envelope"),
        "and which of the two won:\n{said}"
    );
    assert!(
        rendered(&cx, "reviewer")["//"]
            .as_str()
            .unwrap_or_default()
            .contains("Rendered from [envelopes.reviewer]"),
        "the envelope's render is what sits at the shared filename"
    );

    // The shipped set relies on the same rule: acpx is an agent and an envelope.
    assert!(
        said.contains("\"acpx\" is also an envelope"),
        "including the collision this build ships with:\n{said}"
    );
}

#[test]
fn the_credential_quarantine_leads_every_posture_that_can_touch_files() {
    let cx = instance("env-quarantine-set");
    render(&cx).ok();

    for posture in [
        "read_only",
        "ask_always",
        "auto_edit",
        "default",
        "strict",
        "acpx",
        "serve",
    ] {
        let policy = rendered(&cx, posture);
        let rules = policy["rules"].as_array().expect("rules");
        let quarantine: String = rules
            .iter()
            .filter(|rule| {
                rule["tools"] == "local_fs" && rule["tool"] == "*" && rule["action"] == "deny"
            })
            .filter_map(|rule| rule["when"][0]["value"].as_str())
            .collect::<Vec<_>>()
            .join("\n");

        for quarantined in [
            ".ssh",            // key material
            ".aws",            // cloud credentials
            ".password-store", // keyrings and password managers
            "wallet.dat",      // wallets
            ".mozilla",        // browser profiles
            ".bash_history",   // where a secret typed once still lives
            "id_rsa",          // private keys by their conventional names
        ] {
            assert!(
                quarantine.contains(quarantined),
                "{posture} must quarantine {quarantined} on every local_fs tool:\n{quarantine}"
            );
        }

        let last_deny = rules
            .iter()
            .rposition(|rule| rule["tools"] == "local_fs" && rule["tool"] == "*")
            .expect("the quarantine rules");
        let first_grant = rules
            .iter()
            .position(|rule| rule["action"] != "deny")
            .unwrap_or(rules.len());
        assert!(
            last_deny < first_grant,
            "the quarantine must sit ahead of every grant in {posture}, so no posture can reach past it"
        );
    }
}

#[test]
fn axis_lists_are_joined_into_one_condition_while_path_lists_emit_a_rule_each() {
    let cx = instance("env-lists");
    declare(
        &cx,
        r#"[envelopes.lists.shell]
grant = "approve"
prefix_allowlist = ["go test", "go vet"]

[envelopes.lists.files.write]
grant = "allow"
deny_paths = ["**/vault/**", "**/secrets/**"]
"#,
    );
    render(&cx).ok();

    let policy = rendered(&cx, "lists");
    let rules = policy["rules"].as_array().expect("rules");

    let allowlist: Vec<&Value> = rules
        .iter()
        .filter(|rule| rule["when"][0]["op"] == "command_prefix_allowlist")
        .collect();
    assert_eq!(
        allowlist.len(),
        1,
        "a shell tier is one condition, not one per entry:\n{policy:#}"
    );
    assert_eq!(
        allowlist[0]["when"][0]["value"], "go test,go vet",
        "the entries are joined into one comma-separated value — which is why an \
         entry carrying a comma is refused"
    );

    let denies: Vec<&Value> = rules
        .iter()
        .filter(|rule| rule["action"] == "deny")
        .collect();
    assert_eq!(
        denies.len(),
        6,
        "a path list emits one rule per glob per bound tool — 2 globs over 3 write tools:\n{policy:#}"
    );
    for glob in ["**/vault/**", "**/secrets/**"] {
        assert!(
            denies.iter().any(|rule| rule["when"][0]["value"] == glob),
            "each glob keeps its own rule, so a brace expression is still one pattern: {glob}"
        );
    }
    let last_deny = rules
        .iter()
        .rposition(|rule| rule["action"] == "deny")
        .expect("the carve-outs");
    let first_allow = rules
        .iter()
        .position(|rule| rule["action"] == "allow")
        .expect("the grant beneath them");
    assert!(
        last_deny < first_allow,
        "and the carve-outs are emitted ahead of the grant:\n{policy:#}"
    );
}

// ================================================== the unattended shape's record

#[test]
fn a_run_writes_the_envelopes_refusal_into_the_history_it_leaves_behind() {
    let cx = instance("env-run-history");
    declare(
        &cx,
        r#"[envelopes.locked]
description = "An unattended unit that may reach nothing."
default_action = "deny"
"#,
    );
    cx.scripted(&run_calling(
        ToolCall::new("git_status").arg("path", "."),
        "blocked by the envelope",
    ))
    .expect("scripted-test backend");

    cx.cmd(["run", "--policy", "locked", "check the tree"])
        .timeout(Duration::from_secs(180))
        .output()
        .expect("contenox run")
        .ok()
        .expect_stdout("blocked by the envelope");

    // Nobody watched this run, so the history is the only place the refusal is
    // legible afterwards.
    let history = transcript(&cx);
    assert!(
        history.contains("Denied by the active policy"),
        "the refusal is persisted, not just streamed:\n{history}"
    );
    assert!(
        history.contains("locked"),
        "and names the envelope that refused — the name is the whole identity, \
         so `locked` and hitl-policy-locked.json are the same thing:\n{history}"
    );
    assert!(
        history.contains("Do not retry it"),
        "and tells the model not to route around it:\n{history}"
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "a denial is not an ask: nothing was left for a person"
    );
    assert_eq!(
        cx.missions().expect("mission list")[0].envelope,
        "locked",
        "and the mission record keeps the envelope it ran under"
    );
}
