use contenox_e2e::{Capabilities, Instance, Script, Table, ToolCall, Turn};
use serde_json::json;
use std::time::Duration;

#[test]
fn a_typed_script_serialises_to_the_json_the_backend_reads() {
    let script = Script::new()
        .model("scripted-test")
        .context_length(32768)
        .capabilities(Capabilities::new().think(false))
        .route("general")
        .turn(
            Turn::new()
                .text("Let me look at what changed.")
                .call(ToolCall::new("git_diff").arg("path", ".")),
        )
        .turn("Two files changed: README.md and main.go.");

    let emitted: serde_json::Value =
        serde_json::from_str(&script.to_json()).expect("the script is valid JSON");

    assert_eq!(
        emitted,
        json!({
            "model": "scripted-test",
            "context_length": 32768,
            "capabilities": {"think": false},
            "turns": [
                {"text": "general"},
                {
                    "text": "Let me look at what changed.",
                    "tool_calls": [{"name": "git_diff", "arguments": {"path": "."}}]
                },
                {"text": "Two files changed: README.md and main.go."}
            ]
        })
    );
}

#[test]
fn raw_arguments_reach_the_backend_as_a_json_string() {
    let script =
        Script::new().turn(Turn::new().call(ToolCall::new("git_diff").raw_arguments("{not json")));
    let emitted: serde_json::Value = serde_json::from_str(&script.to_json()).expect("valid JSON");
    assert_eq!(
        emitted["turns"][0]["tool_calls"][0]["arguments"],
        json!("{not json")
    );
}

#[test]
fn the_table_reader_keeps_the_spaces_inside_a_summary_cell() {
    let rendered = "\
ID                      KIND        TOOL                   SUMMARY               MISSION  AGE  EXPIRES-IN
scripted-test-0-0-a8bd  permission  native-git.git_status  read the whole tree   m-1      19s  never

Answer with 'contenox approvals respond <id> --approve|--deny'.
";
    let table = Table::parse(
        rendered,
        &[
            "ID",
            "KIND",
            "TOOL",
            "SUMMARY",
            "MISSION",
            "AGE",
            "EXPIRES-IN",
        ],
    )
    .expect("the approvals table parses");

    assert_eq!(table.len(), 1);
    assert_eq!(table.rows[0].get("ID"), "scripted-test-0-0-a8bd");
    assert_eq!(table.rows[0].get("SUMMARY"), "read the whole tree");
    assert_eq!(table.rows[0].get("EXPIRES-IN"), "never");
}

#[test]
fn every_instance_keeps_its_state_inside_its_own_scratch_home() {
    let one = Instance::named("isolation-a").expect("scratch instance");
    let two = Instance::named("isolation-b").expect("scratch instance");
    one.init().ok();
    two.init().ok();

    assert!(one.home().starts_with(std::env::temp_dir()));
    assert!(
        one.home_file("local.db").is_file(),
        "the store should live under the scratch home, not ~/.contenox"
    );

    one.scripted(&Script::new().turn("hello"))
        .expect("scripted backend");

    let mine = one.backends().expect("contenox backend list");
    assert_eq!(mine.len(), 1);
    assert_eq!(mine[0].kind, "scripted-test");

    let theirs = two.backends().expect("contenox backend list");
    assert!(
        theirs.is_empty(),
        "a second instance must not see the first one's backends, got {theirs:?}"
    );
}

#[test]
fn beam_without_a_terminal_refuses_and_names_run() {
    let cx = Instance::named("beam-no-tty").expect("scratch instance");
    cx.init().ok();

    cx.cmd(["beam"])
        .stdin("")
        .timeout(Duration::from_secs(60))
        .output()
        .expect("contenox beam")
        .expect_code(1)
        .expect_stderr("beam needs a terminal — for scripted use run: contenox run \"<task>\"");
}

#[test]
fn beam_under_a_pty_renders_its_composer() {
    let cx = Instance::named("beam-pty").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn("hello"))
        .expect("scripted backend");

    let pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    let screen = pty
        .wait_for("type / for commands", Duration::from_secs(60))
        .expect("beam's composer hint");

    assert!(
        screen.contains("model scripted-test"),
        "a fresh session's welcome header should name the model:\n{screen}"
    );
}
