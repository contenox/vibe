//! Tools: what crosses the boundary.
//!
//! contenox ships no tools of its own. `local_fs` and `local_shell` are
//! forwarded to whoever is holding the project open — an editor over ACP, or
//! beam — and the agent process never reads, writes or spawns anything itself.
//! These cases sit on the client's side of that wire, which is the only place
//! the claim can be checked: the test process *is* the filesystem, so a write
//! it declines to perform is a file that cannot appear.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn, Verdict};
use serde_json::{Value, json};

/// A scratch instance whose model is the scripted dialog.
fn editor(label: &str, script: &Script) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(script).expect("scripted-test backend");
    cx
}

/// The editor shape with the permission gate off, so what a case sees is the
/// tool boundary itself rather than the envelope in front of it.
fn open(cx: &Instance) -> (Acp, String) {
    let mut acp = cx.acp(["acp", "--auto"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    (acp, session)
}

fn read(path: &str) -> ToolCall {
    ToolCall::new("read_file").arg("path", path)
}

fn write(path: &str, content: &str) -> ToolCall {
    ToolCall::new("write_file")
        .arg("path", path)
        .arg("content", content)
}

// ------------------------------------------------ what the agent ships with

#[test]
fn local_fs_is_five_tools_and_every_one_of_them_needs_the_client() {
    let cx = Instance::named("roster-local-fs").expect("scratch instance");
    cx.init().ok();

    let roster = cx.doctor().ok().stdout;
    let entries: Vec<&str> = roster
        .lines()
        .map(str::trim)
        .filter(|line| line.contains(" — local_fs — "))
        .collect();

    assert_eq!(
        entries,
        vec![
            "read_file — local_fs — needs client capability fs.readTextFile",
            "write_file — local_fs — needs client capability fs.readTextFile+fs.writeTextFile",
            "edit_file — local_fs — needs client capability fs.readTextFile+fs.writeTextFile",
            "sed — local_fs — needs client capability fs.readTextFile+fs.writeTextFile",
            "read_file_range — local_fs — needs client capability fs.readTextFile",
        ],
        "local_fs is exactly five tools, none of which the agent can perform alone:\n{roster}"
    );
}

#[test]
fn listing_and_search_are_not_local_fs_but_in_process_browsing() {
    let cx = Instance::named("roster-browse").expect("scratch instance");
    cx.init().ok();

    let roster = cx.doctor().ok().stdout;
    for tool in ["list_dir", "grep", "find_files"] {
        assert!(
            roster.contains(&format!("{tool} — native-fs-browse — local (in-process)")),
            "{tool} browses in-process and is not part of the forwarded five:\n{roster}"
        );
    }
}

#[test]
fn local_shell_is_one_tool_and_it_needs_the_clients_terminal() {
    let cx = Instance::named("roster-local-shell").expect("scratch instance");
    cx.init().ok();

    let roster = cx.doctor().ok().stdout;
    let entries: Vec<&str> = roster
        .lines()
        .map(str::trim)
        .filter(|line| line.contains(" — local_shell — "))
        .collect();

    assert_eq!(
        entries,
        vec!["local_shell — local_shell — needs client capability terminal"],
        "the shell is one tool and the terminal is the client's:\n{roster}"
    );
}

// ------------------------------------------------------- the forwarding wire

#[test]
fn a_write_is_the_clients_to_perform_and_a_client_that_declines_leaves_no_file() {
    let cx = editor(
        "fs-decline-write",
        &Script::new().route("coding").turns([
            Turn::new().text("Writing the note.").call(write(
                "declined.txt",
                "the agent cannot put this here itself\n",
            )),
            Turn::new().text("The client would not write it."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    acp.performs_writes(false);
    let turn = acp.prompt(&session, "write the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert_eq!(
        turn.written_paths().len(),
        1,
        "the write was asked of this process, got {:?}",
        turn.methods()
    );
    assert!(
        cx.read_file("declined.txt").is_err(),
        "the agent has no filesystem of its own: a write this client refused cannot appear"
    );
    assert!(
        turn.tool_outputs().contains("declined to write"),
        "and the model is told the client refused, not that the write succeeded:\n{}",
        turn.tool_outputs()
    );
}

#[test]
fn a_read_the_client_refuses_is_not_quietly_done_by_the_agent_instead() {
    let cx = editor(
        "fs-decline-read",
        &Script::new().route("general").turns([
            Turn::new()
                .text("Reading the note.")
                .call(read("secret.txt")),
            Turn::new().text("The client would not hand it over."),
        ]),
    );
    cx.write_file("secret.txt", "PLAINLY-ON-DISK\n")
        .expect("write the note");

    let (mut acp, session) = open(&cx);
    acp.cannot_read("secret.txt");
    let turn = acp.prompt(&session, "read the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert_eq!(
        turn.read_paths().len(),
        1,
        "the read was asked of this process, got {:?}",
        turn.methods()
    );
    assert!(
        !turn.tool_outputs().contains("PLAINLY-ON-DISK"),
        "the file is right there on disk and the agent still did not read it itself:\n{}",
        turn.tool_outputs()
    );
}

#[test]
fn a_shell_call_is_opened_as_a_terminal_in_the_client_and_its_output_comes_back() {
    let cx = editor(
        "shell-terminal",
        &Script::new().route("general").turns([
            Turn::new().text("Running it.").call(
                ToolCall::new("local_shell")
                    .arg("command", "echo")
                    .arg("args", json!(["over-the-terminal-wire"])),
            ),
            Turn::new().text("It printed what I expected."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "run echo").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.methods().contains(&"terminal/create"),
        "the shell is the client's terminal, not a process the agent spawns, got {:?}",
        turn.methods()
    );
    assert_eq!(
        turn.terminal_commands(),
        vec!["echo over-the-terminal-wire"],
        "the command reaches the client verbatim"
    );
    assert!(
        turn.tool_outputs().contains("over-the-terminal-wire"),
        "and what the client's terminal printed is what the model is told:\n{}",
        turn.tool_outputs()
    );
}

#[test]
fn a_client_that_grants_no_terminal_is_served_no_shell() {
    let cx = editor(
        "shell-no-terminal",
        &Script::new().route("general").turns([
            Turn::new()
                .text("Running it.")
                .call(ToolCall::new("local_shell").arg("command", "echo")),
            Turn::new().text("There was no terminal to run it in."),
        ]),
    );

    let mut acp = cx.acp(["acp", "--auto"]).expect("spawn the ACP surface");
    acp.initialize_with(json!({"fs": {"readTextFile": true, "writeTextFile": true}}))
        .expect("initialize without a terminal");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run echo").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        !turn.methods().contains(&"terminal/create"),
        "a client that granted no terminal is never asked to open one, got {:?}",
        turn.methods()
    );
    assert!(
        turn.tool_outputs().contains("not found"),
        "and the model is told the tool is not there:\n{}",
        turn.tool_outputs()
    );
}

// --------------------------------------------------- read before write

/// The note every mutation case works on, and the read that earns the right
/// to change it.
const NOTE: &str = "alpha\nbeta\ngamma\n";

fn with_note(label: &str, script: &Script) -> Instance {
    let cx = editor(label, script);
    cx.write_file("note.txt", NOTE).expect("write the note");
    cx
}

#[test]
fn a_write_over_an_existing_file_without_reading_it_first_is_refused() {
    let cx = with_note(
        "rbw-unread",
        &Script::new().route("coding").turns([
            Turn::new()
                .text("Overwriting.")
                .call(write("note.txt", "clobbered\n")),
            Turn::new().text("It would not let me."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "overwrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("cannot modify existing file note.txt without reading it first"),
        "the refusal names the file and the call that clears it:\n{}",
        turn.tool_outputs()
    );
    assert!(
        turn.written_paths().is_empty(),
        "and no write ever reaches the client, got {:?}",
        turn.methods()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note survives"),
        NOTE,
        "the file is untouched"
    );
}

#[test]
fn a_full_read_earns_the_write_and_the_client_performs_it() {
    let cx = with_note(
        "rbw-read-then-write",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("note.txt")),
            Turn::new()
                .text("Now overwriting.")
                .call(write("note.txt", "rewritten\n")),
            Turn::new().text("Done."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "rewrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        !turn.tool_outputs().contains("without reading it first"),
        "the prior full read is the permission the write needed:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note was rewritten"),
        "rewritten\n"
    );
}

#[test]
fn a_range_read_is_not_enough_to_overwrite_the_whole_file() {
    let cx = with_note(
        "rbw-range-write",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading a slice.").call(
                ToolCall::new("read_file_range")
                    .arg("path", "note.txt")
                    .arg("start_line", 1)
                    .arg("end_line", 2),
            ),
            Turn::new()
                .text("Overwriting.")
                .call(write("note.txt", "clobbered\n")),
            Turn::new().text("It wanted the whole file."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "rewrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("after only reading a line range"),
        "a slice does not earn a full overwrite, and the refusal says so:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note survives"),
        NOTE,
        "the file is untouched"
    );
}

#[test]
fn a_range_read_is_enough_to_edit_within_the_file() {
    let cx = with_note(
        "rbw-range-edit",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading a slice.").call(
                ToolCall::new("read_file_range")
                    .arg("path", "note.txt")
                    .arg("start_line", 1)
                    .arg("end_line", 2),
            ),
            Turn::new().text("Editing.").call(
                ToolCall::new("edit_file")
                    .arg("path", "note.txt")
                    .arg("old_string", "beta")
                    .arg("new_string", "BETA"),
            ),
            Turn::new().text("Done."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "edit the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        !turn.tool_outputs().contains("without reading it first"),
        "a range read is the read a targeted edit asks for:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note was edited"),
        "alpha\nBETA\ngamma\n"
    );
}

#[test]
fn the_read_that_earns_a_write_does_not_carry_into_another_session() {
    let cx = with_note(
        "rbw-per-session",
        &Script::new()
            .route("general")
            .turns([
                Turn::new().text("Reading first.").call(read("note.txt")),
                Turn::new().text("Read it."),
            ])
            .route("coding")
            .turns([
                Turn::new()
                    .text("Overwriting.")
                    .call(write("note.txt", "clobbered\n")),
                Turn::new().text("A new session knows nothing of that read."),
            ]),
    );

    let (mut acp, first) = open(&cx);
    acp.prompt(&first, "read the note").expect("prompt");

    let second = acp.new_session(cx.work()).expect("a second session/new");
    let turn = acp.prompt(&second, "overwrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("cannot modify existing file note.txt without reading it first"),
        "the guard is per session, so the earlier session's read grants nothing here:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note survives"),
        NOTE,
        "the file is untouched"
    );
}

#[test]
fn reading_a_file_through_the_shell_does_not_earn_the_right_to_write_it() {
    let cx = with_note(
        "rbw-shell-read",
        &Script::new().route("coding").turns([
            Turn::new().text("Catting it.").call(
                ToolCall::new("local_shell")
                    .arg("command", "cat")
                    .arg("args", json!(["note.txt"])),
            ),
            Turn::new()
                .text("Overwriting.")
                .call(write("note.txt", "clobbered\n")),
            Turn::new().text("The shell read did not count."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "rewrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs().contains("alpha"),
        "the shell really did read the file:\n{}",
        turn.tool_outputs()
    );
    assert!(
        turn.tool_outputs()
            .contains("cannot modify existing file note.txt without reading it first"),
        "and it still bought nothing — only local_fs.read_file clears the gate:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note survives"),
        NOTE,
        "the file is untouched"
    );
}

#[test]
fn a_file_that_does_not_exist_yet_is_written_without_any_prior_read() {
    let cx = editor(
        "rbw-new-file",
        &Script::new().route("coding").turns([
            Turn::new()
                .text("Creating it.")
                .call(write("fresh.txt", "brand new\n")),
            Turn::new().text("Created."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "create the file").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        !turn.tool_outputs().contains("without reading it first"),
        "there is no existing version to have read:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("fresh.txt").expect("the file was created"),
        "brand new\n"
    );
}

// ------------------------------------------------------------- edit_file

#[test]
fn edit_file_refuses_a_string_that_is_not_in_the_file_byte_for_byte() {
    let cx = with_note(
        "edit-not-exact",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("note.txt")),
            Turn::new().text("Editing.").call(
                ToolCall::new("edit_file")
                    .arg("path", "note.txt")
                    // The file holds "beta", not "Beta" — one byte apart.
                    .arg("old_string", "Beta")
                    .arg("new_string", "BETA"),
            ),
            Turn::new().text("It would not fuzzy-match."),
        ]),
    );

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "edit the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("old_string not found in note.txt"),
        "a near miss is a refusal, never a guess:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note survives"),
        NOTE,
        "the file is untouched"
    );
}

#[test]
fn edit_file_refuses_a_string_that_occurs_twice_and_names_both_ways_out() {
    let cx = editor(
        "edit-ambiguous",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("dup.txt")),
            Turn::new().text("Editing.").call(
                ToolCall::new("edit_file")
                    .arg("path", "dup.txt")
                    .arg("old_string", "target")
                    .arg("new_string", "replaced"),
            ),
            Turn::new().text("It was ambiguous."),
        ]),
    );
    cx.write_file("dup.txt", "one target\ntwo target\n")
        .expect("write the file");

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "edit the file").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let said = turn.tool_outputs();
    assert!(
        said.contains("old_string occurs 2 times in dup.txt"),
        "the refusal counts the matches:\n{said}"
    );
    assert!(
        said.contains("more surrounding context") && said.contains("replace_all=true"),
        "and names both ways forward:\n{said}"
    );
    assert_eq!(
        cx.read_file("dup.txt").expect("the file survives"),
        "one target\ntwo target\n",
        "the file is untouched"
    );
}

#[test]
fn replace_all_is_how_two_occurrences_are_taken_together() {
    let cx = editor(
        "edit-replace-all",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("dup.txt")),
            Turn::new().text("Editing everywhere.").call(
                ToolCall::new("edit_file")
                    .arg("path", "dup.txt")
                    .arg("old_string", "target")
                    .arg("new_string", "replaced")
                    .arg("replace_all", true),
            ),
            Turn::new().text("Both changed."),
        ]),
    );
    cx.write_file("dup.txt", "one target\ntwo target\n")
        .expect("write the file");

    let (mut acp, session) = open(&cx);
    acp.prompt(&session, "edit the file").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert_eq!(
        cx.read_file("dup.txt").expect("the file was edited"),
        "one replaced\ntwo replaced\n"
    );
}

#[test]
fn an_edit_whose_file_changed_since_the_read_is_refused_rather_than_clobbering() {
    let cx = with_note(
        "edit-stale",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("note.txt")),
            // The client's own terminal moves the file out from under the read.
            Turn::new().text("Replacing it wholesale.").call(
                ToolCall::new("local_shell")
                    .arg("command", "cp")
                    .arg("args", json!(["other.txt", "note.txt"])),
            ),
            Turn::new().text("Editing what I read.").call(
                ToolCall::new("edit_file")
                    .arg("path", "note.txt")
                    .arg("old_string", "beta")
                    .arg("new_string", "BETA"),
            ),
            Turn::new().text("It refused the stale edit."),
        ]),
    );
    cx.write_file("other.txt", "someone else got here first\n")
        .expect("write the replacement");

    let (mut acp, session) = open(&cx);
    let turn = acp.prompt(&session, "edit the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("because it changed since you read it"),
        "an edit against a version that is gone is refused, not applied:\n{}",
        turn.tool_outputs()
    );
    assert_eq!(
        cx.read_file("note.txt").expect("the note is readable"),
        "someone else got here first\n",
        "the newer version stands untouched"
    );
}

// ------------------------------------------------------- the approval card

/// The permission request the agent raised for one tool, by the title it
/// carries — a turn can hold several.
fn ask_for<'a>(turn: &'a contenox_e2e::PromptTurn, title: &str) -> &'a Value {
    turn.permissions
        .iter()
        .find(|ask| ask.pointer("/toolCall/title").and_then(Value::as_str) == Some(title))
        .unwrap_or_else(|| {
            panic!(
                "no permission request titled {title:?}, got {:?}",
                turn.permissions
                    .iter()
                    .map(|ask| ask.pointer("/toolCall/title").cloned())
                    .collect::<Vec<_>>()
            )
        })
}

/// The rendered unified diff the card shows, as the ask carries it.
fn card_diff(ask: &Value) -> String {
    ask.pointer("/toolCall/_meta/diff")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string()
}

#[test]
fn the_card_carries_a_unified_diff_with_three_lines_of_context_either_side() {
    let before: String = (1..=11).map(|n| format!("line {n}\n")).collect();
    let after = before.replace("line 6\n", "line six\n");

    let cx = editor(
        "diff-context",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("lines.txt")),
            Turn::new()
                .text("Rewriting one line.")
                .call(write("lines.txt", &after)),
            Turn::new().text("Done."),
        ]),
    );
    cx.write_file("lines.txt", &before).expect("write the file");

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Allow);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "rewrite line six").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let diff = card_diff(ask_for(&turn, "local_fs.write_file: lines.txt"));
    assert_eq!(
        diff,
        "--- lines.txt (current)\n\
         +++ lines.txt (proposed)\n\
         @@ -3,7 +3,7 @@\n\
         \x20line 3\n\
         \x20line 4\n\
         \x20line 5\n\
         -line 6\n\
         +line six\n\
         \x20line 7\n\
         \x20line 8\n\
         \x20line 9\n",
        "one changed line, three lines of context either side of it"
    );
}

#[test]
fn the_diff_is_built_from_a_fresh_read_not_from_what_the_model_last_saw() {
    let cx = with_note(
        "diff-fresh-read",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("note.txt")),
            // Between the read and the write the file moves under the model.
            Turn::new().text("Replacing it wholesale.").call(
                ToolCall::new("local_shell")
                    .arg("command", "cp")
                    .arg("args", json!(["other.txt", "note.txt"])),
            ),
            Turn::new()
                .text("Writing what I planned.")
                .call(write("note.txt", "what the model wanted\n")),
            Turn::new().text("Done."),
        ]),
    );
    cx.write_file("other.txt", "someone else got here first\n")
        .expect("write the replacement");

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Allow);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "rewrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let ask = ask_for(&turn, "local_fs.write_file: note.txt");
    assert_eq!(
        ask.pointer("/toolCall/_meta/diffOld")
            .and_then(Value::as_str),
        Some("someone else got here first\n"),
        "the card is built from the file as it is now, not the version the model read"
    );
    assert!(
        !card_diff(ask).contains("beta"),
        "the stale version the model is working from is nowhere in the card:\n{}",
        card_diff(ask)
    );
}

#[test]
fn a_file_the_client_cannot_hand_over_is_still_asked_about_without_a_diff() {
    let cx = with_note(
        "diff-unreadable",
        &Script::new().route("coding").turns([
            Turn::new()
                .text("Overwriting.")
                .call(write("note.txt", "written blind\n")),
            Turn::new().text("Asked without a diff."),
        ]),
    );

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Deny).cannot_read("note.txt");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "overwrite the note").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let ask = ask_for(&turn, "local_fs.write_file: note.txt");
    assert_eq!(
        ask.pointer("/toolCall/rawInput/path")
            .and_then(Value::as_str),
        Some("note.txt"),
        "the operator is still asked, and still told what is about to be written"
    );
    assert_eq!(
        card_diff(ask),
        "",
        "a file whose current contents cannot be established carries no diff rather than a made-up one"
    );
}

#[test]
fn a_diff_past_the_line_caps_is_truncated_rather_than_flooding_the_card() {
    let before: String = (1..=600).map(|n| format!("old line {n}\n")).collect();
    let after: String = (1..=600).map(|n| format!("new line {n}\n")).collect();

    let cx = editor(
        "diff-caps",
        &Script::new().route("coding").turns([
            Turn::new().text("Reading first.").call(read("big.txt")),
            Turn::new()
                .text("Rewriting all of it.")
                .call(write("big.txt", &after)),
            Turn::new().text("Done."),
        ]),
    );
    cx.write_file("big.txt", &before).expect("write the file");

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Deny);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "rewrite the file").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let diff = card_diff(ask_for(&turn, "local_fs.write_file: big.txt"));
    assert!(
        diff.contains("... (diff truncated)"),
        "a card is a thing a person reads, so the diff stops and says so:\n{diff}"
    );
    assert!(
        diff.contains("... (file truncated to first 500 lines)"),
        "and names the file cap it hit as well:\n{diff}"
    );
    assert!(
        diff.lines().count() <= 130,
        "the rendered diff stays near its 120-line bound, got {} lines",
        diff.lines().count()
    );
}

// ------------------------------------------------- the toolset's own gate

#[test]
fn a_command_outside_the_toolsets_allowed_commands_is_refused_without_asking_anyone() {
    let cx = editor(
        "shell-not-allowed",
        &Script::new().route("general").turns([
            Turn::new()
                .text("Running it.")
                .call(ToolCall::new("local_shell").arg("command", "kubectl")),
            Turn::new().text("It was never on the list."),
        ]),
    );

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Allow);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run kubectl").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let said = turn.tool_outputs();
    assert!(
        said.contains("kubectl") && said.contains("_allowed_commands"),
        "the refusal names the command and where the list lives:\n{said}"
    );
    assert!(
        said.contains("no approval can grant it"),
        "and says an approval cannot help, because this gate is not the envelope's:\n{said}"
    );
    assert!(
        !turn.asked_permission(),
        "a command that can never run is not put in front of a person, got {:?}",
        turn.permissions
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and leaves no durable ask behind either"
    );
    assert!(
        !turn.methods().contains(&"terminal/create"),
        "and never reaches the client's terminal, got {:?}",
        turn.methods()
    );
}

#[test]
fn a_denied_command_is_refused_by_name_even_where_the_allowlist_would_be_silent() {
    let cx = editor(
        "shell-denied",
        &Script::new().route("general").turns([
            Turn::new()
                .text("Running it.")
                .call(ToolCall::new("local_shell").arg("command", "sudo")),
            Turn::new().text("The denylist stopped it."),
        ]),
    );

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Allow);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run sudo").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("command sudo is denied by policy"),
        "the denylist refuses by name:\n{}",
        turn.tool_outputs()
    );
    assert!(
        !turn.methods().contains(&"terminal/create"),
        "and no terminal is opened for it, got {:?}",
        turn.methods()
    );
}

#[test]
fn shell_true_is_refused_outright_while_a_command_policy_is_active() {
    let cx = editor(
        "shell-mode-forbidden",
        &Script::new().route("general").turns([
            Turn::new().text("Running a pipeline.").call(
                ToolCall::new("local_shell")
                    .arg("command", "echo hello | tr a-z A-Z")
                    .arg("shell", true),
            ),
            Turn::new().text("Shell mode was refused."),
        ]),
    );

    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    acp.answers(Verdict::Allow);
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run a pipeline").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    let said = turn.tool_outputs();
    assert!(
        said.contains("'shell: true' is strictly forbidden"),
        "a raw shell string cannot be checked against a command list, so it is refused:\n{said}"
    );
    assert!(
        said.contains("set shell:false and supply the command and args separately"),
        "and the model is told what to do instead:\n{said}"
    );
    assert!(
        !turn.methods().contains(&"terminal/create"),
        "nothing reaches the client's terminal, got {:?}",
        turn.methods()
    );
}

#[test]
fn a_command_on_the_toolsets_list_must_still_pass_the_envelope() {
    let cx = editor(
        "shell-envelope-too",
        &Script::new().route("general").turns([
            Turn::new().text("Running it.").call(
                ToolCall::new("local_shell")
                    .arg("command", "echo")
                    .arg("args", json!(["allowed by the toolset"])),
            ),
            Turn::new().text("The envelope refused it anyway."),
        ]),
    );

    // read_only denies local_shell outright; `echo` is on the toolset's list.
    let mut acp = cx
        .acp(["acp", "--hitl-policy", "read_only"])
        .expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run echo").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-read_only.json"),
        "passing the toolset's own list buys nothing from the envelope:\n{}",
        turn.tool_outputs()
    );
    assert!(
        !turn.methods().contains(&"terminal/create"),
        "and the terminal is never opened, got {:?}",
        turn.methods()
    );
}

/// The shipped flat chain with `local_shell`'s policy replaced wholesale, so a
/// case can put one key in force on its own.
fn chain_with_shell_policy(cx: &Instance, policy: Value) -> std::path::PathBuf {
    let source = cx.home_file(".generated/chain-agent-acpx.json");
    let text = std::fs::read_to_string(&source).expect("the compiled chain");
    let mut chain: Value = serde_json::from_str(&text).expect("the compiled chain is JSON");
    for task in chain["tasks"].as_array_mut().expect("tasks").iter_mut() {
        let Some(policies) = task
            .pointer_mut("/execute_config/tools_policies")
            .and_then(Value::as_object_mut)
        else {
            continue;
        };
        policies.insert("local_shell".into(), policy.clone());
    }
    let at = cx.work().join("shell-policy-chain.json");
    std::fs::write(&at, chain.to_string()).expect("plant the chain");
    at
}

/// A runnable script inside the directory the policy will name.
fn planted_script(cx: &Instance) -> String {
    use std::os::unix::fs::PermissionsExt;
    let path = cx
        .write_file("bin/greet", "#!/bin/sh\necho from-the-allowed-dir\n")
        .expect("write the script");
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755))
        .expect("make it runnable");
    path.display().to_string()
}

/// Run one prompt through a planted chain under the auto-approving editor.
fn under_chain(cx: &Instance, chain: &std::path::Path) -> contenox_e2e::PromptTurn {
    let mut acp = Acp::spawn(
        cx.cmd(["acp", "--auto"])
            .env("CONTENOX_ACP_CHAIN_PATH", chain),
    )
    .expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run it").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");
    turn
}

#[test]
fn a_command_under_the_allowed_dir_runs_and_one_outside_it_does_not() {
    // The command's path is only known once the instance exists, so the dialog
    // is written after it rather than by the usual helper.
    let cx = Instance::named("shell-allowed-dir").expect("scratch instance");
    cx.init().ok();
    let script = planted_script(&cx);
    cx.scripted(
        &Script::new().turns([
            Turn::new()
                .text("Running the planted one.")
                .call(ToolCall::new("local_shell").arg("command", script.as_str())),
            Turn::new()
                .text("Now one from elsewhere.")
                .call(ToolCall::new("local_shell").arg("command", "/bin/echo")),
            Turn::new().text("Only one of those ran."),
        ]),
    )
    .expect("scripted-test backend");

    let chain = chain_with_shell_policy(
        &cx,
        json!({"_allowed_dir": cx.work().join("bin").display().to_string()}),
    );
    let said = under_chain(&cx, &chain).tool_outputs();

    assert!(
        said.contains("from-the-allowed-dir"),
        "a command inside the named directory runs:\n{said}"
    );
    assert!(
        said.contains("/bin/echo is not under allowed dir"),
        "and one outside it is refused, naming the boundary:\n{said}"
    );
}

#[test]
fn shell_true_is_refused_when_an_allowed_dir_alone_is_in_force() {
    let cx = editor(
        "shell-allowed-dir-shell-mode",
        &Script::new().turns([
            Turn::new().text("Running a pipeline.").call(
                ToolCall::new("local_shell")
                    .arg("command", "echo hello | tr a-z A-Z")
                    .arg("shell", true),
            ),
            Turn::new().text("Shell mode was refused."),
        ]),
    );
    let chain = chain_with_shell_policy(
        &cx,
        json!({"_allowed_dir": cx.work().join("bin").display().to_string()}),
    );

    let said = under_chain(&cx, &chain).tool_outputs();
    assert!(
        said.contains("'shell: true' is strictly forbidden"),
        "an allowed-dir on its own is enough to disable shell mode:\n{said}"
    );
}
