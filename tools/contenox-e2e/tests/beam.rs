//! `contenox beam` — the keyboard shape, driven from outside under a real pty.

use contenox_e2e::{Instance, Pty, Script, ToolCall, Turn};
use serde_json::json;
use std::time::Duration;

const COMPOSER: &str = "type / for commands";
const CARD: &str = "approval required";
const DECISION: &str = "y allow";

fn ready() -> Duration {
    Duration::from_secs(90)
}

fn turn() -> Duration {
    Duration::from_secs(120)
}

/// Polls `contenox approvals list` until the queue drains.
fn await_empty_queue(cx: &Instance) {
    cx.wait_for(Duration::from_secs(60), |cx| {
        cx.approvals().map(|rows| rows.is_empty()).unwrap_or(false)
    })
    .expect("the answered ask leaves the queue");
}

/// Polls until a path the agent was told to write actually exists.
fn await_file(cx: &Instance, relative: &str) -> String {
    let path = cx.work().join(relative);
    cx.wait_for(Duration::from_secs(60), |_| path.is_file())
        .unwrap_or_else(|_| panic!("{} was never written", path.display()));
    std::fs::read_to_string(&path).expect("read what the agent wrote")
}

/// A dialog whose one gated call writes `notes.txt` into beam's workspace.
fn writes_a_note() -> Script {
    Script::new()
        .route("general")
        .turn(
            Turn::new().text("Writing the note.").call(
                ToolCall::new("write_file")
                    .arg("path", "notes.txt")
                    .arg("content", "hello from the agent\n"),
            ),
        )
        .turn("The note is written.")
}

fn refute(screen: &str, needle: &str, why: &str) {
    assert!(
        !screen.contains(needle),
        "{why}: found {needle:?}\n--- screen ---\n{screen}\n"
    );
}

/// The full ACP session name (`beam-<uuid>`), as `contenox session list --all`
/// prints it. The status bar shows only its first segment.
fn only_session(cx: &Instance) -> String {
    let rows = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(rows.len(), 1, "expected exactly one session, got {rows:?}");
    rows[0].name.clone()
}

fn short(name: &str) -> String {
    name.split('-').take(2).collect::<Vec<_>>().join("-")
}

/// A gated call, raised and left waiting on the card.
fn beam_stopped_on_a_card(cx: &Instance, args: &[&str]) -> Pty {
    let mut pty = cx.pty(args).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("write a note").expect("submit the prompt");
    pty.wait_for(DECISION, turn()).expect("the approval card");
    pty
}

// ------------------------------------------------------- the front door

#[test]
fn bare_contenox_on_a_terminal_opens_beam() {
    let cx = Instance::named("beam-bare").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let pty = cx.pty(Vec::<String>::new()).expect("bare contenox");
    let screen = pty
        .wait_for(COMPOSER, ready())
        .expect("a bare invocation on a terminal opens beam's composer");

    assert!(
        screen.contains("model scripted-test"),
        "bare contenox opened beam, welcome header and all:\n{screen}"
    );
}

#[test]
fn bare_contenox_and_contenox_beam_open_the_same_session() {
    let cx = Instance::named("beam-same-command").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut bare = cx.pty(Vec::<String>::new()).expect("bare contenox");
    bare.wait_for(COMPOSER, ready()).expect("beam's composer");
    let opened = only_session(&cx);
    bare.interrupt();
    bare.wait_exit(ready()).expect("bare contenox leaves");

    let named = cx.pty(["beam", "--plain"]).expect("contenox beam");
    let screen = named.wait_for(COMPOSER, ready()).expect("beam's composer");

    assert!(
        screen.contains(&short(&opened)),
        "'contenox beam' reopened the session a bare 'contenox' started ({opened}):\n{screen}"
    );
    assert_eq!(
        only_session(&cx),
        opened,
        "the two spellings are one command, so no second session was started"
    );
}

// --------------------------------------- the terminal and the composer

#[test]
fn the_transcript_goes_into_native_scrollback_not_an_alternate_screen() {
    let cx = Instance::named("beam-scrollback").expect("scratch instance");
    cx.init().ok();
    cx.scripted(
        &Script::new()
            .route("general")
            .turn("the reply that has to stay scrollable"),
    )
    .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("say something").expect("submit the prompt");
    pty.wait_for("the reply that has to stay scrollable", turn())
        .expect("the reply reaches the terminal");

    let raw = pty.raw();
    for enter in [b"\x1b[?1049h".as_slice(), b"\x1b[?47h".as_slice()] {
        assert!(
            !raw.windows(enter.len()).any(|w| w == enter),
            "beam switched to the alternate screen, so its transcript is a managed pane rather than the terminal's own scrollback"
        );
    }
}

#[test]
fn the_composer_takes_slash_for_commands() {
    let cx = Instance::named("beam-slash").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send("/").expect("open the command palette");

    let screen = pty
        .wait_for("/help", Duration::from_secs(30))
        .expect("the slash palette lists the session's commands");
    for command in ["/clear", "/keys"] {
        assert!(
            screen.contains(command),
            "the palette should offer {command}:\n{screen}"
        );
    }
}

#[test]
fn the_composer_takes_at_for_file_mentions() {
    let cx = Instance::named("beam-mention").expect("scratch instance");
    cx.init().ok();
    cx.write_file("spec-notes.md", "a file worth mentioning\n")
        .expect("a file in the workspace");
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send("@spec").expect("open the file picker");

    pty.wait_for("spec-notes.md", Duration::from_secs(30))
        .expect("@ completes against the files in beam's workspace");
}

// ---------------------------------------------------- the approval card

#[test]
fn a_gated_call_raises_a_card_naming_the_tool_its_arguments_and_the_rule() {
    let cx = Instance::named("beam-card").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    // Wide enough that the policy line — file, path and rule number — is not
    // elided by the scratch home's own long path.
    let mut pty = Pty::spawn_sized(cx.cmd(["beam", "--plain"]), 40, 220).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("write a note").expect("submit the prompt");
    let screen = pty.wait_for(DECISION, turn()).expect("the approval card");

    for part in [
        CARD,
        "local_fs.write_file",
        "path = notes.txt",
        "hello from the agent",
        "policy hitl-policy-default.json",
        "rule ",
        "y allow - n deny - Esc cancels turn",
    ] {
        assert!(
            screen.contains(part),
            "the card must show {part:?} — tool, arguments and the rule that gated it:\n{screen}"
        );
    }
}

#[test]
fn one_keystroke_allows_the_card_and_the_turn_carries_straight_on() {
    let cx = Instance::named("beam-allow").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    pty.send("y").expect("one keystroke answers the card");

    pty.wait_for("allowed", turn())
        .expect("the card records the verdict");
    pty.wait_for("The note is written.", turn())
        .expect("the turn carries straight on after the verdict");

    assert_eq!(
        await_file(&cx, "notes.txt"),
        "hello from the agent\n",
        "the gated write ran once it was allowed"
    );
    await_empty_queue(&cx);
}

#[test]
fn one_keystroke_denies_the_card_and_the_gated_tool_never_runs() {
    let cx = Instance::named("beam-deny").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    pty.send("n").expect("one keystroke answers the card");

    pty.wait_for("denied", turn())
        .expect("the card records the refusal");
    pty.wait_for("The note is written.", turn())
        .expect("the turn carries on after a denial too");

    assert!(
        !cx.work().join("notes.txt").exists(),
        "a denied call must not have run"
    );
    await_empty_queue(&cx);
}

// ------------------------------------------ the durable ask behind it

#[test]
fn the_card_is_the_visible_half_of_a_durable_ask_another_terminal_can_answer() {
    let cx = Instance::named("beam-durable").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);

    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the card's row is already durable when the card appears");
    assert_eq!(ask.kind, "permission");
    assert_eq!(ask.tool, "local_fs.write_file");
    assert_eq!(ask.summary, "notes.txt");

    cx.approve(&ask.id)
        .ok()
        .expect_stdout("Verdict recorded for");

    pty.wait_for("The note is written.", turn())
        .expect("answering from another terminal releases the turn waiting in beam");
    assert_eq!(
        await_file(&cx, "notes.txt"),
        "hello from the agent\n",
        "and the call the operator authorised elsewhere ran here"
    );
}

// --------------------------------------------- quitting with an ask open

#[test]
fn quitting_beam_checkpoints_the_turn_beside_the_still_pending_ask() {
    let cx = Instance::named("beam-checkpoint").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the durable row behind the card");

    pty.interrupt();
    assert_eq!(
        pty.wait_exit(ready()).expect("beam leaves"),
        Some(0),
        "quitting with an ask open is not a failure"
    );

    let still = cx.approvals().expect("approvals list");
    assert_eq!(still.len(), 1, "the ask outlives beam, got {still:?}");
    assert_eq!(still[0].id, ask.id, "and it is the same row");

    cx.approve(&ask.id)
        .ok()
        .expect_stdout("the suspended run was resumed in this process");
    assert_eq!(
        await_file(&cx, "notes.txt"),
        "hello from the agent\n",
        "answering later resumed the checkpointed turn and ran the gated call"
    );
}

// ---------------------------------------------- reattaching to a session

#[test]
fn a_reattaching_beam_is_offered_the_still_open_ask_under_its_original_id() {
    let cx = Instance::named("beam-reattach").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut first = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the durable row behind the card");
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let mut second = cx.pty(["beam", "--plain"]).expect("beam again");
    second.wait_for(COMPOSER, ready()).expect("beam's composer");
    second
        .wait_for(CARD, Duration::from_secs(60))
        .expect("a reattaching client is re-presented the session's still-open ask");

    let reoffered = cx.approvals().expect("approvals list");
    assert_eq!(reoffered.len(), 1, "still one ask, got {reoffered:?}");
    assert_eq!(
        reoffered[0].id, ask.id,
        "the ask is re-presented under its original id, not a new one"
    );

    second.send("y").expect("answer the re-presented ask");
    await_empty_queue(&cx);
    assert_eq!(
        await_file(&cx, "notes.txt"),
        "hello from the agent\n",
        "answering it here resumed the checkpointed run"
    );
}

// ------------------------------------------------- the ask's own wait

#[test]
fn an_unanswered_card_settles_on_the_asks_on_timeout_verdict() {
    let cx = Instance::named("beam-ceiling").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");
    cx.run(["config", "set", "approval-ceiling", "5s"]).ok();

    let pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);

    pty.wait_for(
        "Approval timed out. The operation was automatically denied.",
        turn(),
    )
    .expect("an ask nobody answers resolves to its on-timeout verdict, in front of the operator");
    pty.wait_for("The note is written.", turn())
        .expect("and the turn carries on with the refusal");

    assert!(
        !cx.work().join("notes.txt").exists(),
        "the timed-out call never ran"
    );
}

// ------------------------------------- the tools beam performs itself

#[test]
fn local_fs_writes_land_in_the_workspace_beam_was_launched_in() {
    let cx = Instance::named("beam-workspace").expect("scratch instance");
    cx.init().ok();
    cx.write_file("sub/.keep", "").expect("a subdirectory");
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = beam_stopped_on_a_card(&cx, &["beam", "--plain", "sub"]);
    pty.send("y").expect("allow the write");
    pty.wait_for("The note is written.", turn())
        .expect("the turn finishes");

    assert!(
        cx.work().join("sub/notes.txt").is_file(),
        "beam performs local_fs in the workspace it was launched in"
    );
    assert!(
        !cx.work().join("notes.txt").exists(),
        "and not in the directory the process happened to start in"
    );
}

#[test]
fn local_shell_is_gated_and_then_run_by_beam_itself() {
    let cx = Instance::named("beam-shell").expect("scratch instance");
    cx.init().ok();
    cx.scripted(
        &Script::new()
            .route("general")
            .turn(Turn::new().text("Touching the file.").call(
                ToolCall::new("local_shell").arguments(json!({
                    "command": "touch",
                    "args": ["from-the-shell.txt"],
                })),
            ))
            .turn("The shell ran."),
    )
    .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("touch a file").expect("submit the prompt");

    let screen = pty.wait_for(DECISION, turn()).expect("the approval card");
    for part in ["local_shell.local_shell", "command = touch"] {
        assert!(
            screen.contains(part),
            "an unrecognised shell command asks first, naming itself: {part:?}\n{screen}"
        );
    }

    pty.send("y").expect("allow the command");
    pty.wait_for("The shell ran.", turn())
        .expect("the turn carries on");
    assert!(
        cx.work().join("from-the-shell.txt").is_file(),
        "beam is the ACP client and runs local_shell in its own workspace"
    );
}

#[test]
fn a_command_outside_the_toolsets_list_is_refused_by_beam_without_a_card() {
    let cx = Instance::named("beam-shell-not-allowed").expect("scratch instance");
    cx.init().ok();
    cx.scripted(
        &Script::new()
            .route("general")
            .turn(
                Turn::new()
                    .text("Running it.")
                    .call(ToolCall::new("local_shell").arguments(json!({"command": "kubectl"}))),
            )
            .turn("It was never on the list."),
    )
    .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("run kubectl").expect("submit the prompt");
    let screen = pty
        .wait_for("It was never on the list.", turn())
        .expect("the turn runs to its end");

    refute(
        &screen,
        CARD,
        "the toolset's own command list is settled before anyone is asked",
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and no durable ask is raised either"
    );
}

// ------------------------------------------------------------ the flags

#[test]
fn plain_drops_every_colour_sequence() {
    let cx = Instance::named("beam-plain").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut plain = cx.pty(["beam", "--plain"]).expect("beam --plain");
    plain.wait_for(COMPOSER, ready()).expect("beam's composer");
    let quiet = colour_codes(&plain.raw());
    plain.interrupt();
    plain.wait_exit(ready()).expect("beam leaves");

    let mut coloured = cx.pty(["beam"]).expect("beam");
    coloured
        .wait_for(COMPOSER, ready())
        .expect("beam's composer");
    let painted = colour_codes(&coloured.raw());
    coloured.interrupt();
    coloured.wait_exit(ready()).expect("beam leaves");

    assert_eq!(
        quiet,
        Vec::<String>::new(),
        "--plain is for terminals and captures that want no colour"
    );
    assert!(
        !painted.is_empty(),
        "and it is a choice: without it beam paints"
    );
}

#[test]
fn light_paints_the_brand_in_its_light_ladder() {
    let cx = Instance::named("beam-light").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut dark = cx.pty(["beam"]).expect("beam");
    dark.wait_for(COMPOSER, ready()).expect("beam's composer");
    let detected = colour_codes(&dark.raw());
    dark.interrupt();
    dark.wait_exit(ready()).expect("beam leaves");

    let mut light = cx.pty(["beam", "--light"]).expect("beam --light");
    light.wait_for(COMPOSER, ready()).expect("beam's composer");
    let overridden = colour_codes(&light.raw());
    light.interrupt();
    light.wait_exit(ready()).expect("beam leaves");

    // #059669 is the brand mint for a light background, #34D399 for a dark one.
    assert_eq!(
        overridden.first().map(String::as_str),
        Some("38;2;5;150;105"),
        "--light overrides detection and paints the light ladder"
    );
    assert_eq!(
        detected.first().map(String::as_str),
        Some("38;2;52;211;153"),
        "the same terminal, undirected, opens on the dark ladder"
    );
}

#[test]
fn log_dir_puts_beams_logs_where_it_was_told_and_off_the_transcript() {
    let cx = Instance::named("beam-log-dir").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");
    let logs = cx.root().join("elsewhere");
    std::fs::create_dir_all(&logs).expect("a log directory");

    let pty = cx
        .pty([
            std::ffi::OsStr::new("beam"),
            std::ffi::OsStr::new("--plain"),
            std::ffi::OsStr::new("--log-dir"),
            logs.as_os_str(),
        ])
        .expect("beam --log-dir");
    let screen = pty.wait_for(COMPOSER, ready()).expect("beam's composer");

    assert!(
        screen.contains(&format!("logs: {}", logs.display())),
        "beam says where its logs went:\n{screen}"
    );
    refute(
        &screen,
        "level=INFO",
        "beam's stderr is the transcript, so log lines never reach it",
    );

    let written: Vec<_> = std::fs::read_dir(&logs)
        .expect("read the log directory")
        .filter_map(Result::ok)
        .filter(|e| e.path().extension().is_some_and(|x| x == "log"))
        .collect();
    assert_eq!(written.len(), 1, "one log file, got {written:?}");
}

#[test]
fn new_starts_a_fresh_session_instead_of_reopening_the_newest() {
    let cx = Instance::named("beam-new").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut first = cx.pty(["beam", "--plain"]).expect("beam");
    first.wait_for(COMPOSER, ready()).expect("beam's composer");
    let opened = only_session(&cx);
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let second = cx.pty(["beam", "--plain", "--new"]).expect("beam --new");
    let screen = second.wait_for(COMPOSER, ready()).expect("beam's composer");

    refute(
        &screen,
        &short(&opened),
        "--new must not reopen the newest session",
    );
    let rows = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(rows.len(), 2, "a second session was started, got {rows:?}");
}

#[test]
fn session_opens_the_session_it_names() {
    let cx = Instance::named("beam-session").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut first = cx.pty(["beam", "--plain"]).expect("beam");
    first.wait_for(COMPOSER, ready()).expect("beam's composer");
    let wanted = only_session(&cx);
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let mut other = cx.pty(["beam", "--plain", "--new"]).expect("beam --new");
    other.wait_for(COMPOSER, ready()).expect("beam's composer");
    other.interrupt();
    other.wait_exit(ready()).expect("beam leaves");

    let named = cx
        .pty(["beam", "--plain", "--session", &wanted])
        .expect("beam --session");
    let screen = named.wait_for(COMPOSER, ready()).expect("beam's composer");

    assert!(
        screen.contains(&short(&wanted)),
        "--session opens the session it names, not the newest one:\n{screen}"
    );
    assert_eq!(
        cx.sessions_all().expect("session list --all").len(),
        2,
        "and starts none of its own"
    );
}

#[test]
fn session_says_so_when_the_session_it_names_does_not_exist() {
    let cx = Instance::named("beam-session-missing").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("hello"))
        .expect("scripted backend");

    let mut pty = cx
        .pty(["beam", "--plain", "--session", "no-such-session"])
        .expect("beam --session");
    pty.wait_for("no-such-session\" not found", ready())
        .expect("beam names the session it could not open");
    assert_eq!(
        pty.wait_exit(ready()).expect("beam gives up"),
        Some(1),
        "a session that is not there is a failure a script can branch on"
    );
}

#[test]
fn hitl_policy_names_the_envelope_the_card_is_gated_by() {
    let cx = Instance::named("beam-hitl-policy").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let pty = beam_stopped_on_a_card(&cx, &["beam", "--plain", "--hitl-policy", "ask_always"]);
    let screen = pty.screen();

    assert!(
        screen.contains("policy hitl-policy-ask_always.json"),
        "the card names the envelope --hitl-policy put in force:\n{screen}"
    );
    refute(
        &screen,
        "hitl-policy-default.json",
        "and not the one beam would have run under",
    );
}

#[test]
fn hitl_policy_read_only_refuses_the_write_without_raising_an_ask() {
    let cx = Instance::named("beam-read-only").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = cx
        .pty(["beam", "--plain", "--hitl-policy", "read_only"])
        .expect("beam --hitl-policy read_only");
    pty.wait_for(COMPOSER, ready()).expect("beam's composer");
    pty.send_line("write a note").expect("submit the prompt");
    let screen = pty
        .wait_for("The note is written.", turn())
        .expect("the turn runs to its end");

    refute(
        &screen,
        CARD,
        "an envelope that denies writes has nothing to ask a human about",
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and raises no durable ask either"
    );
    assert!(
        !cx.work().join("notes.txt").exists(),
        "the denied write never ran"
    );
}

// ------------------------------------------------- confirmed defects

#[test]
#[ignore = "confirmed defect: a re-offered card claims 'Esc cancels turn' over a turn that ended in another process. Nothing marks a parked ask detached on the re-offer path (acpsvc.offerParkedAsk -> enginebridge PermissionRequested -> beam app.events), so approval.Card.MarkDetached is never called and the footer keeps the live-turn hint."]
fn a_reoffered_card_says_answering_resumes_the_run() {
    let cx = Instance::named("beam-reoffer-hint").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut first = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let second = cx.pty(["beam", "--plain"]).expect("beam again");
    second.wait_for(COMPOSER, ready()).expect("beam's composer");
    let screen = second
        .wait_for(CARD, Duration::from_secs(60))
        .expect("the ask is re-presented");

    assert!(
        screen.contains("answering resumes the run"),
        "a card whose turn has ended offers what answering does:\n{screen}"
    );
    refute(
        &screen,
        "Esc cancels turn",
        "there is no turn here for Esc to cancel",
    );
}

#[test]
#[ignore = "confirmed defect: Esc on a re-offered card is silent and cancels a session with no turn in it. app.dispatch takes the Detached() branch — which prints 'no turn is running …' — only for a card marked detached, and the re-offer path never marks one, so Esc calls Bridge.Cancel instead and prints nothing."]
fn esc_on_a_reoffered_card_says_there_is_no_turn_to_cancel() {
    let cx = Instance::named("beam-reoffer-esc").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut first = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let mut second = cx.pty(["beam", "--plain"]).expect("beam again");
    second.wait_for(COMPOSER, ready()).expect("beam's composer");
    second
        .wait_for(CARD, Duration::from_secs(60))
        .expect("the ask is re-presented");

    second.send("\x1b").expect("press Esc");
    second
        .wait_for("no turn is running", Duration::from_secs(30))
        .expect("Esc cancels nothing here and says why");
    assert_eq!(
        cx.approvals().expect("approvals list").len(),
        1,
        "and leaves the ask open"
    );
}

#[test]
#[ignore = "confirmed defect: a re-offered card names the tool and the rule but nothing about what the call acts on. acpsvc.parkedAskCard deliberately carries no rawInput and puts the row's args_summary on ToolCall.Title and in a content block; approval.Card.Ask renders neither, so the operator is asked to authorise a write without being shown the path that 'contenox approvals list' prints in its SUMMARY column."]
fn a_reoffered_card_still_names_what_the_call_acts_on() {
    let cx = Instance::named("beam-reoffer-target").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut first = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    first.interrupt();
    first.wait_exit(ready()).expect("beam leaves");

    let second = cx.pty(["beam", "--plain"]).expect("beam again");
    second.wait_for(COMPOSER, ready()).expect("beam's composer");
    let screen = second
        .wait_for(CARD, Duration::from_secs(60))
        .expect("the ask is re-presented");

    assert!(
        screen.contains("notes.txt"),
        "the re-offered card names what it is about:\n{screen}"
    );
}

#[test]
#[ignore = "confirmed defect: closing the terminal loses the run. runACPProfile traps SIGINT and SIGTERM only, so the SIGHUP the kernel sends a foreground group when its window closes kills beam outright: the ask survives as a pending row with no checkpoint under it, and answering it later reports 'Nothing was checkpointed under it' and never runs the gated call."]
fn closing_the_terminal_checkpoints_the_turn_the_way_quitting_does() {
    let cx = Instance::named("beam-hangup").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&writes_a_note()).expect("scripted backend");

    let mut pty = beam_stopped_on_a_card(&cx, &["beam", "--plain"]);
    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the durable row behind the card");

    pty.hangup();
    pty.wait_exit(ready())
        .expect("beam goes away with the terminal");

    cx.approve(&ask.id)
        .ok()
        .expect_stdout("the suspended run was resumed in this process");
    assert_eq!(
        await_file(&cx, "notes.txt"),
        "hello from the agent\n",
        "answering later resumed the checkpointed turn"
    );
}

// ------------------------------------------------------------------ helpers

/// Every true-colour SGR sequence in the order the terminal received it.
fn colour_codes(raw: &[u8]) -> Vec<String> {
    let text = String::from_utf8_lossy(raw);
    let mut out = Vec::new();
    let bytes: Vec<char> = text.chars().collect();
    let mut i = 0usize;
    while i + 1 < bytes.len() {
        if bytes[i] != '\u{1b}' || bytes[i + 1] != '[' {
            i += 1;
            continue;
        }
        let start = i + 2;
        let mut j = start;
        while j < bytes.len() && (bytes[j].is_ascii_digit() || bytes[j] == ';') {
            j += 1;
        }
        if j < bytes.len() && bytes[j] == 'm' {
            let code: String = bytes[start..j].iter().collect();
            if code.starts_with("38;2") {
                out.push(code);
            }
        }
        i = j.max(i + 1);
    }
    out
}
