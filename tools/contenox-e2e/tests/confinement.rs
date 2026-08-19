//! Confinement: trusted binaries, the sandbox, and the shell environment.
//!
//! Three walls, walked from outside the product.
//!
//! * **Trusted binaries** — `contenox hitl trust` writes an identity and an
//!   integrity claim into a policy file, and the evaluator withdraws an allow
//!   when either stops holding. The verb is checkable by byte-diffing the file
//!   it edits; the evaluator is checkable by watching a `local_shell` call that
//!   used to run unattended stop for a human, with the reason on the card.
//! * **The shell environment** — what a spawned shell inherits (`contenox
//!   sandbox env`) and what is injected on top of it (`contenox shell-env`).
//!   The preview is one claim and the shell that actually starts is another,
//!   so both are checked, and the shape difference between them is checked too.
//! * **The sandbox wall** — Landlock, which confines a *foreign* agent
//!   process. No case reaches it; see the section at the foot of this file for
//!   why that is a fact about the shipped product, not about this suite.
//!
//! Every assertion is on something a person can see: the policy file, the exit
//! code, the refusal the model was handed, the reason on the approval card, the
//! environment the spawned shell reported, and what `--list`, `vet` and
//! `doctor` print.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn, Verdict};
use serde_json::{Value, json};
use std::path::{Path, PathBuf};
use std::time::Duration;

// --------------------------------------------------------------- the fixtures

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx
}

/// The digest an operator would compute by hand, per
/// docs/guide/confinement/trusted-binaries.md#computing-the-hash.
fn sha256_of(path: &Path) -> String {
    let out = std::process::Command::new("sha256sum")
        .arg(path)
        .output()
        .unwrap_or_else(|err| panic!("sha256sum {}: {err}", path.display()));
    assert!(
        out.status.success(),
        "sha256sum {} failed: {}",
        path.display(),
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout)
        .split_whitespace()
        .next()
        .expect("sha256sum prints the digest first")
        .to_string()
}

/// A policy file of the operator's own, in the directory every surface reads.
/// A top-level copy shadows the rendered envelope beneath it.
fn write_policy(cx: &Instance, name: &str, body: &str) -> PathBuf {
    let path = cx.home_file(format!("hitl-policy-{name}.json"));
    std::fs::write(&path, body).unwrap_or_else(|err| panic!("write {}: {err}", path.display()));
    path
}

fn read(path: &Path) -> String {
    std::fs::read_to_string(path).unwrap_or_else(|err| panic!("read {}: {err}", path.display()))
}

fn trusted_binaries(path: &Path) -> Value {
    let text = read(path);
    let doc: Value = serde_json::from_str(&text)
        .unwrap_or_else(|err| panic!("{} is not JSON: {err}", path.display()));
    doc["trusted_binaries"].clone()
}

/// Plant an executable of our own and put its directory first on `PATH` — the
/// `PATH`-substitution the whole trusted-binaries surface exists to catch.
fn plant(cx: &mut Instance, dir: &str, name: &str, body: &str) -> PathBuf {
    let dir = cx.root().join(dir);
    std::fs::create_dir_all(&dir).expect("create the planted directory");
    let path = dir.join(name);
    std::fs::write(&path, body).expect("write the planted binary");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755))
            .expect("make the planted binary executable");
    }
    path
}

fn prepend_path(cx: &mut Instance, dir: &Path) {
    let inherited = std::env::var("PATH").unwrap_or_default();
    cx.set_env("PATH", format!("{}:{inherited}", dir.display()));
}

// ================================================== `hitl trust`: what it writes

#[test]
fn trust_records_the_absolute_real_path_and_its_sha256() {
    let mut cx = instance("trust-records");
    // A toolchain reached through a symlink, which is the common shape: the
    // name on PATH is not the file that runs.
    let real = plant(&mut cx, "toolchain", "probe-real", "#!/bin/sh\necho real\n");
    let link = cx.root().join("bin");
    std::fs::create_dir_all(&link).expect("create bin");
    std::os::unix::fs::symlink(&real, link.join("probe")).expect("symlink the name onto the file");
    prepend_path(&mut cx, &link);

    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "probe",
    ])
    .ok()
    .expect_stdout("declared ")
    .expect_stdout(&real.display().to_string());

    let block = trusted_binaries(&policy);
    let real_key = real.display().to_string();
    assert_eq!(
        block["hashes"][&real_key],
        sha256_of(&real),
        "the declaration is keyed by the real file and carries its digest, not the symlink's: {block}"
    );
    assert!(
        block["hashes"]
            .as_object()
            .expect("hashes is an object")
            .get(&link.join("probe").display().to_string())
            .is_none(),
        "the name on PATH is never the key — a declared symlink would never match: {block}"
    );
}

#[test]
fn trust_by_absolute_path_declares_that_file_without_consulting_path() {
    let mut cx = instance("trust-abspath");
    let tool = plant(&mut cx, "toolchain", "byhand", "#!/bin/sh\nexit 0\n");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();

    assert_eq!(
        trusted_binaries(&policy)["hashes"][tool.display().to_string()],
        sha256_of(&tool),
        "an absolute path is declared as given"
    );
}

#[test]
fn trust_refuses_a_name_that_resolves_to_nothing_and_writes_no_policy() {
    let cx = instance("trust-unresolvable");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    let before = read(&policy);

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "no-such-command-anywhere",
    ])
    .expect_failure()
    .expect_stderr("cannot declare \"no-such-command-anywhere\"");

    assert_eq!(
        read(&policy),
        before,
        "a declaration that could not be resolved leaves the file alone"
    );
}

#[test]
fn trust_splices_the_block_in_and_disturbs_no_other_byte() {
    let mut cx = instance("trust-splice");
    let tool = plant(&mut cx, "toolchain", "spliced", "#!/bin/sh\nexit 0\n");
    // Deliberately idiosyncratic: a comment key, an odd indent, a trailing
    // newline. All of it has to survive.
    let original = "{\n\t\"//\": \"hand written, and it stays that way\",\n\t\"version\": 1,\n\t\"default_action\": \"approve\",\n\t\"rules\": []\n}\n";
    let policy = write_policy(&cx, "pinned", original);

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();

    let after = read(&policy);
    assert!(
        after.starts_with('{'),
        "the document is still an object opened at byte 0:\n{after}"
    );
    assert!(
        after.ends_with(&original[1..]),
        "every byte after the opening brace is exactly what it was; only the new member was spliced in front of it:\n{after}"
    );
    assert!(
        after.contains("hand written, and it stays that way"),
        "including a comment key no schema knows about:\n{after}"
    );
}

#[test]
fn a_second_declaration_replaces_the_block_and_still_disturbs_nothing_else() {
    let mut cx = instance("trust-splice-again");
    let first = plant(&mut cx, "toolchain", "first", "#!/bin/sh\nexit 0\n");
    let second = plant(&mut cx, "toolchain", "second", "#!/bin/sh\nexit 1\n");
    let original = "{\n  \"//\": \"keep me\",\n  \"version\": 1,\n  \"rules\": []\n}\n";
    let policy = write_policy(&cx, "pinned", original);

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &first.display().to_string(),
    ])
    .ok();
    let once = read(&policy);
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &second.display().to_string(),
    ])
    .ok();
    let twice = read(&policy);

    let block = trusted_binaries(&policy);
    assert_eq!(
        block["hashes"][first.display().to_string()],
        sha256_of(&first)
    );
    assert_eq!(
        block["hashes"][second.display().to_string()],
        sha256_of(&second)
    );
    assert!(
        twice.ends_with(&original[1..]) && once.ends_with(&original[1..]),
        "the rest of the document is untouched by either write:\n{twice}"
    );
}

#[test]
fn trust_refuses_to_write_a_policy_this_build_would_not_load() {
    let mut cx = instance("trust-validates");
    let tool = plant(&mut cx, "toolchain", "novalidate", "#!/bin/sh\nexit 0\n");
    // A rule naming an action no build accepts: valid JSON, invalid policy.
    let broken = "{\"version\":1,\"rules\":[{\"tools\":\"local_shell\",\"tool\":\"*\",\"action\":\"maybe\"}]}";
    let policy = write_policy(&cx, "broken", broken);

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-broken.json",
        &tool.display().to_string(),
    ])
    .expect_failure()
    .expect_stderr("the result would not validate");

    assert_eq!(
        read(&policy),
        broken,
        "a policy that would not load is never written: a broken envelope falls back to approve-everything"
    );
}

// ==================================================== `hitl trust`: the verbs

#[test]
fn list_reports_every_declaration_and_changes_nothing() {
    let mut cx = instance("trust-list");
    let tool = plant(&mut cx, "toolchain", "listed", "#!/bin/sh\nexit 0\n");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();
    let declared = read(&policy);

    let listed = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-pinned.json",
            "--list",
        ])
        .ok();
    assert!(
        listed.stdout.contains(&policy.display().to_string()),
        "--list names the file it read:\n{}",
        listed.stdout
    );
    assert!(
        listed.stdout.contains(&format!("ok   {}", tool.display())),
        "and reports the entry's state on this host:\n{}",
        listed.stdout
    );
    assert!(
        listed.stdout.contains("dirs: (none declared"),
        "including the half that was left empty:\n{}",
        listed.stdout
    );
    assert_eq!(
        read(&policy),
        declared,
        "--list is a read: not one byte of the policy moves"
    );
}

#[test]
fn list_reports_a_declaration_that_no_longer_matches_the_host() {
    let mut cx = instance("trust-list-drift");
    let tool = plant(&mut cx, "toolchain", "drifting", "#!/bin/sh\nexit 0\n");
    write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();

    std::fs::write(&tool, "#!/bin/sh\necho upgraded\n").expect("the binary changes underneath");

    let listed = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-pinned.json",
            "--list",
        ])
        .ok();
    assert!(
        listed.stdout.contains("BAD ")
            && listed.stdout.contains("does not match the declared hash"),
        "a drifted entry is named, not quietly listed as fine:\n{}",
        listed.stdout
    );
}

#[test]
fn refresh_rewrites_a_changed_digest_and_reports_the_change() {
    let mut cx = instance("trust-refresh");
    let tool = plant(&mut cx, "toolchain", "upgrading", "#!/bin/sh\nexit 0\n");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();
    let old = sha256_of(&tool);

    std::fs::write(&tool, "#!/bin/sh\necho the legitimate upgrade\n").expect("upgrade the binary");
    let new = sha256_of(&tool);
    assert_ne!(old, new, "the fixture must actually change the bytes");

    let refreshed = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-pinned.json",
            "--refresh",
        ])
        .ok();
    assert!(
        refreshed
            .stdout
            .contains(&format!("refreshed {}", tool.display()))
            && refreshed.stdout.contains(&new),
        "--refresh reports each change so the diff is reviewable before it is committed:\n{}",
        refreshed.stdout
    );
    assert_eq!(
        trusted_binaries(&policy)["hashes"][tool.display().to_string()],
        new,
        "and the new digest is what the file now declares"
    );
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "--list",
    ])
    .ok()
    .expect_stdout(&format!("ok   {}", tool.display()));
}

#[test]
fn refresh_keeps_a_declaration_it_can_no_longer_read() {
    let mut cx = instance("trust-refresh-missing");
    let tool = plant(&mut cx, "toolchain", "vanishing", "#!/bin/sh\nexit 0\n");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &tool.display().to_string(),
    ])
    .ok();
    let declared = sha256_of(&tool);
    std::fs::remove_file(&tool).expect("the binary goes away");

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "--refresh",
    ])
    .ok()
    .expect_stdout(&format!("kept {}", tool.display()));

    assert_eq!(
        trusted_binaries(&policy)["hashes"][tool.display().to_string()],
        declared,
        "a declaration whose file is gone is kept, not silently dropped — dropping it would widen the allow"
    );
}

#[test]
fn remove_drops_a_declaration_and_says_so_when_there_was_none() {
    let mut cx = instance("trust-remove");
    let tool = plant(&mut cx, "toolchain", "removable", "#!/bin/sh\nexit 0\n");
    let policy = write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");
    let path = tool.display().to_string();
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &path,
    ])
    .ok();

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "--remove",
        &path,
    ])
    .ok()
    .expect_stdout(&format!("removed {path}"));
    assert!(
        trusted_binaries(&policy)["hashes"][&path].is_null(),
        "the declaration is gone: {}",
        trusted_binaries(&policy)
    );

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        "--remove",
        &path,
    ])
    .ok()
    .expect_stdout(&format!("not declared: {path}"));
}

#[test]
fn trust_with_nothing_to_do_says_so_rather_than_succeeding_silently() {
    let cx = instance("trust-noop");
    write_policy(&cx, "pinned", "{\"version\":1,\"rules\":[]}");

    cx.run(["hitl", "trust", "--policy", "hitl-policy-pinned.json"])
        .expect_failure()
        .expect_stderr(
            "nothing to do: name at least one command or path, or pass --refresh or --list",
        );
}

#[test]
fn trust_names_the_search_path_when_the_policy_is_not_on_it() {
    let cx = instance("trust-nopolicy");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-nowhere.json",
        "--list",
    ])
    .expect_failure()
    .expect_stderr("policy \"hitl-policy-nowhere.json\" not found on the search path");
}

// ============================================== the rendered-envelope refusal

#[test]
fn trust_refuses_a_rendered_envelope_and_names_the_copy_to_make() {
    let mut cx = instance("trust-generated");
    let tool = plant(&mut cx, "toolchain", "generated", "#!/bin/sh\nexit 0\n");
    let rendered = cx.home_file(".generated/hitl-policy-default.json");
    let before = read(&rendered);

    let refused = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-default.json",
            &tool.display().to_string(),
        ])
        .expect_failure();

    let told = refused.stderr.clone();
    assert!(
        told.contains(&rendered.display().to_string()),
        "the refusal names the file it would have written:\n{told}"
    );
    assert!(
        told.contains("[envelopes.default] in agents.toml")
            && told.contains("rewritten on every run")
            && told.contains("silently discarded"),
        "and says why a hash declared there would not survive:\n{told}"
    );
    assert!(
        told.contains("Copy it to ") && told.contains("hitl-policy-default.json"),
        "and names the copy to make instead:\n{told}"
    );
    assert_eq!(
        read(&rendered),
        before,
        "the render itself is left exactly as the transpiler wrote it"
    );
}

#[test]
fn list_reads_a_rendered_envelope_because_reading_one_discards_nothing() {
    let cx = instance("trust-generated-list");
    let rendered = cx.home_file(".generated/hitl-policy-default.json");

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-default.json",
        "--list",
    ])
    .ok()
    .expect_stdout(&rendered.display().to_string())
    .expect_stdout("hashes: (none declared");
}

#[test]
fn the_copy_the_refusal_names_is_where_trust_then_writes() {
    let mut cx = instance("trust-generated-copy");
    let tool = plant(&mut cx, "toolchain", "copied", "#!/bin/sh\nexit 0\n");
    let refused = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-default.json",
            &tool.display().to_string(),
        ])
        .expect_failure();

    let named = named_copy(&refused.stderr);
    std::fs::create_dir_all(named.parent().expect("the copy has a parent"))
        .expect("create the named directory");
    std::fs::copy(cx.home_file(".generated/hitl-policy-default.json"), &named)
        .expect("make the copy the message asked for");

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-default.json",
        &tool.display().to_string(),
    ])
    .ok()
    .expect_stdout(&format!("updated {}", named.display()));

    assert_eq!(
        trusted_binaries(&named)["hashes"][tool.display().to_string()],
        sha256_of(&tool),
        "following the instruction verbatim lands the declaration in the named file"
    );
}

/// The path the refusal tells the operator to copy the render to.
fn named_copy(stderr: &str) -> PathBuf {
    let marker = "Copy it to ";
    let start = stderr
        .find(marker)
        .unwrap_or_else(|| panic!("the refusal names no copy to make:\n{stderr}"))
        + marker.len();
    let rest = &stderr[start..];
    let end = rest.find(" first").unwrap_or_else(|| {
        panic!("the refusal's copy clause is not the documented shape:\n{stderr}")
    });
    PathBuf::from(rest[..end].trim())
}

// ================================================ the gate: what an allow means
//
// From here on the subject is the evaluator, not the verb. Each case writes a
// policy whose FIRST rule grants `local_shell` unattended on a prefix
// allowlist, and whose SECOND stops it for a human. Which of the two catches a
// call is the whole observation: no card means the allow held, a card means it
// was withdrawn — and the card carries a reason a model and an operator can
// both act on.
//
// The two subject commands are chosen so neither outcome could have come from
// the shipped default envelope instead: `printf` is on no shipped allowlist
// (the default stops it), and `echo` is on all of them (the default runs it).
// So an unattended `printf` and a card for `echo` can only be this policy's
// doing — and every card is checked for the name of the file that raised it.

const ALLOWED: &str = "printf";
const NOT_ALLOWED: &str = "echo";

/// rule 0 grants the allowlist; rule 1 is the approve floor a withdrawn allow
/// falls to. `default_action` stays out of the way of the rest of the chain.
fn gate_policy(allowlist: &str, trusted: Option<Value>) -> String {
    let mut doc = json!({
        "version": 1,
        "default_action": "allow",
        "rules": [
            {
                "tools": "local_shell",
                "tool": "local_shell",
                "action": "allow",
                "when": [{"key": "command", "op": "command_prefix_allowlist", "value": allowlist}]
            },
            {"tools": "local_shell", "tool": "local_shell", "action": "approve"}
        ]
    });
    if let Some(block) = trusted {
        doc["trusted_binaries"] = block;
    }
    serde_json::to_string_pretty(&doc).expect("the policy always serialises")
}

/// The one editor turn: make the shell call under test, then answer.
fn shell_turn(call: ToolCall) -> Script {
    Script::new().route("general").turns([
        Turn::new().text("Running it.").call(call),
        Turn::new().text("That is what came back."),
    ])
}

struct Gated {
    asked: bool,
    policy: String,
    detail: String,
    told: String,
    ran: Vec<String>,
}

impl Gated {
    /// A card is only evidence about the policy under test if that policy is
    /// the one that raised it.
    fn raised_by(&self, policy: &str) -> &Self {
        assert!(self.asked, "no card was raised at all");
        assert_eq!(
            self.policy,
            format!("hitl-policy-{policy}.json"),
            "the card must name the envelope under test, or it is evidence about some other file"
        );
        self
    }
}

/// Fire one `local_shell` call at a policy and report what a person saw. The
/// client refuses every card, so a withdrawn allow never runs the command.
fn gated(cx: &Instance, policy: &str, call: ToolCall) -> Gated {
    cx.scripted(&shell_turn(call)).expect("scripted backend");
    let mut acp: Acp = cx
        .acp(["acp", "--hitl-policy", policy])
        .expect("spawn the ACP surface");
    acp.answers(Verdict::Deny);
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "run it").expect("prompt");
    acp.close().expect("the surface exits with its client");

    let ask = turn.permissions.first().cloned().unwrap_or(Value::Null);
    Gated {
        asked: turn.asked_permission(),
        policy: text_at(&ask["toolCall"]["_meta"]["policyName"]),
        detail: text_at(&ask["toolCall"]["_meta"]["detail"]),
        told: turn.tool_outputs(),
        ran: turn.terminal_commands(),
    }
}

fn text_at(value: &Value) -> String {
    value.as_str().unwrap_or_default().to_string()
}

#[test]
fn a_command_on_the_prefix_allowlist_runs_without_asking() {
    let cx = instance("gate-allow");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["allowlisted-and-unattended"])),
    );

    assert!(
        !seen.asked,
        "a matched prefix allow runs the call and asks nobody — and no shipped envelope allowlists this command, so only this policy could have granted it"
    );
    assert_eq!(
        seen.ran,
        vec![format!("{ALLOWED} allowlisted-and-unattended")],
        "and the command reaches the client's terminal"
    );
}

#[test]
fn a_command_off_the_prefix_allowlist_stops_at_the_approve_floor() {
    let cx = instance("gate-offlist");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", NOT_ALLOWED)
            .arg("args", json!(["never-on-this-allowlist"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.ran.is_empty(),
        "a command this allowlist never named is not unattended work, and the refused card means it never ran: {:?}",
        seen.ran
    );
}

#[test]
fn a_control_character_in_an_argument_takes_the_allowlist_off_the_table() {
    let cx = instance("gate-control");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    // The program word still reads `printf`; a second command is smuggled into
    // the arguments, where only a shell would ever separate it out.
    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["hello;", "id"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.ran.is_empty(),
        "a token carrying a shell control character can never match a prefix — the allowlist pins argv, not a line: {:?}",
        seen.ran
    );
}

#[test]
fn a_substitution_in_an_argument_takes_the_allowlist_off_the_table() {
    let cx = instance("gate-substitution");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["$(id -u)"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.ran.is_empty(),
        "an argument that a shell would evaluate before the program ever saw it is not the call the allowlist read: {:?}",
        seen.ran
    );
}

#[test]
fn shell_mode_is_refused_outright_and_never_reaches_the_allowlist() {
    let cx = instance("gate-shellmode");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg(
                "command",
                format!("{ALLOWED} allowlisted-but-through-a-shell"),
            )
            .arg("shell", true),
    );

    assert!(
        seen.told.contains("'shell: true' is strictly forbidden"),
        "shell mode is settled by the toolset before any policy is consulted:\n{}",
        seen.told
    );
    assert!(
        !seen.asked && seen.ran.is_empty(),
        "so there is no card to answer and nothing runs: asked={} ran={:?}",
        seen.asked,
        seen.ran
    );
}

#[test]
fn a_whole_shell_line_in_the_command_word_is_refused_a_layer_before_the_policy() {
    let cx = instance("gate-control-line");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell").arg("command", format!("{ALLOWED} hello; id")),
    );

    assert!(
        seen.told
            .contains("is not in this chain's allowed commands")
            && seen.told.contains("no approval can grant it"),
        "the command word is a program name, not a line: the toolset settles it and says so:\n{}",
        seen.told
    );
    assert!(
        !seen.asked && seen.ran.is_empty(),
        "so nothing is put in front of a person and nothing runs: asked={} ran={:?}",
        seen.asked,
        seen.ran
    );
}

// ================================ the gate: identity and integrity of the binary
//
// `command_prefix_allowlist` pins a NAME. These cases are the gap that leaves,
// and the two declarations that close it.

/// Rewrite one declared digest to something the file will never hash to — the
/// binary changing underneath a declaration, without waiting for an upgrade.
fn corrupt_digest(policy: &Path) {
    let text = read(policy);
    let mut doc: Value = serde_json::from_str(&text).expect("the policy is JSON");
    let hashes = doc["trusted_binaries"]["hashes"]
        .as_object_mut()
        .expect("something was declared");
    let key = hashes
        .keys()
        .next()
        .expect("at least one declaration")
        .clone();
    hashes.insert(key, Value::String("0".repeat(64)));
    std::fs::write(
        policy,
        serde_json::to_string_pretty(&doc).expect("re-encode"),
    )
    .expect("rewrite");
}

#[test]
fn an_empty_trusted_binaries_block_withdraws_nothing() {
    let cx = instance("trust-gate-inert");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, Some(json!({}))));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["nothing-declared-nothing-withdrawn"])),
    );

    assert!(
        !seen.asked,
        "a policy that declares neither half behaves exactly as it did before this existed"
    );
    assert_eq!(
        seen.ran,
        vec![format!("{ALLOWED} nothing-declared-nothing-withdrawn")]
    );
}

#[test]
fn a_declared_binary_that_still_matches_keeps_its_allow() {
    let cx = instance("trust-gate-ok");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        ALLOWED,
    ])
    .ok();

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["still-the-declared-file"])),
    );

    assert!(
        !seen.asked,
        "the declaration holds, so the allow holds: {}",
        seen.detail
    );
    assert_eq!(seen.ran, vec![format!("{ALLOWED} still-the-declared-file")]);
}

#[test]
fn a_declared_binary_that_no_longer_matches_loses_its_allow_and_the_card_says_why() {
    let cx = instance("trust-gate-mismatch");
    let policy = write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        ALLOWED,
    ])
    .ok();
    corrupt_digest(&policy);

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["not-the-file-that-was-declared"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.detail.contains(
            "does not match the declared hash — re-declare after verifying the upgrade, or investigate"
        ),
        "a withdrawn allow stops for a human, and the card carries the reason rather than appearing unexplained: {:?}",
        seen.detail
    );
    assert!(seen.ran.is_empty(), "nothing ran: {:?}", seen.ran);
}

#[test]
fn declaring_any_hash_makes_the_pin_strict_for_the_whole_policy() {
    let mut cx = instance("trust-gate-strict");
    let other = plant(&mut cx, "toolchain", "unrelated", "#!/bin/sh\nexit 0\n");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    // One declaration, for something else entirely.
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        &other.display().to_string(),
    ])
    .ok();

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["allowlisted-but-undeclared"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.detail
            .contains("has no declared hash — allow refused; declare it with `contenox hitl trust"),
        "a partial pin that waved everything undeclared through would pin nothing, and the card names the verb that would declare it: {:?}",
        seen.detail
    );
}

#[test]
fn a_binary_planted_on_path_is_refused_by_the_hash_pin_alone() {
    let mut cx = instance("trust-gate-planted-hash");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    // Declared while the name still means what it should.
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        ALLOWED,
    ])
    .ok();

    // Then something the agent can write to lands earlier on PATH.
    let planted = plant(
        &mut cx,
        "toolbox",
        ALLOWED,
        "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
    );
    prepend_path(&mut cx, planted.parent().expect("the planted directory"));

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["the-name-was-blessed-not-this-file"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.detail.contains(&planted.display().to_string())
            && seen.detail.contains("has no declared hash"),
        "declaring only hashes still stops PATH substitution — the substituted binary has no declared hash — and the card names the file that actually resolved: {:?}",
        seen.detail
    );
    assert!(seen.ran.is_empty(), "nothing ran: {:?}", seen.ran);
}

#[test]
fn a_binary_outside_every_trusted_dir_is_refused_before_its_hash_is_consulted() {
    let mut cx = instance("trust-gate-dirs");
    let planted = plant(
        &mut cx,
        "toolbox",
        ALLOWED,
        "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
    );
    prepend_path(&mut cx, planted.parent().expect("the planted directory"));
    // The operator declared the neighbourhood a toolchain may come from, and
    // the planted directory is not in it.
    write_policy(
        &cx,
        "pinned",
        &gate_policy(ALLOWED, Some(json!({"dirs": ["/usr/bin", "/bin"]}))),
    );

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["planted-in-a-directory-nobody-trusts"])),
    );

    seen.raised_by("pinned");
    assert!(
        seen.detail.contains(&planted.display().to_string())
            && seen.detail.contains(
                "is not under any trusted_binaries.dirs entry — allow refused; declare its directory after verifying what it is"
            ),
        "the identity half refuses a whole neighbourhood, and says which directory it came from: {:?}",
        seen.detail
    );
    assert!(seen.ran.is_empty(), "nothing ran: {:?}", seen.ran);
}

#[test]
fn the_verb_declares_exactly_what_the_evaluator_will_look_up() {
    let mut cx = instance("trust-gate-agree");
    let planted = plant(
        &mut cx,
        "toolbox",
        ALLOWED,
        "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
    );
    prepend_path(&mut cx, planted.parent().expect("the planted directory"));
    let policy = write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));

    // Declared through the same PATH the evaluator resolves through.
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        ALLOWED,
    ])
    .ok();
    assert_eq!(
        trusted_binaries(&policy)["hashes"][planted.display().to_string()],
        sha256_of(&planted),
        "the verb wrote the file the name resolves to here, not the one it resolves to elsewhere"
    );

    let seen = gated(
        &cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["declared-and-therefore-allowed"])),
    );

    assert!(
        !seen.asked,
        "and the evaluator then accepts it, which is what 'resolved exactly as the evaluator resolves it' means: {}",
        seen.detail
    );
}

#[test]
fn a_top_level_copy_shadows_the_render_and_is_the_file_that_gates() {
    let cx = instance("trust-shadow-global");
    // The same file name as the render, one directory up, where the search
    // path says a hand-written copy outranks a transpiled one.
    write_policy(&cx, "default", &gate_policy(ALLOWED, None));
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-default.json",
        ALLOWED,
    ])
    .ok()
    .expect_stdout(&format!(
        "updated {}",
        cx.home_file("hitl-policy-default.json").display()
    ));

    let seen = gated(
        &cx,
        "default",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["granted-by-the-copy-not-the-render"])),
    );

    assert!(
        !seen.asked,
        "the shipped render stops this command at its approve floor; the copy above it grants it, so the copy is what gated: {}",
        seen.detail
    );
}

#[test]
#[ignore = "confirmed defect: the refusal names a copy nothing will ever read. `hitl trust` builds the sentence with ResolveContenoxDir(cmd) — the nearest WORKSPACE .contenox — so it says `Copy it to <workspace>/.contenox/hitl-policy-default.json`. That path is first on the verb's own search path (hitl_policy_staleness.go policyDirs), so the declaration is written there and reported as `updated`, and then no evaluating surface ever reads it: acp/acpx/beam and the mission unit a run dispatches all build their policy source from globalContenoxDir(), the same seam tests/envelopes.rs already documents. The instruction should name <home>/.contenox/hitl-policy-default.json, which is both on the verb's search path and the file the gate reads — see a_top_level_copy_shadows_the_render_and_is_the_file_that_gates just above, which is the same case with the global path substituted and passes."]
fn the_copy_the_refusal_names_is_the_file_that_then_gates() {
    let cx = instance("trust-generated-gates");
    let refused = cx
        .run([
            "hitl",
            "trust",
            "--policy",
            "hitl-policy-default.json",
            ALLOWED,
        ])
        .expect_failure();
    let named = named_copy(&refused.stderr);
    std::fs::create_dir_all(named.parent().expect("the copy has a parent"))
        .expect("create the named directory");

    // The copy the message asked for, carrying an allow the render does not:
    // the render stops `printf` at its approve floor.
    std::fs::write(&named, gate_policy(ALLOWED, None)).expect("write the copy");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-default.json",
        ALLOWED,
    ])
    .ok()
    .expect_stdout(&format!("updated {}", named.display()));

    let seen = gated(
        &cx,
        "default",
        ToolCall::new("local_shell")
            .arg("command", ALLOWED)
            .arg("args", json!(["declared-in-the-copy-we-were-told-to-make"])),
    );

    assert!(
        !seen.asked,
        "the copy the refusal named must be the one the gate reads, or the instruction leads nowhere: {}",
        seen.detail
    );
}

// ============================================== the shell environment: the scrub
//
// `contenox sandbox env` is a dry run of the filter every agent-reachable
// shell is built with, evaluated against THIS process's environment. Each case
// hands the preview a marked environment and reads back which names survived.
// A value must never appear — that is the whole point of a preview you can
// paste into a ticket.

const PLAIN: &str = "CONFINEMENT_PROBE_PLAIN";
const PLAIN_VALUE: &str = "a-value-no-preview-may-print";
const SECRET: &str = "CONFINEMENT_PROBE_TOKEN";
const SECRET_VALUE: &str = "a-credential-no-shell-may-see";

/// `contenox sandbox env` run against a marked environment. `extra` sets the
/// scrub's own knobs; the three marked variables are always present.
fn preview(cx: &Instance, args: &[&str], extra: &[(&str, &str)]) -> String {
    let mut cmd = cx
        .cmd(["sandbox", "env"])
        .args(args)
        .env(PLAIN, PLAIN_VALUE)
        .env(SECRET, SECRET_VALUE)
        .env("CONTENOX_CONTROL_PLANE_PROBE", "control-plane-value");
    for (key, value) in extra {
        cmd = cmd.env(key, value);
    }
    cmd.output()
        .expect("contenox sandbox env")
        .ok()
        .stdout
        .clone()
}

fn lists(preview: &str, name: &str) -> bool {
    preview.lines().any(|line| line.trim() == name)
}

#[test]
fn sandbox_env_prints_names_and_never_a_value() {
    let cx = instance("sandbox-env-names");
    let shown = preview(&cx, &[], &[]);

    assert!(
        lists(&shown, PLAIN),
        "a variable an agent shell inherits is named:\n{shown}"
    );
    assert!(
        !shown.contains(PLAIN_VALUE) && !shown.contains(SECRET_VALUE),
        "and no value is ever printed, so the output is safe to share:\n{shown}"
    );
    assert!(
        shown.contains("values withheld"),
        "the preview says so in as many words:\n{shown}"
    );
}

#[test]
fn the_default_agent_shell_policy_drops_credentials_and_the_control_plane() {
    let cx = instance("sandbox-env-default");
    let shown = preview(&cx, &[], &[]);

    assert!(
        shown.contains("agent shells (local_shell, shell_session) — scrub mode: deny-secrets"),
        "agent-reachable shells scrub by default, because the agent is untrusted:\n{shown}"
    );
    assert!(
        !lists(&shown, SECRET),
        "a *_TOKEN never reaches a shell an agent can drive:\n{shown}"
    );
    assert!(
        !lists(&shown, "CONTENOX_CONTROL_PLANE_PROBE"),
        "and neither does the control plane's own configuration:\n{shown}"
    );
    assert!(
        lists(&shown, PLAIN),
        "while the toolchain keeps the environment it expects:\n{shown}"
    );
}

#[test]
fn the_terminal_flag_shows_the_other_policy_and_its_other_default() {
    let cx = instance("sandbox-env-terminal");
    let shown = preview(&cx, &["--terminal"], &[]);

    assert!(
        shown.contains("interactive terminal — scrub mode: off"),
        "the operator's own shell is a different surface with a different default:\n{shown}"
    );
    assert!(
        lists(&shown, SECRET),
        "which is what `off` means — and why the flag exists to show it:\n{shown}"
    );
    assert!(
        !shown.contains(SECRET_VALUE),
        "values stay withheld on this policy too:\n{shown}"
    );
}

#[test]
fn strict_mode_hands_a_shell_only_the_safe_base_set() {
    let cx = instance("sandbox-env-strict");
    let shown = preview(&cx, &[], &[("SANDBOX_SHELL_SCRUB", "strict")]);

    assert!(
        shown.contains("scrub mode: strict"),
        "the header names the mode in force:\n{shown}"
    );
    for base in ["PATH", "TERM"] {
        assert!(
            lists(&shown, base),
            "the safe base set is what a shell still needs to be a shell: {base}\n{shown}"
        );
    }
    assert!(
        !lists(&shown, PLAIN),
        "and everything else is absent, not merely credential-shaped:\n{shown}"
    );
}

#[test]
fn the_allow_list_readmits_a_variable_strict_mode_would_have_dropped() {
    let cx = instance("sandbox-env-allow");
    let shown = preview(
        &cx,
        &[],
        &[
            ("SANDBOX_SHELL_SCRUB", "strict"),
            ("SANDBOX_ENV_ALLOW", PLAIN),
        ],
    );

    assert!(
        lists(&shown, PLAIN),
        "naming a variable passes through the one the process already has:\n{shown}"
    );
    assert!(
        !lists(&shown, SECRET),
        "and grants nothing it was not asked for:\n{shown}"
    );
}

#[test]
fn the_deny_list_strips_a_variable_the_default_mode_would_have_kept() {
    let cx = instance("sandbox-env-deny");
    let shown = preview(&cx, &[], &[("SANDBOX_ENV_DENY", PLAIN)]);

    assert!(
        !lists(&shown, PLAIN),
        "an explicit deny wins over the default posture:\n{shown}"
    );
}

#[test]
fn a_trailing_wildcard_denies_a_whole_family_of_names() {
    let cx = instance("sandbox-env-glob");
    let shown = preview(&cx, &[], &[("SANDBOX_ENV_DENY", "CONFINEMENT_PROBE_*")]);

    assert!(
        !lists(&shown, PLAIN) && !lists(&shown, SECRET),
        "a single trailing wildcard matches the family, not just one name:\n{shown}"
    );
    assert!(lists(&shown, "PATH"), "and nothing outside it:\n{shown}");
}

// ============================================= the shell environment: injection

#[test]
fn shell_env_set_list_and_unset_round_trip() {
    let cx = instance("shell-env-crud");

    cx.run(["shell-env", "list"])
        .ok()
        .expect_stdout("# no global shell-env variables set");

    cx.run([
        "shell-env",
        "set",
        "HTTP_PROXY=http://proxy:3128",
        "GOCACHE=/var/cache/go",
    ])
    .ok()
    .expect_stdout("HTTP_PROXY=http://proxy:3128")
    .expect_stdout("GOCACHE=/var/cache/go");

    cx.run(["shell-env", "list"])
        .ok()
        .expect_stdout("GOCACHE=/var/cache/go")
        .expect_stdout("HTTP_PROXY=http://proxy:3128");

    cx.run(["shell-env", "unset", "HTTP_PROXY"])
        .ok()
        .expect_stdout("GOCACHE=/var/cache/go")
        .refute_stdout("HTTP_PROXY");
}

#[test]
fn shell_env_refuses_an_argument_that_is_not_a_variable_assignment() {
    let cx = instance("shell-env-bad-args");

    cx.run(["shell-env", "set", "NOEQUALSHERE"])
        .expect_failure()
        .expect_stderr("is not KEY=VALUE");
    cx.run(["shell-env", "set", "9NOTANAME=x"])
        .expect_failure()
        .expect_stderr("is not a valid variable name");
    cx.run(["shell-env", "list"])
        .ok()
        .expect_stdout("# no global shell-env variables set");
}

#[test]
fn an_injected_variable_is_layered_on_top_of_even_the_strictest_scrub() {
    let cx = instance("shell-env-layered");
    cx.run([
        "shell-env",
        "set",
        &format!("{PLAIN}=injected-not-inherited"),
    ])
    .ok();

    let shown = preview(&cx, &[], &[("SANDBOX_SHELL_SCRUB", "strict")]);

    assert!(
        lists(&shown, PLAIN),
        "strict mode would have dropped this name; the injection puts it back:\n{shown}"
    );
    assert!(
        !shown.contains("injected-not-inherited"),
        "and the preview still withholds the value:\n{shown}"
    );
}

#[test]
fn the_preview_and_the_injection_are_read_from_different_commands() {
    let cx = instance("shell-env-two-views");
    cx.run(["shell-env", "set", "CONFINEMENT_INJECTED=on-purpose"])
        .ok();

    cx.run(["shell-env", "list"])
        .ok()
        .expect_stdout("CONFINEMENT_INJECTED=on-purpose");
    assert!(
        lists(&preview(&cx, &[], &[]), "CONFINEMENT_INJECTED"),
        "`shell-env list` shows what you inject and its value; `sandbox env` shows only that the name arrives"
    );
}

// ================================ the shell environment: the shell that is spawned
//
// Everything above is the preview. This is the shell itself — and the shape
// matters. Under `contenox acp` the agent's shell is the CLIENT's terminal:
// contenox forwards `terminal/create` and never spawns anything, so no scrub
// of its own applies (docs/guide/confinement/sandbox.md says exactly this).
// Under `contenox beam`, beam IS the client, so beam spawns the process and
// the composed scrub-and-inject is what that process gets. `contenox run` and
// `contenox serve` have no client at all and serve no shell.
//
// So beam is the one shape where the promise is observable end to end, and
// what it inherited is read back through `contenox session show`.

const BEAM_READY: &str = "type / for commands";
const BEAM_DECISION: &str = "y allow";

/// Everything the surface told its model, read back through the product.
fn transcript(cx: &Instance) -> String {
    let rows = cx.sessions_all().expect("contenox session list --all");
    let row = rows.last().expect("the surface recorded a session");
    let shown = cx.session_show(&row.id);
    assert!(
        shown.success(),
        "contenox session show {} failed\n{}",
        row.id,
        shown.render()
    );
    shown.stdout
}

#[test]
fn the_shell_beam_spawns_takes_the_injected_value_over_the_inherited_one() {
    let mut cx = instance("shell-env-beam");
    cx.run([
        "shell-env",
        "set",
        &format!("{PLAIN}=injected-and-therefore-winning"),
    ])
    .ok();
    // The same name in beam's own environment, with a different value, plus a
    // credential and a control-plane variable that must not travel.
    cx.set_env(PLAIN, "inherited-and-therefore-losing");
    cx.set_env(SECRET, SECRET_VALUE);
    cx.set_env("CONTENOX_CONTROL_PLANE_PROBE", "control-plane-value");

    cx.scripted(
        &Script::new()
            .route("general")
            .turn(
                Turn::new()
                    .text("Reading the environment.")
                    .call(ToolCall::new("local_shell").arg("command", "env")),
            )
            .turn("That is the environment I was given."),
    )
    .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(BEAM_READY, Duration::from_secs(90))
        .expect("beam's composer");
    pty.send_line("what environment do you have?")
        .expect("submit the prompt");
    pty.wait_for(BEAM_DECISION, Duration::from_secs(120))
        .expect("the approval card for the shell call");
    pty.send("y").expect("allow the command");
    pty.wait_for(
        "That is the environment I was given.",
        Duration::from_secs(120),
    )
    .expect("the turn runs to its end");
    pty.interrupt();
    pty.wait_exit(Duration::from_secs(60)).ok();

    // The preview, run against the very same environment, has to be saying the
    // same thing — that is the whole claim behind offering a preview at all.
    let shown = cx.run(["sandbox", "env"]).ok().stdout;
    assert!(
        lists(&shown, PLAIN) && !lists(&shown, SECRET),
        "the preview's account of this environment:\n{shown}"
    );

    let told = transcript(&cx);
    assert!(
        told.contains(&format!("{PLAIN}=injected-and-therefore-winning")),
        "an injected value is layered on top of the scrub, so it wins:\n{told}"
    );
    assert!(
        !told.contains("inherited-and-therefore-losing"),
        "and the inherited value of the same name is not also there:\n{told}"
    );
    assert!(
        !told.contains(SECRET_VALUE),
        "the credential in beam's own environment never reaches the shell it spawns:\n{told}"
    );
    assert!(
        !told.contains("control-plane-value"),
        "and neither does the control plane's own configuration:\n{told}"
    );
}

#[test]
fn the_shell_an_editor_opens_is_the_editors_own_and_takes_no_injection() {
    let cx = instance("shell-env-acp");
    cx.run([
        "shell-env",
        "set",
        &format!("{PLAIN}=injected-and-therefore-winning"),
    ])
    .ok();
    cx.scripted(
        &Script::new().route("general").turns([
            Turn::new()
                .text("Reading the environment.")
                .call(ToolCall::new("local_shell").arg("command", "env")),
            Turn::new().text("That is the environment I was given."),
        ]),
    )
    .expect("scripted backend");

    let mut acp: Acp = cx.acp(["acp", "--auto"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp
        .prompt(&session, "what environment do you have?")
        .expect("prompt");
    acp.close().expect("the surface exits with its client");

    assert!(
        turn.methods().contains(&"terminal/create"),
        "under an editor the shell is the CLIENT's terminal, not a process contenox spawns, got {:?}",
        turn.methods()
    );
    assert!(
        turn.tool_outputs().contains("PATH="),
        "the command really did print an environment, so the absence below means something:\n{}",
        turn.tool_outputs()
    );
    assert!(
        !turn
            .tool_outputs()
            .contains("injected-and-therefore-winning"),
        "and contenox's injection reaches nothing here — the client's own environment is what the command ran in:\n{}",
        turn.tool_outputs()
    );
}

// ================================================ drift, reported where it shows

/// A policy in the workspace `.contenox/`, which is what bare `contenox vet`
/// reads, carrying one declaration that no longer describes this host.
fn drifted_workspace_policy(cx: &mut Instance, file: &str) -> PathBuf {
    let tool = plant(cx, "toolchain", "drifted", "#!/bin/sh\nexit 0\n");
    let policy = cx.work().join(".contenox").join(file);
    std::fs::create_dir_all(policy.parent().expect("the workspace .contenox"))
        .expect("create the workspace policy dir");
    std::fs::write(&policy, gate_policy(ALLOWED, None)).expect("write the workspace policy");
    cx.run([
        "hitl",
        "trust",
        "--policy",
        file,
        &tool.display().to_string(),
    ])
    .ok()
    .expect_stdout(&format!("updated {}", policy.display()));
    std::fs::write(&tool, "#!/bin/sh\necho changed underneath\n").expect("the binary changes");
    policy
}

#[test]
fn vet_warns_about_a_declaration_that_no_longer_describes_this_host() {
    let mut cx = instance("drift-vet");
    let policy = drifted_workspace_policy(&mut cx, "hitl-policy-pinned.json");

    let vetted = cx.run(["vet"]).ok();
    assert!(
        vetted.stdout.contains("WARN") && vetted.stdout.contains("trusted_binaries.hashes:"),
        "drift is reported against the file it is declared in:\n{}",
        vetted.stdout
    );
    assert!(
        vetted.stdout.contains("does not match the declared hash"),
        "and says what stopped holding:\n{}",
        vetted.stdout
    );
    assert!(
        vetted.stdout.contains(&policy.display().to_string()),
        "naming the file:\n{}",
        vetted.stdout
    );
    assert!(
        vetted.success(),
        "a drifted entry is a warning, not a failure: the envelope is still valid and the runtime's answer for it is already a refusal"
    );
}

#[test]
fn doctor_reports_the_drift_and_names_the_verb_that_fixes_it() {
    let mut cx = instance("drift-doctor");
    let policy = drifted_workspace_policy(&mut cx, "hitl-policy-default.json");

    let seen = cx.doctor();
    assert!(
        seen.stdout
            .contains("HITL trusted-binary declaration(s) no longer describe this host"),
        "a stale declaration is invisible from the inside — you see a card for a command that used to run unattended — so doctor says it:\n{}",
        seen.stdout
    );
    assert!(
        seen.stdout.contains(&policy.display().to_string()),
        "naming the file:\n{}",
        seen.stdout
    );
    assert!(
        seen.stdout.contains("contenox hitl trust --refresh"),
        "and the verb that resolves it, once the change is understood:\n{}",
        seen.stdout
    );
}

#[test]
#[ignore = "confirmed defect: `contenox doctor` reports trusted-binary drift only for the six legacy preset FILE NAMES. contenoxcli/hitl_cmd.go trustedBinaryDrift() loops over HITLPolicyPresets — hitl-policy-{default,strict,dev,acp,acpx,oracle}.json — so a declaration in any other policy drifts silently: an operator's own file (`hitl trust --policy ./ops/host.json` is a documented form), and every rendered envelope other than those six, which is most of the shipped set (run, change, chat, reviewer, researcher, triage, serve, read_only, ask_always, auto_edit). `contenox vet` reads every .json on the path it is given and does report it, so the same drift is visible from one command and invisible from the other — while docs/guide/confinement/trusted-binaries.md#how-vet-and-doctor-report-drift says doctor reports 'the same drift'. The fix is to walk the policy files present on policyDirs rather than a fixed preset list."]
fn doctor_reports_drift_in_a_policy_that_is_not_a_shipped_preset_name() {
    let mut cx = instance("drift-doctor-other");
    let policy = drifted_workspace_policy(&mut cx, "hitl-policy-pinned.json");

    let seen = cx.doctor();
    assert!(
        seen.stdout
            .contains("HITL trusted-binary declaration(s) no longer describe this host")
            && seen.stdout.contains(&policy.display().to_string()),
        "a declaration is a declaration whatever the file is called:\n{}",
        seen.stdout
    );
}

#[test]
fn refreshing_the_declaration_clears_the_report_from_both_commands() {
    let mut cx = instance("drift-cleared");
    drifted_workspace_policy(&mut cx, "hitl-policy-default.json");

    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-default.json",
        "--refresh",
    ])
    .ok()
    .expect_stdout("refreshed ");

    let vetted = cx.run(["vet"]).ok();
    assert!(
        !vetted.stdout.contains("trusted_binaries.hashes:"),
        "the legitimate-upgrade path clears the warning:\n{}",
        vetted.stdout
    );
    assert!(
        !cx.doctor()
            .stdout
            .contains("HITL trusted-binary declaration(s) no longer describe this host"),
        "in both places that report it"
    );
}

// ==================================================== the wall, and where it is not
//
// The sandbox wall (Landlock plus, opt-in, a network namespace) confines a
// FOREIGN agent: an external ACP agent contenox spawns as a subprocess. No
// case here reaches it, and that is a property of the shipped product rather
// than of this suite: `external_acp` agents are internal-only — every agent an
// operator can declare is a Markdown file, which is a chain-kind agent — so a
// stock install never spawns a walled process. docs/guide/confinement/sandbox.md
// says the same in its own words. What IS reachable, and what the same page is
// most concerned an operator will get wrong, is the boundary: the wall does not
// confine contenox's own chains, so the shell a beam session runs is an
// ordinary child process with an ordinary view of the disk.

#[test]
fn the_shell_beam_spawns_is_not_walled_off_from_the_rest_of_the_disk() {
    let mut cx = instance("wall-boundary");
    // Asking for the strictest wall there is. It governs foreign agents, and
    // this session is not one.
    cx.set_env("CONTENOX_SANDBOX_NETWORK_WALL", "1");
    let outside = cx.root().join("outside-the-workspace.txt");
    std::fs::write(&outside, "reachable from a shell contenox spawns\n")
        .expect("a file outside the workspace");

    cx.scripted(
        &Script::new()
            .route("general")
            .turn(
                Turn::new().text("Reading it.").call(
                    ToolCall::new("local_shell")
                        .arg("command", "cat")
                        .arg("args", json!([outside.display().to_string()])),
                ),
            )
            .turn("That is what the file says."),
    )
    .expect("scripted backend");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for(BEAM_READY, Duration::from_secs(90))
        .expect("beam's composer");
    pty.send_line("read the file outside the workspace")
        .expect("submit the prompt");
    pty.wait_for("That is what the file says.", Duration::from_secs(120))
        .expect("the turn runs to its end");
    pty.interrupt();
    pty.wait_exit(Duration::from_secs(60)).ok();

    assert!(
        transcript(&cx).contains("reachable from a shell contenox spawns"),
        "the wall confines foreign agent processes, not contenox's own chains: what governs this shell is the approval gate, not a kernel fence — and an operator who assumed otherwise would be wrong:\n{}",
        transcript(&cx)
    );
}

// ============================== the gate: a shell line, when there is a shell
//
// Everything above ran `local_shell` in argv form, because the shipped
// toolset policy refuses `shell: true` outright. An operator can clear that
// policy — agents.toml documents `_denied_commands` as "the gate that still
// holds when a deployment clears the allowlist entirely", so clearing both is
// a configuration, not a trick — and then a real shell line reaches the
// evaluator. This is where `command_prefix_allowlist` has to read structure
// instead of refusing wholesale, and the per-platform matrix in
// docs/guide/confinement/trusted-binaries.md promises it does on POSIX sh.

/// Clear the shipped `local_shell` command policy, which is the only thing
/// standing between a `shell: true` call and the evaluator.
fn unpolice_the_toolset(cx: &Instance) {
    std::fs::write(
        cx.home_file("agents.toml"),
        "version = 1\n\n[tools_policies.local_shell]\n_allowed_commands = \"\"\n_denied_commands = \"\"\n",
    )
    .expect("write the operator's agents.toml");
    cx.init().ok();
}

fn shell_line(cx: &Instance, line: &str) -> Gated {
    gated(
        cx,
        "pinned",
        ToolCall::new("local_shell")
            .arg("command", line)
            .arg("shell", true),
    )
}

#[test]
fn a_shell_line_running_one_allowlisted_command_is_still_refused_an_allow() {
    let cx = instance("shell-line-single");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    unpolice_the_toolset(&cx);

    let seen = shell_line(&cx, &format!("{ALLOWED} one"));

    seen.raised_by("pinned");
    assert!(
        seen.ran.is_empty(),
        "the same words in argv form run unattended; handed to a shell they are a line, and a line the reader cannot show is more than its prefix does not get the allow: {:?}",
        seen.ran
    );
}

#[test]
fn a_compound_line_whose_every_command_is_allowlisted_is_read_and_allowed() {
    let cx = instance("shell-line-compound");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    unpolice_the_toolset(&cx);

    let seen = shell_line(&cx, &format!("{ALLOWED} one && {ALLOWED} two"));

    assert!(
        !seen.asked,
        "a compound line is read rather than blanket-refused, and every command in it is on the list: {}",
        seen.detail
    );
    assert_eq!(
        seen.ran.len(),
        1,
        "and it is the shell that runs it, once: {:?}",
        seen.ran
    );
    assert!(
        seen.told.contains("onetwo"),
        "both halves ran:\n{}",
        seen.told
    );
}

#[test]
fn one_command_off_the_list_withdraws_the_allow_from_the_whole_line() {
    let cx = instance("shell-line-mixed");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    unpolice_the_toolset(&cx);

    let seen = shell_line(&cx, &format!("{ALLOWED} one && id"));

    seen.raised_by("pinned");
    assert!(
        seen.ran.is_empty(),
        "reading the line is not the same as trusting it: one unlisted command prices the whole line: {:?}",
        seen.ran
    );
}

#[test]
fn a_redirect_withdraws_the_allow_because_a_reader_with_one_is_a_writer() {
    let cx = instance("shell-line-redirect");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    unpolice_the_toolset(&cx);

    let seen = shell_line(&cx, &format!("{ALLOWED} one > written.txt"));

    seen.raised_by("pinned");
    assert!(
        !cx.work().join("written.txt").exists(),
        "a redirect is not an argument, and the refused card means nothing was written"
    );
}

#[test]
fn the_pin_is_checked_against_the_commands_the_line_runs_not_the_shell_running_it() {
    let cx = instance("shell-line-pinned");
    write_policy(&cx, "pinned", &gate_policy(ALLOWED, None));
    unpolice_the_toolset(&cx);
    // Only the command in the line is declared. The `sh` that will interpret
    // it is not, and under a strict pin an undeclared binary is refused.
    cx.run([
        "hitl",
        "trust",
        "--policy",
        "hitl-policy-pinned.json",
        ALLOWED,
    ])
    .ok();

    let seen = shell_line(&cx, &format!("{ALLOWED} one && {ALLOWED} two"));

    assert!(
        !seen.asked,
        "the identity check is on what the line runs, not on the interpreter contenox reaches for: {}",
        seen.detail
    );
    assert!(
        seen.ran.first().is_some_and(|line| line.contains("sh -c")),
        "and a shell really was the thing that ran, so this was not the argv path in disguise: {:?}",
        seen.ran
    );
}
