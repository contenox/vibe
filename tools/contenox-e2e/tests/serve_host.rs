//! `contenox serve` — the standing host.
//!
//! Every case drives the shipped binary and reads the host back the way an
//! operator does: the status screen it prints, the log files it writes, and
//! `contenox doctor`, `contenox pair` and `contenox vet`.

use contenox_e2e::{Instance, Pty, Script};
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::time::Duration;

// ---------------------------------------------------------------- the host

/// Start a host under a real terminal and block until its screen is finished.
///
/// The last line of the screen is written immediately before the host parks on
/// its stop signal, so a screen carrying it is a host that is up.
fn host(cx: &Instance, args: &[&str]) -> Pty {
    let mut argv = vec!["serve".to_string()];
    argv.extend(args.iter().map(|a| a.to_string()));
    // 200 columns: a scratch instance's log path is long, and a wrapped line
    // would fail an assertion about a path the host got right.
    let pty = Pty::spawn_sized(cx.cmd(&argv), 40, 200).expect("spawn contenox serve under a pty");
    pty.wait_for(RUNNING, Duration::from_secs(90))
        .expect("the host never finished its status screen");
    pty
}

const RUNNING: &str = "Running. Press Ctrl-C to stop.";
const STOPPING: &str = "Stopping — the app can no longer reach this machine.";

/// A host that can answer, so `Setup` reads `ready` and the screen names a model.
fn ready_host_instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn("a host is not asked anything by this case"))
        .expect("scripted-test backend");
    cx
}

/// One `  Label        value` row of the status screen.
fn field(screen: &str, label: &str) -> String {
    screen
        .lines()
        .filter_map(|line| line.strip_prefix("  "))
        .find(|line| line.starts_with(label) && line[label.len()..].starts_with(' '))
        .map(|line| line[label.len()..].trim().to_string())
        .unwrap_or_else(|| panic!("the status screen has no {label:?} row:\n{screen}"))
}

fn has_field(screen: &str, label: &str) -> bool {
    screen
        .lines()
        .filter_map(|line| line.strip_prefix("  "))
        .any(|line| line.starts_with(label) && line[label.len()..].starts_with(' '))
}

/// A directory the host can be pointed at, created inside the case's scratch.
fn workspace_dir(cx: &Instance, name: &str) -> PathBuf {
    let dir = cx.work().join(name);
    std::fs::create_dir_all(&dir).expect("create the workspace directory");
    dir
}

fn log_dir(cx: &Instance, name: &str) -> PathBuf {
    let dir = cx.root().join(name);
    std::fs::create_dir_all(&dir).expect("create the log directory");
    dir
}

// ---------------------------------------------------------------- the pairing

/// A stored pairing, written where `contenox pair` stores one.
///
/// The endpoint is a loopback port nothing listens on: the dial is refused
/// instantly and never leaves the machine, which is what a case is allowed to
/// need. Everything the screen shows about a pairing is in this file.
fn pair_the_machine(cx: &Instance, token: &str) {
    let creds = format!(
        r#"{{
  "endpoint": "https://127.0.0.1:9",
  "instance_token": "{token}",
  "instance_id": "ca2e8376-99ab-4669-9ca4-b32e5605bb4d",
  "account_id": "acct-7",
  "relay_public_key": "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE="
}}
"#
    );
    std::fs::write(cx.home_file("relay.json"), creds).expect("store the pairing");
}

// ---------------------------------------------------------------- the logs

/// `serve-YYYY-MM-DD.log` -> (day, 1); `serve-YYYY-MM-DD.7.log` -> (day, 7).
fn parse_part(name: &str) -> Option<(String, u32)> {
    let rest = name.strip_prefix("serve-")?.strip_suffix(".log")?;
    let (day, part) = match rest.split_once('.') {
        Some((day, number)) => (day, number.parse().ok()?),
        None => (rest, 1),
    };
    if day.len() != 10 {
        return None;
    }
    for (i, byte) in day.bytes().enumerate() {
        let shaped = if i == 4 || i == 7 {
            byte == b'-'
        } else {
            byte.is_ascii_digit()
        };
        if !shaped {
            return None;
        }
    }
    Some((day.to_string(), part))
}

fn dir_entries(dir: &Path) -> Vec<String> {
    let mut names: Vec<String> = std::fs::read_dir(dir)
        .map(|entries| {
            entries
                .flatten()
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default();
    names.sort();
    names
}

/// The host's own log files in dir, oldest part first.
fn host_logs(dir: &Path) -> Vec<String> {
    let mut owned: Vec<(String, u32, String)> = dir_entries(dir)
        .into_iter()
        .filter_map(|name| parse_part(&name).map(|(day, part)| (day, part, name)))
        .collect();
    owned.sort();
    owned.into_iter().map(|(_, _, name)| name).collect()
}

fn read_logs(dir: &Path) -> String {
    host_logs(dir)
        .iter()
        .filter_map(|name| std::fs::read_to_string(dir.join(name)).ok())
        .collect::<Vec<_>>()
        .join("\n")
}

// ============================================================ one workspace

#[test]
fn serve_scopes_the_host_to_the_path_it_was_given() {
    let cx = ready_host_instance("serve-path");
    let served = workspace_dir(&cx, "api");

    // A relative path: the screen must name the directory the sessions get, not
    // the spelling the operator typed.
    let mut pty = host(&cx, &["api"]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    assert_eq!(
        field(&screen, "Workspace"),
        served.display().to_string(),
        "the host must serve the path it was given, made absolute:\n{screen}"
    );
    assert_eq!(
        screen.matches("  Workspace ").count(),
        1,
        "a host serves exactly one workspace:\n{screen}"
    );
}

#[test]
fn serve_with_no_path_scopes_the_host_to_the_home_directory() {
    let cx = ready_host_instance("serve-bare");

    let mut pty = host(&cx, &[]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    assert_eq!(
        field(&screen, "Workspace"),
        cx.home().display().to_string(),
        "a host outlives the shell that started it, so a bare serve is machine-scoped:\n{screen}"
    );
    assert_ne!(
        field(&screen, "Workspace"),
        cx.work().display().to_string(),
        "a bare serve must not inherit the launch directory:\n{screen}"
    );
}

// ============================================================ no fs, no shell

#[test]
fn doctor_says_local_fs_and_local_shell_are_not_mounted_under_serve() {
    let cx = ready_host_instance("serve-roster");

    let out = cx.doctor().ok();

    // The contrast is the point: an attended shape can carry these because a
    // client performs them, and the host cannot because it has no client.
    out.clone()
        .expect_stdout("read_file — local_fs — needs client capability")
        .expect_stdout("local_shell — local_shell — needs client capability")
        .expect_stdout(
            "local_fs, local_shell — not mounted under `contenox serve`: this host serves no filesystem and no terminal",
        )
        .expect_stdout("declare an MCP tool for it (contenox mcp add)");
}

#[test]
fn a_host_refuses_a_declared_local_fs_and_local_shell_and_names_the_alternative() {
    let cx = ready_host_instance("serve-unserved");

    // The operator's own copy of the host chain, shadowing the compiled one,
    // asking the host for the two toolsets it does not have.
    let compiled = cx.home_file(".generated/chain-agent-acp.json");
    let mut chain: Value =
        serde_json::from_str(&std::fs::read_to_string(&compiled).expect("the compiled host chain"))
            .expect("the compiled host chain is JSON");
    let mut asked = false;
    for task in chain["tasks"].as_array_mut().expect("tasks") {
        // Only the task that already carries a toolset is touched: every other
        // line of the shipped chain reaches the host exactly as it ships.
        let Some(tools) = task
            .get_mut("execute_config")
            .and_then(|config| config.get_mut("tools"))
        else {
            continue;
        };
        if tools.as_array().map(|t| t.len()) == Some(1) && tools[0] == "*" {
            *tools = serde_json::json!(["local_fs", "local_shell"]);
            asked = true;
            break;
        }
    }
    assert!(asked, "no task in the shipped host chain carried a toolset");
    std::fs::write(
        cx.home_file("chain-agent-acp.json"),
        serde_json::to_string_pretty(&chain).expect("render the chain"),
    )
    .expect("write the operator's chain");

    // Under an envelope that hands out file writes without even asking:
    // absence is the shape, so no grant anywhere can hand these back.
    let mut pty = host(&cx, &[".", "--hitl-policy", "auto_edit"]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    for tool in ["local_fs", "local_shell"] {
        assert!(
            screen.contains(&format!(
                "contenox serve: \"{tool}\" is declared but not served"
            )),
            "the host must say it will not serve {tool}:\n{screen}"
        );
    }
    assert!(
        screen.contains("declare an MCP tool for it (contenox mcp add), or run this agent from `contenox beam` or an ACP editor"),
        "the refusal must name what to do instead:\n{screen}"
    );
    assert!(
        screen.contains(RUNNING),
        "the host still serves everything else:\n{screen}"
    );
}

#[test]
fn the_serve_envelope_denies_files_and_shell_while_still_defaulting_to_approve() {
    let cx = ready_host_instance("serve-envelope");
    let mut pty = host(&cx, &["."]);
    pty.send_ctrl('c').ok();

    let path = cx.home_file(".generated/hitl-policy-serve.json");
    let rendered: Value =
        serde_json::from_str(&std::fs::read_to_string(&path).expect("the rendered serve envelope"))
            .expect("the rendered serve envelope is JSON");

    assert_eq!(
        rendered["default_action"], "approve",
        "what a host can reach is what you connected, so the default still asks a human"
    );

    let rules = rendered["rules"].as_array().expect("rules");
    let denied = |toolset: &str, tool: &str| {
        rules.iter().any(|rule| {
            rule["tools"] == toolset && rule["tool"] == tool && rule["action"] == "deny"
        })
    };
    for (toolset, tool) in [
        ("local_fs", "read_file"),
        ("local_fs", "read_file_range"),
        ("local_fs", "list_dir"),
        ("local_fs", "write_file"),
        ("local_fs", "edit_file"),
        ("local_fs", "sed"),
        ("local_shell", "local_shell"),
    ] {
        assert!(
            denied(toolset, tool),
            "the serve envelope must deny {toolset}/{tool}:\n{rendered:#}"
        );
    }
    assert!(
        !rules.iter().any(
            |rule| (rule["tools"] == "local_fs" || rule["tools"] == "local_shell")
                && rule["action"] != "deny"
        ),
        "no rule may hand a host back a file or a shell:\n{rendered:#}"
    );

    // The product's own linter, on the file the product rendered.
    cx.run(["vet", path.to_str().expect("policy path")])
        .ok()
        .expect_stdout("ok");
}

// ============================================================ the status screen

#[test]
fn the_status_screen_reports_setup_workspace_model_relay_and_log_retention() {
    let cx = ready_host_instance("serve-screen");
    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    assert_eq!(field(&screen, "Setup"), "ready", "screen:\n{screen}");
    assert_eq!(
        field(&screen, "Workspace"),
        cx.work().display().to_string(),
        "screen:\n{screen}"
    );
    assert_eq!(
        field(&screen, "Model"),
        "scripted-test/scripted-test",
        "screen:\n{screen}"
    );
    assert_eq!(
        field(&screen, "Relay"),
        "not paired — reachable on this machine only",
        "screen:\n{screen}"
    );

    let logs = cx.home_file("logs");
    let named = field(&screen, "Logs");
    assert_eq!(
        Path::new(&named),
        logs.join(host_logs(&logs).last().expect("a log file")),
        "the screen must name the file the host is writing:\n{screen}"
    );
    assert!(
        screen.contains("new part at 10MB · keep 14 files · 14 days"),
        "the screen must state the retention bounds in force:\n{screen}"
    );
    assert!(
        screen.contains("contenox config set log-max-size"),
        "the screen must name the key that changes what it just showed:\n{screen}"
    );
}

#[test]
fn a_host_with_no_model_says_so_and_serves_anyway() {
    let cx = Instance::named("serve-nomodel").expect("scratch instance");
    cx.init().ok();

    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();

    assert_eq!(
        field(&screen, "Setup"),
        "no model configured",
        "screen:\n{screen}"
    );
    assert!(
        !has_field(&screen, "Model"),
        "there is no model to name:\n{screen}"
    );
    assert!(
        screen.contains("This host can be attached to, but nothing it is asked to do will run."),
        "screen:\n{screen}"
    );
    assert!(
        screen.contains("Configure a provider and model:  contenox setup"),
        "the screen must name the remedy:\n{screen}"
    );
    assert!(
        pty.wait_exit(Duration::from_millis(500)).is_err(),
        "a host with no model is still reachable, so it keeps running:\n{screen}"
    );
    pty.send_ctrl('c').ok();
}

// ============================================================ pairing

#[test]
fn an_unpaired_host_prints_the_pairing_steps_and_keeps_running() {
    let cx = ready_host_instance("serve-unpaired");
    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();

    assert_eq!(
        field(&screen, "Relay"),
        "not paired — reachable on this machine only",
        "screen:\n{screen}"
    );
    for step in [
        "To reach this host from the contenox app:",
        "1. Sign in at https://app.contenox.com and tap Pair device",
        "2. contenox pair <key>",
        "3. restart this host",
    ] {
        assert!(
            screen.contains(step),
            "the pairing funnel is missing {step:?}:\n{screen}"
        );
    }
    assert!(
        pty.wait_exit(Duration::from_millis(500)).is_err(),
        "an unpaired host is reachable on this machine, so it keeps running:\n{screen}"
    );
    pty.send_ctrl('c').ok();
}

#[test]
fn a_paired_host_names_the_relay_the_instance_and_the_app() {
    let cx = ready_host_instance("serve-paired");
    pair_the_machine(&cx, "token-for-the-paired-screen");

    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    assert_eq!(
        field(&screen, "Relay"),
        "attached to https://127.0.0.1:9",
        "screen:\n{screen}"
    );
    assert_eq!(
        field(&screen, "Instance"),
        "ca2e8376-99ab-4669-9ca4-b32e5605bb4d",
        "screen:\n{screen}"
    );
    assert_eq!(
        field(&screen, "App"),
        "https://127.0.0.1:9",
        "the screen must name the URL a human opens:\n{screen}"
    );
    assert!(
        !screen.contains("contenox pair <key>"),
        "a paired host must not still be asking to be paired:\n{screen}"
    );
}

#[test]
fn the_instance_token_never_reaches_the_screen_or_the_log_files() {
    const TOKEN: &str = "instance-token-that-must-never-be-printed-9f3c";
    let cx = ready_host_instance("serve-token");
    pair_the_machine(&cx, TOKEN);
    let logs = log_dir(&cx, "hostlogs");

    let mut pty = host(&cx, &[".", "--log-dir", logs.to_str().expect("log dir")]);
    // Long enough for the relay dial to fail and be reported, which is where a
    // credential would leak if it leaked anywhere.
    std::thread::sleep(Duration::from_secs(3));
    let screen = pty.screen();
    pty.send_ctrl('c').ok();
    pty.wait_exit(Duration::from_secs(30))
        .expect("the host stops");

    assert!(
        screen.contains("attached to https://127.0.0.1:9"),
        "the case proves nothing unless the host was paired:\n{screen}"
    );
    assert!(
        !screen.contains(TOKEN),
        "the instance token reached the screen:\n{screen}"
    );
    let written = read_logs(&logs);
    assert!(
        written.contains("connect"),
        "the case proves nothing unless the host used the credential:\n{written}"
    );
    assert!(
        !written.contains(TOKEN),
        "the instance token reached {}",
        logs.display()
    );
}

// ============================================================ stopping

#[test]
fn ctrl_c_stops_the_host() {
    let cx = ready_host_instance("serve-ctrlc");
    let mut pty = host(&cx, &["."]);

    pty.send_ctrl('c').expect("send Ctrl-C");

    let screen = pty
        .wait_for(STOPPING, Duration::from_secs(30))
        .expect("a stopping host must say the app can no longer reach the machine");
    assert_eq!(
        pty.wait_exit(Duration::from_secs(30))
            .expect("the host exits"),
        Some(0),
        "stopping a host on purpose is not a failure:\n{screen}"
    );
}

#[test]
fn a_host_started_without_a_terminal_prints_its_status_screen_and_stops_on_a_signal() {
    let cx = ready_host_instance("serve-headless");
    let logs = log_dir(&cx, "hostlogs");

    // The shape a service manager starts: no terminal, stdin closed, stdout a
    // pipe. Nothing can be read back until it exits, so the readiness gate is
    // the log file, which the host opens after installing its stop handler.
    let running = cx
        .cmd(["serve", ".", "--log-dir", logs.to_str().expect("log dir")])
        .stdin("")
        .timeout(Duration::from_secs(120))
        .start()
        .expect("spawn contenox serve");
    cx.wait_for(Duration::from_secs(60), |_| !host_logs(&logs).is_empty())
        .expect("the host never opened its log file");
    std::thread::sleep(Duration::from_secs(3));

    running.interrupt();
    let out = running
        .wait_timeout(Duration::from_secs(60))
        .expect("the host exits after a signal")
        .ok();

    assert_eq!(
        field(&out.stdout, "Workspace"),
        cx.work().display().to_string(),
        "a redirected host prints its whole screen to stdout:\n{}",
        out.render()
    );
    out.expect_stdout(RUNNING).expect_stdout(STOPPING);
}

#[test]
fn stopping_a_host_leaves_the_machine_paired() {
    let cx = ready_host_instance("serve-stop-paired");
    pair_the_machine(&cx, "token-that-outlives-the-host");

    let mut pty = host(&cx, &["."]);
    pty.send_ctrl('c').expect("send Ctrl-C");
    pty.wait_exit(Duration::from_secs(30))
        .expect("the host exits");

    // Read back through the command that owns the pairing.
    cx.run(["pair"])
        .ok()
        .expect_stdout("Paired with https://127.0.0.1:9.")
        .expect_stdout("Instance ca2e8376-99ab-4669-9ca4-b32e5605bb4d")
        .refute_stdout("token-that-outlives-the-host");
}

// ============================================================ the log files

#[test]
fn host_logs_land_in_the_data_dir_named_for_the_day_they_hold() {
    let cx = ready_host_instance("serve-logdir");
    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    let logs = cx.home_file("logs");
    let files = host_logs(&logs);
    assert_eq!(
        files.len(),
        1,
        "one boot writes one part in {}: {files:?}",
        logs.display()
    );
    let (day, part) = parse_part(&files[0]).expect("a dated log name");
    assert_eq!(part, 1, "the day's first part carries no number: {files:?}");

    let first = std::fs::read_to_string(logs.join(&files[0])).expect("read the log");
    let stamp = first
        .lines()
        .next()
        .and_then(|line| line.strip_prefix("time="))
        .map(|rest| rest[..10].to_string())
        .expect("a log line carries its timestamp");
    assert_eq!(
        stamp, day,
        "the file is named for the day whose records it holds"
    );
    assert!(
        screen.contains(&logs.join(&files[0]).display().to_string()),
        "the screen must name the log file it opened:\n{screen}"
    );
}

#[test]
fn a_log_past_its_size_bound_continues_in_numbered_parts() {
    let cx = ready_host_instance("serve-parts");
    let logs = log_dir(&cx, "hostlogs");
    cx.run(["config", "set", "log-max-size", "1KB"]).ok();
    cx.run(["config", "set", "log-max-files", "0"]).ok();

    let mut pty = host(&cx, &[".", "--log-dir", logs.to_str().expect("log dir")]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    let files = host_logs(&logs);
    let parts: Vec<u32> = files
        .iter()
        .map(|name| parse_part(name).expect("a dated log name").1)
        .collect();
    assert!(
        parts.len() >= 3,
        "a boot writing past a 1KB bound must open numbered parts, got {files:?}"
    );
    assert_eq!(
        parts,
        (1..=parts.len() as u32).collect::<Vec<_>>(),
        "parts must run 1, 2, 3 … with no gap: {files:?}"
    );
    let (day, _) = parse_part(&files[0]).expect("a dated log name");
    assert_eq!(files[0], format!("serve-{day}.log"));
    assert_eq!(files[1], format!("serve-{day}.2.log"));
    assert!(
        screen.contains("new part at 1KB · keep every file"),
        "the screen must state the bounds it is keeping:\n{screen}"
    );
}

#[test]
fn restarting_a_host_continues_the_current_log_part() {
    let cx = ready_host_instance("serve-restart");
    let logs = log_dir(&cx, "hostlogs");
    let arg = logs.to_str().expect("log dir");

    let mut first = host(&cx, &[".", "--log-dir", arg]);
    first.send_ctrl('c').ok();
    first
        .wait_exit(Duration::from_secs(30))
        .expect("first host exits");
    let after_one = host_logs(&logs);
    assert_eq!(after_one.len(), 1, "one boot, one part: {after_one:?}");
    let size_one = std::fs::metadata(logs.join(&after_one[0]))
        .expect("log size")
        .len();

    let mut second = host(&cx, &[".", "--log-dir", arg]);
    second.send_ctrl('c').ok();
    second
        .wait_exit(Duration::from_secs(30))
        .expect("second host exits");

    let after_two = host_logs(&logs);
    assert_eq!(
        after_two, after_one,
        "a restart continues the current part rather than opening a file per launch"
    );
    let size_two = std::fs::metadata(logs.join(&after_two[0]))
        .expect("log size")
        .len();
    assert!(
        size_two > size_one,
        "the second boot must have appended to {} ({size_one} -> {size_two} bytes)",
        after_two[0]
    );
}

#[test]
fn log_retention_spares_files_the_host_does_not_own_and_the_one_it_is_writing() {
    let cx = ready_host_instance("serve-retention");
    let logs = log_dir(&cx, "hostlogs");

    // Three files this host did not write, one of them as old as they come, and
    // one of its own from another century as the control that retention ran.
    for (name, body) in [
        ("notes.txt", "an operator's own note"),
        ("beam-2001-01-01.log", "another surface's log"),
        ("serve-notadate.log", "not this log's naming"),
        ("serve-2001-01-01.log", "this host's own, long expired"),
    ] {
        std::fs::write(logs.join(name), body).expect("plant a file");
    }
    cx.run(["config", "set", "log-max-size", "1KB"]).ok();
    cx.run(["config", "set", "log-max-files", "1"]).ok();
    cx.run(["config", "set", "log-max-age-days", "1"]).ok();

    let mut pty = host(&cx, &[".", "--log-dir", logs.to_str().expect("log dir")]);
    pty.send_ctrl('c').ok();
    pty.wait_exit(Duration::from_secs(30))
        .expect("the host exits");

    let left = dir_entries(&logs);
    assert!(
        !left.contains(&"serve-2001-01-01.log".to_string()),
        "retention did not run, so this case proves nothing: {left:?}"
    );
    for spared in ["notes.txt", "beam-2001-01-01.log", "serve-notadate.log"] {
        assert!(
            left.contains(&spared.to_string()),
            "a host must not delete {spared}, which it does not own: {left:?}"
        );
    }

    let own = host_logs(&logs);
    assert_eq!(
        own.len(),
        1,
        "keep 1 file means one of its own is left: {left:?}"
    );
    assert!(
        std::fs::metadata(logs.join(&own[0]))
            .expect("log size")
            .len()
            > 0,
        "the part the host was writing must survive its own retention, with its records"
    );
}

// ============================================================ known defect

#[test]
#[ignore = "confirmed defect: serve resolves its workspace ID from ~/.contenox instead of the workspace it serves, so the screen's Workspace and ID rows describe two different workspaces and every session the host opens is recorded outside the workspace an operator's own commands read. Seam: runACPProfile's workspaceID = ResolveWorkspaceID(globalContenoxDir())."]
fn serve_names_the_workspace_id_of_the_workspace_it_serves() {
    let cx = ready_host_instance("serve-workspace-id");

    // What every contenox command standing in this directory calls the workspace.
    cx.run(["session", "new", "probe"]).ok();
    let rows = cx.sessions_all().expect("contenox session list --all");
    let workspace = rows
        .iter()
        .find(|row| row.name == "probe")
        .map(|row| row.workspace.clone())
        .expect("the session lands in a workspace");

    let mut pty = host(&cx, &["."]);
    let screen = pty.screen();
    pty.send_ctrl('c').ok();

    assert_eq!(
        field(&screen, "ID"),
        workspace,
        "the host serves {} and must name that workspace's ID:\n{screen}",
        cx.work().display()
    );
}
