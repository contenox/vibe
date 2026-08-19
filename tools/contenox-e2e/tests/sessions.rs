//! `contenox session`, and the one workspace an instance serves.
//!
//! Sessions are read back through the product's own commands — `session list`,
//! `session show`, `session list --all`, `session workspaces`. The single file
//! a case opens directly is `.contenox/workspace.id`, because that file *is*
//! one of the promises: a stable UUID that travels with the directory.

use contenox_e2e::{Instance, Pty, Script, SessionRow, Table, ToolCall, Turn, WorkspaceSessionRow};
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// Anything that builds an engine: a chat turn, a fork that summarises, a run.
const SLOW: Duration = Duration::from_secs(180);

/// A scratch instance whose model answers `answer` whenever it is asked.
///
/// Every command is its own process and the scripted backend replays from the
/// top, so a single turn serves any number of invocations.
fn instance(label: &str, answer: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn(answer))
        .expect("scripted-test backend");
    cx
}

/// The dialog an unattended run needs to land: file a result, finish, answer
/// the loop's last question.
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

/// Point the scripted backend at a different dialog for the next process.
fn rescript(cx: &Instance, script: &Script) {
    cx.write_script("scripted.json", script)
        .expect("rewrite the scripted dialog");
}

/// A marked project of its own, whatever the surrounding directories carry:
/// `--project` pins the marker to this directory instead of walking up to a
/// parent's.
fn marked_project(cx: &Instance, name: &str) -> PathBuf {
    let dir = cx.work().join(name);
    std::fs::create_dir_all(&dir).expect("create the project directory");
    cx.cmd(["init", "--project"])
        .cwd(&dir)
        .output()
        .expect("contenox init --project")
        .ok();
    dir
}

/// The workspace UUID a directory carries — the scoping token every session
/// under the project is filed under.
fn marker_id(project: &Path) -> String {
    let path = project.join(".contenox").join("workspace.id");
    let raw = std::fs::read_to_string(&path)
        .unwrap_or_else(|err| panic!("no marker at {}: {err}", path.display()));
    serde_json::from_str::<Value>(&raw)
        .ok()
        .and_then(|value| value.get("id").and_then(Value::as_str).map(str::to_string))
        .unwrap_or_else(|| raw.trim().to_string())
}

/// Hold one turn of conversation in the active session of `dir`.
fn chat(cx: &Instance, dir: &Path, message: &str) {
    cx.cmd(["chat", message])
        .cwd(dir)
        .timeout(SLOW)
        .output()
        .expect("contenox chat")
        .ok();
}

fn names(rows: &[SessionRow]) -> Vec<&str> {
    let mut out: Vec<&str> = rows.iter().map(|row| row.name.as_str()).collect();
    out.sort_unstable();
    out
}

fn active(rows: &[SessionRow]) -> Option<&str> {
    rows.iter()
        .find(|row| row.current)
        .map(|row| row.name.as_str())
}

fn messages_of(rows: &[SessionRow], name: &str) -> u32 {
    rows.iter()
        .find(|row| row.name == name)
        .unwrap_or_else(|| panic!("no session named {name:?} in {rows:?}"))
        .messages
}

fn row_named<'a>(rows: &'a [WorkspaceSessionRow], name: &str) -> &'a WorkspaceSessionRow {
    rows.iter()
        .find(|row| row.name == name)
        .unwrap_or_else(|| panic!("no session named {name:?} in {rows:?}"))
}

/// The one session an instance recorded that a unit ran under.
fn only_unit_session(rows: &[WorkspaceSessionRow]) -> &WorkspaceSessionRow {
    let units: Vec<&WorkspaceSessionRow> = rows
        .iter()
        .filter(|row| row.identity == "acp-client")
        .collect();
    match units.as_slice() {
        [only] => only,
        other => panic!("expected exactly one unit session, got {other:?}"),
    }
}

// =========================================================== sessions persist

#[test]
fn list_says_how_to_start_when_no_session_exists_yet() {
    let cx = Instance::named("session-empty").expect("scratch instance");
    cx.init().ok();

    cx.run(["session", "list"])
        .ok()
        .expect_stdout("No sessions yet. Run: contenox session new");
    assert!(
        cx.sessions().expect("session list").is_empty(),
        "an empty inventory is a value, not an error"
    );
}

#[test]
fn a_new_session_becomes_the_active_one_and_the_star_marks_it() {
    let cx = Instance::named("session-new").expect("scratch instance");
    cx.init().ok();

    cx.run(["session", "new", "api"])
        .ok()
        .expect_stdout("Created session \"api\". Now active.");
    cx.run(["session", "new", "docs"]).ok();

    let rows = cx.sessions().expect("session list");
    assert_eq!(names(&rows), vec!["api", "docs"], "both sessions persist");
    assert_eq!(
        active(&rows),
        Some("docs"),
        "the star marks the session just created: {rows:?}"
    );
}

#[test]
fn switch_moves_the_star_to_the_session_it_names() {
    let cx = Instance::named("session-switch").expect("scratch instance");
    cx.init().ok();
    cx.run(["session", "new", "api"]).ok();
    cx.run(["session", "new", "docs"]).ok();

    cx.run(["session", "switch", "api"])
        .ok()
        .expect_stdout("Switched to session \"api\".");

    let rows = cx.sessions().expect("session list");
    assert_eq!(active(&rows), Some("api"), "{rows:?}");

    cx.run(["session", "switch", "no-such-session"])
        .expect_failure()
        .expect_stderr("contenox session list");
}

#[test]
fn each_session_keeps_a_conversation_of_its_own_across_processes() {
    let cx = instance("session-threads", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "how does the api authenticate?");
    cx.run(["session", "new", "docs"]).ok();
    chat(&cx, &work, "what is missing from the guide?");

    let rows = cx.sessions().expect("session list");
    assert_eq!(messages_of(&rows, "api"), 2);
    assert_eq!(messages_of(&rows, "docs"), 2);

    cx.run(["session", "show", "api"])
        .ok()
        .expect_stdout("how does the api authenticate?")
        .refute_stdout("what is missing from the guide?");
}

#[test]
fn delete_takes_the_session_and_its_messages_with_it() {
    let cx = instance("session-delete", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "how does the api authenticate?");
    cx.run(["session", "new", "docs"]).ok();

    cx.run(["session", "delete", "api"])
        .ok()
        .expect_stdout("Deleted session \"api\".");

    assert_eq!(names(&cx.sessions().expect("session list")), vec!["docs"]);
    cx.run(["session", "show", "api"])
        .expect_failure()
        .expect_stderr("not found");
}

#[test]
fn deleting_the_active_session_leaves_none_active() {
    let cx = Instance::named("session-delete-active").expect("scratch instance");
    cx.init().ok();
    cx.run(["session", "new", "api"]).ok();

    cx.run(["session", "delete", "api"])
        .ok()
        .expect_stdout("was active");

    cx.run(["session", "show"])
        .expect_failure()
        .expect_stderr("no active session");
}

// ============================================================== session show

#[test]
fn head_and_tail_take_the_two_ends_of_the_conversation() {
    let cx = instance("session-head-tail", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "first question");
    chat(&cx, &work, "second question");
    chat(&cx, &work, "third question");

    cx.run(["session", "show", "api", "--head", "2"])
        .ok()
        .expect_stdout("(2/6 messages)")
        .expect_stdout("first question")
        .refute_stdout("third question");

    cx.run(["session", "show", "api", "--tail", "2"])
        .ok()
        .expect_stdout("(2/6 messages)")
        .expect_stdout("third question")
        .refute_stdout("first question");
}

#[test]
fn show_takes_a_session_id_from_a_workspace_a_name_cannot_reach() {
    let cx = instance("session-cross-workspace", "noted");
    let api = marked_project(&cx, "api");
    let web = marked_project(&cx, "web");

    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();
    chat(&cx, &api, "how does the api authenticate?");

    let all = cx.sessions_all().expect("session list --all");
    let id = row_named(&all, "api-thread").id.clone();

    // Standing in the other project the name is out of scope …
    cx.cmd(["session", "show", "api-thread"])
        .cwd(&web)
        .output()
        .expect("session show by name")
        .expect_failure()
        .expect_stderr("not found");

    // … and the id still reaches it, whatever workspace it belongs to.
    cx.cmd(["session", "show", id.as_str()])
        .cwd(&web)
        .output()
        .expect("session show by id")
        .ok()
        .expect_stdout("how does the api authenticate?");
}

// ============================================================== session fork

#[test]
fn fork_copies_the_conversation_and_the_original_does_not_follow_the_branch() {
    let cx = instance("session-fork", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "how does the api authenticate?");
    chat(&cx, &work, "and what refreshes the token?");

    cx.run(["session", "fork", "alternative"])
        .ok()
        .expect_stdout("Forked 4 messages to session \"alternative\". Now active.");

    let rows = cx.sessions().expect("session list");
    assert_eq!(active(&rows), Some("alternative"));
    assert_eq!(messages_of(&rows, "api"), 4);
    assert_eq!(messages_of(&rows, "alternative"), 4);

    chat(&cx, &work, "what happens when it expires?");

    let rows = cx.sessions().expect("session list");
    assert_eq!(
        messages_of(&rows, "alternative"),
        6,
        "the branch carries on"
    );
    assert_eq!(
        messages_of(&rows, "api"),
        4,
        "the original is left where it was"
    );
    cx.run(["session", "show", "api"])
        .ok()
        .refute_stdout("what happens when it expires?");
}

#[test]
fn fork_summary_folds_the_older_turns_and_keeps_the_named_number_verbatim() {
    let cx = instance("session-fork-summary", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "first question");
    chat(&cx, &work, "second question");
    chat(&cx, &work, "third question");

    // The compaction is its own process, so it reads its own dialog.
    rescript(&cx, &Script::new().turn("EARLIER TURNS FOLDED UP"));

    cx.cmd(["session", "fork", "compacted", "--summary", "--keep", "2"])
        .timeout(SLOW)
        .output()
        .expect("session fork --summary")
        .ok()
        .expect_stdout("Forked 6 messages to session \"compacted\" (compacted to 3).");

    cx.run(["session", "show", "compacted"])
        .ok()
        .expect_stdout("(3/3 messages)")
        .expect_stdout("EARLIER TURNS FOLDED UP")
        .expect_stdout("third question")
        .refute_stdout("first question");

    cx.run(["session", "show", "api"])
        .ok()
        .expect_stdout("(6/6 messages)")
        .expect_stdout("first question");
}

#[test]
fn fork_summary_keeps_eight_messages_verbatim_by_default() {
    let cx = instance("session-fork-keep-default", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "first question");
    chat(&cx, &work, "second question");
    chat(&cx, &work, "third question");

    // Six messages is fewer than the default keep, so there is nothing older
    // left to fold — and the refusal names the number it applied.
    cx.cmd(["session", "fork", "compacted", "--summary"])
        .timeout(SLOW)
        .output()
        .expect("session fork --summary")
        .expect_failure()
        .expect_stderr("keep=8");

    assert_eq!(
        names(&cx.sessions().expect("session list")),
        vec!["api"],
        "a refused fork starts no session"
    );
}

#[test]
fn fork_summary_compacts_with_the_workspaces_own_compact_chain() {
    let cx = instance("session-fork-chain", "noted");
    let work = cx.work().to_path_buf();

    cx.run(["session", "new", "api"]).ok();
    chat(&cx, &work, "first question");
    chat(&cx, &work, "second question");
    chat(&cx, &work, "third question");

    // A copy in the workspace outranks the shipped one, so a broken copy is
    // what the compaction trips over — and names.
    let planted = cx
        .write_file(".contenox/chain-compact-default.json", "{ not a chain")
        .expect("plant the workspace copy");

    let out = cx
        .cmd(["session", "fork", "compacted", "--summary", "--keep", "2"])
        .timeout(SLOW)
        .output()
        .expect("session fork --summary")
        .expect_failure();

    assert!(
        out.stderr_has(&planted.display().to_string()),
        "the workspace's own chain-compact-default.json is the one read:\n{}",
        out.render()
    );
}

// ======================================================= inspecting the whole db

#[test]
fn a_plain_list_stays_in_this_workspace_while_list_all_reaches_the_database() {
    let cx = instance("session-scope", "noted");
    let api = marked_project(&cx, "api");
    let web = marked_project(&cx, "web");

    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();
    cx.cmd(["session", "new", "web-thread"])
        .cwd(&web)
        .output()
        .expect("session new")
        .ok();

    assert_eq!(
        names(&cx.sessions_in(&api).expect("session list")),
        vec!["api-thread"],
        "one instance serves exactly one workspace"
    );
    assert_eq!(
        names(&cx.sessions_in(&web).expect("session list")),
        vec!["web-thread"]
    );

    let all = cx.sessions_all().expect("session list --all");
    assert_eq!(row_named(&all, "api-thread").workspace, marker_id(&api));
    assert_eq!(row_named(&all, "web-thread").workspace, marker_id(&web));
}

#[test]
fn list_filters_the_whole_database_by_workspace_and_by_namespace() {
    let cx = instance("session-filters", "noted");
    let api = marked_project(&cx, "api");
    let web = marked_project(&cx, "web");

    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();
    cx.cmd(["session", "new", "web-thread"])
        .cwd(&web)
        .output()
        .expect("session new")
        .ok();

    cx.run(["session", "list", "--workspace", marker_id(&api).as_str()])
        .ok()
        .expect_stdout("api-thread")
        .refute_stdout("web-thread");

    cx.run(["session", "list", "--namespace", "web-thread"])
        .ok()
        .expect_stdout("web-thread")
        .refute_stdout("api-thread");

    cx.run([
        "session",
        "list",
        "--workspace",
        "00000000-0000-0000-0000-999999999999",
    ])
    .ok()
    .expect_stdout("No matching sessions.");
}

#[test]
fn workspaces_counts_the_sessions_and_messages_of_every_workspace() {
    let cx = instance("session-workspaces", "noted");
    let api = marked_project(&cx, "api");

    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();
    chat(&cx, &api, "how does the api authenticate?");
    cx.run(["session", "new", "work-thread"]).ok();

    let out = cx.run(["session", "workspaces"]).ok();
    let table = Table::parse(
        &out.stdout,
        &["WORKSPACE", "NAMESPACE", "IDENTITY", "SESSIONS", "MESSAGES"],
    )
    .expect("the workspaces table");

    let mine = table
        .rows
        .iter()
        .find(|row| row.get("NAMESPACE") == "api-thread")
        .unwrap_or_else(|| panic!("no api-thread namespace in\n{}", out.stdout));
    assert_eq!(mine.get("WORKSPACE"), marker_id(&api));
    assert_eq!(mine.get("IDENTITY"), "local-user");
    assert_eq!(mine.get("SESSIONS"), "1");
    assert_eq!(mine.get("MESSAGES"), "2");

    let workspaces: Vec<&str> = table.rows.iter().map(|row| row.get("WORKSPACE")).collect();
    assert!(
        workspaces
            .iter()
            .filter(|ws| **ws != marker_id(&api))
            .count()
            >= 1,
        "the other project's workspace is listed beside it: {workspaces:?}"
    );
}

#[test]
fn a_namespace_is_the_session_name_prefix_before_its_generated_id() {
    let cx = Instance::named("session-namespace").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&lands_reporting("namespace probe"))
        .expect("scripted-test backend");

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .timeout(SLOW)
        .output()
        .expect("contenox run")
        .ok();

    let all = cx.sessions_all().expect("session list --all");
    let unit = only_unit_session(&all).clone();

    let out = cx.run(["session", "workspaces"]).ok();
    let table = Table::parse(
        &out.stdout,
        &["WORKSPACE", "NAMESPACE", "IDENTITY", "SESSIONS", "MESSAGES"],
    )
    .expect("the workspaces table");
    let namespace = table
        .rows
        .iter()
        .find(|row| row.get("IDENTITY") == "acp-client")
        .map(|row| row.get("NAMESPACE").to_string())
        .unwrap_or_else(|| panic!("no unit namespace in\n{}", out.stdout));

    let tail = unit
        .name
        .strip_prefix(&format!("{namespace}-"))
        .unwrap_or_else(|| panic!("{:?} is not {namespace:?} plus an id", unit.name));
    assert!(
        !tail.is_empty() && tail.chars().all(|c| c.is_ascii_hexdigit() || c == '-'),
        "what follows the namespace is the generated id, not more name: {tail:?}"
    );

    cx.run(["session", "list", "--namespace", namespace.as_str()])
        .ok()
        .expect_stdout(&unit.name);
}

// ========================================================= workspace authority

#[test]
fn the_workspace_is_the_marked_project_the_launch_directory_belongs_to() {
    let cx = instance("workspace-walk-up", "noted");
    let api = marked_project(&cx, "api");
    let deep = api.join("services").join("billing");
    std::fs::create_dir_all(&deep).expect("create the nested directory");

    cx.cmd(["session", "new", "api-thread"])
        .cwd(&deep)
        .output()
        .expect("session new")
        .ok();

    assert_eq!(
        names(&cx.sessions_in(&api).expect("session list")),
        vec!["api-thread"],
        "a session started deep inside the project is the project's"
    );
    assert_eq!(
        row_named(
            &cx.sessions_all().expect("session list --all"),
            "api-thread"
        )
        .workspace,
        marker_id(&api)
    );
    assert!(
        !deep.join(".contenox").exists(),
        "and no second workspace is minted on the way"
    );
}

#[test]
fn the_workspace_marker_travels_with_the_directory() {
    let cx = instance("workspace-travels", "noted");
    let api = marked_project(&cx, "api");
    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();
    let before = marker_id(&api);

    let moved = cx.root().join("relocated");
    std::fs::rename(&api, &moved).expect("move the project somewhere else");

    assert_eq!(
        marker_id(&moved),
        before,
        "the id belongs to the directory, not to its path"
    );
    assert_eq!(
        names(&cx.sessions_in(&moved).expect("session list")),
        vec!["api-thread"],
        "so the project's sessions are still the project's after the move"
    );
}

#[test]
fn a_run_and_the_mission_it_fires_are_filed_under_the_directory_it_started_in() {
    let cx = Instance::named("workspace-run").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&lands_reporting("run reporting home"))
        .expect("scripted-test backend");
    let api = marked_project(&cx, "api");

    cx.cmd(["run", "--policy", "run", "report what you know"])
        .cwd(&api)
        .timeout(SLOW)
        .output()
        .expect("contenox run")
        .ok()
        .expect_stdout("run reporting home");

    let all = cx.sessions_all().expect("session list --all");
    assert_eq!(
        only_unit_session(&all).workspace,
        marker_id(&api),
        "a mission inherits the workspace of the instance that fired it"
    );

    let missions = cx.missions().expect("mission list");
    assert_eq!(missions[0].status, "landed");
}

#[test]
#[ignore = "confirmed defect: beam files its session under the shared DefaultWorkspaceID instead of the marker of the project it was launched in, so the session an operator opened in a project is invisible to that project's own `contenox session list`. Seam: runACPProfile (acp_cmd.go) resolves workspaceID from globalContenoxDir() — ~/.contenox, which contenox init never marks — instead of the launch directory or the session cwd, and acpsvc NewSession takes that as t.workspaceID() without ever consulting req.Cwd. One seam, every session-holding surface: beam, acp, acpx and serve."]
fn beam_files_its_session_under_the_project_it_was_launched_in() {
    let cx = instance("workspace-beam", "nothing is asked of the model here");
    let api = marked_project(&cx, "api");

    let mut pty = Pty::spawn(cx.cmd(["beam", "--plain"]).cwd(&api)).expect("beam under a pty");
    pty.wait_for("type / for commands", Duration::from_secs(90))
        .expect("beam's composer");
    pty.interrupt();
    pty.wait_exit(Duration::from_secs(90)).expect("beam leaves");

    let all = cx.sessions_all().expect("session list --all");
    assert_eq!(all.len(), 1, "beam opened exactly one session: {all:?}");
    assert_eq!(
        all[0].workspace,
        marker_id(&api),
        "the attended shape works in the directory it was started in, like every other"
    );
}

#[test]
fn the_envelope_a_run_carries_does_not_move_it_out_of_its_workspace() {
    let cx = Instance::named("workspace-policy").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&lands_reporting("policy makes no difference here"))
        .expect("scripted-test backend");
    let api = marked_project(&cx, "api");

    for policy in ["run", "read_only", "strict"] {
        cx.cmd(["run", "--policy", policy, "report what you know"])
            .cwd(&api)
            .timeout(SLOW)
            .output()
            .expect("contenox run")
            .ok();
    }

    let all = cx.sessions_all().expect("session list --all");
    let units: Vec<&WorkspaceSessionRow> = all
        .iter()
        .filter(|row| row.identity == "acp-client")
        .collect();
    assert_eq!(units.len(), 3, "one unit session per run: {all:?}");
    for unit in units {
        assert_eq!(
            unit.workspace,
            marker_id(&api),
            "no envelope widens or narrows where a session may run"
        );
    }
}

#[test]
fn the_control_plane_directory_is_not_a_workspace_of_its_own() {
    let cx = instance("workspace-control-plane", "noted");
    let api = marked_project(&cx, "api");
    cx.cmd(["session", "new", "api-thread"])
        .cwd(&api)
        .output()
        .expect("session new")
        .ok();

    let control_plane = api.join(".contenox");
    assert_eq!(
        names(&cx.sessions_in(&control_plane).expect("session list")),
        vec!["api-thread"],
        "standing in the control plane is still standing in the project it configures"
    );
    assert!(
        !control_plane.join(".contenox").exists(),
        "nothing mints a workspace inside the control plane"
    );
    assert_eq!(
        cx.sessions_all().expect("session list --all").len(),
        1,
        "and no second workspace appeared"
    );
}

#[test]
fn the_cli_offers_no_verb_that_selects_a_workspace() {
    let cx = Instance::named("workspace-no-verb").expect("scratch instance");
    cx.init().ok();

    let help = cx.run(["--help"]).ok();
    let verbs: Vec<&str> = help
        .stdout
        .lines()
        .skip_while(|line| !line.starts_with("Available Commands:"))
        .skip(1)
        .take_while(|line| line.starts_with("  "))
        .filter_map(|line| line.split_whitespace().next())
        .collect();

    assert!(verbs.contains(&"session"), "sanity check: {verbs:?}");
    assert!(
        !verbs.contains(&"workspace"),
        "a workspace is fixed at launch, so there is nothing to select: {verbs:?}"
    );

    let refused = cx.run(["workspace", "list"]).expect_failure();
    assert!(
        refused.stderr_has("unknown command") || refused.stdout_has("unknown command"),
        "and the verb does not exist:\n{}",
        refused.render()
    );
}

#[test]
fn an_editor_is_offered_no_workspace_picker() {
    let cx = instance("workspace-no-picker", "nothing is asked of the model here");

    let mut acp = cx.acp(["acp"]).expect("spawn contenox acp");
    let init = acp.initialize().expect("initialize");
    let options: Vec<String> = init["_meta"]["contenox.workspaceConfigOptions"]
        .as_array()
        .unwrap_or_else(|| panic!("initialize advertises no config options: {init:#}"))
        .iter()
        .filter_map(|option| option["id"].as_str().map(str::to_string))
        .collect();

    assert!(
        options.iter().any(|id| id == "model"),
        "sanity check: {options:?}"
    );
    for id in &options {
        assert!(
            !id.contains("workspace") && !id.contains("cwd") && !id.contains("root"),
            "the editor may change what the session runs as, never where it runs: {options:?}"
        );
    }

    acp.close()
        .expect("the agent exits when its client hangs up");
}

#[test]
#[ignore = "confirmed defect: an editor session is filed under the shared DefaultWorkspaceID instead of the marker of the project the editor opened, so the operator's own `contenox session list --workspace <marker>` cannot see it. Seam: runACPProfile (acp_cmd.go) resolves workspaceID from globalContenoxDir() — ~/.contenox, which contenox init never marks — instead of the launch directory or the session cwd, and acpsvc NewSession takes that as t.workspaceID() without ever consulting req.Cwd. One seam, every session-holding surface: beam, acp, acpx and serve."]
fn an_editor_session_is_filed_under_the_marked_project_the_editor_opened() {
    let cx = instance("workspace-editor", "nothing is asked of the model here");
    let api = marked_project(&cx, "api");
    let marker = marker_id(&api);

    let mut acp = cx.acp(["acp"]).expect("spawn contenox acp");
    acp.initialize().expect("initialize");
    let session = acp.new_session(&api).expect("session/new");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let all = cx.sessions_all().expect("session list --all");
    assert_eq!(
        row_named(&all, &session).workspace,
        marker,
        "the marker is the token EVERY session under the project is filed under — \
         the CLI's sessions and the editor's alike"
    );

    cx.run(["session", "list", "--workspace", marker.as_str()])
        .ok()
        .expect_stdout(&session);
}

#[test]
#[ignore = "confirmed defect: a directory with no .contenox/workspace.id is not a workspace of its own — every unmarked directory on the machine shares the single DefaultWorkspaceID, so two unrelated directories show each other's sessions and share one active session, against the documented rule that a beam or run works in the directory it was started in. Seam: contenoxcli.ResolveWorkspaceID falling back to DefaultWorkspaceID when the marker is absent, and neither beam nor run writing one."]
fn two_unmarked_directories_are_not_one_shared_workspace() {
    let cx = Instance::named("workspace-unmarked").expect("scratch instance");
    cx.init().ok();

    // Outside work/, so the marker `init` wrote there cannot stand in for one.
    let one = cx.root().join("plain-one");
    let two = cx.root().join("plain-two");
    for dir in [&one, &two] {
        std::fs::create_dir_all(dir).expect("create the unmarked directory");
    }

    cx.cmd(["session", "new", "one-thread"])
        .cwd(&one)
        .output()
        .expect("session new")
        .ok();
    cx.cmd(["session", "new", "two-thread"])
        .cwd(&two)
        .output()
        .expect("session new")
        .ok();

    assert_eq!(
        names(&cx.sessions_in(&one).expect("session list")),
        vec!["one-thread"],
        "a directory's sessions are its own"
    );
    assert_eq!(
        names(&cx.sessions_in(&two).expect("session list")),
        vec!["two-thread"]
    );
}
