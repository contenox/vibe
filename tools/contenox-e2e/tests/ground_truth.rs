//! `contenox init`, `contenox vet`, `contenox doctor` — the operator's ground truth.
//!
//! These three are what an operator reaches for before anything runs: init
//! scaffolds, vet judges what init wrote, doctor says whether the host can work
//! at all. Every case here reads them the way an operator does — the files they
//! leave on disk, the lines they print, and the exit code a script branches on.

use contenox_e2e::{Capabilities, Instance, Script};
use serde_json::Value;
use std::path::Path;

// --------------------------------------------------------------- utilities

/// The `Rendered <path>` lines init prints, one per envelope it wrote.
fn rendered_paths(stdout: &str) -> Vec<String> {
    stdout
        .lines()
        .filter_map(|line| line.trim().strip_prefix("Rendered "))
        .map(str::to_string)
        .collect()
}

fn basename(path: &str) -> String {
    Path::new(path)
        .file_name()
        .expect("a rendered path has a filename")
        .to_string_lossy()
        .into_owned()
}

/// The `[envelopes.<name>]` sections of an agents.toml, in file order — the
/// declarations every rendered policy is transpiled from.
fn declared_envelopes(toml: &str) -> Vec<String> {
    toml.lines()
        .map(str::trim)
        .filter_map(|line| line.strip_prefix("[envelopes."))
        .filter_map(|rest| rest.strip_suffix(']'))
        // Only the section itself: [envelopes.default.files.read] is a subsection.
        .filter(|name| !name.contains('.'))
        .map(str::to_string)
        .collect()
}

/// Filenames directly inside a directory, sorted; an absent directory is empty.
fn entries(dir: &Path) -> Vec<String> {
    let Ok(read) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut names: Vec<String> = read
        .filter_map(Result::ok)
        .map(|entry| entry.file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    names
}

fn marker(cx: &Instance, dir: &Path) -> Value {
    let path = dir.join(".contenox").join("workspace.id");
    let text = std::fs::read_to_string(&path).unwrap_or_else(|err| {
        panic!(
            "{} is unreadable ({err}) in {}",
            path.display(),
            cx.root().display()
        )
    });
    serde_json::from_str(&text)
        .unwrap_or_else(|err| panic!("{} is not JSON ({err}): {text}", path.display()))
}

/// A stale operator policy: one rule for one toolset, so every toolset this
/// build ships and this file never mentions falls through to default_action.
const STALE_POLICY: &str = r#"{"default_action":"deny","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}"#;

fn contains_bytes(haystack: &[u8], needle: &str) -> bool {
    haystack
        .windows(needle.len())
        .any(|window| window == needle.as_bytes())
}

// ===========================================================================
// init — what it writes, and what it refuses to write twice
// ===========================================================================

#[test]
fn init_marks_the_workspace_and_seeds_the_shipped_files_under_the_home_dir() {
    let cx = Instance::named("init-seed").expect("scratch instance");

    let out = cx.init().ok().expect_stdout("Done.");

    let id = marker(&cx, cx.work());
    let uuid = id["id"].as_str().expect("the marker carries an id");
    assert_eq!(
        uuid.len(),
        36,
        "the marker id is a UUID, got {uuid:?} from {id}"
    );
    assert_eq!(
        uuid.matches('-').count(),
        4,
        "the marker id is a UUID, got {uuid:?}"
    );

    assert!(
        cx.home_file("agents.toml").is_file(),
        "agents.toml is seeded once, globally: {}",
        out.render()
    );
    assert!(
        cx.home_file("agents").is_dir(),
        "the declarations directory is seeded beside it: {}",
        out.render()
    );
    for chain in [
        "chain-oracle-default.json",
        "chain-compact-default.json",
        "chain-fim-default.json",
        "chain-planner-default.json",
    ] {
        assert!(
            cx.home_file("system").join(chain).is_file(),
            "the shipped chain {chain} belongs under ~/.contenox/system/, which holds {:?}\n{}",
            entries(&cx.home_file("system")),
            out.render()
        );
    }

    // The funnel an operator follows out of init.
    out.expect_stdout("Next steps:")
        .expect_stdout("contenox mission fire acp");
}

#[test]
fn a_second_init_keeps_the_workspace_id_and_leaves_every_seeded_file_alone() {
    let cx = Instance::named("init-idempotent").expect("scratch instance");
    cx.init().ok();
    let first = marker(&cx, cx.work());

    let again = cx.init().ok();

    assert_eq!(
        marker(&cx, cx.work()),
        first,
        "the workspace id is stable once written — sessions and missions hang off it"
    );
    assert!(
        again
            .stdout
            .contains("already exists (use --force to overwrite or --update to refresh)"),
        "a seeded file is reported, never silently rewritten:\n{}",
        again.render()
    );
}

#[test]
fn init_refuses_a_provider_it_does_not_know_before_scaffolding_anything() {
    let cx = Instance::named("init-bad-provider").expect("scratch instance");

    cx.run(["init", "notaprovider"])
        .expect_code(1)
        .expect_stderr(
            "unknown provider \"notaprovider\" — valid options: ollama, openai, gemini, anthropic, bedrock, vertex-google",
        );

    assert!(
        !cx.work().join(".contenox").exists(),
        "a refused init leaves no half-scaffolded workspace behind"
    );
}

#[test]
fn init_renders_one_policy_per_declared_envelope_and_prints_each_one_it_wrote() {
    let cx = Instance::named("init-render").expect("scratch instance");

    let out = cx.init().ok();

    let toml =
        std::fs::read_to_string(cx.home_file("agents.toml")).expect("init seeded an agents.toml");
    let mut declared: Vec<String> = declared_envelopes(&toml)
        .into_iter()
        .map(|name| format!("hitl-policy-{name}.json"))
        .collect();
    declared.sort();
    assert!(
        !declared.is_empty(),
        "the shipped agents.toml declares envelopes; parsed none from:\n{toml}"
    );

    let rendered = rendered_paths(&out.stdout);
    let mut written: Vec<String> = rendered.iter().map(|path| basename(path)).collect();
    written.sort();

    assert_eq!(
        written,
        declared,
        "every [envelopes.<name>] renders to .generated/hitl-policy-<name>.json, and init prints each one:\n{}",
        out.render()
    );
    for path in &rendered {
        assert!(
            Path::new(path).is_file(),
            "init printed {path} but wrote nothing there"
        );
        assert_eq!(
            Path::new(path).parent().and_then(Path::file_name),
            Some(std::ffi::OsStr::new(".generated")),
            "a rendered envelope is the transpiler's, so it lands in .generated/: {path}"
        );
    }
}

#[test]
fn approval_policies_are_never_seeded_beside_the_chains() {
    let cx = Instance::named("init-no-policy-seed").expect("scratch instance");

    cx.init().ok();

    for dir in [cx.home().join(".contenox"), cx.work().join(".contenox")] {
        let seeded: Vec<String> = entries(&dir)
            .into_iter()
            .filter(|name| name.starts_with("hitl-policy"))
            .collect();
        assert!(
            seeded.is_empty(),
            "nothing seeds a policy where an operator's own file goes; {} holds {seeded:?}",
            dir.display()
        );
    }
    assert!(
        cx.home_file(".generated/hitl-policy-default.json")
            .is_file(),
        "the name resolves because the envelope behind it was rendered, not because a preset was copied"
    );
}

#[test]
fn a_rendered_envelope_deleted_by_hand_comes_back_on_the_next_init() {
    let cx = Instance::named("init-rerender").expect("scratch instance");
    cx.init().ok();
    let policy = cx.home_file(".generated/hitl-policy-strict.json");
    std::fs::remove_file(&policy).expect("remove the rendered envelope");

    let out = cx.init().ok();

    assert!(policy.is_file(), "the render is redone, not remembered");
    assert_eq!(
        rendered_paths(&out.stdout),
        vec![policy.display().to_string()],
        "only the missing one is written again, and it is the one printed:\n{}",
        out.render()
    );
}

#[test]
fn init_walks_up_to_the_ancestor_workspace_the_way_git_does() {
    let cx = Instance::named("init-walkup").expect("scratch instance");
    cx.cmd(["init", "--name", "the-root"])
        .output()
        .expect("contenox init")
        .ok();
    let root_marker = marker(&cx, cx.work());

    let nested = cx.work().join("services").join("api");
    std::fs::create_dir_all(&nested).expect("create the nested directory");
    let out = cx
        .cmd(["init"])
        .cwd(&nested)
        .output()
        .expect("contenox init from a descendant")
        .ok();

    assert!(
        out.stdout.contains(&format!(
            "Marked project \"the-root\" ({})",
            cx.work().join(".contenox").join("workspace.id").display()
        )),
        "init from a descendant marks the ancestor it found:\n{}",
        out.render()
    );
    assert!(
        !nested.join(".contenox").exists(),
        "no second marker is planted inside a workspace that already has one"
    );
    assert_eq!(
        marker(&cx, cx.work()),
        root_marker,
        "the ancestor's identity is reused, not rewritten"
    );
}

#[test]
fn init_project_forces_a_fresh_marker_in_the_directory_it_runs_in() {
    let cx = Instance::named("init-project").expect("scratch instance");
    cx.cmd(["init", "--name", "the-root"])
        .output()
        .expect("contenox init")
        .ok();
    let root_id = marker(&cx, cx.work())["id"].clone();

    let nested = cx.work().join("vendor").join("sdk");
    std::fs::create_dir_all(&nested).expect("create the nested directory");
    let out = cx
        .cmd(["init", "--project", "--name", "the-child"])
        .cwd(&nested)
        .output()
        .expect("contenox init --project")
        .ok();

    let child = marker(&cx, &nested);
    assert_eq!(child["name"], "the-child", "--name names the new marker");
    assert_ne!(
        child["id"], root_id,
        "--project is a new workspace, so it gets its own id: {child}"
    );
    assert!(
        out.stdout
            .contains(&format!("Open it with: contenox beam {}", nested.display())),
        "the new project is handed straight to the front door:\n{}",
        out.render()
    );
}

#[test]
fn init_local_seeds_workspace_copies_that_shadow_the_global_ones() {
    let cx = Instance::named("init-local").expect("scratch instance");

    let out = cx
        .cmd(["init", "--local"])
        .output()
        .expect("contenox init --local")
        .ok()
        .expect_stdout("These workspace copies shadow the ones in ~/.contenox");

    let workspace = cx.work().join(".contenox");
    for seeded in [
        "workspace.id",
        "agents.toml",
        "agents",
        "chain-oracle-default.json",
    ] {
        assert!(
            workspace.join(seeded).exists(),
            "--local puts {seeded} in the workspace, which holds {:?}\n{}",
            entries(&workspace),
            out.render()
        );
    }
    assert!(
        workspace
            .join(".generated")
            .join("hitl-policy-default.json")
            .is_file(),
        "the workspace renders its own envelopes too"
    );
    assert!(
        !cx.home_file("agents.toml").exists(),
        "--local is a deliberate override: it writes nothing into ~/.contenox"
    );
}

#[test]
fn init_update_renames_a_pre_v038_chain_file_and_keeps_what_is_in_it() {
    let cx = Instance::named("init-update-rename").expect("scratch instance");
    cx.init().ok();
    // A file an operator edited under the old name: the rename must be
    // byte-for-byte, or an operator loses a chain to a housekeeping flag.
    let mine = r#"{"tasks":[{"id":"mine","handler":"noop"}]}"#;
    let legacy = cx
        .write_home_file("default-fim-chain.json", mine)
        .expect("plant the legacy chain file");

    let out = cx
        .cmd(["init", "--update"])
        .output()
        .expect("contenox init --update")
        .ok();

    let renamed = cx.home_file("chain-fim-default.json");
    assert!(
        out.stdout.contains(&format!(
            "Renamed {} -> {}",
            legacy.display(),
            renamed.display()
        )),
        "--update says which file it moved:\n{}",
        out.render()
    );
    assert!(!legacy.exists(), "the pre-v0.38 name is gone");
    assert_eq!(
        std::fs::read_to_string(&renamed).expect("the renamed chain"),
        mine,
        "a hand-edited chain keeps its content under the new name"
    );
}

#[test]
#[ignore = "confirmed defect: blessedChainHashes in internal/surfaces/contenoxcli/init.go is an empty map, so --update matches no known hash and reports every shipped default — including one init wrote seconds earlier and nobody touched — as '(has been modified)'; nothing is ever refreshed"]
fn init_update_refreshes_a_shipped_default_nobody_has_touched() {
    let cx = Instance::named("init-update-refresh").expect("scratch instance");
    cx.init().ok();

    let out = cx
        .cmd(["init", "--update"])
        .output()
        .expect("contenox init --update")
        .ok();

    assert!(
        !out.stdout.contains("(has been modified)"),
        "every one of these files was written by init and touched by no one:\n{}",
        out.render()
    );
}

#[test]
fn init_refresh_policies_rewrites_the_copies_on_disk_and_never_touches_generated() {
    let cx = Instance::named("init-refresh-policies").expect("scratch instance");
    cx.init().ok();
    let operator_copy = cx
        .write_home_file("hitl-policy-default.json", STALE_POLICY)
        .expect("plant an operator policy copy");
    let rendered = cx.home_file(".generated/hitl-policy-strict.json");
    std::fs::write(&rendered, STALE_POLICY).expect("age the rendered envelope too");

    let out = cx
        .cmd(["init", "--refresh-policies"])
        .output()
        .expect("contenox init --refresh-policies")
        .ok()
        .expect_stdout(&format!("Refreshed {}", operator_copy.display()))
        .expect_stdout("Chains, config and sessions were not touched.");

    assert_ne!(
        std::fs::read_to_string(&operator_copy).expect("the operator copy"),
        STALE_POLICY,
        "the copy already on disk is rewritten to this build's preset:\n{}",
        out.render()
    );
    assert_eq!(
        std::fs::read_to_string(&rendered).expect("the rendered envelope"),
        STALE_POLICY,
        "a file under .generated/ belongs to the transpiler, and the refresh verb never writes there"
    );
    assert!(
        !cx.home_file("hitl-policy-dev.json").exists(),
        "a preset the directory never held is not planted: the rendered envelope answers for that name"
    );
}

// ===========================================================================
// vet — what it classifies, what it fails, what it merely warns about
// ===========================================================================

/// A directory of files to vet, outside .contenox so only the argument is read.
fn probe_dir(cx: &Instance, files: &[(&str, &str)]) -> std::path::PathBuf {
    for (name, body) in files {
        cx.write_file(Path::new("probe").join(name), body)
            .expect("write the file to vet");
    }
    cx.work().join("probe")
}

#[test]
fn vet_classifies_a_file_by_what_is_in_it_not_by_what_it_is_called() {
    let cx = Instance::named("vet-classify").expect("scratch instance");
    cx.init().ok();
    let dir = probe_dir(
        &cx,
        &[
            // A tasks array is a chain whatever the file is called.
            (
                "anything.json",
                r#"{"tasks":[{"id":"a","handler":"noop"}]}"#,
            ),
            // A rules array is an envelope whatever the file is called — and
            // this one names a tool local_fs does not serve.
            (
                "not-a-policy-name.json",
                r#"{"rules":[{"tools":"local_fs","tool":"frobnicate","action":"allow"}]}"#,
            ),
            // Neither key: not ours to judge.
            ("notes.json", r#"{"note":"neither a chain nor a policy"}"#),
            // The hitl-policy-* name carries the classification when the
            // content cannot: this does not even parse.
            ("hitl-policy-broken.json", "this is not json at all"),
        ],
    );

    let out = cx
        .cmd(["vet"])
        .arg(&dir)
        .output()
        .expect("contenox vet")
        .expect_code(1);

    let line = |name: &str| {
        out.stdout
            .lines()
            .find(|line| line.ends_with(name) || line.contains(&format!("{name} ")))
            .unwrap_or_else(|| panic!("no verdict for {name}:\n{}", out.render()))
            .to_string()
    };
    assert!(
        line("anything.json").starts_with("ok   "),
        "a tasks array is a chain: {}",
        line("anything.json")
    );
    assert!(
        line("not-a-policy-name.json").starts_with("FAIL"),
        "a rules array is an envelope, and this rule can never match: {}",
        line("not-a-policy-name.json")
    );
    assert!(
        out.stdout
            .contains(r#"local_fs serves no tool "frobnicate", so this rule can never match"#),
        "the envelope check names the inert rule:\n{}",
        out.render()
    );
    assert!(
        line("notes.json").starts_with("skip"),
        "neither shape is skipped, not failed: {}",
        line("notes.json")
    );
    assert!(
        line("hitl-policy-broken.json").starts_with("FAIL"),
        "the hitl-policy-* name makes it an envelope even when the bytes are not JSON: {}",
        line("hitl-policy-broken.json")
    );
}

#[test]
fn vet_runs_the_load_time_linter_over_a_chain() {
    let cx = Instance::named("vet-lint").expect("scratch instance");
    cx.init().ok();
    let dir = probe_dir(
        &cx,
        &[(
            "chain-dangling.json",
            r#"{"tasks":[{"id":"a","handler":"noop","transition":{"branches":[{"operator":"default","goto":"nowhere"}]}}]}"#,
        )],
    );

    cx.cmd(["vet"])
        .arg(&dir)
        .output()
        .expect("contenox vet")
        .expect_code(1)
        .expect_stdout(
            r#"chain failed load-time validation: chain[]: task "a": branch goto references unknown task "nowhere""#,
        );
}

#[test]
fn vet_exits_nonzero_and_counts_the_files_that_failed() {
    let cx = Instance::named("vet-exit").expect("scratch instance");
    cx.init().ok();
    let dir = probe_dir(
        &cx,
        &[
            ("chain-empty.json", r#"{"tasks":[]}"#),
            (
                "chain-fine.json",
                r#"{"tasks":[{"id":"a","handler":"noop"}]}"#,
            ),
        ],
    );

    // Exit 1 is what a CI step branches on; the tally is what a human reads.
    cx.cmd(["vet"])
        .arg(&dir)
        .output()
        .expect("contenox vet")
        .expect_code(1)
        .expect_stdout("chain failed load-time validation: chain[]: chain has no tasks")
        .expect_stdout("vet: 1 of 2 file(s) failed");
}

#[test]
fn vet_warns_without_failing_when_a_policy_file_shadows_a_rendered_envelope() {
    let cx = Instance::named("vet-shadow").expect("scratch instance");
    cx.init().ok();
    let rendered = cx.home_file(".generated/hitl-policy-default.json");
    let shadow = cx
        .write_home_file(
            "hitl-policy-default.json",
            &std::fs::read_to_string(&rendered).expect("the rendered envelope"),
        )
        .expect("plant the shadowing copy");

    let out = cx
        .cmd(["vet"])
        .arg(&shadow)
        .output()
        .expect("contenox vet")
        // A warning is not a defect: the file is valid and the runtime reads it.
        .ok()
        .expect_stdout(&format!("ok   {}", shadow.display()))
        .expect_stdout(&format!("WARN {}", shadow.display()))
        .expect_stdout(&format!(
            "this file shadows the rendered envelope {}",
            rendered.display()
        ));

    assert!(
        out.stdout
            .contains("editing [envelopes] in agents.toml changes nothing here"),
        "the warning says why it matters — the transpiled file is inert:\n{}",
        out.render()
    );
}

#[test]
fn vet_with_nothing_to_judge_says_so_and_exits_zero() {
    let cx = Instance::named("vet-empty").expect("scratch instance");
    cx.init().ok();

    // A plain init leaves the workspace holding only its marker.
    cx.run(["vet"]).ok().expect_stdout(
        "Nothing to vet: no .json files found. Run 'contenox init' to scaffold .contenox/, or pass a path.",
    );
}

// ===========================================================================
// doctor — the verdict, and the report behind it
// ===========================================================================

/// A host that can answer, so the verdict is `yes`.
fn ready_host(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn("doctor asks this backend nothing"))
        .expect("scripted-test backend");
    cx
}

#[test]
fn doctors_first_line_is_the_verdict_and_the_command_that_follows_it() {
    let cx = ready_host("doctor-verdict");

    let out = cx.doctor().ok();

    assert_eq!(
        out.stdout.lines().next(),
        Some("Ready: yes — run: contenox beam"),
        "the verdict is the first thing on stdout, and it names the front door:\n{}",
        out.render()
    );
}

#[test]
fn a_host_that_cannot_work_says_no_and_names_the_next_command() {
    let cx = Instance::named("doctor-not-ready").expect("scratch instance");
    cx.init().ok();

    let out = cx.doctor().ok();

    let first = out.stdout.lines().next().unwrap_or_default();
    assert!(
        first.starts_with("Ready: no — "),
        "an unusable host says so first, with the reason: {first:?}\n{}",
        out.render()
    );
    assert!(
        out.stdout
            .lines()
            .nth(1)
            .unwrap_or_default()
            .starts_with("Next:  "),
        "and hands over one command to run:\n{}",
        out.render()
    );
}

#[test]
fn doctor_exits_zero_whether_or_not_the_host_is_ready() {
    let unusable = Instance::named("doctor-exit-no").expect("scratch instance");
    unusable.init().ok();
    let usable = ready_host("doctor-exit-yes");

    // The verdict is text, not status: a script has to read the line. Both
    // shapes exit 0, so branching on $? silently treats a broken host as fine.
    unusable.run(["doctor"]).expect_code(0);
    usable.run(["doctor"]).expect_code(0);
}

#[test]
fn doctor_json_is_one_machine_readable_payload_with_no_verdict_and_no_roster() {
    let cx = ready_host("doctor-json");

    let payload = cx.doctor_json().expect("contenox doctor --json");
    let raw = cx.run(["doctor", "--json"]).ok();

    let mut keys: Vec<&str> = payload
        .as_object()
        .expect("the payload is an object")
        .keys()
        .map(String::as_str)
        .collect();
    keys.sort();
    assert_eq!(
        keys,
        vec![
            "backendChecks",
            "backendCount",
            "defaultChain",
            "defaultModel",
            "defaultProvider",
            "hitlPolicyName",
            "issues",
            "reachableBackendCount",
            "resolvedFrom",
        ],
        "the JSON contract an integrator reads: {payload:#}"
    );
    assert_eq!(payload["defaultProvider"], "scripted-test");
    assert_eq!(payload["backendCount"], 1);
    assert_eq!(payload["reachableBackendCount"], 1);

    // Two things the text report has that the payload deliberately does not.
    raw.refute_stdout("Ready:").refute_stdout("Tool roster");
}

#[test]
fn doctor_skip_cycle_still_answers_with_the_whole_verdict() {
    let cx = ready_host("doctor-skip-cycle");

    let fast = cx.run(["doctor", "--skip-cycle"]).ok();

    assert_eq!(
        fast.stdout.lines().next(),
        Some("Ready: yes — run: contenox beam"),
        "the faster path is still a complete answer:\n{}",
        fast.render()
    );
    assert!(
        fast.stdout.contains("Backends (registered): 1"),
        "and it still names what is registered:\n{}",
        fast.render()
    );
}

#[test]
#[ignore = "confirmed defect: --skip-cycle skips enginesvc's RunBackendCycle, but the read behind it (stateservice.Get -> State.ReconcileIfStale) runs the same cycle on a process's first read, so every backend is probed anyway; the flag's help promises 'faster; status may be outdated' and an operator gets neither"]
fn doctor_skip_cycle_serves_the_last_status_instead_of_probing_again() {
    let cx = ready_host("doctor-skip-cycle-cached");
    // One full report, so a synced status exists to serve.
    cx.doctor().ok();
    std::fs::remove_file(cx.work().join("scripted.json")).expect("take the backend away");

    let out = cx.run(["doctor", "--json", "--skip-cycle"]).ok();
    let payload: Value = serde_json::from_str(&out.stdout)
        .unwrap_or_else(|err| panic!("doctor --json --skip-cycle ({err}):\n{}", out.render()));

    assert_eq!(
        payload["reachableBackendCount"], 1,
        "skipping the sync means answering from the last one, outdated and all: {payload:#}"
    );
}

#[test]
fn doctor_flags_a_stale_top_level_policy_copy_and_names_the_command_that_fixes_it() {
    let cx = ready_host("doctor-stale-policy");
    let copy = cx
        .write_home_file("hitl-policy-default.json", STALE_POLICY)
        .expect("plant a policy copy that predates this build");

    let out = cx.doctor().ok();

    assert!(
        out.stdout
            .contains(&format!("{} predates toolsets", copy.display())),
        "doctor names the file and what it predates:\n{}",
        out.render()
    );
    out.clone()
        .expect_stdout("The tools stay visible to the model")
        .expect_stdout("Try: contenox init --refresh-policies");

    let codes: Vec<String> = cx.doctor_json().expect("contenox doctor --json")["issues"]
        .as_array()
        .expect("issues is an array")
        .iter()
        .map(|issue| issue["code"].as_str().unwrap_or_default().to_string())
        .collect();
    assert!(
        codes.contains(&"hitl_policy_presets_stale".to_string()),
        "and reports it to an integrator too, got {codes:?}"
    );
}

#[test]
fn doctor_never_calls_a_generated_envelope_stale() {
    let cx = ready_host("doctor-generated-not-stale");
    // Byte-identical to the copy doctor does flag when an operator owns it —
    // the only difference is which directory it sits in.
    std::fs::write(
        cx.home_file(".generated/hitl-policy-default.json"),
        STALE_POLICY,
    )
    .expect("age the rendered envelope");

    let out = cx.doctor().ok();

    assert!(
        !out.stdout.contains("predates"),
        "a file under .generated/ is the transpiler's; judging it would report the product's own render as the operator's stale copy:\n{}",
        out.render()
    );
}

#[test]
fn doctor_warns_when_default_max_tokens_exceeds_the_provider_ceiling() {
    let cx = Instance::named("doctor-max-tokens").expect("scratch instance");
    cx.init().ok();
    cx.scripted(
        &Script::new()
            .max_output_tokens(256)
            .turn("doctor asks this backend nothing"),
    )
    .expect("scripted-test backend");
    cx.run(["config", "set", "default-max-tokens", "9000"]).ok();

    cx.doctor()
        .ok()
        .expect_stdout(
            "⚠️  Advisory: default-max-tokens=9000 exceeds scripted-test provider ceiling (256).",
        )
        .expect_stdout("Requests will be clamped automatically")
        .expect_stdout("contenox config set default-max-tokens 256");
}

#[test]
fn doctor_names_the_models_that_accept_images() {
    let cx = ready_host("doctor-vision");

    cx.doctor()
        .ok()
        .expect_stdout("Vision: 1 model(s) accept images (e.g. scripted-test).");
}

#[test]
fn doctor_says_so_when_no_model_accepts_images() {
    let cx = Instance::named("doctor-no-vision").expect("scratch instance");
    cx.init().ok();
    cx.scripted(
        &Script::new()
            .capabilities(Capabilities::new().vision(false))
            .turn("doctor asks this backend nothing"),
    )
    .expect("scripted-test backend");

    cx.doctor().ok().expect_stdout(
        "Vision: no vision-capable models available — requests with images will be refused.",
    );
}

#[test]
fn doctor_has_no_state_storage_section_when_everything_is_local() {
    let cx = ready_host("doctor-local-state");

    let out = cx.doctor().ok();

    assert!(
        !out.stdout.contains("State storage:"),
        "a host with no external substrate has nothing to report about one:\n{}",
        out.render()
    );
}

#[test]
fn doctor_reports_the_external_substrate_it_was_pointed_at_and_cannot_reach() {
    let cx = ready_host("doctor-external-state");

    // A bus this host is told to use, at an address nothing answers on: the
    // report has to name the setting that selected it, because while it is set
    // contenox never falls back to the local file.
    let out = cx
        .cmd(["doctor"])
        .env("CONTENOX_NATS_URL", "nats://127.0.0.1:1")
        .output()
        .expect("contenox doctor")
        .expect_code(1)
        .expect_stdout("State storage:")
        .expect_stdout("message bus: NATS (nats://127.0.0.1:1, from CONTENOX_NATS_URL)")
        .expect_stdout("Status: unreachable")
        .expect_stdout("Hint: Start that server or unset CONTENOX_NATS_URL");

    assert!(
        out.stdout.contains("store: SQLite"),
        "the substrates that stayed local are named as local:\n{}",
        out.render()
    );
}

#[test]
fn doctor_bundle_writes_a_redacted_zip_and_an_issue_link_carrying_no_log_content() {
    let cx = ready_host("doctor-bundle");
    // A backend whose URL carries a credential, and a log line with a key in
    // it: both are what an operator would otherwise paste into a public issue.
    cx.run([
        "backend",
        "add",
        "creds",
        "--type",
        "ollama",
        "--url",
        "http://operator:supersecret@127.0.0.1:9",
    ])
    .ok();
    cx.write_home_file(
        "telemetry.log",
        "time=2026-01-01 level=INFO msg=MARKER-XYZZY api_key=\"sk-abcdefghijklmnopqrst\"\n",
    )
    .expect("plant a telemetry log");
    let bundle = cx.work().join("bundle.zip");

    let out = cx
        .cmd(["doctor", "--bundle", "--bundle-out"])
        .arg(&bundle)
        .output()
        .expect("contenox doctor --bundle")
        .ok()
        .expect_stdout(&format!("Bundle: {}", bundle.display()))
        .expect_stdout("Contents: doctor.json, build.txt, and any telemetry.log found.")
        .expect_stdout("Review the file before sharing it.")
        .expect_stdout("Report:   https://github.com/contenox/contenox/issues/new?");

    let redacted = out
        .stdout
        .lines()
        .find_map(|line| line.trim().strip_prefix("Redacted: "))
        .and_then(|rest| rest.split_whitespace().next())
        .and_then(|count| count.parse::<u32>().ok())
        .unwrap_or_else(|| panic!("no redaction count on:\n{}", out.render()));
    assert!(
        redacted > 0,
        "a credential in a backend URL and a key in a log are credential-shaped:\n{}",
        out.render()
    );

    let bytes = std::fs::read(&bundle).expect("the bundle the run wrote");
    assert_eq!(&bytes[..2], b"PK", "the bundle is a zip");
    // Member names are stored uncompressed in a zip's local headers, so the
    // manifest is readable without unpacking anything.
    for member in ["doctor.json", "build.txt", "telemetry.log"] {
        assert!(
            contains_bytes(&bytes, member),
            "the bundle should carry {member}, {} bytes hold no such entry",
            bytes.len()
        );
    }
    assert!(
        !contains_bytes(&bytes, "supersecret"),
        "the credential must not survive into the file an operator attaches"
    );

    let link = out
        .stdout
        .lines()
        .find_map(|line| line.trim().strip_prefix("Report:   "))
        .expect("the pre-filled issue link");
    for leaked in ["MARKER-XYZZY", "supersecret", "sk-abcdefghijklmnopqrst"] {
        assert!(
            !link.contains(leaked),
            "the link is opened in a browser, so it carries a summary and never log content: {link}"
        );
    }
    assert!(
        link.contains(&urlencoded(&bundle.display().to_string())),
        "it points at the bundle to attach instead: {link}"
    );
}

/// The subset of percent-encoding the issue link uses for a scratch path.
fn urlencoded(path: &str) -> String {
    path.replace('/', "%2F")
}
