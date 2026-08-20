//! The durable ask: raised, answered, expired, resumed.
//!
//! Every case here crosses a process boundary. A `contenox run` blocks on a
//! gated call in one process; a second process answers it with `contenox
//! approvals respond` and reads the outcome back through `approvals list`,
//! `mission list`, and the working tree the approved call was supposed to
//! change. Nothing here links Go, and nothing here opens the store.

use contenox_e2e::{Acp, ApprovalRow, Instance, Running, Script, ToolCall, Turn, Verdict};
use std::process::Command;
use std::time::{Duration, Instant};

const COMPOSER: &str = "type / for commands";
const CARD: &str = "approval required";
const DECISION: &str = "y allow";

/// A cold `contenox run` compiles its agents before it reaches the first gate.
fn patiently() -> Duration {
    Duration::from_secs(180)
}

fn instance(label: &str, script: &Script) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(script).expect("scripted-test backend");
    cx
}

fn git(cx: &Instance, args: &[&str]) -> String {
    let out = Command::new("git")
        .args(args)
        .current_dir(cx.work())
        .output()
        .expect("git is on PATH");
    assert!(
        out.status.success(),
        "git {args:?} failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).into_owned()
}

/// A repository holding one file nobody has staged. Staging it is the side
/// effect a gated `native-git.git_add` leaves behind — or does not, which is
/// the only way an outside process can tell an authorised call that ran from
/// one that was merely reported as having run.
fn repo_with_an_unstaged_note(cx: &Instance) {
    git(cx, &["init", "-q"]);
    cx.write_file("note.txt", "hello\n").expect("the note");
}

fn staged(cx: &Instance) -> String {
    git(cx, &["diff", "--cached", "--name-only"])
        .trim()
        .to_string()
}

/// The dialog whose first move is a gated call, and which finishes once it is
/// answered either way.
fn stages_the_note() -> Script {
    Script::new()
        .turn(
            Turn::new()
                .text("Staging the note.")
                .call(ToolCall::new("git_add").arg("paths", "note.txt")),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

/// The dialog that asks a human for words rather than a verdict, twice.
fn asks_a_question() -> Script {
    Script::new()
        .turn(
            Turn::new().text("I need a decision.").call(
                ToolCall::new("mission_ask_attention")
                    .arg("summary", "which bucket should I use?")
                    .arg("detail", "staging or prod"),
            ),
        )
        .turn(
            Turn::new().text("One more.").call(
                ToolCall::new("mission_ask_attention")
                    .arg("summary", "which region should I use?")
                    .arg("detail", "eu or us"),
            ),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

/// A run left blocked on its first gated call, and the ask it raised.
fn run_blocked_on_its_ask(cx: &Instance, task: &str) -> (Running, ApprovalRow) {
    let fired = cx
        .cmd(["run", "--policy", "default", task])
        .timeout(patiently())
        .start()
        .expect("contenox run");
    let ask = cx
        .await_approval(Duration::from_secs(120))
        .expect("the gated call reaches 'contenox approvals list'");
    (fired, ask)
}

/// A run whose process is gone with the ask still open: the state a second
/// terminal finds when the process that asked is no longer there.
///
/// The host is killed once the ask is durable rather than left to expire on its
/// own `--timeout`. A timeout teardown races the host's own shutdown: stopping
/// the unit makes the shepherd's in-flight turn fail, and `failTurn` finishes
/// the mission `derailed` on a context that deliberately outlives the caller's,
/// so whether that write lands before the process exits is decided by how fast
/// the machine is — open here, derailed on a slower runner. Killing the host
/// settles it. The ask row is already durable by then, and no shutdown write
/// can land afterwards, so every caller below sees one state rather than two.
fn run_gone_with_its_ask_open(cx: &Instance, task: &str) -> ApprovalRow {
    let (mut fired, ask) = run_blocked_on_its_ask(cx, task);
    fired.kill();
    fired
        .wait_timeout(Duration::from_secs(60))
        .expect("the killed run is reaped")
        .expect_failure();
    ask
}

// ------------------------------------------------- the row comes first

#[test]
fn an_ask_is_a_durable_row_that_survives_the_death_of_the_process_that_raised_it() {
    let cx = instance("ask-durable-row", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let (mut fired, ask) = run_blocked_on_its_ask(&cx, "stage the note");
    assert_eq!(ask.kind, "permission");
    assert_eq!(ask.tool, "native-git.git_add");
    assert!(!ask.mission.is_empty(), "the ask names its mission");

    fired.kill();
    fired
        .wait_timeout(Duration::from_secs(60))
        .expect("the killed run is reaped")
        .expect_failure();

    let still = cx.approvals().expect("approvals list");
    assert_eq!(
        still.len(),
        1,
        "a crash from the instant the ask exists still shows it pending, got {still:?}"
    );
    assert_eq!(still[0].id, ask.id, "and it is the same row");
    assert_eq!(staged(&cx), "", "the gated call never ran");
    assert_eq!(
        cx.missions().expect("mission list")[0].status,
        "open",
        "the mission is still waiting on the answer"
    );
}

#[test]
fn the_empty_inbox_says_what_lands_there() {
    let cx = instance("ask-empty-inbox", &Script::new().turn("nothing to do"));

    cx.run(["approvals", "list"]).ok().expect_stdout(
        "No pending asks. Gated tool calls and mission questions land here when nobody is watching the session.",
    );
}

// ------------------------------------------------ answered from anywhere

#[test]
fn a_verdict_from_a_second_terminal_releases_the_blocked_call_and_the_same_turn_carries_on() {
    let cx = instance("ask-answered-live", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let (fired, ask) = run_blocked_on_its_ask(&cx, "stage the note");
    assert_eq!(staged(&cx), "", "nothing ran while the ask was open");

    cx.approve(&ask.id)
        .ok()
        .expect_stdout("Nothing was checkpointed under it, so nothing resumed here");

    let out = fired
        .wait_timeout(patiently())
        .expect("the released run finishes");
    out.clone().ok();
    assert!(
        out.stderr.contains("finished: landed"),
        "the same turn carried on to a terminal status:\n{}",
        out.render()
    );
    assert_eq!(
        staged(&cx),
        "note.txt",
        "the call the operator authorised elsewhere ran in the run's own process"
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "the answered ask leaves the queue"
    );
}

#[test]
fn an_answered_question_comes_back_as_the_tools_result_without_a_second_prompt() {
    let cx = instance("ask-question-live", &asks_a_question());

    let (fired, ask) = run_blocked_on_its_ask(&cx, "decide the bucket");
    assert_eq!(ask.kind, "question");
    assert_eq!(ask.tool, "mission.mission_ask_attention");
    assert_eq!(ask.summary, "which bucket should I use?");

    cx.answer(&ask.id, "use the staging bucket")
        .ok()
        .expect_stdout("Nothing was checkpointed under it, so nothing resumed here");

    // The second question is the same turn asking again, not a resumed run.
    let next = cx
        .await_approval(Duration::from_secs(60))
        .expect("the turn carried on and asked its second question");
    assert_ne!(next.id, ask.id, "a new question, not the answered one");
    cx.answer(&next.id, "use eu").ok();

    let out = fired
        .wait_timeout(patiently())
        .expect("the answered run finishes");
    assert!(
        out.stderr.contains("finished: landed"),
        "the run landed after both answers:\n{}",
        out.render()
    );
}

#[test]
fn a_second_verdict_on_the_same_ask_is_refused_because_a_row_is_terminal_once() {
    let cx = instance("ask-terminal-once", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let (fired, ask) = run_blocked_on_its_ask(&cx, "stage the note");
    cx.approve(&ask.id).ok();
    fired
        .wait_timeout(patiently())
        .expect("the released run finishes");

    for second in [["--approve"], ["--deny"]] {
        cx.cmd(["approvals", "respond", &ask.id])
            .args(second)
            .output()
            .expect("approvals respond")
            .expect_failure()
            .expect_stderr("was already answered — a verdict is recorded exactly once");
    }
    assert_eq!(
        staged(&cx),
        "note.txt",
        "and the call ran exactly once, whatever the second screen said"
    );
}

// -------------------------------------------------- the process goes away

/// `run_shape.rs` already pins that the row outlives the wait. What only the
/// working tree can say is the other half: nothing the ask gates has happened.
#[test]
fn a_run_torn_down_with_its_ask_open_has_not_run_the_call_the_ask_gates() {
    let cx = instance("ask-run-timeout", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let ask = run_gone_with_its_ask_open(&cx, "stage the note");

    assert_eq!(ask.tool, "native-git.git_add");
    assert_eq!(staged(&cx), "", "the gated call has not run");
    assert_eq!(
        cx.missions().expect("mission list")[0].status,
        "open",
        "the mission is checkpointed, not finished"
    );
}

#[test]
#[ignore = "confirmed defect: a run torn down with an ask still open announces only \
'mission <id> did not finish within <timeout> … Re-run with a larger --timeout'. It names \
neither the pending ask nor 'contenox approvals respond', so the one action that resumes the \
checkpointed work is invisible and the advice it does give fires a second mission instead \
(internal/surfaces/contenoxcli/run_cmd.go's wait-timeout message, against the promise its own \
--help makes: 'the run is checkpointed beside the still-pending ask, so answering it afterwards \
resumes the work rather than losing it')"]
fn a_timed_out_run_announces_itself_suspended_and_names_the_ask_that_resumes_it() {
    let cx = instance("ask-run-suspend-notice", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let out = cx
        .cmd([
            "run",
            "--policy",
            "default",
            "--timeout",
            "10s",
            "stage the note",
        ])
        .timeout(patiently())
        .output()
        .expect("contenox run")
        .expect_failure();

    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the ask outlives the run");
    assert!(
        out.stderr.contains(&ask.id),
        "the operator is told which ask is holding the run:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains("approvals respond"),
        "and the command that resumes it:\n{}",
        out.render()
    );
}

#[test]
#[ignore = "confirmed defect: 'approvals respond' resumes the checkpointed run through \
contenoxcli.BuildEngine, whose localToolset registers only mission/local_fs/local_shell — not \
the native-* toolsets an unattended unit actually ran with. The approved native-git.git_add \
therefore comes back to the resumed chain as 'tool git_add not found': nothing is staged, while \
the command reports 'the suspended run was resumed in this process' and 'mission list' flips the \
mission to landed. The same path under beam passes only because beam's gated tool is local_fs, \
which the resuming engine does happen to have"]
fn answering_after_the_run_is_gone_resumes_it_and_runs_the_call_that_was_approved() {
    let cx = instance("ask-resume-runs-the-call", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let ask = run_gone_with_its_ask_open(&cx, "stage the note");

    cx.approve(&ask.id)
        .ok()
        .expect_stdout("the suspended run was resumed in this process");

    assert_eq!(
        staged(&cx),
        "note.txt",
        "resuming from exactly that point means running the call that was approved"
    );
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "and the ask is closed"
    );
    assert_eq!(
        cx.missions().expect("mission list")[0].status,
        "landed",
        "the resumed run reached its terminal status"
    );
}

#[test]
fn a_resumed_runs_asks_are_detached_so_it_suspends_again_under_its_own_ask_id() {
    let cx = instance("ask-resume-detached", &asks_a_question());

    let first = run_gone_with_its_ask_open(&cx, "decide the bucket");
    assert_eq!(first.kind, "question");

    // Answering resumes the run HERE. It must not park this terminal on the
    // next question: it suspends again on a row of its own.
    cx.answer(&first.id, "use the staging bucket")
        .ok()
        .expect_stdout("the suspended run was resumed in this process")
        .expect_stdout("'contenox approvals list' shows it");

    let pending = cx.approvals().expect("approvals list");
    assert_eq!(
        pending.len(),
        1,
        "the resumed run suspended again on one ask, got {pending:?}"
    );
    assert_ne!(
        pending[0].id, first.id,
        "under its own ask id, not the one this terminal already answered"
    );
    assert_eq!(pending[0].kind, "question");
    assert_eq!(
        cx.missions().expect("mission list")[0].status,
        "open",
        "and the mission is still waiting"
    );
}

// ------------------------------------------- a verdict is not spent blindly

#[test]
fn respond_refuses_before_recording_anything_when_it_cannot_build_an_engine() {
    let cx = instance("ask-no-engine", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let ask = run_gone_with_its_ask_open(&cx, "stage the note");

    cx.cmd([
        "approvals",
        "respond",
        &ask.id,
        "--approve",
        "--think",
        "bogus",
    ])
    .output()
    .expect("approvals respond")
    .expect_failure()
    .expect_stderr("cannot build an engine to resume it")
    .expect_stderr("The verdict was NOT recorded — the ask is still pending");

    let still = cx.approvals().expect("approvals list");
    assert_eq!(
        still.len(),
        1,
        "the one-shot verdict was not spent, got {still:?}"
    );
    assert_eq!(still[0].id, ask.id);

    // And a terminal that can build one still answers it.
    cx.approve(&ask.id)
        .ok()
        .expect_stdout("the suspended run was resumed in this process");
}

// ---------------------------------------------------- the ask's own wait

#[test]
fn approvals_list_is_the_reconciling_read_that_applies_an_expired_asks_verdict() {
    let cx = instance("ask-expiry-sweep", &stages_the_note());
    repo_with_an_unstaged_note(&cx);
    cx.run(["config", "set", "approval-ceiling", "10s"]).ok();

    cx.cmd([
        "run",
        "--policy",
        "default",
        "--timeout",
        "5s",
        "stage the note",
    ])
    .timeout(patiently())
    .output()
    .expect("contenox run")
    .expect_failure();
    let ask = cx
        .await_approval(Duration::from_secs(20))
        .expect("the ask outlives the run");
    assert_ne!(ask.expires_in, "never", "this ask has a deadline");

    wait_past(Duration::from_secs(16));

    cx.run(["approvals", "list"])
        .ok()
        .expect_stdout("Swept 1 expired ask(s) to their on-timeout verdict.")
        .expect_stdout("Resumed 1 stranded run(s) to completion in this process.")
        .expect_stdout("No pending asks.");

    assert_eq!(staged(&cx), "", "an expired ask denies the call it gated");
    assert_ne!(
        cx.missions().expect("mission list")[0].status,
        "open",
        "and the run behind it was carried to a terminal status"
    );
}

#[test]
fn answering_an_ask_whose_window_already_closed_is_refused_naming_the_verdict_applied() {
    let cx = instance("ask-expired-answer", &stages_the_note());
    repo_with_an_unstaged_note(&cx);
    cx.run(["config", "set", "approval-ceiling", "10s"]).ok();

    cx.cmd([
        "run",
        "--policy",
        "default",
        "--timeout",
        "5s",
        "stage the note",
    ])
    .timeout(patiently())
    .output()
    .expect("contenox run")
    .expect_failure();
    let ask = cx
        .await_approval(Duration::from_secs(20))
        .expect("the ask outlives the run");

    wait_past(Duration::from_secs(16));
    cx.run(["approvals", "list"]).ok();

    cx.approve(&ask.id)
        .expect_failure()
        .expect_stderr("expired before this answer; its on-timeout verdict (deny) already applied");
    assert_eq!(staged(&cx), "", "and the call it gated stays unrun");
}

#[test]
#[ignore = "confirmed defect: an ask's deadline is applied only by a sweep, and the only sweeper \
in a CLI-shaped host is 'approvals list'. Until someone happens to list, hitlservice.resolve's CAS \
still sees state=pending, so 'approvals respond --approve' is accepted hours after the window \
closed — the on_timeout verdict never applies and the gated call is authorised anyway. \
'approvals respond --help' and internal/services/hitlservice/hitlservice.go:632 both describe the \
refusal this case asserts"]
fn an_ask_past_its_deadline_is_refused_even_when_nothing_has_swept_it_yet() {
    let cx = instance("ask-expired-unswept", &stages_the_note());
    repo_with_an_unstaged_note(&cx);
    cx.run(["config", "set", "approval-ceiling", "10s"]).ok();

    cx.cmd([
        "run",
        "--policy",
        "default",
        "--timeout",
        "5s",
        "stage the note",
    ])
    .timeout(patiently())
    .output()
    .expect("contenox run")
    .expect_failure();
    let ask = cx
        .await_approval(Duration::from_secs(20))
        .expect("the ask outlives the run");

    wait_past(Duration::from_secs(16));

    // No 'approvals list' in between: nothing has swept the row yet.
    cx.approve(&ask.id)
        .expect_failure()
        .expect_stderr("expired before this answer");
}

// ------------------------------------------- the verdict must fit the ask

#[test]
fn respond_requires_exactly_one_of_approve_deny_or_answer() {
    let cx = instance("ask-one-verdict", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let ask = run_gone_with_its_ask_open(&cx, "stage the note");

    for flags in [vec![], vec!["--approve", "--deny"]] {
        cx.cmd(["approvals", "respond", &ask.id])
            .args(flags.clone())
            .output()
            .expect("approvals respond")
            .expect_failure()
            .expect_stderr("exactly one of --approve, --deny, or --answer is required");
    }
    assert_eq!(
        cx.approvals().expect("approvals list").len(),
        1,
        "a rejected invocation spends nothing"
    );
}

#[test]
fn respond_refuses_words_where_the_ask_wants_a_verdict() {
    let cx = instance("ask-kind-permission", &stages_the_note());
    repo_with_an_unstaged_note(&cx);

    let ask = run_gone_with_its_ask_open(&cx, "stage the note");

    cx.answer(&ask.id, "go ahead")
        .expect_failure()
        .expect_stderr(
            "is a permission ask (native-git.git_add) — it takes --approve or --deny, not text",
        );
    assert_eq!(cx.approvals().expect("approvals list").len(), 1);
}

#[test]
fn respond_refuses_a_yes_no_verdict_where_the_ask_wants_words() {
    let cx = instance("ask-kind-question", &asks_a_question());

    let ask = run_gone_with_its_ask_open(&cx, "decide the bucket");

    cx.approve(&ask.id)
        .expect_failure()
        .expect_stderr("is a QUESTION (which bucket should I use?) — answer it with --answer");
    assert_eq!(cx.approvals().expect("approvals list").len(), 1);
}

#[test]
fn respond_names_an_ask_that_does_not_exist() {
    let cx = instance("ask-unknown-id", &Script::new().turn("nothing to do"));

    cx.approve("no-such-ask").expect_failure().expect_stderr(
        "no ask \"no-such-ask\" exists — 'contenox approvals list' shows what is pending",
    );
}

// ------------------------------------------------- a card is not the ask

#[test]
fn a_card_the_editor_dismisses_is_not_an_answer() {
    let cx = instance(
        "ask-card-dismissed",
        &Script::new()
            .route("coding")
            .turn(
                Turn::new().text("Writing the note.").call(
                    ToolCall::new("write_file")
                        .arg("path", "notes.txt")
                        .arg("content", "answered elsewhere\n"),
                ),
            )
            .turn("That is done."),
    );

    let mut acp: Acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    // The operator dismisses the card without deciding.
    acp.answers(Verdict::Cancel);
    let session = acp.new_session(cx.work()).expect("session/new");

    let ask = std::thread::scope(|scope| {
        let elsewhere = scope.spawn(|| {
            let ask = cx
                .await_approval(Duration::from_secs(90))
                .expect("the card's row is durable before the card is answered");
            // Give the dismissal time to land, then prove it decided nothing.
            std::thread::sleep(Duration::from_secs(3));
            let still = cx.approvals().expect("approvals list");
            assert_eq!(
                still.len(),
                1,
                "a dismissed card is not a verdict, got {still:?}"
            );
            assert_eq!(still[0].id, ask.id, "and it is the same row");
            assert!(
                !cx.work().join("notes.txt").exists(),
                "nothing ran on a dismissal"
            );
            cx.approve(&ask.id).ok();
            ask
        });
        let _ = acp.prompt(&session, "write a note");
        elsewhere.join().expect("the answering thread")
    });

    assert_eq!(ask.tool, "local_fs.write_file");
    cx.wait_for(Duration::from_secs(60), |cx| {
        cx.approvals().map(|rows| rows.is_empty()).unwrap_or(false)
    })
    .expect("the answered ask leaves the queue");
    assert_eq!(
        cx.read_file("notes.txt").expect("the note"),
        "answered elsewhere\n",
        "the verdict that did arrive is the one that ran the call"
    );
}

#[test]
fn an_answered_ask_is_not_offered_again_when_a_client_comes_back() {
    let cx = instance(
        "ask-answered-not-reoffered",
        &Script::new()
            .route("general")
            .turn(
                Turn::new().text("Writing the note.").call(
                    ToolCall::new("write_file")
                        .arg("path", "notes.txt")
                        .arg("content", "answered while away\n"),
                ),
            )
            .turn("That is done."),
    );

    let mut first = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    first
        .wait_for(COMPOSER, Duration::from_secs(90))
        .expect("beam's composer");
    first.send_line("write a note").expect("the prompt");
    first
        .wait_for(DECISION, Duration::from_secs(120))
        .expect("the gated write raises a card");

    let ask = cx
        .await_approval(Duration::from_secs(30))
        .expect("the durable row behind the card");
    first.interrupt();
    first
        .wait_exit(Duration::from_secs(90))
        .expect("beam leaves with the ask open");

    // Answered from a second terminal, while nothing is attached.
    cx.approve(&ask.id)
        .ok()
        .expect_stdout("the suspended run was resumed in this process");
    assert!(
        cx.approvals().expect("approvals list").is_empty(),
        "the ask is closed before anything reattaches"
    );

    let second = cx.pty(["beam", "--plain"]).expect("beam again");
    second
        .wait_for(COMPOSER, Duration::from_secs(90))
        .expect("beam's composer");
    assert!(
        second.wait_for(CARD, Duration::from_secs(15)).is_err(),
        "an answered ask must not be put back in front of anyone:\n{}",
        second.screen()
    );
}

// ------------------------------------------------- the documented surface

#[test]
#[ignore = "confirmed defect: 'contenox mission asks' is documented in docs/guide/missions.md, \
docs/reference/contenox-cli.md, docs/use-cases/auto-attention.md, in the 'mission' command's own \
--help, and in the footer 'mission show' prints — but no such subcommand is registered \
(internal/surfaces/contenoxcli/mission_cmd.go's init adds list/show/reports/plan/fire/stop and \
nothing else). Running it prints the parent help and exits 0, so a script branching on the exit \
code cannot tell that it did nothing"]
fn mission_asks_narrows_the_view_to_one_missions_pending_questions() {
    let cx = instance("ask-mission-asks", &asks_a_question());

    let ask = run_gone_with_its_ask_open(&cx, "decide the bucket");
    let mission = cx.missions().expect("mission list")[0].id.clone();

    cx.run(["mission", "asks", &mission])
        .ok()
        .expect_stdout(&ask.id)
        .expect_stdout("which bucket should I use?");
}

fn wait_past(wait: Duration) {
    let until = Instant::now() + wait;
    while Instant::now() < until {
        std::thread::sleep(Duration::from_millis(200));
    }
}
