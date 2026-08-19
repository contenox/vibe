//! `contenox acp` / `contenox acpx` — the editor shape, driven from outside as
//! an ACP client over stdio.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn, Verdict};
use serde_json::{Value, json};
use std::time::Duration;

/// A scratch instance whose model is the scripted dialog.
fn editor(label: &str, script: &Script) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(script).expect("scripted-test backend");
    cx
}

/// The turn that writes a file, and the answer after it.
fn write_note(content: &str) -> Vec<Turn> {
    vec![
        Turn::new().text("Writing the note.").call(
            ToolCall::new("write_file")
                .arg("path", "notes.txt")
                .arg("content", content),
        ),
        Turn::new().text("That is done."),
    ]
}

fn picker<'a>(init: &'a Value, id: &str) -> &'a Value {
    init.get("_meta")
        .and_then(|meta| meta.get("contenox.workspaceConfigOptions"))
        .and_then(Value::as_array)
        .expect("initialize advertises the workspace config options")
        .iter()
        .find(|option| option["id"] == id)
        .unwrap_or_else(|| panic!("no {id:?} picker in {init:#}"))
}

fn handshake(cx: &Instance, argv: &[&str]) -> (Acp, Value) {
    let mut acp = cx.acp(argv).expect("spawn the ACP surface");
    let init = acp.initialize().expect("initialize");
    (acp, init)
}

// ---------------------------------------------------------------- the protocol

#[test]
fn acp_speaks_the_protocol_over_stdio_and_answers_a_prompt() {
    let cx = editor(
        "acp-stdio",
        &Script::new().route("general").turn("Two files changed."),
    );
    let (mut acp, init) = handshake(&cx, &["acp"]);

    assert_eq!(init["protocolVersion"], json!(1));
    assert_eq!(init["agentInfo"]["name"], json!("contenox"));
    let capabilities = &init["agentCapabilities"];
    assert_eq!(capabilities["loadSession"], json!(true));
    assert!(
        capabilities["sessionCapabilities"]["list"].is_object(),
        "session/list must be advertised: {capabilities:#}"
    );

    let session = acp.new_session(cx.work()).expect("session/new");
    assert!(!session.is_empty(), "session/new must return a session id");

    let turn = acp
        .prompt(&session, "what changed?")
        .expect("session/prompt");
    assert_eq!(turn.stop_reason, "end_turn");
    assert_eq!(turn.text(), "Two files changed.");
}

#[test]
fn acpx_speaks_the_same_protocol_for_a_headless_driver() {
    // The acpx chain is a single loop, so its script needs no routing label.
    let cx = editor("acpx-stdio", &Script::new().turn("Reporting in."));
    let (mut acp, init) = handshake(&cx, &["acpx"]);

    assert_eq!(init["protocolVersion"], json!(1));
    assert_eq!(init["agentInfo"]["name"], json!("contenox"));

    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "report").expect("session/prompt");
    assert_eq!(turn.stop_reason, "end_turn");
    assert_eq!(turn.text(), "Reporting in.");
}

#[test]
fn a_prompt_streams_the_answer_with_the_usage_and_title_a_client_renders() {
    let cx = editor(
        "acp-streaming",
        &Script::new().route("general").turn("Two files changed."),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "what changed?")
        .expect("session/prompt");

    let kinds = turn.kinds();
    assert!(
        kinds
            .iter()
            .filter(|k| **k == "agent_message_chunk")
            .count()
            > 1,
        "the answer arrives in chunks, not one blob: {kinds:?}"
    );
    assert!(
        kinds.contains(&"usage_update"),
        "the client is told what the turn cost: {kinds:?}"
    );
    let info = turn
        .of_kind("session_info_update")
        .first()
        .copied()
        .cloned()
        .unwrap_or_else(|| panic!("no session_info_update in {kinds:?}"));
    assert_eq!(
        info["title"],
        json!("what changed?"),
        "the first user message titles the session"
    );
}

#[test]
fn a_turn_that_fails_comes_back_as_a_json_rpc_error() {
    // One label turn and nothing behind it: the leaf's own call runs the script out.
    let cx = editor("acp-turn-error", &Script::new().route("general"));
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let reply = acp
        .call(
            "session/prompt",
            json!({"sessionId": session, "prompt": [{"type": "text", "text": "hello"}]}),
        )
        .expect("session/prompt");

    assert_eq!(
        reply.error_code(),
        -32603,
        "a failed turn is an error an integrator can branch on, not a quiet end_turn: {reply:?}"
    );
    let message = reply.error_message();
    assert!(
        message.contains("is exhausted") && message.contains("chain-acp-general-agent"),
        "and the reason travels verbatim, naming the task that failed, got {message:?}"
    );
}

#[test]
fn initialize_advertises_the_pickers_before_any_session_exists() {
    let cx = editor("acp-pickers", &Script::new().turn("hello"));
    let (mut acp, init) = handshake(&cx, &["acp"]);

    assert_eq!(picker(&init, "model")["type"], json!("select"));
    assert_eq!(picker(&init, "think")["type"], json!("select"));
    assert_eq!(picker(&init, "token-limit")["type"], json!("select"));

    let envelopes: Vec<&str> = picker(&init, "hitl-policy")["options"]
        .as_array()
        .expect("the hitl-policy picker lists its options")
        .iter()
        .filter_map(|value| value["name"].as_str())
        .collect();
    for name in ["default", "acpx", "strict", "read_only"] {
        assert!(
            envelopes.contains(&name),
            "every resolvable envelope must be listed by name, {name:?} is missing from {envelopes:?}"
        );
    }

    acp.close()
        .expect("the agent exits when its client hangs up");
}

// ------------------------------------------------------- approvals in the editor

#[test]
fn a_gated_write_asks_the_editors_permission_ui_and_shows_the_diff() {
    let cx = editor(
        "acp-permission",
        &Script::new()
            .route("coding")
            .turns(write_note("approved\n")),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    let ask = turn
        .permissions
        .first()
        .unwrap_or_else(|| panic!("the gated write must reach the client's permission UI"));
    assert_eq!(ask["sessionId"], json!(session));
    assert_eq!(
        ask["toolCall"]["title"],
        json!("local_fs.write_file: notes.txt")
    );
    assert_eq!(ask["toolCall"]["rawInput"]["path"], json!("notes.txt"));
    let kinds: Vec<&str> = ask["options"]
        .as_array()
        .expect("the ask offers options")
        .iter()
        .filter_map(|option| option["kind"].as_str())
        .collect();
    assert!(
        kinds.contains(&"allow_once") && kinds.contains(&"reject_once"),
        "the editor is offered allow and deny, got {kinds:?}"
    );

    let completed = turn
        .tool_call_updates()
        .into_iter()
        .find(|update| update["status"] == json!("completed"))
        .unwrap_or_else(|| panic!("the allowed call must complete: {:#?}", turn.updates));
    let diff = &completed["content"][0];
    assert_eq!(diff["type"], json!("diff"));
    assert_eq!(
        diff["path"],
        json!(cx.work().join("notes.txt").display().to_string())
    );
    assert_eq!(diff["newText"], json!("approved\n"));

    assert_eq!(turn.stop_reason, "end_turn");
    assert_eq!(cx.read_file("notes.txt").expect("the note"), "approved\n");
}

#[test]
fn the_ask_names_the_envelope_and_the_command_that_can_answer_it_elsewhere() {
    let cx = editor(
        "acp-ask-meta",
        &Script::new().route("coding").turns(write_note("meta\n")),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");
    let meta = &turn.permissions[0]["toolCall"]["_meta"];

    assert_eq!(meta["policyName"], json!("hitl-policy-default.json"));
    assert_eq!(meta["toolName"], json!("write_file"));
    assert_eq!(meta["toolsName"], json!("local_fs"));
    let recovery = meta["recoveryCommand"].as_str().unwrap_or_default();
    assert!(
        recovery.starts_with("contenox approvals respond "),
        "the same ask is answerable from a terminal, got {recovery:?}"
    );
}

#[test]
fn the_editors_ask_is_the_durable_ask_a_terminal_can_answer() {
    let cx = editor(
        "acp-durable-ask",
        &Script::new()
            .route("coding")
            .turns(write_note("answered elsewhere\n")),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    // The editor never answers: the ask has to stand on its own.
    acp.answers(Verdict::Defer);
    let session = acp.new_session(cx.work()).expect("session/new");

    let (turn, ask) = std::thread::scope(|scope| {
        let elsewhere = scope.spawn(|| {
            let ask = cx
                .await_approval(Duration::from_secs(90))
                .expect("the editor's ask reaches 'contenox approvals list'");
            cx.approve(&ask.id).ok();
            ask
        });
        let turn = acp
            .prompt(&session, "write a note")
            .expect("session/prompt");
        (turn, elsewhere.join().expect("the answering thread"))
    });

    assert_eq!(ask.kind, "permission");
    assert_eq!(ask.tool, "local_fs.write_file");
    assert!(
        turn.asked_permission(),
        "the same ask was offered to the editor too"
    );
    assert_eq!(
        turn.stop_reason, "end_turn",
        "answering elsewhere continues the turn in place"
    );
    assert_eq!(
        cx.read_file("notes.txt").expect("the note"),
        "answered elsewhere\n"
    );
}

#[test]
fn a_denied_call_is_refused_at_the_tool_boundary_and_the_model_is_told() {
    let cx = editor(
        "acp-denied",
        &Script::new()
            .route("coding")
            .turn(
                Turn::new().text("Writing the note.").call(
                    ToolCall::new("write_file")
                        .arg("path", "notes.txt")
                        .arg("content", "denied\n"),
                ),
            )
            .turn("I could not write it."),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    acp.answers(Verdict::Deny);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert!(turn.asked_permission(), "the write must be gated at all");
    assert!(
        !turn.methods().contains(&"fs/write_text_file"),
        "a denied write must never reach the client, got {:?}",
        turn.methods()
    );
    assert!(
        cx.read_file("notes.txt").is_err(),
        "a denied write must leave no file behind"
    );

    let reported: Vec<&Value> = turn
        .updates
        .iter()
        .filter(|update| update["status"] == json!("failed"))
        .collect();
    assert!(
        !reported.is_empty(),
        "the editor is shown the call as failed: {:#?}",
        turn.updates
    );
    assert!(
        turn.tool_outputs().contains("User denied the operation"),
        "the model is told why, got {:?}",
        turn.tool_outputs()
    );

    assert_eq!(
        turn.stop_reason, "end_turn",
        "a refusal ends the turn cleanly"
    );
    assert!(
        turn.text().ends_with("I could not write it."),
        "the model answers after being told, got {:?}",
        turn.text()
    );
}

// -------------------------------------------------------------- the workspace

#[test]
fn the_session_works_in_the_directory_the_editor_opened() {
    let cx = editor(
        "acp-cwd",
        &Script::new()
            .route("coding")
            .turns(write_note("in project\n")),
    );
    // The agent process is started in work/, the editor opens work/project.
    let project = cx.work().join("project");
    std::fs::create_dir_all(&project).expect("the project directory");

    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(&project).expect("session/new");

    let listed = acp.session_list().expect("session/list");
    assert_eq!(
        listed[0]["cwd"],
        json!(project.display().to_string()),
        "session/list reports the cwd the editor sent"
    );

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert_eq!(
        turn.written_paths(),
        vec![project.join("notes.txt").display().to_string()],
        "a relative path resolves against the editor's cwd, not the agent's"
    );
    assert!(
        project.join("notes.txt").is_file(),
        "the file lands in the project the editor has open"
    );
    assert!(
        !cx.work().join("notes.txt").exists(),
        "and not in the directory the agent process was started in"
    );
}

#[test]
fn a_client_that_grants_no_filesystem_is_served_none() {
    let cx = editor(
        "acp-no-fs",
        &Script::new()
            .route("coding")
            .turns(write_note("unreachable\n")),
    );
    let mut acp = cx.acp(["acp", "--auto"]).expect("spawn acp");
    acp.initialize_with(json!({})).expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert!(
        turn.tool_outputs().contains("write_file not found"),
        "with no fs capability the write tool is not served at all, got {}",
        turn.tool_outputs()
    );
    assert!(
        turn.methods().is_empty(),
        "nothing may be proxied to a client that granted nothing, got {:?}",
        turn.methods()
    );
    assert!(
        cx.read_file("notes.txt").is_err(),
        "and the agent must not fall back to the host filesystem"
    );
}

// ---------------------------------------------------------------- the envelopes

#[test]
fn acp_runs_under_the_default_envelope() {
    let cx = editor("acp-envelope", &Script::new().turn("hello"));
    let (mut acp, init) = handshake(&cx, &["acp"]);

    assert_eq!(
        picker(&init, "hitl-policy")["options"][0]["description"],
        json!("Use hitl-policy-default.json")
    );

    acp.close()
        .expect("the agent exits when its client hangs up");
}

#[test]
fn acpx_runs_under_the_hardened_acpx_envelope() {
    let cx = editor(
        "acpx-envelope",
        &Script::new().turns(write_note("hardened\n")),
    );
    let (mut acp, init) = handshake(&cx, &["acpx"]);

    assert_eq!(
        picker(&init, "hitl-policy")["options"][0]["description"],
        json!("Use hitl-policy-acpx.json")
    );

    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert!(
        !turn.asked_permission(),
        "the hardened envelope is allow/deny: an untrusted driver has nobody to ask"
    );
    let told = turn.tool_outputs();
    assert!(
        told.contains("Denied by the active policy hitl-policy-acpx.json"),
        "the refusal names the envelope that refused, got {told}"
    );
    assert!(
        told.contains("Do not retry it"),
        "and tells the model this is the envelope, not a transient error: {told}"
    );
    assert!(
        cx.read_file("notes.txt").is_err(),
        "the hardened profile denies writes"
    );
}

#[test]
fn hitl_policy_names_another_envelope_for_the_editor_profile() {
    let cx = editor(
        "acp-policy-flag",
        &Script::new()
            .route("coding")
            .turns(write_note("override\n")),
    );
    let (mut acp, init) = handshake(&cx, &["acp", "--hitl-policy", "acpx"]);

    assert_eq!(
        picker(&init, "hitl-policy")["options"][0]["description"],
        json!("Use hitl-policy-acpx.json"),
        "--hitl-policy replaces the profile's own envelope"
    );

    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-acpx.json"),
        "and the named envelope is the one enforced, got {}",
        turn.tool_outputs()
    );
    assert!(cx.read_file("notes.txt").is_err(), "the write is refused");
}

#[test]
fn auto_disables_the_editors_permission_prompt() {
    let cx = editor(
        "acp-auto",
        &Script::new()
            .route("coding")
            .turns(write_note("unattended\n")),
    );
    let (mut acp, _) = handshake(&cx, &["acp", "--auto"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp
        .prompt(&session, "write a note")
        .expect("session/prompt");

    assert!(
        !turn.asked_permission(),
        "--auto sends no session/request_permission"
    );
    assert_eq!(
        cx.read_file("notes.txt").expect("the note"),
        "unattended\n",
        "the gated tool runs unattended instead"
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and nothing is parked for anyone to answer"
    );
}

// -------------------------------------------------------------- the command menu

#[test]
fn the_editor_profile_offers_the_whole_command_menu() {
    let cx = editor("acp-commands", &Script::new().turn("hello"));
    let (mut acp, _) = handshake(&cx, &["acp"]);
    acp.new_session(cx.work()).expect("session/new");

    assert_eq!(
        acp.commands(),
        vec![
            "capability",
            "clear",
            "compact",
            "doctor",
            "help",
            "link",
            "max-tokens",
            "mission",
            "model",
            "new",
            "pair",
            "plan",
            "policy",
            "provider",
            "rename",
            "sessions",
            "think",
            "unpair",
        ]
    );
}

#[test]
fn the_hardened_profile_never_offers_mission() {
    let cx = editor("acpx-commands", &Script::new().turn("hello"));
    let (mut acp, _) = handshake(&cx, &["acpx"]);
    acp.new_session(cx.work()).expect("session/new");

    assert!(
        !acp.offers("mission"),
        "an untrusted driver may not fire missions: {:?}",
        acp.commands()
    );
    assert!(
        !acp.offers("plan"),
        "and /plan dispatches the same way: {:?}",
        acp.commands()
    );
    assert!(
        acp.offers("policy") && acp.offers("help"),
        "the rest of the menu is still served: {:?}",
        acp.commands()
    );
}

// ------------------------------------------------------------- the declaration tree

#[test]
fn the_editor_chain_is_a_router_that_reaches_the_review_leaf() {
    let cx = editor(
        "acp-route-review",
        &Script::new().route("review").turn("Nothing to flag."),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "review this").expect("session/prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let steps = cx.executed_tasks().expect("contenox state show");
    assert_eq!(steps[0].task, "chain-acp-route");
    assert_eq!(steps[0].handler, "route");
    assert_eq!(
        steps[0].transition, "review",
        "the classifier's label is the branch it takes"
    );
    assert_eq!(steps[1].task, "chain-acp-review-agent");
}

#[test]
fn the_editor_chain_routes_a_change_request_to_the_coding_leaf() {
    let cx = editor(
        "acp-route-coding",
        &Script::new().route("coding").turns(write_note("routed\n")),
    );
    let (mut acp, _) = handshake(&cx, &["acp", "--auto"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "change this").expect("session/prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let tasks: Vec<String> = cx
        .executed_tasks()
        .expect("contenox state show")
        .iter()
        .map(|step| step.task.clone())
        .collect();
    assert_eq!(tasks[0], "chain-acp-route");
    assert!(
        tasks.contains(&"chain-acp-coding-agent".to_string())
            && tasks.contains(&"chain-acp-coding-tools".to_string()),
        "the coding leaf is its own loop — an agent turn and the tools it asked for, got {tasks:?}"
    );
}

#[test]
#[ignore = "confirmed defect: the review leaf's `posture: read_only` never reaches the compiled chain. \
`.generated/chain-agent-acp.json` gives chain-acp-review-agent and chain-acp-review-tools `tools: [\"*\", \"mission\"]` \
and no hide_tools, so the write runs. Seam: agentdecl emits HideTools from ir.Tools.Deny only (emit.go), \
while a posture is turned into an envelope — and an ACP session runs one envelope for the whole process, \
not one per leaf. docs/guide/chains/routing.md and preseed/agents/acp/review/agent.md both promise the withholding."]
fn the_review_leaf_withholds_the_write_tools() {
    let cx = editor(
        "acp-review-readonly",
        &Script::new()
            .route("review")
            .turns(write_note("a reviewer must not write\n")),
    );
    // --auto removes the approval gate, leaving only the loop's own tool scope.
    let (mut acp, _) = handshake(&cx, &["acp", "--auto"]);
    let session = acp.new_session(cx.work()).expect("session/new");

    let turn = acp.prompt(&session, "review this").expect("session/prompt");

    assert!(
        !turn.methods().contains(&"fs/write_text_file"),
        "the read-only review loop must not reach the client's writer, got {:?}",
        turn.methods()
    );
    assert!(
        cx.read_file("notes.txt").is_err(),
        "and must leave no file behind"
    );
}

#[test]
fn an_unrecognised_label_lands_on_the_general_leaf() {
    let cx = editor(
        "acp-route-default",
        &Script::new()
            .route("something the router has never heard of")
            .turn("Answering anyway."),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "hello").expect("session/prompt");
    assert_eq!(turn.text(), "Answering anyway.");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let steps = cx.executed_tasks().expect("contenox state show");
    assert_eq!(steps[0].task, "chain-acp-route");
    assert_eq!(
        steps[1].task, "chain-acp-general-agent",
        "the default branch is the least powerful leaf, not the most"
    );
}

// ------------------------------------------------------------ chain resolution

/// The acpx chain is flat where the acp chain is a router, so serving it under
/// the acp profile is visible in what actually ran.
fn plant_flat_chain(cx: &Instance, at: std::path::PathBuf) -> std::path::PathBuf {
    let source = cx.home_file(".generated/chain-agent-acpx.json");
    if let Some(parent) = at.parent() {
        std::fs::create_dir_all(parent).expect("the destination directory");
    }
    std::fs::copy(&source, &at)
        .unwrap_or_else(|err| panic!("copy {} to {}: {err}", source.display(), at.display()));
    at
}

fn ran_the_flat_chain(cx: &Instance) {
    let steps = cx.executed_tasks().expect("contenox state show");
    assert_eq!(
        steps[0].task, "chain-acpx-agent",
        "the planted chain is the one that ran, got {steps:?}"
    );
}

#[test]
fn the_editor_profile_reads_the_chain_named_by_contenox_acp_chain_path() {
    let cx = editor(
        "acp-chain-env",
        &Script::new().turn("From the named chain."),
    );
    let planted = plant_flat_chain(&cx, cx.work().join("elsewhere/chain.json"));

    let mut acp =
        Acp::spawn(cx.cmd(["acp"]).env("CONTENOX_ACP_CHAIN_PATH", &planted)).expect("spawn acp");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "hello").expect("session/prompt");
    assert_eq!(turn.text(), "From the named chain.");
    acp.close()
        .expect("the agent exits when its client hangs up");

    ran_the_flat_chain(&cx);
}

#[test]
fn the_headless_profile_reads_the_chain_named_by_contenox_acpx_chain_path() {
    let cx = editor(
        "acpx-chain-env",
        &Script::new().route("general").turn("Routed."),
    );
    // The acp chain is the router, planted where the headless profile reads.
    let planted = cx.work().join("elsewhere/acpx.json");
    std::fs::create_dir_all(planted.parent().unwrap()).expect("the destination directory");
    std::fs::copy(cx.home_file(".generated/chain-agent-acp.json"), &planted)
        .expect("copy the router chain");

    let mut acp =
        Acp::spawn(cx.cmd(["acpx"]).env("CONTENOX_ACPX_CHAIN_PATH", &planted)).expect("spawn acpx");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "hello").expect("session/prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let steps = cx.executed_tasks().expect("contenox state show");
    assert_eq!(
        steps[0].task, "chain-acp-route",
        "acpx read the chain its own variable named, got {steps:?}"
    );
}

#[test]
fn an_operator_copy_outranks_the_generated_chain() {
    let cx = editor(
        "acp-chain-operator",
        &Script::new().turn("From the operator copy."),
    );
    plant_flat_chain(&cx, cx.home_file("chain-agent-acp.json"));

    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "hello").expect("session/prompt");
    assert_eq!(turn.text(), "From the operator copy.");
    acp.close()
        .expect("the agent exits when its client hangs up");

    ran_the_flat_chain(&cx);
}

#[test]
fn the_system_copy_serves_when_nothing_nearer_exists() {
    let cx = editor(
        "acp-chain-system",
        &Script::new().turn("From the system copy."),
    );
    plant_flat_chain(&cx, cx.home_file("system/chain-agent-acp.json"));
    std::fs::remove_file(cx.home_file(".generated/chain-agent-acp.json"))
        .expect("drop the compiled copy");

    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "hello").expect("session/prompt");
    assert_eq!(turn.text(), "From the system copy.");
    acp.close()
        .expect("the agent exits when its client hangs up");

    ran_the_flat_chain(&cx);
}

#[test]
fn a_named_chain_path_that_is_missing_is_a_hard_error() {
    let cx = editor("acp-chain-missing", &Script::new().turn("never reached"));

    cx.cmd(["acp"])
        .env("CONTENOX_ACP_CHAIN_PATH", cx.work().join("nowhere.json"))
        .stdin("")
        .timeout(Duration::from_secs(60))
        .output()
        .expect("contenox acp")
        .expect_code(1)
        .expect_stderr("nowhere.json\" not found")
        .expect_stderr("CONTENOX_ACP_CHAIN_PATH");
}

// -------------------------------------------------------------- what it leaves

#[test]
fn an_editor_session_transcript_is_readable_from_the_cli_afterwards() {
    let cx = editor(
        "acp-transcript",
        &Script::new().route("general").turn("Two files changed."),
    );
    let (mut acp, _) = handshake(&cx, &["acp"]);
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "what changed?")
        .expect("session/prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let rows = cx.sessions_all().expect("contenox session list --all");
    let row = rows
        .iter()
        .find(|row| row.name == session)
        .unwrap_or_else(|| panic!("the editor's session is missing from {rows:?}"));
    assert_eq!(row.identity, "acp-client");
    assert_eq!(row.messages, "2");

    let shown = cx.session_show(&row.id).ok();
    assert!(
        shown.stdout_has("what changed?") && shown.stdout_has("Two files changed."),
        "the whole exchange is readable back:\n{}",
        shown.render()
    );
}

// -------------------------------------------- autocomplete: the other editor wire
//
// `contenox autocomplete --stdio` is the second thing an editor can attach to:
// fill-in-the-middle completions over JSON lines, for a client that wants them
// without a full ACP session. It is not ACP, but it is the same audience, and
// the reference documents it beside `contenox acp`.

/// The whole point of the JSON-lines shape: an editor learns about a
/// misconfiguration *on the protocol*, in the response to the request it made,
/// rather than by watching the process it spawned disappear. So a host with no
/// autocomplete model configured still starts, still exits 0, and still answers
/// — with an error object naming the config key that fixes it.
#[test]
fn autocomplete_with_no_model_answers_on_the_protocol_instead_of_refusing_to_start() {
    let cx = Instance::named("autocomplete-unconfigured").expect("scratch instance");
    cx.init().ok();

    let out = cx
        .cmd(["autocomplete", "--stdio"])
        .stdin("{\"id\":\"1\",\"path\":\"a.go\",\"language\":\"go\",\"prefix\":\"func \",\"suffix\":\"}\"}\n")
        .timeout(Duration::from_secs(60))
        .output()
        .expect("contenox autocomplete --stdio")
        .expect_code(0)
        .expect_stderr("no autocomplete model is configured");

    let reply: Value = serde_json::from_str(out.stdout_trimmed())
        .unwrap_or_else(|err| panic!("one JSON response per line ({err}):\n{}", out.render()));
    assert_eq!(reply["id"], json!("1"), "the response is matched by id");
    assert!(
        reply["completion"].is_null(),
        "an unconfigured host completes nothing: {reply:#}"
    );
    assert!(
        reply["error"]
            .as_str()
            .unwrap_or_default()
            .contains("contenox config set default-autocomplete-model"),
        "the error names the command that fixes it: {reply:#}"
    );
}

/// Configured, it serves the model named by the autocomplete role — a role of
/// its own, separate from the chat default, so an editor's completions and its
/// conversation can run on different models.
#[test]
fn autocomplete_serves_the_completion_from_the_model_the_role_names() {
    let cx = Instance::named("autocomplete-configured").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn("fmt.Println(\"hello\")"))
        .expect("scripted-test backend");
    cx.run([
        "config",
        "set",
        "default-autocomplete-model",
        "scripted-test",
    ])
    .ok();
    cx.run([
        "config",
        "set",
        "default-autocomplete-provider",
        "scripted-test",
    ])
    .ok();

    let out = cx
        .cmd(["autocomplete", "--stdio"])
        .stdin("{\"id\":\"7\",\"path\":\"a.go\",\"language\":\"go\",\"prefix\":\"func main() {\\n\\t\",\"suffix\":\"\\n}\"}\n")
        .timeout(Duration::from_secs(120))
        .output()
        .expect("contenox autocomplete --stdio")
        .expect_code(0)
        .expect_stderr("model scripted-test");

    let reply: Value = serde_json::from_str(out.stdout_trimmed())
        .unwrap_or_else(|err| panic!("one JSON response per line ({err}):\n{}", out.render()));
    assert_eq!(reply["id"], json!("7"));
    assert_eq!(
        reply["completion"],
        json!("fmt.Println(\"hello\")"),
        "the turn the dialog served is the completion the editor gets: {reply:#}"
    );
}
