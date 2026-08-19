//! Pairing, the relay and the phone — `contenox pair` / `contenox unpair`, the
//! `/pair`, `/unpair` and `/link` slash commands, and what a paired machine
//! puts on the wire.
//!
//! Redemption is one POST, so the relay here is a loopback stand-in: it
//! listens on 127.0.0.1, answers `/v1/pair/redeem`, and records everything it
//! is sent. That is what makes the privacy claims checkable — "exactly two
//! things are sent" has no observer on the machine's own side — while still
//! needing no network and no credentials.
//!
//! The relay's own decisions (a key's lifetime, its single use, revoking an
//! instance) are the relay's, not the binary's. What is asserted here is what
//! the machine sends, what it stores, what it shows, and how it reports a
//! refusal.

use contenox_e2e::{Acp, CmdOutput, Instance, Script};
use serde_json::Value;
use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

// ---------------------------------------------------------------- the relay

/// The stand-in relay, its port, and the journal of everything it saw.
struct Relay {
    child: Child,
    port: u16,
    journal: PathBuf,
}

impl Relay {
    fn start(cx: &Instance, args: &[&str]) -> Relay {
        Relay::start_named(cx, "relay-journal.jsonl", args)
    }

    fn start_named(cx: &Instance, journal: &str, args: &[&str]) -> Relay {
        let journal = cx.root().join(journal);
        let mut child = Command::new(env!("CARGO_BIN_EXE_relay_stub"))
            .arg(&journal)
            .args(args)
            .stdout(Stdio::piped())
            .spawn()
            .expect("spawn the stand-in relay");

        let mut line = String::new();
        BufReader::new(child.stdout.take().expect("the relay kept no stdout"))
            .read_line(&mut line)
            .expect("the relay announces its port");
        let port = line
            .trim()
            .strip_prefix("PORT=")
            .and_then(|port| port.parse().ok())
            .unwrap_or_else(|| panic!("the relay announced {line:?}"));

        Relay {
            child,
            port,
            journal,
        }
    }

    /// The address a pairing is redeemed against.
    fn url(&self) -> String {
        format!("http://127.0.0.1:{}", self.port)
    }

    /// The same address as a machine stores it for dialling out.
    fn dial_url(&self) -> String {
        format!("https://127.0.0.1:{}", self.port)
    }

    fn entries(&self) -> Vec<Value> {
        let text = std::fs::read_to_string(&self.journal).unwrap_or_default();
        text.lines()
            .filter(|line| !line.trim().is_empty())
            .map(|line| serde_json::from_str(line).expect("the journal holds JSON lines"))
            .collect()
    }

    /// Every HTTP request it was sent, in order.
    fn requests(&self) -> Vec<Value> {
        self.entries()
            .into_iter()
            .filter(|entry| entry["event"] == "request")
            .collect()
    }

    /// Every connection a machine opened to dial in.
    fn dials(&self) -> usize {
        self.entries()
            .iter()
            .filter(|entry| entry["event"] == "dial")
            .count()
    }

    fn await_dial(&self, timeout: Duration) {
        let deadline = Instant::now() + timeout;
        while Instant::now() < deadline {
            if self.dials() > 0 {
                return;
            }
            std::thread::sleep(Duration::from_millis(200));
        }
        panic!("nothing dialled the relay within {timeout:?}");
    }
}

impl Drop for Relay {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

// ---------------------------------------------------------------- the machine

/// This machine's name, which is the only thing about it a pairing carries.
fn hostname() -> String {
    let mut buf = vec![0u8; 256];
    // SAFETY: gethostname fills at most buf.len() bytes of a buffer we own.
    let rc = unsafe { libc::gethostname(buf.as_mut_ptr() as *mut libc::c_char, buf.len()) };
    assert_eq!(rc, 0, "this machine has no hostname to pair under");
    let end = buf.iter().position(|byte| *byte == 0).unwrap_or(buf.len());
    String::from_utf8_lossy(&buf[..end]).into_owned()
}

fn machine(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx
}

/// A machine whose model can answer, for the shapes that refuse to start
/// without one.
fn machine_with_a_model(label: &str) -> Instance {
    let cx = machine(label);
    cx.scripted(&Script::new().turn("this case never asks the model anything"))
        .expect("scripted-test backend");
    cx
}

/// Where a pairing lives: one file, in the machine's home, never in a project.
fn pairing_file(cx: &Instance) -> PathBuf {
    cx.home_file("relay.json")
}

fn stored_pairing(cx: &Instance) -> Value {
    let text = std::fs::read_to_string(pairing_file(cx)).expect("the stored pairing");
    serde_json::from_str(&text).expect("the pairing file is JSON")
}

/// `contenox pair <key>`, redeeming against the relay the environment names.
fn pair_with(cx: &Instance, relay: &Relay, key: &str) -> CmdOutput {
    cx.cmd(["pair", key])
        .env("CONTENOX_RELAY_ENDPOINT", relay.url())
        .output()
        .expect("run contenox pair")
}

/// A pairing already on the machine, stored the way redemption stores one.
/// Used by the cases about dialling out, which need an endpoint a dial can
/// actually reach rather than one a redemption can.
fn plant_pairing(cx: &Instance, endpoint: &str) {
    let creds = format!(
        r#"{{
  "endpoint": "{endpoint}",
  "instance_token": "instance-token-e2e",
  "instance_id": "instance-1",
  "account_id": "acct-e2e",
  "relay_public_key": "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE="
}}
"#
    );
    std::fs::create_dir_all(cx.home_file(".")).expect("the machine's contenox directory");
    std::fs::write(pairing_file(cx), creds).expect("store the pairing");
}

/// An editor session on this machine, ready to be typed a slash command.
fn session(cx: &Instance) -> (Acp, String) {
    let mut acp = cx.acp(["acp"]).expect("spawn the ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    (acp, session)
}

// ============================================================ not paired at all

#[test]
fn pair_with_no_key_on_an_unpaired_machine_prints_the_two_step_funnel() {
    let cx = machine("pair-unpaired");

    let out = cx.run(["pair"]).ok();
    assert!(
        out.stdout_has("This machine is not paired with a relay."),
        "{}",
        out.render()
    );
    for step in [
        "1. Sign in at https://app.contenox.com and tap Pair device",
        "2. contenox pair <key>",
    ] {
        assert!(
            out.stdout_has(step),
            "the funnel is missing {step:?}\n{}",
            out.render()
        );
    }
    assert!(
        !pairing_file(&cx).exists(),
        "asking what a machine is attached to must not attach it"
    );
}

#[test]
fn unpair_on_an_unpaired_machine_reports_nothing_to_do_and_exits_zero() {
    let cx = machine("unpair-unpaired");

    cx.run(["unpair"])
        .ok()
        .expect_stdout("This machine is not paired with a relay — nothing to do.");
    assert!(
        !pairing_file(&cx).exists(),
        "unpairing an unpaired machine must not create a pairing"
    );
}

#[test]
fn slash_pair_reports_the_unpaired_state_from_inside_a_session() {
    let cx = machine_with_a_model("slash-pair-unpaired");
    let (mut acp, sid) = session(&cx);

    let turn = acp.prompt(&sid, "/pair").expect("session/prompt");
    let said = turn.text();
    assert!(
        said.contains("This machine is not paired with a relay."),
        "/pair said: {said:?}"
    );
    assert!(
        said.contains("/pair <key>"),
        "it must name the command that pairs it: {said:?}"
    );
    assert!(
        !pairing_file(&cx).exists(),
        "reporting the pairing must not create one"
    );
    acp.close().ok();
}

#[test]
fn slash_link_on_an_unpaired_machine_points_at_pairing_instead_of_a_url() {
    let cx = machine_with_a_model("slash-link-unpaired");
    let (mut acp, sid) = session(&cx);

    let said = acp.prompt(&sid, "/link").expect("session/prompt").text();
    assert!(
        said.contains("not paired with a relay, so this session has no app link"),
        "/link said: {said:?}"
    );
    assert!(
        said.contains("/pair <key>"),
        "an unpaired /link must point at /pair: {said:?}"
    );
    assert!(
        !said.contains("http://") && !said.contains("https://"),
        "there is no link to print, so it must print none: {said:?}"
    );
    acp.close().ok();
}

#[test]
fn nothing_reaches_the_relay_until_a_key_is_typed() {
    let cx = machine("relay-silent");
    let relay = Relay::start(&cx, &[]);
    let endpoint = relay.url();

    for argv in [
        vec!["init"],
        vec!["pair"],
        vec!["unpair"],
        vec!["doctor"],
        vec!["session", "list"],
    ] {
        cx.cmd(&argv)
            .env("CONTENOX_RELAY_ENDPOINT", &endpoint)
            .output()
            .expect("run the command");
    }

    assert!(
        relay.entries().is_empty(),
        "a configured relay address is not a connection — nothing may be sent \
         before a key is redeemed, but the relay saw {:#?}",
        relay.entries()
    );

    // The same environment, with a key in it, is heard immediately: the
    // silence above is the product's, not a variable that never arrived.
    pair_with(&cx, &relay, "K7M-3PQ").ok();
    assert_eq!(relay.requests().len(), 1);
}

// ============================================================ redeeming a key

#[test]
fn pair_redeems_a_key_against_the_relay_the_environment_names() {
    let cx = machine("pair-redeem");
    let relay = Relay::start(&cx, &["--accept", "K7M-3PQ", "--instance", "inst-42"]);

    let out = pair_with(&cx, &relay, "K7M-3PQ").ok();
    assert!(
        out.stdout_has(&format!("Paired as {:?} (instance inst-42).", hostname())),
        "{}",
        out.render()
    );
    out.clone()
        .expect_stdout("Attached to account acct-e2e.")
        .expect_stdout(&format!("Relay: {}", relay.url()))
        .expect_stdout(&format!(
            "Stored in {} — 'contenox unpair' removes it.",
            pairing_file(&cx).display()
        ))
        .expect_stdout(&format!("Open {} to reach this machine.", relay.url()))
        .expect_stdout("Keep it reachable with: contenox serve");

    let redeems = relay.requests();
    assert_eq!(redeems.len(), 1, "one key, one call: {redeems:#?}");
    assert_eq!(redeems[0]["method"], "POST");
    assert_eq!(redeems[0]["path"], "/v1/pair/redeem");
}

#[test]
fn an_inline_endpoint_overrides_the_environment() {
    let cx = machine("pair-inline-endpoint");
    let configured = Relay::start_named(&cx, "configured.jsonl", &[]);
    let named = Relay::start_named(&cx, "named.jsonl", &["--instance", "inst-inline"]);

    cx.cmd(["pair", "K7M-3PQ", &named.url()])
        .env("CONTENOX_RELAY_ENDPOINT", configured.url())
        .output()
        .expect("run contenox pair")
        .ok()
        .expect_stdout("(instance inst-inline)");

    assert_eq!(
        named.requests().len(),
        1,
        "the relay named on the command line is the one redeemed against"
    );
    assert!(
        configured.entries().is_empty(),
        "the configured relay must hear nothing: {:#?}",
        configured.entries()
    );
    assert_eq!(stored_pairing(&cx)["endpoint"], Value::from(named.url()));
}

#[test]
fn redeeming_sends_the_key_and_this_machines_hostname_and_nothing_else() {
    let cx = machine("pair-privacy");
    let relay = Relay::start(&cx, &[]);

    // Something on the machine worth not leaking: a workspace with a name.
    cx.write_file("secret-project/notes.txt", "the customer's name\n")
        .expect("a file on the machine");

    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let requests = relay.requests();
    assert_eq!(requests.len(), 1, "one call: {requests:#?}");
    let sent: Value = serde_json::from_str(requests[0]["body"].as_str().expect("a body"))
        .expect("the redemption body is JSON");
    let mut fields: Vec<&String> = sent.as_object().expect("an object").keys().collect();
    fields.sort();
    assert_eq!(
        fields,
        vec!["instance_name", "key"],
        "a redemption sends the key and the hostname, nothing else: {sent:#}"
    );
    assert_eq!(sent["key"], "K7M-3PQ");
    assert_eq!(sent["instance_name"], Value::from(hostname()));

    let whole = serde_json::to_string(&requests[0]).expect("the recorded request");
    for leak in [
        "secret-project",
        "notes.txt",
        cx.work().display().to_string().as_str(),
    ] {
        assert!(
            !whole.contains(leak),
            "nothing about this machine's files may ride along, found {leak:?} in {whole}"
        );
    }
}

#[test]
fn the_credential_lands_in_the_machines_pairing_file_not_beside_the_project() {
    let cx = machine("pair-storage");
    let relay = Relay::start(&cx, &["--token", "instance-token-9f3c"]);

    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let stored = stored_pairing(&cx);
    assert_eq!(stored["endpoint"], Value::from(relay.url()));
    assert_eq!(stored["instance_id"], "instance-1");
    assert_eq!(stored["account_id"], "acct-e2e");
    assert_eq!(stored["instance_token"], "instance-token-9f3c");
    assert_eq!(
        stored["relay_public_key"], "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE=",
        "the key the machine recognises that relay by from now on"
    );

    assert!(
        !cx.work().join(".contenox/relay.json").exists(),
        "a pairing describes the machine, so it may not land beside a project"
    );
}

#[test]
fn a_pairing_made_in_a_session_is_the_one_every_later_process_finds() {
    let cx = machine_with_a_model("pair-machine-wide");
    let relay = Relay::start(&cx, &["--instance", "inst-shared"]);
    let (mut acp, sid) = session(&cx);

    let said = acp
        .prompt(&sid, &format!("/pair K7M-3PQ {}", relay.url()))
        .expect("session/prompt")
        .text();
    assert!(
        said.contains("(instance inst-shared)"),
        "/pair said: {said:?}"
    );
    acp.close().ok();

    // Another process, standing somewhere else entirely.
    let elsewhere = cx
        .cmd(["pair"])
        .cwd(cx.root())
        .output()
        .expect("run contenox pair")
        .ok();
    assert!(
        elsewhere.stdout_has("Instance inst-shared, account acct-e2e."),
        "the pairing a session made is the machine's: {}",
        elsewhere.render()
    );
}

#[test]
fn pair_with_no_key_reports_relay_instance_and_account_without_the_credential() {
    const TOKEN: &str = "instance-token-that-must-never-be-printed-9f3c";
    let cx = machine("pair-status");
    let relay = Relay::start(&cx, &["--token", TOKEN]);
    pair_with(&cx, &relay, "K7M-3PQ").ok();
    let before = relay.entries().len();

    let out = cx.run(["pair"]).ok();
    out.clone()
        .expect_stdout(&format!("Paired with {}.", relay.url()))
        .expect_stdout("Instance instance-1, account acct-e2e.")
        .expect_stdout(&format!("App: {}", relay.url()))
        .expect_stdout("'contenox unpair' removes this pairing.");
    assert!(
        !out.stdout.contains(TOKEN) && !out.stderr.contains(TOKEN),
        "the credential must never be printed:\n{}",
        out.render()
    );
    assert!(
        !out.stdout.contains("contenox.com"),
        "a machine paired to its own relay is not shown the hosted service:\n{}",
        out.render()
    );
    assert_eq!(
        relay.entries().len(),
        before,
        "reporting a pairing changes nothing and asks nobody"
    );
}

#[test]
fn slash_pair_reports_the_pairing_without_the_credential() {
    const TOKEN: &str = "instance-token-that-must-never-reach-an-editor-9f3c";
    let cx = machine_with_a_model("slash-pair-status");
    let relay = Relay::start(&cx, &["--token", TOKEN, "--instance", "inst-in-session"]);
    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let (mut acp, sid) = session(&cx);
    let said = acp.prompt(&sid, "/pair").expect("session/prompt").text();

    assert!(
        said.contains(&format!("Paired with {}.", relay.url())),
        "/pair said: {said:?}"
    );
    assert!(
        said.contains("Instance inst-in-session, account acct-e2e."),
        "/pair said: {said:?}"
    );
    assert!(
        !said.contains(TOKEN),
        "a session is a place a credential would be logged and shown: {said:?}"
    );
    acp.close().ok();
}

// ============================================================ a refused key

#[test]
fn a_redeemed_key_is_refused_the_second_time_and_the_stored_pairing_survives() {
    let cx = machine("pair-single-use");
    let relay = Relay::start(&cx, &["--accept", "K7M-3PQ", "--instance", "inst-first"]);

    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let again = pair_with(&cx, &relay, "K7M-3PQ").expect_code(1);
    assert!(
        again.stderr_has("the relay refused the pairing key")
            && again.stderr_has("this pairing key has already been redeemed"),
        "the relay's own reason must reach the person who typed the key:\n{}",
        again.render()
    );
    assert_eq!(
        stored_pairing(&cx)["instance_id"],
        "inst-first",
        "a refused redemption may not disturb the pairing the machine already has"
    );
}

#[test]
fn slash_pair_surfaces_the_relays_refusal_and_names_the_remedy() {
    let cx = machine_with_a_model("slash-pair-refused");
    let relay = Relay::start(&cx, &["--accept", "K7M-3PQ"]);
    let (mut acp, sid) = session(&cx);

    let said = acp
        .prompt(&sid, &format!("/pair EXPIRED-KEY {}", relay.url()))
        .expect("session/prompt")
        .text();
    assert!(
        said.contains("the relay refused the pairing key"),
        "/pair said: {said:?}"
    );
    assert!(
        said.contains("it expired or was never minted"),
        "the relay's own reason must survive to the session: {said:?}"
    );
    assert!(
        said.contains("mint a new key in the app and try again"),
        "a refused key has one remedy and the session must name it: {said:?}"
    );
    assert!(!pairing_file(&cx).exists(), "a refused key pairs nothing");
    acp.close().ok();
}

#[test]
fn a_relay_that_hands_out_no_identity_key_pairs_nothing() {
    let cx = machine("pair-no-identity");
    let relay = Relay::start(&cx, &["--no-public-key"]);

    let out = pair_with(&cx, &relay, "K7M-3PQ").expect_failure();
    assert!(
        out.stderr_has("no way to tell that relay from any other"),
        "the machine verifies the relay by the key it was given, so it must \
         refuse a relay that hands out none:\n{}",
        out.render()
    );
    assert!(
        !pairing_file(&cx).exists(),
        "a refused answer must leave no half-written pairing"
    );
}

// ============================================================ unpairing

#[test]
fn unpair_deletes_the_local_credential_and_tells_the_relay_nothing() {
    let cx = machine("unpair-local");
    let relay = Relay::start(&cx, &[]);
    pair_with(&cx, &relay, "K7M-3PQ").ok();
    let after_pairing = relay.entries().len();

    let out = cx.run(["unpair"]).ok();
    out.clone()
        .expect_stdout(&format!("Removed {}.", pairing_file(&cx).display()))
        .expect_stdout("This machine will no longer dial the relay.")
        .expect_stdout("Revoking the instance is done in the app.");

    assert!(
        !pairing_file(&cx).exists(),
        "the credential file is what unpairing removes"
    );
    assert_eq!(
        relay.entries().len(),
        after_pairing,
        "unpairing is local: the relay is not told, which is why revoking is \
         a separate act in the app"
    );
    cx.run(["pair"])
        .ok()
        .expect_stdout("This machine is not paired with a relay.");
}

#[test]
fn slash_unpair_removes_the_pairing_every_process_on_the_machine_shares() {
    let cx = machine_with_a_model("slash-unpair");
    let relay = Relay::start(&cx, &[]);
    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let (mut acp, sid) = session(&cx);
    let said = acp.prompt(&sid, "/unpair").expect("session/prompt").text();
    assert!(
        said.contains(&format!("Removed {}.", pairing_file(&cx).display())),
        "/unpair said: {said:?}"
    );
    assert!(
        said.contains("Revoking the instance is done in the app."),
        "the session must say what it did not do: {said:?}"
    );
    acp.close().ok();

    cx.run(["pair"])
        .ok()
        .expect_stdout("This machine is not paired with a relay.");
}

// ============================================================ the phone

#[test]
fn slash_link_prints_the_url_that_opens_this_session_in_the_app() {
    let cx = machine_with_a_model("slash-link-paired");
    let relay = Relay::start(&cx, &["--instance", "inst-linked"]);
    pair_with(&cx, &relay, "K7M-3PQ").ok();

    let (mut acp, sid) = session(&cx);
    let said = acp.prompt(&sid, "/link").expect("session/prompt").text();
    let link = said.lines().next().unwrap_or_default().trim().to_string();

    assert_eq!(
        link,
        format!("{}/session/inst-linked/{sid}", relay.url()),
        "the link must name this instance and this session: {said:?}"
    );
    assert!(
        said.contains("sign-in required"),
        "a link is not an admission: {said:?}"
    );
    acp.close().ok();
}

// ============================================================ staying reachable

#[test]
fn a_paired_machine_with_nothing_running_dials_nobody() {
    let cx = machine("reach-idle");
    let relay = Relay::start(&cx, &[]);
    plant_pairing(&cx, &relay.dial_url());

    // Paired, and to this relay: what follows is about the machine being idle,
    // not about a pairing that failed to land.
    cx.run(["pair"])
        .ok()
        .expect_stdout(&format!("Paired with {}.", relay.dial_url()));
    cx.run(["doctor"]);
    cx.run(["session", "list"]);
    std::thread::sleep(Duration::from_secs(2));

    assert_eq!(
        relay.dials(),
        0,
        "pairing attaches the machine; it does not keep anything running, so \
         nothing dials until a session or a host does"
    );
}

#[test]
fn a_running_host_dials_the_relay_so_the_app_can_reach_it() {
    let cx = machine_with_a_model("reach-serve");
    let relay = Relay::start(&cx, &[]);
    plant_pairing(&cx, &relay.dial_url());

    let mut host = cx.pty(["serve", "."]).expect("spawn contenox serve");
    host.wait_for("Running. Press Ctrl-C to stop.", Duration::from_secs(90))
        .expect("the host never finished its status screen");

    relay.await_dial(Duration::from_secs(60));
    host.send_ctrl('c').ok();
}

#[test]
fn an_editor_session_dials_the_relay_the_same_way() {
    let cx = machine_with_a_model("reach-editor");
    let relay = Relay::start(&cx, &[]);
    plant_pairing(&cx, &relay.dial_url());

    let (mut acp, _sid) = session(&cx);
    relay.await_dial(Duration::from_secs(60));
    acp.close().ok();
}

#[test]
fn an_open_beam_session_dials_the_relay_too() {
    let cx = machine_with_a_model("reach-beam");
    let relay = Relay::start(&cx, &[]);
    plant_pairing(&cx, &relay.dial_url());

    let mut beam = cx.pty(["beam", "--plain"]).expect("spawn contenox beam");
    beam.wait_for("type / for commands", Duration::from_secs(90))
        .expect("beam never came up");

    relay.await_dial(Duration::from_secs(60));
    beam.send_ctrl('c').ok();
}
