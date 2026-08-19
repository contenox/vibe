//! One declaration, four shapes. `tools:` is the same sentence everywhere, but
//! what it can actually admit is bounded by the shape first: `local_fs` and
//! `local_shell` are forwarded to the connected client, so beam and an editor
//! carry them, an unattended run does not, and a `contenox serve` host mounts
//! neither.
//!
//! Testing one shape and assuming the rest is the assumption these cases exist
//! to remove.

use contenox_e2e::{Instance, Pty, Script, ToolCall, Turn};
use std::time::Duration;

/// One declaration naming `Read`, which resolves to a tool in `local_fs`.
const READS_FILES: &str = "---\nname: filer\ndescription: Reads a file it is pointed at\ntools: Read\n---\n\
     You read files.\n";

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.write_file(".contenox/agents/filer.md", READS_FILES)
        .expect("write the declaration");
    cx.write_file("note.txt", "the shape decides whether this is readable\n")
        .expect("write the note");
    cx
}

/// Point a surface at one declared agent's compiled chain. The path is the one
/// `contenox agent show` prints, and the variable is the documented override.
fn compile_and_point_at(cx: &mut Instance, agent: &str) {
    let shown = cx.run(["agent", "show", agent]).ok();
    let path = cx.generated(&format!("chain-agent-{agent}.json"));
    assert!(
        shown.stdout.contains(&path.display().to_string()),
        "contenox agent show {agent} must point at the compiled chain:\n{}",
        shown.render()
    );
    cx.set_env("CONTENOX_ACP_CHAIN_PATH", &path);
}

// ---------------------------------------------------------------------------

/// The editor shape: the client declares `fs.readTextFile`, so `local_fs` is
/// mounted and the declaration's grant is real — the read is forwarded back
/// into the editor rather than done behind its back.
#[test]
fn a_declaration_naming_read_reaches_the_file_under_an_editor() {
    let mut cx = instance("shape-editor-read");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Reading the note.")
                    .call(ToolCall::new("read_file").arg("path", "note.txt")),
            )
            .turn("The note says what the shape decides."),
    )
    .expect("scripted-test backend");
    compile_and_point_at(&mut cx, "filer");

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "read the note").expect("prompt");

    assert!(
        turn.methods().contains(&"fs/read_text_file"),
        "the read is forwarded to the client that has the project open, got {:?}",
        turn.methods()
    );
    assert!(
        !turn.tool_outputs().contains("tool read_file not found"),
        "an editor mounts local_fs, so the grant is real:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(turn.stop_reason, "end_turn");
}

/// The program shape: nobody is holding a filesystem open for it, so the same
/// declaration's `Read` admits a toolset that is not mounted. The refusal is
/// visible to the model — `tool read_file not found` comes back as the call's
/// own result, which is what a model can act on.
#[test]
fn the_same_declaration_cannot_reach_a_file_under_an_unattended_run() {
    let cx = instance("shape-run-read");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Reading the note.")
                    .call(ToolCall::new("read_file").arg("path", "note.txt")),
            )
            .turn(
                Turn::new()
                    .text("Filing.")
                    .call(ToolCall::mission_report("result", "the probe ran")),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted-test backend");

    cx.cmd(["run", "filer", "read the note", "--policy", "run"])
        .timeout(Duration::from_secs(240))
        .output()
        .expect("contenox run")
        .ok();

    let sessions = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(sessions.len(), 1, "one run, one session, got {sessions:?}");
    let said = cx.session_show(&sessions[0].id).ok().stdout;
    assert!(
        said.contains("tool read_file not found"),
        "an unattended run mounts no filesystem, and the model is told so:\n{said}"
    );
    assert!(
        !said.contains("the shape decides whether this is readable"),
        "and the file's content never reaches the conversation:\n{said}"
    );
}

/// The attended shape carries the filesystem natively: beam is the client, so
/// the same declaration reads the file with nobody wiring an editor up.
#[test]
fn a_declaration_naming_read_reaches_the_file_under_beam() {
    let mut cx = instance("shape-beam-read");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Reading the note.")
                    .call(ToolCall::new("read_file").arg("path", "note.txt")),
            )
            .turn("The note is about shapes."),
    )
    .expect("scripted-test backend");
    compile_and_point_at(&mut cx, "filer");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for("type / for commands", Duration::from_secs(90))
        .expect("beam's composer hint");
    pty.send_line("read the note").expect("send the prompt");
    pty.wait_for("local_fs.read_file: note.txt", Duration::from_secs(90))
        .expect("beam renders the tool the declaration granted");
    let screen = pty
        .wait_for("The note is about shapes.", Duration::from_secs(90))
        .expect("beam answers");

    assert!(
        !screen.contains("tool read_file not found"),
        "beam carries local_fs natively, so the declaration's grant is real:\n{screen}"
    );
    pty.send_ctrl('c').ok();
}

/// The host shape has no client at all, so a declaration's `Read` and `Bash`
/// name toolsets this host does not mount. Absence is the shape, not a policy
/// setting: the host says so at startup, per toolset, and keeps serving
/// everything else. (`tests/serve_host.rs` proves the same for a hand-written
/// chain; this is the declaration route to the same wall.)
#[test]
fn a_host_names_each_toolset_a_declaration_asked_for_and_cannot_serve() {
    let mut cx = instance("shape-serve-declared");
    cx.write_file(
        ".contenox/agents/handy.md",
        "---\nname: handy\ndescription: Wants a filesystem and a shell\ntools: Read, Bash\n---\n\
         You edit and you run things.\n",
    )
    .expect("write the declaration");
    cx.scripted(&Script::new().turn("a host is not asked anything by this case"))
        .expect("scripted-test backend");
    compile_and_point_at(&mut cx, "handy");

    let mut pty = Pty::spawn_sized(cx.cmd(["serve", "."]), 40, 200).expect("spawn contenox serve");
    let screen = pty
        .wait_for("Running. Press Ctrl-C to stop.", Duration::from_secs(120))
        .expect("the host never finished its status screen");
    pty.send_ctrl('c').ok();

    for toolset in ["local_fs", "local_shell"] {
        assert!(
            screen.contains(&format!(
                "contenox serve: \"{toolset}\" is declared but not served"
            )),
            "the host must name {toolset} as declared and unserved:\n{screen}"
        );
    }
    assert!(
        screen.contains(
            "declare an MCP tool for it (contenox mcp add), or run this agent from \
             `contenox beam` or an ACP editor"
        ),
        "and say what to do instead:\n{screen}"
    );
}
